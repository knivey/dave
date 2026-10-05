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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLikesMigrationCreatesTable(t *testing.T) {
	db := setupTestDB(t)

	var got string
	require.NoError(t, db.Get(&got,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'likes'`))
	assert.Equal(t, "likes", got, "likes table must exist")

	// The PK (image_id, token) is what enforces one like per anonymous
	// token per image — the whole dedupe story — so pin its shape.
	type col struct {
		CID     int         `db:"cid"`
		Name    string      `db:"name"`
		Type    string      `db:"type"`
		NotNull int         `db:"notnull"`
		Dflt    interface{} `db:"dflt_value"`
		PK      int         `db:"pk"`
	}
	var cols []col
	require.NoError(t, db.Select(&cols, `PRAGMA table_info(likes)`))
	assert.Len(t, cols, 3, "sqlite reports cid+name for each column")
	for _, c := range cols {
		assert.Equal(t, 1, c.NotNull, "%s is NOT NULL", c.Name)
	}
	byName := map[string]col{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	require.Len(t, byName, 3, "likes has exactly image_id, token, created_at")
	assert.Equal(t, 1, byName["image_id"].PK, "image_id is the first PK column")
	assert.Equal(t, 2, byName["token"].PK, "token is the second PK column")
	assert.Equal(t, 0, byName["created_at"].PK, "created_at is not part of the PK")

	var idx string
	require.NoError(t, db.Get(&idx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_likes_image'`))
	assert.Equal(t, "idx_likes_image", idx, "count-lookup index must exist")
}

func TestToggleLikeRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "lik0001", SHA256: "aa", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-05 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))

	// Like, unlike, re-like from one token; a second token stacks.
	liked, count, err := dbToggleLike(db, "lik0001", "tok1")
	require.NoError(t, err)
	assert.True(t, liked, "first toggle likes")
	assert.Equal(t, 1, count)

	liked, count, err = dbToggleLike(db, "lik0001", "tok1")
	require.NoError(t, err)
	assert.False(t, liked, "second toggle un-likes")
	assert.Equal(t, 0, count)

	liked, count, err = dbToggleLike(db, "lik0001", "tok1")
	require.NoError(t, err)
	assert.True(t, liked, "third toggle re-likes")
	assert.Equal(t, 1, count)

	liked, count, err = dbToggleLike(db, "lik0001", "tok2")
	require.NoError(t, err)
	assert.True(t, liked)
	assert.Equal(t, 2, count, "independent tokens count independently")
}

func TestDbGetLikeState(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "lik0002", SHA256: "bb", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-05 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))
	for _, tok := range []string{"tok1", "tok2", "tok3"} {
		_, _, err := dbToggleLike(db, "lik0002", tok)
		require.NoError(t, err)
	}

	count, liked, err := dbGetLikeState(db, "lik0002", "tok2")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.True(t, liked, "token that liked reports liked")

	count, liked, err = dbGetLikeState(db, "lik0002", "stranger")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.False(t, liked, "unknown token reports not-liked")

	// No likes at all: count 0, not liked, no error.
	count, liked, err = dbGetLikeState(db, "lik0003", "tok1")
	require.Error(t, err, "unknown image id surfaces the row miss")
	assert.Zero(t, count)
	assert.False(t, liked)
}

func TestHydrateLikeCounts(t *testing.T) {
	db := setupTestDB(t)
	ids := []string{"hyd0001", "hyd0002", "hyd0003"}
	for i, id := range ids {
		in := &dbImage{ID: id, SHA256: id, Filename: "f.webp", MimeType: "image/webp",
			SizeBytes: 1, CreatedAt: "2026-10-05 01:00:0" + string(rune('0'+i)), ThumbStatus: thumbStatusPending}
		require.NoError(t, dbInsertImage(db, in))
	}
	// hyd0001: 2 likes, hyd0002: none, hyd0003: 1 like.
	for _, tok := range []string{"a", "b"} {
		_, _, err := dbToggleLike(db, "hyd0001", tok)
		require.NoError(t, err)
	}
	_, _, err := dbToggleLike(db, "hyd0003", "c")
	require.NoError(t, err)

	rows := []dbImage{{ID: "hyd0001"}, {ID: "hyd0002"}, {ID: "hyd0003"}}
	ptrs := []*dbImage{&rows[0], &rows[1], &rows[2]}
	require.NoError(t, hydrateLikeCounts(db, ptrs))
	assert.Equal(t, 2, rows[0].LikeCount)
	assert.Equal(t, 0, rows[1].LikeCount, "no likes hydrates to zero")
	assert.Equal(t, 1, rows[2].LikeCount)
}

