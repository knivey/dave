package main

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReactionsMigrationCreatesTable(t *testing.T) {
	db := setupTestDB(t)

	var got string
	require.NoError(t, db.Get(&got,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'reactions'`))
	assert.Equal(t, "reactions", got, "reactions table must exist")

	// The PK (image_id, token, emoji) is what lets a token hold MANY
	// different reactions while holding each one at most once — pin
	// its shape.
	type col struct {
		CID     int         `db:"cid"`
		Name    string      `db:"name"`
		Type    string      `db:"type"`
		NotNull int         `db:"notnull"`
		Dflt    interface{} `db:"dflt_value"`
		PK      int         `db:"pk"`
	}
	var cols []col
	require.NoError(t, db.Select(&cols, `PRAGMA table_info(reactions)`))
	assert.Len(t, cols, 4)
	for _, c := range cols {
		assert.Equal(t, 1, c.NotNull, "%s is NOT NULL", c.Name)
	}
	byName := map[string]col{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	require.Len(t, byName, 4, "reactions has exactly image_id, token, emoji, created_at")
	assert.Equal(t, 1, byName["image_id"].PK)
	assert.Equal(t, 2, byName["token"].PK)
	assert.Equal(t, 3, byName["emoji"].PK, "emoji is the third PK column")
	assert.Equal(t, 0, byName["created_at"].PK)

	var idx string
	require.NoError(t, db.Get(&idx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_reactions_image_emoji'`))
	assert.Equal(t, "idx_reactions_image_emoji", idx, "count-lookup index must exist")
}

// reactionTestConfig is testConfig with a small deterministic preset
// (testConfig is a struct literal — normalize never runs, so tests
// that want reactions must supply the set explicitly).
func reactionTestConfig() Config {
	cfg := testConfig()
	cfg.Reactions = ReactionConfig{Emojis: []ReactionEmoji{
		{Name: "fire", Glyph: "\U0001F525"},
		{Name: "laugh", Glyph: "\U0001F602"},
		{Name: "wow", Glyph: "\U0001F62E"},
	}}
	return cfg
}

func TestToggleReactionRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "rxn0001", SHA256: "aa", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-06 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))

	// React, un-react, re-react with one emoji; a second token stacks.
	reacted, tally, err := dbToggleReaction(db, "rxn0001", "tok1", "fire")
	require.NoError(t, err)
	assert.True(t, reacted, "first toggle reacts")
	assert.Equal(t, map[string]int{"fire": 1}, tally)

	reacted, tally, err = dbToggleReaction(db, "rxn0001", "tok1", "fire")
	require.NoError(t, err)
	assert.False(t, reacted, "second toggle removes the reaction")
	assert.Empty(t, tally)

	reacted, tally, err = dbToggleReaction(db, "rxn0001", "tok1", "fire")
	require.NoError(t, err)
	assert.True(t, reacted, "third toggle re-reacts")

	reacted, tally, err = dbToggleReaction(db, "rxn0001", "tok2", "fire")
	require.NoError(t, err)
	assert.True(t, reacted)
	assert.Equal(t, map[string]int{"fire": 2}, tally, "independent tokens count independently")

	// MULTI-emoji cardinality: the same token adds different emojis
	// without releasing the ones it holds.
	reacted, tally, err = dbToggleReaction(db, "rxn0001", "tok2", "wow")
	require.NoError(t, err)
	assert.True(t, reacted, "second emoji from the same token lands")
	assert.Equal(t, map[string]int{"fire": 2, "wow": 1}, tally)
	reacted, tally, err = dbToggleReaction(db, "rxn0001", "tok2", "laugh")
	require.NoError(t, err)
	assert.True(t, reacted)
	assert.Equal(t, map[string]int{"fire": 2, "laugh": 1, "wow": 1}, tally)

	// Removing one emoji leaves the token's others (and other
	// tokens' reactions) untouched: tok1's fire survives, tok2's
	// does not.
	_, tally, err = dbToggleReaction(db, "rxn0001", "tok2", "fire")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"fire": 1, "laugh": 1, "wow": 1}, tally, "removing tok2's fire releases only tok2's fire")
}

