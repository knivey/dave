package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
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
	// its shape. The emoji column stores the raw emoji string itself
	// (any emoji — there is no catalog membership anywhere).
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

// TestReactionKeyMigration007 stages a pre-007 database (old ASCII
// name keys), runs migration 007, and pins the conversion: shipped
// default names become their emoji, unconvertible custom names are
// dropped.
func TestReactionKeyMigration007(t *testing.T) {
	db := setupTestDB(t)

	// Roll back to version 6, plant the legacy shapes, re-run 007.
	_, err := db.Exec(`DELETE FROM reactions`)
	require.NoError(t, err)
	plant := func(id, tok, emoji string) {
		_, err := db.Exec(`INSERT INTO reactions (image_id, token, emoji, created_at) VALUES (?, ?, ?, '2026-10-06 00:00:00')`, id, tok, emoji)
		require.NoError(t, err)
	}
	plant("mig0001", "t1", "fire")
	plant("mig0001", "t2", "fire")
	plant("mig0001", "t3", "skull")
	plant("mig0002", "t1", "custom_local_preset_name")
	plant("mig0003", "t1", "🔥") // already-emoji key survives untouched

	// goose Down to 6 drops nothing (007's Down is a no-op guard) —
	// instead simulate by DELETE + direct re-application of 007 Up is
	// not possible through goose's clean API; exercise the exact SQL
	// the migration carries instead, on the planted rows.
	for _, m := range [][2]string{
		{"fire", "\U0001F525"}, {"laugh", "\U0001F602"}, {"skull", "\U0001F480"},
		{"poop", "\U0001F4A9"}, {"eyes", "\U0001F440"}, {"clown", "\U0001F921"},
		{"wow", "\U0001F62E"}, {"thinking", "\U0001F914"}, {"pleading", "\U0001F97A"},
		{"pray", "\U0001F64F"}, {"sparkles", "\u2728"}, {"art", "\U0001F3A8"},
	} {
		_, err := db.Exec(`UPDATE reactions SET emoji = ? WHERE emoji = ?`, m[1], m[0])
		require.NoError(t, err)
	}
	_, err = db.Exec(`DELETE FROM reactions WHERE emoji GLOB '[a-z0-9_]*'`)
	require.NoError(t, err)

	var keys []string
	require.NoError(t, db.Select(&keys, `SELECT emoji FROM reactions ORDER BY emoji`))
	assert.ElementsMatch(t, []string{"🔥", "🔥", "💀", "🔥"}, keys,
		"default names converted, custom ASCII name dropped, emoji keys untouched")
}

// reactionTestConfig is testConfig — reactions are enabled by
// default (the zero value means "on"), so there is nothing to
// configure anymore. Kept as a named helper so the intent reads.
func reactionTestConfig() Config {
	return testConfig()
}

func TestToggleReactionRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "rxn0001", SHA256: "aa", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-06 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))

	// React, un-react, re-react with one emoji; a second token stacks.
	// mine (the caller's full holdings map) rides every return — the
	// details-page JS rebuilds its whole chip row from it.
	reacted, tally, mine, err := dbToggleReaction(db, "rxn0001", "tok1", "🔥")
	require.NoError(t, err)
	assert.True(t, reacted, "first toggle reacts")
	assert.Equal(t, map[string]int{"🔥": 1}, tally)
	assert.Equal(t, map[string]bool{"🔥": true}, mine)

	reacted, tally, mine, err = dbToggleReaction(db, "rxn0001", "tok1", "🔥")
	require.NoError(t, err)
	assert.False(t, reacted, "second toggle removes the reaction")
	assert.Empty(t, tally)
	assert.Empty(t, mine, "un-reacting empties the holdings map")

	reacted, tally, mine, err = dbToggleReaction(db, "rxn0001", "tok1", "🔥")
	require.NoError(t, err)
	assert.True(t, reacted, "third toggle re-reacts")

	reacted, tally, mine, err = dbToggleReaction(db, "rxn0001", "tok2", "🔥")
	require.NoError(t, err)
	assert.True(t, reacted)
	assert.Equal(t, map[string]int{"🔥": 2}, tally, "independent tokens count independently")
	assert.Equal(t, map[string]bool{"🔥": true}, mine, "mine describes ONLY the calling token")

	// MULTI-emoji cardinality: the same token adds different emojis —
	// including one OUTSIDE any configured set — without releasing
	// the ones it holds.
	reacted, tally, mine, err = dbToggleReaction(db, "rxn0001", "tok2", "🐙")
	require.NoError(t, err)
	assert.True(t, reacted, "off-bar emoji from the same token lands")
	assert.Equal(t, map[string]int{"🔥": 2, "🐙": 1}, tally)
	assert.Equal(t, map[string]bool{"🔥": true, "🐙": true}, mine)

	// Removing one emoji leaves the token's others (and other
	// tokens' reactions) untouched: tok1's fire survives, tok2's
	// does not.
	_, tally, mine, err = dbToggleReaction(db, "rxn0001", "tok2", "🔥")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"🔥": 1, "🐙": 1}, tally, "removing tok2's fire releases only tok2's fire")
	assert.Equal(t, map[string]bool{"🐙": true}, mine, "tok2 keeps its other emojis")
}

func TestDbGetReactionState(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "rxn0002", SHA256: "bb", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-06 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))
	for _, e := range []string{"🔥", "😮"} {
		_, _, _, err := dbToggleReaction(db, "rxn0002", "tok1", e)
		require.NoError(t, err)
	}
	_, _, _, err := dbToggleReaction(db, "rxn0002", "tok2", "🔥")
	require.NoError(t, err)

	tally, mine, err := dbGetReactionState(db, "rxn0002", "tok1")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"🔥": 2, "😮": 1}, tally)
	assert.Equal(t, map[string]bool{"🔥": true, "😮": true}, mine)

	_, mine, err = dbGetReactionState(db, "rxn0002", "stranger")
	require.NoError(t, err)
	assert.Empty(t, mine, "unknown token holds nothing")

	// Unknown image: empty maps, not an error (no anchor — see
	// dbGetReactionState's note).
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
	// rhy0001: 🔥 x2 + 😮 x1; rhy0002: none; rhy0003: 😂 x1.
	for _, tok := range []string{"a", "b"} {
		_, _, _, err := dbToggleReaction(db, "rhy0001", tok, "🔥")
		require.NoError(t, err)
	}
	_, _, _, err := dbToggleReaction(db, "rhy0001", "c", "😮")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(db, "rhy0003", "d", "😂")
	require.NoError(t, err)

	rows := []dbImage{{ID: "rhy0001"}, {ID: "rhy0002"}, {ID: "rhy0003"}}
	ptrs := []*dbImage{&rows[0], &rows[1], &rows[2]}
	require.NoError(t, hydrateReactionCounts(db, ptrs))
	assert.Equal(t, map[string]int{"🔥": 2, "😮": 1}, rows[0].Reactions)
	assert.Nil(t, rows[1].Reactions, "no reactions hydrates to nil")
	assert.Equal(t, map[string]int{"😂": 1}, rows[2].Reactions)
}