// ---------------------------------------------------------------------------
// POST /{id}/like — toggle handler
// ---------------------------------------------------------------------------

// postLike posts to the toggle endpoint and returns the response (body
// NOT closed — tests either read it or pass through).
func postLike(t *testing.T, ts *httptest.Server, id, cookie, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/"+id+"/like", nil)
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

// likeCookieHeader extracts the imgsite_liker Set-Cookie value from a
// response as a ready-to-send Cookie header.
func likeCookieHeader(t *testing.T, resp *http.Response) string {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == likeCookieName {
			return likeCookieName + "=" + c.Value
		}
	}
	t.Fatal("no imgsite_liker Set-Cookie on response")
	return ""
}

func decodeLikeResponse(t *testing.T, resp *http.Response) (liked bool, count int) {
	t.Helper()
	var payload struct {
		Liked bool `json:"liked"`
		Count int  `json:"count"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Liked, payload.Count
}

func TestLikeToggleJSONRoundTrip(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1000", "2026-10-05 01:00:00")

	// First like: mints the cookie, likes, reports count 1.
	resp := postLike(t, ts, "lik1000", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	liked, count := decodeLikeResponse(t, resp)
	assert.True(t, liked)
	assert.Equal(t, 1, count)
	cookie := likeCookieHeader(t, resp)
	for _, c := range resp.Cookies() {
		if c.Name != likeCookieName {
			continue
		}
		assert.True(t, c.HttpOnly, "like cookie is HttpOnly")
		assert.True(t, c.Secure, "like cookie is Secure")
		assert.Equal(t, http.SameSiteLaxMode, c.SameSite, "like cookie is SameSite=Lax")
		assert.Positive(t, c.MaxAge, "like cookie is long-lived")
	}

	// Same token toggles off (and no re-mint: state travels with the
	// token, the response must not need a second Set-Cookie).
	resp = postLike(t, ts, "lik1000", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	liked, count = decodeLikeResponse(t, resp)
	assert.False(t, liked, "second toggle un-likes")
	assert.Equal(t, 0, count)
	assert.Empty(t, resp.Cookies(), "existing token is not re-minted")

	// And back on.
	resp = postLike(t, ts, "lik1000", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	liked, count = decodeLikeResponse(t, resp)
	assert.True(t, liked)
	assert.Equal(t, 1, count)
}

// TestLikeToggleMalformedCookieMintsFresh proves a garbage cookie
// value is ignored (never trusted as identity) and a fresh token is
// issued instead.
func TestLikeToggleMalformedCookieMintsFresh(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1010", "2026-10-05 01:00:00")

	resp := postLike(t, ts, "lik1010", likeCookieName+"=!!!not-hex!!!", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	liked, _ := decodeLikeResponse(t, resp)
	assert.True(t, liked, "garbage cookie falls back to a fresh mint")
	require.NotEmpty(t, resp.Cookies(), "fresh token is issued via Set-Cookie")
	require.NoError(t, resp.Body.Close())
}

func TestLikeToggleFormRedirectsBack(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	// The no-JS path must be observable as a redirect, so this test's
	// client must NOT auto-follow the 303 (the default client would
	// convert it to a GET and hand back the details page's 200).
	ts.Client().CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	insertImage(t, app, "lik1011", "2026-10-05 01:00:00")

	// No Accept: application/json — the no-JS form path.
	resp := postLike(t, ts, "lik1011", "", "")
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/lik1011", resp.Header.Get("Location"), "redirect returns to the details page")
	require.NotEmpty(t, resp.Cookies(), "cookie is minted on the form path too")
}

func TestLikeToggleNotFoundAndGone(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1012", "2026-10-05 01:00:00", func(img *dbImage) {
		img.Hidden = true
	})

	resp := postLike(t, ts, "zzzzzzz", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "unknown id 404s")

	resp = postLike(t, ts, "bad!", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "malformed id 404s")

	resp = postLike(t, ts, "lik1012", "", "application/json")
	assert.Equal(t, http.StatusGone, resp.StatusCode, "hidden id 410s")
}

// TestLikeToggleSiteMatrix pins the safe-host behavior: liking an
// image invisible on the requesting site 404s exactly like the details
// page does (no existence leak), while the default host likes it
// normally.
func TestLikeToggleSiteMatrix(t *testing.T) {
	app := newTestApp(t, safeSiteTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1020", "2026-10-05 01:00:00", func(img *dbImage) {
		img.Network = ptrStr("efnet") // not in allowed_networks, unknown safety
	})

	// Safe host: invisible row → 404, no like recorded.
	req, err := http.NewRequest("POST", ts.URL+"/lik1020/like", nil)
	require.NoError(t, err)
	req.Host = "safe.example.com"
	req.Header.Set("Accept", "application/json")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	count, _, err := dbGetLikeState(app.db, "lik1020", "unused")
	require.NoError(t, err)
	assert.Zero(t, count, "404 path writes no like")

	// Default host: same id serves normally.
	resp = postLike(t, ts, "lik1020", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, count = decodeLikeResponse(t, resp)
	assert.Equal(t, 1, count)
}

// TestLikeToggleMethodGuard pins that a GET cannot perform the toggle.
// The response is the GET catch-all's 404, not a mux-generated 405:
// "GET /" (handleImageRoutes) matches every path, so ServeMux's
// method-mismatch branch never runs for GETs — the observable contract
// is simply "GET /<id>/like does nothing and answers not-found".
func TestLikeToggleMethodGuard(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1021", "2026-10-05 01:00:00")

	resp, err := http.Get(ts.URL + "/lik1021/like")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	count, _, err := dbGetLikeState(app.db, "lik1021", "unused")
	require.NoError(t, err)
	assert.Zero(t, count, "GET performed no like")
}

func TestLikeToggleRateLimited(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1022", "2026-10-05 01:00:00")

	// One IP hammers past the per-IP budget and trips 429...
	saw429 := false
	for i := 0; i < likesPerMinute+10 && !saw429; i++ {
		req, err := http.NewRequest("POST", ts.URL+"/lik1022/like", nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			saw429 = true
		} else {
			require.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
		}
	}
	require.True(t, saw429, "hammering the toggle trips the per-IP rate limit")

	// ...while a DIFFERENT IP keeps liking normally: the bucket is
	// per-IP (keyed like the SSE cap, XFF first), never a shared
	// process-wide budget one client could exhaust for everyone.
	// Each pre-429 hammer request was itself a cookieless like (fresh
	// minted token each), so the baseline is tracked, not guessed.
	before, _, err := dbGetLikeState(app.db, "lik1022", "none")
	require.NoError(t, err)
	resp := postLike(t, ts, "lik1022", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode, "RemoteAddr bucket is independent of the hammered XFF bucket")
	_, count := decodeLikeResponse(t, resp)
	assert.Equal(t, before+1, count, "the independent bucket's like landed")
}

// TestLikeCookieHelpers pins the token validator: 32 lowercase hex
// chars is the one accepted shape.
func TestLikeCookieHelpers(t *testing.T) {
	assert.True(t, validLikeToken("0123456789abcdef0123456789abcdef"))
	assert.False(t, validLikeToken("short"))
	assert.False(t, validLikeToken("0123456789ABCDEF0123456789ABCDEF"), "uppercase rejected")
	assert.False(t, validLikeToken("g123456789abcdef0123456789abcde"), "non-hex rejected")
	assert.False(t, validLikeToken(""))
}

// TestLikeTogglePublishesImageLiked drives the full path: a committed
// toggle fans out image-liked with the post-toggle count, and the
// payload shape is exactly {id, count}.
func TestLikeTogglePublishesImageLiked(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1030", "2026-10-05 01:00:00")

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	resp := postLike(t, ts, "lik1030", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageLiked, f.name)
	var p imageLikedEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, "lik1030", p.ID)
	assert.Equal(t, 1, p.Count)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &raw))
	assert.ElementsMatch(t, []string{"id", "count"}, mapKeys(raw), "payload fields are exactly id+count")

	// Commit-before-publish: re-querying at event-observation time
	// must already see the like.
	count, _, err := dbGetLikeState(app.db, "lik1030", "unused")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// TestLikeToggleSiteFiltersImageLiked pins the per-site delivery rule:
// a like on an image invisible on the safe host reaches default-site
// subscribers only — the safe stream must stay silent (no existence
// leak via like events).
func TestLikeToggleSiteFiltersImageLiked(t *testing.T) {
	app := newTestApp(t, safeSiteTestConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1031", "2026-10-05 01:00:00", func(img *dbImage) {
		img.Network = ptrStr("efnet") // invisible on the safe host
	})

	defFrames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, defFrames) // retry hint
	safeFrames, _, _ := openEventsHost(t, ts, "safe.example.com", nil, "")
	nextSSEFrame(t, safeFrames) // retry hint

	resp := postLike(t, ts, "lik1031", "", "application/json") // default host likes
	require.Equal(t, http.StatusOK, resp.StatusCode)

	f := nextSSEEvent(t, defFrames)
	require.Equal(t, eventImageLiked, f.name)

	for _, f := range drainSSEFrames(t, safeFrames) {
		assert.NotEqual(t, eventImageLiked, f.name, "safe stream never hears about invisible images")
	}
}

// TestPublishImageLikedNilHubSafe mirrors TestPublishesAreNilHubSafe.
func TestPublishImageLikedNilHubSafe(t *testing.T) {
	app := newTestApp(t, testConfig()) // no hub attached
	app.publishImageLiked(&dbImage{ID: "lik1032", Safety: safetyUnknown}, 1)
}

// ---------------------------------------------------------------------------
// Rendering: card counts + details-page button
// ---------------------------------------------------------------------------

func TestDbGetGalleryPageHydratesCounts(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImage(t, app, "lik1040", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1041", "2026-10-05 02:00:00")
	for _, tok := range []string{"t1", "t2", "t3"} {
		_, _, err := dbToggleLike(app.db, "lik1041", tok)
		require.NoError(t, err)
	}

	rows, err := dbGetGalleryPage(app.db, "", "", 10, siteCtx{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	byID := map[string]int{rows[0].ID: rows[0].LikeCount, rows[1].ID: rows[1].LikeCount}
	assert.Equal(t, 0, byID["lik1040"], "unliked row hydrates to zero")
	assert.Equal(t, 3, byID["lik1041"])
}

func TestGalleryCardsShowLikeCounts(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1042", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1043", "2026-10-05 02:00:00")
	insertImage(t, app, "lik1044", "2026-10-05 03:00:00")
	for _, tok := range []string{"t1", "t2"} {
		_, _, err := dbToggleLike(app.db, "lik1043", tok)
		require.NoError(t, err)
	}

	status, body := getPage(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, status)

	// Liked card: right-justified count on the time line.
	assert.Contains(t, body, `<span class="likes" data-count="2">&#9829; 2</span>`,
		"liked card renders a count-carrying likes span")
	// Exactly one .likes span exists — zero-like cards render none.
	assert.Equal(t, 1, strings.Count(body, `class="likes"`), "zero-like cards show no span")
}