func TestDbGetReactionState(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "rxn0002", SHA256: "bb", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-06 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))
	for _, e := range []string{"fire", "wow"} {
		_, _, err := dbToggleReaction(db, "rxn0002", "tok1", e)
		require.NoError(t, err)
	}
	_, _, err := dbToggleReaction(db, "rxn0002", "tok2", "fire")
	require.NoError(t, err)

	tally, mine, err := dbGetReactionState(db, "rxn0002", "tok1")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"fire": 2, "wow": 1}, tally)
	assert.Equal(t, map[string]bool{"fire": true, "wow": true}, mine)

	_, mine, err = dbGetReactionState(db, "rxn0002", "stranger")
	require.NoError(t, err)
	assert.Empty(t, mine, "unknown token holds nothing")

	// Unknown image: empty maps, not an error (no anchor — see
	// dbReactionTally's note).
	tally, mine, err = dbGetReactionState(db, "rxnZZZ", "tok1")
	require.NoError(t, err)
	assert.Empty(t, tally)
	assert.Empty(t, mine)
}

func TestHydrateReactionCounts(t *testing.T) {
	db := setupTestDB(t)
	ids := []string{"rhy0001", "rhy0002", "rhy0003"}
	for i, id := range ids {
		in := &dbImage{ID: id, SHA256: id, Filename: "f.webp", MimeType: "image/webp",
			SizeBytes: 1, CreatedAt: "2026-10-06 01:00:0" + string(rune('0'+i)), ThumbStatus: thumbStatusPending}
		require.NoError(t, dbInsertImage(db, in))
	}
	// rhy0001: fire x2 + wow x1; rhy0002: none; rhy0003: laugh x1.
	for _, tok := range []string{"a", "b"} {
		_, _, err := dbToggleReaction(db, "rhy0001", tok, "fire")
		require.NoError(t, err)
	}
	_, _, err := dbToggleReaction(db, "rhy0001", "c", "wow")
	require.NoError(t, err)
	_, _, err = dbToggleReaction(db, "rhy0003", "d", "laugh")
	require.NoError(t, err)

	rows := []dbImage{{ID: "rhy0001"}, {ID: "rhy0002"}, {ID: "rhy0003"}}
	ptrs := []*dbImage{&rows[0], &rows[1], &rows[2]}
	require.NoError(t, hydrateReactionCounts(db, ptrs))
	assert.Equal(t, map[string]int{"fire": 2, "wow": 1}, rows[0].Reactions)
	assert.Nil(t, rows[1].Reactions, "no reactions hydrates to nil")
	assert.Equal(t, map[string]int{"laugh": 1}, rows[2].Reactions)
}

func TestTopCardReactions(t *testing.T) {
	cfg := reactionTestConfig()
	// fire=1 laugh=2 wow=2 → top 2 by count are laugh and wow
	// (configured order breaks the tie in their favor over fire, and
	// orders laugh before wow despite equal counts).
	badges := topCardReactions(cfg, map[string]int{"fire": 1, "laugh": 2, "wow": 2})
	require.Len(t, badges, 2)
	assert.Equal(t, "laugh", badges[0].Name)
	assert.Equal(t, "wow", badges[1].Name)
	assert.Equal(t, "\U0001F602", badges[0].Glyph, "glyph comes from the config")

	// Dormant names (not configured) never badge.
	badges = topCardReactions(cfg, map[string]int{"sparkles": 9})
	assert.Empty(t, badges)

	// Zero counts never badge; empty tally is fine.
	assert.Empty(t, topCardReactions(cfg, map[string]int{"fire": 0}))
	assert.Empty(t, topCardReactions(cfg, nil))

	// Exactly at the cap: 3 configured, cap 2.
	badges = topCardReactions(cfg, map[string]int{"fire": 3, "laugh": 2, "wow": 1})
	require.Len(t, badges, 2)
	assert.Equal(t, "fire", badges[0].Name)
	assert.Equal(t, "laugh", badges[1].Name)
}