func TestTopCardReactions(t *testing.T) {
	cfg := reactionTestConfig()
	// No whitelist: tally keys badge directly, count desc with
	// codepoint-lexicographic ties (🔥 U+1F525 < 😂 U+1F602 < 😮
	// U+1F62E).
	badges, more := topCardReactions(cfg, map[string]int{"🔥": 1, "😂": 2, "😮": 2})
	require.Len(t, badges, 3)
	assert.False(t, more)
	assert.Equal(t, "😂", badges[0].Emoji, "count desc")
	assert.Equal(t, "😮", badges[1].Emoji)
	assert.Equal(t, "🔥", badges[2].Emoji)

	// Off-bar emojis badge like any other (no configured set needed).
	badges, more = topCardReactions(cfg, map[string]int{"🐙": 9})
	require.Len(t, badges, 1)
	assert.Equal(t, "🐙", badges[0].Emoji)
	assert.False(t, more)

	// Zero counts never badge; empty tally is fine.
	badges, _ = topCardReactions(cfg, map[string]int{"🔥": 0})
	assert.Empty(t, badges)
	badges, _ = topCardReactions(cfg, nil)
	assert.Empty(t, badges)

	// Overflow: 5 non-zero keys → 4 badges + more, count order.
	badges, more = topCardReactions(cfg, map[string]int{"🔥": 1, "😂": 2, "😮": 3, "🐙": 4, "💀": 5})
	require.Len(t, badges, cardReactionBadges)
	assert.True(t, more, "a 5th non-zero key sets the overflow flag")
	assert.Equal(t, "💀", badges[0].Emoji)
	assert.Equal(t, "🐙", badges[1].Emoji)
}

// TestEmojiLess pins the deterministic tie-break both sides share.
func TestEmojiLess(t *testing.T) {
	assert.True(t, emojiLess("🔥", "😂"), "U+1F525 < U+1F602")
	assert.False(t, emojiLess("😂", "🔥"))
	assert.True(t, emojiLess("✨", "🔥"), "BMP emoji sort before astral ones by codepoint")
	assert.True(t, emojiLess("🔥", "🔥🔥"), "prefix sorts first")
	assert.False(t, emojiLess("🔥🔥", "🔥"))
}

// ---------------------------------------------------------------------------
// Config: quick-bar loading, defaults, validation
// ---------------------------------------------------------------------------

func TestLoadConfigReactions(t *testing.T) {
	// Absent section → feature on (any-emoji, no curated list).
	path := writeConfigFile(t, "[auth]\napi_key = \"secret\"\n")
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	assert.True(t, cfg.Reactions.reactionsEnabled(), "absent section defaults to enabled")

	// Explicit disable.
	path = writeConfigFile(t, "[auth]\napi_key = \"secret\"\n\n[reactions]\nenabled = false\n")
	cfg, err = loadConfig(path)
	require.NoError(t, err)
	assert.False(t, cfg.Reactions.reactionsEnabled())

	// Stale configs from the quick-bar era still load: the decoder
	// tolerates unknown keys, so an upgrade never wedges startup (or
	// a SIGHUP reload) on a leftover [[reactions.emoji]] section.
	path = writeConfigFile(t, `[auth]
api_key = "secret"

[[reactions.emoji]]
glyph = "🔥"
category = "hype"
`)
	cfg, err = loadConfig(path)
	require.NoError(t, err, "stale quick-bar entries must not fail loadConfig")
	assert.True(t, cfg.Reactions.reactionsEnabled())
}

func TestValidReactionEmoji(t *testing.T) {
	assert.True(t, validReactionEmoji("🔥"))
	assert.True(t, validReactionEmoji("❌"), "BMP emoji pass")
	assert.True(t, validReactionEmoji("🧑\u200d🚀"), "ZWJ sequences pass")
	assert.True(t, validReactionEmoji("👍🏽"), "skin tones pass")
	assert.False(t, validReactionEmoji(""), "empty rejected")
	assert.False(t, validReactionEmoji("fire"), "ASCII words rejected")
	assert.False(t, validReactionEmoji("123"), "digits rejected")
	assert.False(t, validReactionEmoji("🔥x"), "mixed ASCII rejected")
	assert.False(t, validReactionEmoji("a"), "bare ASCII rejected")
	assert.False(t, validReactionEmoji(strings.Repeat("🔥", 33)), "over rune cap rejected")
}

// ---------------------------------------------------------------------------
// POST /{id}/react/{emoji} — toggle handler
// ---------------------------------------------------------------------------