func TestSearchCardsShowLikeCounts(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1045", "2026-10-05 01:00:00", func(img *dbImage) {
		img.OriginalPrompt = "shrew parade five"
	})
	_, _, err := dbToggleLike(app.db, "lik1045", "t9")
	require.NoError(t, err)

	status, body := getPage(t, ts.URL, "/search?q=shrew")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `<span class="likes" data-count="1">&#9829; 1</span>`,
		"search cards carry counts via the shared partial + hydration")
}

// ---------------------------------------------------------------------------
// Liked sort mode (?sort=liked)
// ---------------------------------------------------------------------------

func TestParseLikedKeysetCursor(t *testing.T) {
	count, createdAt, id, ok := parseLikedKeysetCursor("3~2026-10-05 01:00:00.000~lik1050")
	require.True(t, ok)
	assert.Equal(t, 3, count)
	assert.Equal(t, "2026-10-05 01:00:00.000", createdAt)
	assert.Equal(t, "lik1050", id)

	for _, bad := range []string{
		"",                                        // empty
		"~2026-10-05 01:00:00.000~lik1050",        // empty count
		"x~2026-10-05 01:00:00.000~lik1050",       // non-numeric count
		"-1~2026-10-05 01:00:00.000~lik1050",      // negative count
		"3~not-a-time~lik1050",                    // bad timestamp
		"3~2026-10-05 01:00:00.000~nope!!",        // bad id
		"3~lik1050",                               // wrong arity
		"3~2026-10-05 01:00:00.000~lik1050~extra", // wrong arity
	} {
		_, _, _, ok := parseLikedKeysetCursor(bad)
		assert.False(t, ok, "cursor %q must be rejected", bad)
	}

	// Zero count is valid (the first row of a zero-like tail is a
	// legitimate cursor position).
	_, _, _, ok = parseLikedKeysetCursor("0~2026-10-05 01:00:00.000~lik1050")
	assert.True(t, ok, "zero count is a valid cursor")
}