func TestReactionsGlyphJSON(t *testing.T) {
	got := reactionsGlyphJSON(reactionTestConfig())
	// Exact string: key ORDER is the configured order (the JS badge
	// tie-break depends on it), which JSONEq cannot pin.
	assert.Equal(t, `{"fire":"🔥","laugh":"😂","wow":"😮"}`, got)

	// Disabled feature embeds nothing.
	disabled := reactionTestConfig()
	f := false
	disabled.Reactions.Enabled = &f
	assert.Empty(t, reactionsGlyphJSON(disabled))
}

// ---------------------------------------------------------------------------
// Config: preset loading, defaults, validation
// ---------------------------------------------------------------------------

func TestLoadConfigReactions(t *testing.T) {
	// Absent section → built-in default set, enabled.
	path := writeConfigFile(t, "[auth]\napi_key = \"secret\"\n")
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	assert.True(t, cfg.Reactions.reactionsEnabled(), "absent section defaults to enabled")
	require.NotEmpty(t, cfg.Reactions.Emojis, "absent section fills the default preset")
	names := []string{}
	for _, e := range cfg.Reactions.Emojis {
		require.True(t, validReactionName(e.Name), "default names are valid")
		assert.NotEmpty(t, e.Glyph)
		names = append(names, e.Name)
	}
	assert.Contains(t, names, "fire")
	assert.Contains(t, names, "laugh")

	// Custom preset parses verbatim (order preserved).
	path = writeConfigFile(t, `[auth]
api_key = "secret"

[[reactions.emoji]]
name = "sparkles"
glyph = "✨"

[[reactions.emoji]]
name = "clown"
glyph = "🤡"
`)
	cfg, err = loadConfig(path)
	require.NoError(t, err)
	require.Len(t, cfg.Reactions.Emojis, 2)
	assert.Equal(t, "sparkles", cfg.Reactions.Emojis[0].Name)
	assert.Equal(t, "✨", cfg.Reactions.Emojis[0].Glyph)
	assert.Equal(t, "clown", cfg.Reactions.Emojis[1].Name)

	// Explicit disable.
	path = writeConfigFile(t, `[auth]
api_key = "secret"

[reactions]
enabled = false
`)
	cfg, err = loadConfig(path)
	require.NoError(t, err)
	assert.False(t, cfg.Reactions.reactionsEnabled())
	assert.Empty(t, cfg.Reactions.Emojis, "disabled feature loads no preset")
}

func TestLoadConfigReactionsInvalid(t *testing.T) {
	for _, tt := range []struct {
		name, section string
	}{
		{"bad name chars", `[[reactions.emoji]]
name = "Big Fire!"
glyph = "🔥"
`},
		{"uppercase name", `[[reactions.emoji]]
name = "Fire"
glyph = "🔥"
`},
		{"empty name", `[[reactions.emoji]]
name = ""
glyph = "🔥"
`},
		{"empty glyph", `[[reactions.emoji]]
name = "fire"
glyph = ""
`},
		{"all-digit name", `[[reactions.emoji]]
name = "123"
glyph = "🔥"
`},
		{"duplicated name", `[[reactions.emoji]]
name = "fire"
glyph = "🔥"

[[reactions.emoji]]
name = "fire"
glyph = "🔥"
`},
		{"too many entries", func() string {
			var b strings.Builder
			for i := 0; i < maxReactionEmojis+1; i++ {
				b.WriteString("[[reactions.emoji]]\nname = \"e" + string(rune('a'+i)) + "\"\nglyph = \"🔥\"\n")
			}
			return b.String()
		}()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, "[auth]\napi_key = \"secret\"\n"+tt.section)
			_, err := loadConfig(path)
			require.Error(t, err, "invalid preset must fail loadConfig (and thus SIGHUP reload)")
		})
	}
}