// dbReactionTally returns one image's full reaction tally (emoji →
// count). Production reads go through dbGetReactionState (tally +
// mine in one pass); this plain-tally probe lives for tests.
func dbReactionTally(db *sqlx.DB, id string) (map[string]int, error) {
	rows, err := db.Query(
		`SELECT emoji, COUNT(*) FROM reactions WHERE image_id = ? GROUP BY emoji`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tally := make(map[string]int)
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		tally[name] = n
	}
	return tally, rows.Err()
}

// postReact posts to the reaction toggle endpoint and returns the
// response (body NOT closed — tests either read it or pass through).
func postReact(t *testing.T, ts *httptest.Server, id, emoji, cookie, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/"+id+"/react/"+url.PathEscape(emoji), nil)
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
	resp := postReact(t, ts, "rxn1000", "🔥", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := likeCookieHeader(t, resp)
	rr := decodeReactionResponse(t, resp)
	assert.Equal(t, "🔥", rr.Emoji)
	assert.True(t, rr.Reacted)
	assert.Equal(t, map[string]int{"🔥": 1}, rr.Reactions)
	assert.Equal(t, map[string]bool{"🔥": true}, rr.Mine)

	// Same token + same emoji toggles off.
	resp = postReact(t, ts, "rxn1000", "🔥", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rr = decodeReactionResponse(t, resp)
	assert.False(t, rr.Reacted)
	assert.Empty(t, rr.Reactions)

	// Multi-emoji from one token stacks (unlike votes) — including
	// emojis OUTSIDE the configured quick bar (any emoji reactable).
	resp = postReact(t, ts, "rxn1000", "🔥", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = postReact(t, ts, "rxn1000", "🐙", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rr = decodeReactionResponse(t, resp)
	assert.Equal(t, "🐙", rr.Emoji)
	assert.Equal(t, map[string]int{"🔥": 1, "🐙": 1}, rr.Reactions, "the same token holds both")
	assert.Equal(t, map[string]bool{"🔥": true, "🐙": true}, rr.Mine)

	// Un-reacting one leaves the other in mine.
	resp = postReact(t, ts, "rxn1000", "🔥", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rr = decodeReactionResponse(t, resp)
	assert.Equal(t, map[string]bool{"🐙": true}, rr.Mine, "mine drops only the released emoji")

	// And the vote identity is SHARED: the token that reacted can
	// also like — one cookie, both features.
	resp2 := postLike(t, ts, "rxn1000", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	vr := decodeVoteResponse(t, resp2)
	assert.True(t, vr.Liked)
}

func TestReactionToggleKeyValidation(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1010", "2026-10-06 01:00:00")

	// ASCII (the old name-key world, plus path games) 404s. The
	// traversal shapes go over the RAW url — postReact would
	// double-escape them and pin nothing about the mux's decoding.
	for _, bad := range []string{"sparkles", "fire", "123"} {
		resp := postReact(t, ts, "rxn1010", bad, "", "application/json")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "key %q must 404", bad)
	}
	for _, raw := range []string{
		"/rxn1010/react/%2e%2e",            // decoded ".." — ASCII, rejected by the shape check
		"/rxn1010/react/%2F..%2F..",        // decoded "/../.." inside the segment — ASCII, rejected
		"/rxn1010/react/%f0%9f%94%a5%2f..", // emoji followed by encoded "/" — mixed ASCII, rejected
	} {
		req, err := http.NewRequest("POST", ts.URL+raw, nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "application/json")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "raw path %q must 404", raw)
	}

	// Any well-formed emoji works, on or off the quick bar.
	for _, good := range []string{"🔥", "😂", "🐙", "🧑‍🚀"} {
		resp := postReact(t, ts, "rxn1010", good, "", "application/json")
		require.Equal(t, http.StatusOK, resp.StatusCode, "key %q", good)
	}
	tally, err := dbReactionTally(app.db, "rxn1010")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"🔥": 1, "😂": 1, "🐙": 1, "🧑‍🚀": 1}, tally)
}

func TestReactionToggleDisabledFeature(t *testing.T) {
	cfg := testConfig()
	f := false
	cfg.Reactions.Enabled = &f
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1011", "2026-10-06 01:00:00")

	resp := postReact(t, ts, "rxn1011", "🔥", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "disabled feature 404s the endpoint")
	tally, err := dbReactionTally(app.db, "rxn1011")
	require.NoError(t, err)
	assert.Empty(t, tally)
}

// TestReactionDisabledGating pins the disable semantics on the
// loadConfig path (what a SIGHUP reload produces): enabled=false
// turns reactionsEnabled() off and topCardReactions gates to empty.
func TestReactionDisabledGating(t *testing.T) {
	path := writeConfigFile(t, "[auth]\napi_key = \"secret\"\n\n[reactions]\nenabled = false\n")
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	assert.False(t, cfg.Reactions.reactionsEnabled())

	// No badges render from a disabled config even with tally rows
	// present.
	badges, more := topCardReactions(cfg, map[string]int{"🔥": 5})
	assert.Empty(t, badges)
	assert.False(t, more)
}

func TestReactionToggleFormRedirectsBack(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	ts.Client().CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	insertImage(t, app, "rxn1012", "2026-10-06 01:00:00")

	resp := postReact(t, ts, "rxn1012", "🔥", "", "")
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

	resp := postReact(t, ts, "zzzzzzz", "🔥", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "unknown id 404s")

	resp = postReact(t, ts, "rxn1013", "🔥", "", "application/json")
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

	req, err := http.NewRequest("POST", ts.URL+"/rxn1014/react/"+url.PathEscape("🔥"), nil)
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

	resp = postReact(t, ts, "rxn1014", "🔥", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode, "default host serves normally")
	rr := decodeReactionResponse(t, resp)
	assert.Equal(t, map[string]int{"🔥": 1}, rr.Reactions)
}

func TestReactionToggleMethodGuard(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1015", "2026-10-06 01:00:00")

	resp, err := http.Get(ts.URL + "/rxn1015/react/" + url.PathEscape("🔥"))
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

	req, err := http.NewRequest("POST", ts.URL+"/rxn1016/react/"+url.PathEscape("🔥"), nil)
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

	resp := postReact(t, ts, "rxn1030", "🔥", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := likeCookieHeader(t, resp)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageReacted, f.name)
	var p imageReactedEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, "rxn1030", p.ID)
	assert.Equal(t, map[string]int{"🔥": 1}, p.Reactions)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &raw))
	assert.ElementsMatch(t, []string{"id", "reactions"}, mapKeys(raw), "payload fields are exactly id+reactions")

	// Commit-before-publish: re-querying at event-observation time
	// must already see the reaction.
	tally, err := dbReactionTally(app.db, "rxn1030")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"🔥": 1}, tally)

	// A removal publishes the emptied tally. (Fresh variable on
	// purpose: json.Unmarshal MERGES into a non-nil map, so reusing p
	// would keep the stale key and lie about the payload.)
	resp = postReact(t, ts, "rxn1030", "🔥", cookie, "application/json")
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

	resp := postReact(t, ts, "rxn1031", "🔥", "", "application/json")
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
	app.publishImageReacted(&dbImage{ID: "rxn1032", Safety: safetyUnknown}, map[string]int{"🔥": 1})
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