func TestDbGetGalleryPageLikedOrderingAndPaging(t *testing.T) {
	app := newTestApp(t, testConfig())
	// Counts: lik1050=2, lik1051=0, lik1052=3, lik1053=2 (created
	// NEWER than lik1050 — the count-tie tiebreak must favor it).
	// Expected liked order: lik1052, lik1053, lik1050, lik1051.
	insertImage(t, app, "lik1050", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1051", "2026-10-05 04:00:00")
	insertImage(t, app, "lik1052", "2026-10-05 03:00:00")
	insertImage(t, app, "lik1053", "2026-10-05 02:00:00")
	likes := map[string]int{"lik1050": 2, "lik1051": 0, "lik1052": 3, "lik1053": 2}
	for id, n := range likes {
		for i := 0; i < n; i++ {
			_, _, err := dbToggleLike(app.db, id, fmt.Sprintf("t%d", i))
			require.NoError(t, err)
		}
	}

	rows, err := dbGetGalleryPageLiked(app.db, 0, "", "", 10, siteCtx{})
	require.NoError(t, err)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.ID
		assert.Equal(t, likes[r.ID], r.LikeCount, "rows come back hydrated")
	}
	assert.Equal(t, []string{"lik1052", "lik1053", "lik1050", "lik1051"}, got,
		"count DESC, then created DESC on ties, zero-like rows last")

	// Keyset paging with limit 2: page 2 continues exactly where page
	// 1 ended (no dupes, no skips — on static data).
	p1, err := dbGetGalleryPageLiked(app.db, 0, "", "", 2, siteCtx{})
	require.NoError(t, err)
	require.Len(t, p1, 2)
	last := p1[1]
	p2, err := dbGetGalleryPageLiked(app.db, last.LikeCount, last.CreatedAt, last.ID, 2, siteCtx{})
	require.NoError(t, err)
	require.Len(t, p2, 2)
	assert.Equal(t, []string{"lik1050", "lik1051"}, []string{p2[0].ID, p2[1].ID},
		"cursor excludes everything at-or-above its position")
}