// ---------------------------------------------------------------------------
// POST /{id}/react/{emoji} — toggle handler
// ---------------------------------------------------------------------------

// postReact posts to the reaction toggle endpoint and returns the
// response (body NOT closed — tests either read it or pass through).
func postReact(t *testing.T, ts *httptest.Server, id, emoji, cookie, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/"+id+"/react/"+emoji, nil)
	require.NoError(t, err)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeReactionResponse(t *testing.T, resp *http.Response) reactionResponse {
	t.Helper()
	var payload reactionResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload
}

func TestReactionToggleJSONRoundTrip(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1000", "2026-10-06 01:00:00")

	// First reaction mints the SAME liker cookie the votes use.
	resp := postReact(t, ts, "rxn1000", "fire", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := likeCookieHeader(t, resp)
	rr := decodeReactionResponse(t, resp)
	assert.Equal(t, "fire", rr.Emoji)
	assert.True(t, rr.Reacted)
	assert.Equal(t, map[string]int{"fire": 1}, rr.Reactions)

	// Same token + same emoji toggles off.
	resp = postReact(t, ts, "rxn1000", "fire", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rr = decodeReactionResponse(t, resp)
	assert.False(t, rr.Reacted)
	assert.Empty(t, rr.Reactions)

	// Multi-emoji from one token stacks (unlike votes): the same
	// cookie reacts fire, then wow, holding both.
	resp = postReact(t, ts, "rxn1000", "fire", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = postReact(t, ts, "rxn1000", "wow", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rr = decodeReactionResponse(t, resp)
	assert.Equal(t, "wow", rr.Emoji)
	assert.Equal(t, map[string]int{"fire": 1, "wow": 1}, rr.Reactions, "the same token holds both")

	// And the vote identity is SHARED: the token that reacted can
	// also like — one cookie, both features.
	resp2 := postLike(t, ts, "rxn1000", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	vr := decodeVoteResponse(t, resp2)
	assert.True(t, vr.Liked)
}

func TestReactionToggleUnknownEmoji(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1010", "2026-10-06 01:00:00")

	resp := postReact(t, ts, "rxn1010", "sparkles", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "emoji outside the configured set 404s")

	// Nothing was written.
	tally, err := dbReactionTally(app.db, "rxn1010")
	require.NoError(t, err)
	assert.Empty(t, tally)
}

func TestReactionToggleDisabledFeature(t *testing.T) {
	// Struct literal (normalize never ran): an explicit disable with a
	// leftover list still 404s.
	cfg := testConfig()
	f := false
	cfg.Reactions.Enabled = &f
	cfg.Reactions.Emojis = []ReactionEmoji{{Name: "fire", Glyph: "\U0001F525"}}
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1011", "2026-10-06 01:00:00")

	resp := postReact(t, ts, "rxn1011", "fire", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "disabled feature 404s the endpoint")
	tally, err := dbReactionTally(app.db, "rxn1011")
	require.NoError(t, err)
	assert.Empty(t, tally)
}

// TestReactionDisabledDropsPreset pins normalize's disable semantics:
// enabled=false discards even an explicitly configured list, so every
// surface (card badges included, which read the list without their
// own flag check) agrees. This is the loadConfig path — what a
// SIGHUP reload actually produces.
func TestReactionDisabledDropsPreset(t *testing.T) {
	path := writeConfigFile(t, `[auth]
api_key = "secret"

[reactions]
enabled = false

[[reactions.emoji]]
name = "fire"
glyph = "🔥"
`)
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	assert.False(t, cfg.Reactions.reactionsEnabled())
	assert.Empty(t, cfg.Reactions.Emojis, "disabled config drops the preset list")
	assert.Empty(t, reactionsGlyphJSON(cfg))

	// And no badges render from a disabled config even with tally
	// rows present (belt-and-braces: topCardReactions also gates).
	assert.Empty(t, topCardReactions(cfg, map[string]int{"fire": 5}))
}

func TestReactionToggleFormRedirectsBack(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	ts.Client().CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	insertImage(t, app, "rxn1012", "2026-10-06 01:00:00")

	resp := postReact(t, ts, "rxn1012", "fire", "", "")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/rxn1012", resp.Header.Get("Location"), "redirect returns to the details page")
	require.NotEmpty(t, resp.Cookies(), "cookie is minted on the form path too")
}

func TestReactionToggleNotFoundAndGone(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1013", "2026-10-06 01:00:00", func(img *dbImage) {
		img.Hidden = true
	})

	resp := postReact(t, ts, "zzzzzzz", "fire", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "unknown id 404s")

	resp = postReact(t, ts, "rxn1013", "fire", "", "application/json")
	assert.Equal(t, http.StatusGone, resp.StatusCode, "hidden id 410s")
}

func TestReactionToggleSiteMatrix(t *testing.T) {
	cfg := safeSiteTestConfig()
	cfg.Reactions = reactionTestConfig().Reactions
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1014", "2026-10-06 01:00:00", func(img *dbImage) {
		img.Network = ptrStr("efnet") // invisible on the safe host
	})

	req, err := http.NewRequest("POST", ts.URL+"/rxn1014/react/fire", nil)
	require.NoError(t, err)
	req.Host = "safe.example.com"
	req.Header.Set("Accept", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "safe host 404s invisible rows")
	tally, err := dbReactionTally(app.db, "rxn1014")
	require.NoError(t, err)
	assert.Empty(t, tally, "404 path writes no reaction")

	resp = postReact(t, ts, "rxn1014", "fire", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode, "default host serves normally")
	rr := decodeReactionResponse(t, resp)
	assert.Equal(t, map[string]int{"fire": 1}, rr.Reactions)
}

func TestReactionToggleMethodGuard(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1015", "2026-10-06 01:00:00")

	resp, err := http.Get(ts.URL + "/rxn1015/react/fire")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "GET is the catch-all's 404")
	tally, err := dbReactionTally(app.db, "rxn1015")
	require.NoError(t, err)
	assert.Empty(t, tally, "GET performed no reaction")
}

// TestReactionToggleSharesVoteBucket pins that reactions ride the
// SAME per-IP bucket as the vote endpoints (one identity surface, one
// abuse budget).
func TestReactionToggleSharesVoteBucket(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1016", "2026-10-06 01:00:00")

	saw429 := false
	for i := 0; i < likesPerMinute+10 && !saw429; i++ {
		req, err := http.NewRequest("POST", ts.URL+"/rxn1016/like", nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Forwarded-For", "198.51.100.7")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		saw429 = resp.StatusCode == http.StatusTooManyRequests
	}
	require.True(t, saw429, "hammering the votes exhausts the bucket")

	req, err := http.NewRequest("POST", ts.URL+"/rxn1016/react/fire", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "reactions share the vote bucket")
}

// ---------------------------------------------------------------------------
// SSE
// ---------------------------------------------------------------------------

func TestReactionTogglePublishesImageReacted(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1030", "2026-10-06 01:00:00")

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	resp := postReact(t, ts, "rxn1030", "fire", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := likeCookieHeader(t, resp)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageReacted, f.name)
	var p imageReactedEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, "rxn1030", p.ID)
	assert.Equal(t, map[string]int{"fire": 1}, p.Reactions)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &raw))
	assert.ElementsMatch(t, []string{"id", "reactions"}, mapKeys(raw), "payload fields are exactly id+reactions")

	// Commit-before-publish: re-querying at event-observation time
	// must already see the reaction.
	tally, err := dbReactionTally(app.db, "rxn1030")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"fire": 1}, tally)

	// A removal publishes the emptied tally. (Fresh variable on
	// purpose: json.Unmarshal MERGES into a non-nil map, so reusing p
	// would keep the stale fire key and lie about the payload.)
	resp = postReact(t, ts, "rxn1030", "fire", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	f = nextSSEEvent(t, frames)
	require.Equal(t, eventImageReacted, f.name)
	var p2 imageReactedEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p2))
	assert.Empty(t, p2.Reactions, "un-react publishes the empty tally")
}