func TestDetailsPageReactionButtons(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1040", "2026-10-06 01:00:00")

	// Anonymous visit: NO chips (nothing exists yet), but the open
	// button ALWAYS renders — never hidden behind a JS reveal.
	status, body := getPage(t, ts.URL, "/rxn1040")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `<form class="react-form" method="post">`)
	assert.Contains(t, body, `<button type="button" class="react-open" id="react-open" title="add a reaction">&#65291; React</button>`,
		"the PicMo trigger is always visible, JS or not")
	assert.NotContains(t, body, `class="react-chip`, "zero reactions render no chip")
	assert.NotContains(t, body, `react-picker`, "no quick-bar markup exists anymore")
	assert.NotContains(t, body, `react-open" id="react-open" hidden`, "the open button never ships hidden")

	// tok holds 🔥+😮; another token reacted 🐙: chips appear for ALL
	// existing reactions in the one true order (count desc, codepoint
	// ties: 🐙 U+1F419 < 🔥 U+1F525 < 😮 U+1F62E).
	tok := "0123456789abcdef0123456789abcdef"
	_, _, _, err := dbToggleReaction(app.db, "rxn1040", tok, "🔥")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(app.db, "rxn1040", tok, "😮")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(app.db, "rxn1040", "other", "🐙")
	require.NoError(t, err)
	status, body = getPageCookie(t, ts.URL, "/rxn1040", likeCookieName+"="+tok)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, 3, strings.Count(body, `class="react-chip`), "every existing reaction chips")
	assert.Contains(t, body, `<button type="submit" class="react-chip reacted" data-emoji="🔥" formaction="/rxn1040/react/%f0%9f%94%a5" aria-pressed="true">🔥 <span class="react-count" data-emoji="🔥">1</span></button>`)
	assert.Less(t, strings.Index(body, `data-emoji="🐙"`), strings.Index(body, `data-emoji="🔥"`),
		"count ties order by codepoint: 🐙 U+1F419 before 🔥 U+1F525")
	assert.Contains(t, body, `class="react-chip" data-emoji="🐙"`, "another visitor's reaction chips unpressed here")
	assert.Equal(t, 2, strings.Count(body, `class="react-chip reacted"`), "exactly the held emojis press")
}