func TestGalleryLikedSortMarkup(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1060", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1061", "2026-10-05 02:00:00")
	for _, tok := range []string{"t1", "t2"} {
		_, _, err := dbToggleLike(app.db, "lik1061", tok)
		require.NoError(t, err)
	}

	status, body := getPage(t, ts.URL, "/?sort=liked")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `data-sort="liked"`, "body carries the sort mode for the fragment fetcher")
	assert.Contains(t, body, `href="/?sort=liked" aria-current="page"`, "active sort link is marked")
	assert.Contains(t, body, `<a href="/">newest</a>`, "inactive link carries no aria-current")
	assert.Less(t, strings.Index(body, "prompt for lik1061"), strings.Index(body, "prompt for lik1060"),
		"liked card sorts above the unliked one")

	// Without the param: no data-sort, default newest ordering.
	status, body = getPage(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body, `data-sort="liked"`)
	assert.Contains(t, body, `href="/" aria-current="page"`, "newest is the current mode by default")
	assert.Less(t, strings.Index(body, "prompt for lik1061"), strings.Index(body, "prompt for lik1060"),
		"default mode is newest-first (lik1061 is newer)")
	assert.NotContains(t, body, `href="/?sort=liked" aria-current="page"`, "liked is not current in default mode")

	// Garbage sort param falls back to default (no 400, no liked mode).
	status, body = getPage(t, ts.URL, "/?sort=garbage")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body, `data-sort="liked"`, "unknown sort values are ignored")
}