// TestReactionToggleSiteFiltersImageReacted pins the per-site delivery
// rule: a reaction on an image invisible on the safe host reaches
// default-site subscribers only.
func TestReactionToggleSiteFiltersImageReacted(t *testing.T) {
	cfg := safeSiteTestConfig()
	cfg.Reactions = reactionTestConfig().Reactions
	app := newTestApp(t, cfg)
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1031", "2026-10-06 01:00:00", func(img *dbImage) {
		img.Network = ptrStr("efnet")
	})

	defFrames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, defFrames) // retry hint
	safeFrames, _, _ := openEventsHost(t, ts, "safe.example.com", nil, "")
	nextSSEFrame(t, safeFrames) // retry hint

	resp := postReact(t, ts, "rxn1031", "fire", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	f := nextSSEEvent(t, defFrames)
	require.Equal(t, eventImageReacted, f.name)

	for _, f := range drainSSEFrames(t, safeFrames) {
		assert.NotEqual(t, eventImageReacted, f.name, "safe stream never hears about invisible images")
	}
}

// TestPublishImageReactedNilHubSafe mirrors TestPublishesAreNilHubSafe.
func TestPublishImageReactedNilHubSafe(t *testing.T) {
	app := newTestApp(t, reactionTestConfig()) // no hub attached
	app.publishImageReacted(&dbImage{ID: "rxn1032", Safety: safetyUnknown}, map[string]int{"fire": 1})
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func TestDetailsPageReactionButtons(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1040", "2026-10-06 01:00:00")

	// Anonymous visit: full button row, zero counts shown (the
	// buttons are the affordance), no pressed states.
	status, body := getPage(t, ts.URL, "/rxn1040")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `<form class="react-form" method="post">`)
	assert.Contains(t, body, `data-emoji="fire" formaction="/rxn1040/react/fire"`)
	assert.Contains(t, body, `data-emoji="laugh" formaction="/rxn1040/react/laugh"`)
	assert.Contains(t, body, `data-emoji="wow" formaction="/rxn1040/react/wow"`)
	assert.Contains(t, body, `<span class="react-count" data-emoji="fire">0</span>`)
	assert.NotContains(t, body, `class="react-btn reacted"`)

	// data-reactions embed (attribute-escaped JSON on the wire).
	assert.Contains(t, body, `data-reactions="`)
	assert.Contains(t, html.UnescapeString(body), `{"fire":"🔥","laugh":"😂","wow":"😮"}`)

	// A visitor holding fire + wow sees those pressed, their counts.
	tok := "0123456789abcdef0123456789abcdef"
	_, _, err := dbToggleReaction(app.db, "rxn1040", tok, "fire")
	require.NoError(t, err)
	_, _, err = dbToggleReaction(app.db, "rxn1040", tok, "wow")
	require.NoError(t, err)
	status, body = getPageCookie(t, ts.URL, "/rxn1040", likeCookieName+"="+tok)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, 2, strings.Count(body, `class="react-btn reacted"`), "exactly the held emojis press")
	assert.Contains(t, body, `<span class="react-count" data-emoji="fire">1</span>`)
	assert.NotContains(t, body, `data-emoji="laugh" formaction="/rxn1040/react/laugh" aria-pressed="true"`)
}