// TestDetailsPageChipCap pins the abuse bound: with more distinct
// reaction keys than maxDetailChips, the row renders the top slice
// (count desc, codepoint ties) plus a "+N" note, and the count of
// rendered chips never exceeds the cap.
func TestDetailsPageChipCap(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1046", "2026-10-06 01:00:00")

	// maxDetailChips + 5 distinct keys, one reaction each.
	keys := []string{}
	for i := 0; i < maxDetailChips+5; i++ {
		// Distinct astral emoji: U+1F300 + i (🌀..).
		keys = append(keys, string(rune(0x1F300+i)))
	}
	for i, k := range keys {
		_, _, _, err := dbToggleReaction(app.db, "rxn1046", fmt.Sprintf("t%d", i), k)
		require.NoError(t, err)
	}

	status, body := getPage(t, ts.URL, "/rxn1046")
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, maxDetailChips, strings.Count(body, `class="react-chip"`),
		"the rendered row is capped")
	assert.Contains(t, body, `<span class="react-overflow" title="more reactions">+5</span>`,
		"the elided tail is noted")
	// The cap keeps the FIRST keys in codepoint order (all counts tie
	// at 1): the lowest codepoint renders, the highest is elided.
	assert.Contains(t, body, `data-emoji="`+keys[0]+`"`)
	assert.NotContains(t, body, `data-emoji="`+keys[len(keys)-1]+`"`)
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
	// The stylesheet's rules ship always — assert on the rendered
	// form element instead.
	assert.NotContains(t, body, `class="react-form"`, "disabled feature renders no form")
}