func TestGalleryFragmentLikedSort(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1062", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1063", "2026-10-05 02:00:00")
	insertImage(t, app, "lik1064", "2026-10-05 03:00:00")
	for _, tok := range []string{"t1", "t2"} {
		_, _, err := dbToggleLike(app.db, "lik1062", tok)
		require.NoError(t, err)
	}

	status, body := getPage(t, ts.URL, "/gallery?sort=liked")
	require.Equal(t, http.StatusOK, status)
	assert.Less(t, strings.Index(body, "lik1062"), strings.Index(body, "lik1064"),
		"fragment honors the liked sort")

	// Cursor paging through the fragment endpoint: after the most
	// liked row, the remainder follows.
	cursor := "2~2026-10-05 01:00:00~lik1062"
	status, body = getPage(t, ts.URL, "/gallery?sort=liked&after="+url.QueryEscape(cursor))
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "lik1064")
	assert.NotContains(t, body, "lik1062", "cursor excludes its own row")

	// Malformed liked cursor: 400 like the default mode.
	status, _ = getPage(t, ts.URL, "/gallery?sort=liked&after=garbage")
	assert.Equal(t, http.StatusBadRequest, status)

	// Default-mode cursors are NOT valid liked-mode cursors (2 parts,
	// no count) — must 400, not silently mis-parse.
	status, _ = getPage(t, ts.URL, "/gallery?sort=liked&after="+url.QueryEscape("2026-10-05 01:00:00~lik1062"))
	assert.Equal(t, http.StatusBadRequest, status)
}

// getPageCookie is getPage with a Cookie header (the liked-state render
// path keys off the liker token).
func getPageCookie(t *testing.T, tsURL, path, cookie string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", tsURL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func TestDetailsPageLikeButton(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1046", "2026-10-05 01:00:00")

	// Anonymous visit: not liked, count 0, form posts back to the page.
	status, body := getPage(t, ts.URL, "/lik1046")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `action="/lik1046/like"`, "no-JS form targets the toggle")
	assert.Contains(t, body, `aria-pressed="false"`)
	assert.Contains(t, body, `id="like-count">0<`, "count renders inside the button")
	assert.NotContains(t, body, `class="like-btn liked"`)

	// Liked visit: same page renders the pressed state + count.
	_, _, err := dbToggleLike(app.db, "lik1046", "0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	status, body = getPageCookie(t, ts.URL, "/lik1046", likeCookieName+"=0123456789abcdef0123456789abcdef")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `aria-pressed="true"`)
	assert.Contains(t, body, `class="like-btn liked"`)
	assert.Contains(t, body, `id="like-count">1<`)
}