func TestDetailsPageReactionsDisabled(t *testing.T) {
	cfg := testConfig()
	f := false
	cfg.Reactions.Enabled = &f
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1041", "2026-10-06 01:00:00")

	status, body := getPage(t, ts.URL, "/rxn1041")
	require.Equal(t, http.StatusOK, status)
	// The stylesheet's .react-form rule ships always — assert on the
	// rendered form element and the glyph embed instead.
	assert.NotContains(t, body, `class="react-form"`, "disabled feature renders no row")
	assert.NotContains(t, body, `data-reactions=`, "disabled feature embeds no glyph map")
}

func TestGalleryCardsShowReactionBadges(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1042", "2026-10-06 01:00:00")
	insertImage(t, app, "rxn1043", "2026-10-06 02:00:00")
	insertImage(t, app, "rxn1044", "2026-10-06 03:00:00")
	// rxn1043: fire x2, laugh x1, wow x1 → badges fire + laugh
	// (count desc, configured order ties). Plus votes so the wrapper
	// carries all three groups.
	_, _, _, err := dbToggleVote(app.db, "rxn1043", "v1", voteLike)
	require.NoError(t, err)
	for _, tok := range []string{"a", "b"} {
		_, _, err := dbToggleReaction(app.db, "rxn1043", tok, "fire")
		require.NoError(t, err)
	}
	_, _, err = dbToggleReaction(app.db, "rxn1043", "c", "laugh")
	require.NoError(t, err)
	_, _, err = dbToggleReaction(app.db, "rxn1043", "d", "wow")
	require.NoError(t, err)
	// rxn1044: reactions only (wrapper without vote spans).
	_, _, err = dbToggleReaction(app.db, "rxn1044", "e", "fire")
	require.NoError(t, err)

	status, body := getPage(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, status)

	// Votes + top-2 badges share the wrapper; wow (count 1, behind
	// laugh on the tie) is NOT badged.
	assert.Contains(t, body,
		`<span class="counts"><span class="likes" data-count="1">&#9829; 1</span><span class="reacts"><span class="react" data-emoji="fire">🔥 2</span><span class="react" data-emoji="laugh">😂 1</span></span></span>`,
		"wrapper carries votes then the top-2 badges in count order")
	// Reactions-only card renders the wrapper without vote spans.
	assert.Contains(t, body,
		`<span class="counts"><span class="reacts"><span class="react" data-emoji="fire">🔥 1</span></span></span>`)
	// The un-reacted card renders neither wrapper nor badges.
	assert.Equal(t, 2, strings.Count(body, `class="reacts"`), "only the two reacted cards badge")

	// The gallery page embeds the glyph map for the SSE path.
	assert.Contains(t, body, `data-reactions="`)
}