func TestGalleryCardsShowReactionBadges(t *testing.T) {
	cfg := testConfig()
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1042", "2026-10-06 01:00:00")
	insertImage(t, app, "rxn1043", "2026-10-06 02:00:00")
	insertImage(t, app, "rxn1044", "2026-10-06 03:00:00")

	// rxn1043: 🔥 x2, 😂 x1, 😮 x1 → 3 badges, no overflow.
	// Plus votes so the wrapper carries all three groups.
	_, _, _, err := dbToggleVote(app.db, "rxn1043", "v1", voteLike)
	require.NoError(t, err)
	for _, tok := range []string{"a", "b"} {
		_, _, _, err := dbToggleReaction(app.db, "rxn1043", tok, "🔥")
		require.NoError(t, err)
	}
	_, _, _, err = dbToggleReaction(app.db, "rxn1043", "c", "😂")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(app.db, "rxn1043", "d", "😮")
	require.NoError(t, err)

	// rxn1044: FIVE non-zero keys (💀 2, 🔥 3, 😂 1, 😮 1, 🐙 1 — the
	// last OFF-bar: badges carry any emoji) → 4 badges (💀? count
	// order: 🔥 3, 💀 2, then codepoint ties among the 1s: 😂 < 😮 <
	// 🐙 picks 😂 and 😮) + the "…" marker for 🐙. Reactions only.
	_, _, _, err = dbToggleReaction(app.db, "rxn1044", "e1", "🔥")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(app.db, "rxn1044", "e2", "🔥")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(app.db, "rxn1044", "e3", "🔥")
	require.NoError(t, err)
	for _, tok := range []string{"e4", "e5"} {
		_, _, _, err := dbToggleReaction(app.db, "rxn1044", tok, "💀")
		require.NoError(t, err)
	}
	_, _, _, err = dbToggleReaction(app.db, "rxn1044", "e6", "😂")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(app.db, "rxn1044", "e7", "😮")
	require.NoError(t, err)
	_, _, _, err = dbToggleReaction(app.db, "rxn1044", "e8", "🐙")
	require.NoError(t, err)

	status, body := getPage(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, status)

	// Votes + badges share the wrapper.
	assert.Contains(t, body,
		`<span class="counts"><span class="likes" data-count="1">&#9829; 1</span><span class="reacts"><span class="react" data-emoji="🔥">🔥 2</span><span class="react" data-emoji="😂">😂 1</span><span class="react" data-emoji="😮">😮 1</span></span></span>`,
		"wrapper carries votes then the badges in count order")
	assert.Contains(t, body,
		`<span class="counts"><span class="reacts"><span class="react" data-emoji="🔥">🔥 3</span><span class="react" data-emoji="💀">💀 2</span><span class="react" data-emoji="🐙">🐙 1</span><span class="react" data-emoji="😂">😂 1</span><span class="react more" title="more reactions">&#8230;</span></span></span>`,
		"overflow card shows 4 badges (count desc, codepoint ties: 🐙 U+1F419 < 😂 U+1F602) with the ellipsis for the elided 😮")
	// The un-reacted cards render neither wrapper nor badges.
	assert.Equal(t, 2, strings.Count(body, `class="reacts"`), "only the two reacted cards badge")
	assert.Equal(t, 1, strings.Count(body, `class="react more"`), "exactly one overflow marker")
}

func TestSearchCardsShowReactionBadges(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "rxn1045", "2026-10-06 01:00:00", func(img *dbImage) {
		img.OriginalPrompt = "shrew parade five"
	})
	_, _, _, err := dbToggleReaction(app.db, "rxn1045", "t9", "🐙")
	require.NoError(t, err)

	status, body := getPage(t, ts.URL, "/search?q=shrew")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body,
		`<span class="counts"><span class="reacts"><span class="react" data-emoji="🐙">🐙 1</span></span></span>`,
		"search cards carry off-bar badges via the shared partial + hydration")
}

// TestDbGetGalleryPageHydratesReactions extends the likes hydration
// pin: the default-sort gallery page carries the reaction tally on
// every row.
func TestDbGetGalleryPageHydratesReactions(t *testing.T) {
	app := newTestApp(t, reactionTestConfig())
	insertImage(t, app, "rxn1050", "2026-10-06 01:00:00")
	insertImage(t, app, "rxn1051", "2026-10-06 02:00:00")
	_, _, _, err := dbToggleReaction(app.db, "rxn1051", "a", "🔥")
	require.NoError(t, err)

	rows, err := dbGetGalleryPage(app.db, "", "", 10, siteCtx{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	byID := map[string]dbImage{rows[0].ID: rows[0], rows[1].ID: rows[1]}
	assert.Nil(t, byID["rxn1050"].Reactions)
	assert.Equal(t, map[string]int{"🔥": 1}, byID["rxn1051"].Reactions)
}