func TestSearchCardsShowReactionBadges(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1045", "2026-10-06 01:00:00", func(img *dbImage) {
		img.OriginalPrompt = "shrew parade five"
	})
	_, _, err := dbToggleReaction(app.db, "rxn1045", "t9", "wow")
	require.NoError(t, err)

	status, body := getPage(t, ts.URL, "/search?q=shrew")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body,
		`<span class="counts"><span class="reacts"><span class="react" data-emoji="wow">😮 1</span></span></span>`,
		"search cards carry badges via the shared partial + hydration")
	assert.Contains(t, body, `data-reactions="`, "search pages embed the glyph map too")
}

// TestDbGetGalleryPageHydratesReactions extends the likes hydration
// pin: the default-sort gallery page carries the reaction tally on
// every row.
func TestDbGetGalleryPageHydratesReactions(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	insertImage(t, app, "rxn1050", "2026-10-06 01:00:00")
	insertImage(t, app, "rxn1051", "2026-10-06 02:00:00")
	_, _, err := dbToggleReaction(app.db, "rxn1051", "a", "fire")
	require.NoError(t, err)

	rows, err := dbGetGalleryPage(app.db, "", "", 10, siteCtx{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	byID := map[string]dbImage{rows[0].ID: rows[0], rows[1].ID: rows[1]}
	assert.Nil(t, byID["rxn1050"].Reactions)
	assert.Equal(t, map[string]int{"fire": 1}, byID["rxn1051"].Reactions)
}
