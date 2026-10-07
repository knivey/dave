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

	// The PK (image_id, token) is what enforces one vote per anonymous
	// token per image — the whole dedupe story, and the reason like and
	// dislike are mutually exclusive — so pin its shape. The vote
	// column (migration 005) says WHICH stance the row holds.
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
	assert.Len(t, cols, 4, "sqlite reports cid+name for each column")
	for _, c := range cols {
		assert.Equal(t, 1, c.NotNull, "%s is NOT NULL", c.Name)
	}
	byName := map[string]col{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	require.Len(t, byName, 4, "likes has exactly image_id, token, created_at, vote")
	assert.Equal(t, 1, byName["image_id"].PK, "image_id is the first PK column")
	assert.Equal(t, 2, byName["token"].PK, "token is the second PK column")
	assert.Equal(t, 0, byName["created_at"].PK, "created_at is not part of the PK")
	assert.Equal(t, 0, byName["vote"].PK, "vote is not part of the PK")
	assert.Equal(t, "INTEGER", byName["vote"].Type)
	// modernc's PRAGMA reports dflt_value as text; either driver shape
	// stringifies to "1".
	assert.Equal(t, "1", fmt.Sprint(byName["vote"].Dflt), "existing rows backfill as likes (vote = 1)")

	// Count lookups filter (image_id, vote): like count, dislike
	// count, and the net-score ORDER BY of the liked sort.
	var idx string
	require.NoError(t, db.Get(&idx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_likes_image_vote'`))
	assert.Equal(t, "idx_likes_image_vote", idx, "vote-filtered count index must exist")

	// The pre-dislikes image_id-only index was replaced by the
	// composite (its leading column covers the same lookups).
	var old string
	err := db.Get(&old,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_likes_image'`)
	assert.Error(t, err, "redundant prefix index was dropped in the same migration")
}

func TestToggleVoteRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "lik0001", SHA256: "aa", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-05 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))

	// Like, unlike, re-like from one token; a second token stacks.
	held, likes, dislikes, err := dbToggleVote(db, "lik0001", "tok1", voteLike)
	require.NoError(t, err)
	assert.True(t, held, "first toggle likes")
	assert.Equal(t, 1, likes)
	assert.Equal(t, 0, dislikes)

	held, likes, dislikes, err = dbToggleVote(db, "lik0001", "tok1", voteLike)
	require.NoError(t, err)
	assert.False(t, held, "second toggle un-likes")
	assert.Equal(t, 0, likes)
	assert.Equal(t, 0, dislikes)

	held, likes, dislikes, err = dbToggleVote(db, "lik0001", "tok1", voteLike)
	require.NoError(t, err)
	assert.True(t, held, "third toggle re-likes")
	assert.Equal(t, 1, likes)

	held, likes, dislikes, err = dbToggleVote(db, "lik0001", "tok2", voteLike)
	require.NoError(t, err)
	assert.True(t, held)
	assert.Equal(t, 2, likes, "independent tokens count independently")
}

// TestToggleVoteMutualExclusivity drives the whole stance state
// machine for one token: like → switch to dislike (no double-count,
// no second row) → switch back to like → retract to neutral.
func TestToggleVoteMutualExclusivity(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "lik0004", SHA256: "dd", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-05 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))

	held, likes, dislikes, err := dbToggleVote(db, "lik0004", "tok1", voteLike)
	require.NoError(t, err)
	assert.True(t, held)
	assert.Equal(t, 1, likes)
	assert.Equal(t, 0, dislikes)

	// Disliking a liked image SWITCHES the stance: the like is gone,
	// the dislike replaces it — never both at once.
	held, likes, dislikes, err = dbToggleVote(db, "lik0004", "tok1", voteDislike)
	require.NoError(t, err)
	assert.True(t, held, "dislike on a held like switches, not stacks")
	assert.Equal(t, 0, likes)
	assert.Equal(t, 1, dislikes)

	// The PK forbids a token holding two rows even if some writer
	// tried: exactly one row for this token.
	var n int
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM likes WHERE image_id = ? AND token = ?`, "lik0004", "tok1"))
	assert.Equal(t, 1, n, "one token, one row — the PK is the mutual exclusivity")

	// A second dislike toggle retracts to neutral.
	held, likes, dislikes, err = dbToggleVote(db, "lik0004", "tok1", voteDislike)
	require.NoError(t, err)
	assert.False(t, held, "repeat dislike retracts")
	assert.Equal(t, 0, likes)
	assert.Equal(t, 0, dislikes)

	// And like can switch from a held dislike too.
	_, _, _, err = dbToggleVote(db, "lik0004", "tok1", voteDislike)
	require.NoError(t, err)
	held, likes, dislikes, err = dbToggleVote(db, "lik0004", "tok1", voteLike)
	require.NoError(t, err)
	assert.True(t, held, "like on a held dislike switches")
	assert.Equal(t, 1, likes)
	assert.Equal(t, 0, dislikes)

	// Mixed tokens: the tallies report both sides independently.
	_, _, _, err = dbToggleVote(db, "lik0004", "tok2", voteDislike)
	require.NoError(t, err)
	_, _, _, err = dbToggleVote(db, "lik0004", "tok3", voteDislike)
	require.NoError(t, err)
	likes, dislikes, _, err = dbGetVoteState(db, "lik0004", "nobody")
	require.NoError(t, err)
	assert.Equal(t, 1, likes)
	assert.Equal(t, 2, dislikes)
}

func TestDbGetVoteState(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{ID: "lik0002", SHA256: "bb", Filename: "f.webp", MimeType: "image/webp",
		SizeBytes: 1, CreatedAt: "2026-10-05 01:00:00", ThumbStatus: thumbStatusPending}
	require.NoError(t, dbInsertImage(db, in))
	for _, tok := range []string{"tok1", "tok2", "tok3"} {
		_, _, _, err := dbToggleVote(db, "lik0002", tok, voteLike)
		require.NoError(t, err)
	}
	_, _, _, err := dbToggleVote(db, "lik0002", "tok3", voteDislike) // tok3 switches
	require.NoError(t, err)

	likes, dislikes, mine, err := dbGetVoteState(db, "lik0002", "tok1")
	require.NoError(t, err)
	assert.Equal(t, 2, likes)
	assert.Equal(t, 1, dislikes)
	assert.Equal(t, voteLike, mine, "token holding a like reports voteLike")

	_, _, mine, err = dbGetVoteState(db, "lik0002", "tok3")
	require.NoError(t, err)
	assert.Equal(t, voteDislike, mine, "token holding a dislike reports voteDislike")

	_, _, mine, err = dbGetVoteState(db, "lik0002", "stranger")
	require.NoError(t, err)
	assert.Equal(t, 0, mine, "unknown token reports no stance")

	// No votes at all: zero tallies, no stance, no error.
	likes, dislikes, mine, err = dbGetVoteState(db, "lik0003", "tok1")
	require.Error(t, err, "unknown image id surfaces the row miss")
	assert.Zero(t, likes)
	assert.Zero(t, dislikes)
	assert.Zero(t, mine)
}

func TestHydrateVoteCounts(t *testing.T) {
	db := setupTestDB(t)
	ids := []string{"hyd0001", "hyd0002", "hyd0003"}
	for i, id := range ids {
		in := &dbImage{ID: id, SHA256: id, Filename: "f.webp", MimeType: "image/webp",
			SizeBytes: 1, CreatedAt: "2026-10-05 01:00:0" + string(rune('0'+i)), ThumbStatus: thumbStatusPending}
		require.NoError(t, dbInsertImage(db, in))
	}
	// hyd0001: 2 likes + 1 dislike, hyd0002: none, hyd0003: 2 dislikes.
	for _, tok := range []string{"a", "b"} {
		_, _, _, err := dbToggleVote(db, "hyd0001", tok, voteLike)
		require.NoError(t, err)
	}
	_, _, _, err := dbToggleVote(db, "hyd0001", "c", voteDislike)
	require.NoError(t, err)
	for _, tok := range []string{"d", "e"} {
		_, _, _, err := dbToggleVote(db, "hyd0003", tok, voteDislike)
		require.NoError(t, err)
	}

	rows := []dbImage{{ID: "hyd0001"}, {ID: "hyd0002"}, {ID: "hyd0003"}}
	ptrs := []*dbImage{&rows[0], &rows[1], &rows[2]}
	require.NoError(t, hydrateVoteCounts(db, ptrs))
	assert.Equal(t, 2, rows[0].LikeCount)
	assert.Equal(t, 1, rows[0].DislikeCount)
	assert.Equal(t, 0, rows[1].LikeCount, "no votes hydrate to zero")
	assert.Equal(t, 0, rows[1].DislikeCount)
	assert.Equal(t, 0, rows[2].LikeCount)
	assert.Equal(t, 2, rows[2].DislikeCount)
}

// ---------------------------------------------------------------------------
// POST /{id}/like and POST /{id}/dislike — toggle handlers
// ---------------------------------------------------------------------------

// postVote posts to one of the toggle endpoints and returns the
// response (body NOT closed — tests either read it or pass through).
func postVote(t *testing.T, ts *httptest.Server, id, kind, cookie, accept string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/"+id+"/"+kind, nil)
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

func postLike(t *testing.T, ts *httptest.Server, id, cookie, accept string) *http.Response {
	t.Helper()
	return postVote(t, ts, id, "like", cookie, accept)
}

func postDislike(t *testing.T, ts *httptest.Server, id, cookie, accept string) *http.Response {
	t.Helper()
	return postVote(t, ts, id, "dislike", cookie, accept)
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

type voteResponse struct {
	Liked    bool `json:"liked"`
	Disliked bool `json:"disliked"`
	Likes    int  `json:"likes"`
	Dislikes int  `json:"dislikes"`
}

func decodeVoteResponse(t *testing.T, resp *http.Response) voteResponse {
	t.Helper()
	var payload voteResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload
}

func TestLikeToggleJSONRoundTrip(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1000", "2026-10-05 01:00:00")

	// First like: mints the cookie, likes, reports tallies 1/0.
	resp := postLike(t, ts, "lik1000", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vr := decodeVoteResponse(t, resp)
	assert.True(t, vr.Liked)
	assert.False(t, vr.Disliked)
	assert.Equal(t, 1, vr.Likes)
	assert.Equal(t, 0, vr.Dislikes)
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
	vr = decodeVoteResponse(t, resp)
	assert.False(t, vr.Liked, "second toggle un-likes")
	assert.False(t, vr.Disliked)
	assert.Equal(t, 0, vr.Likes)
	assert.Equal(t, 0, vr.Dislikes)
	assert.Empty(t, resp.Cookies(), "existing token is not re-minted")

	// And back on.
	resp = postLike(t, ts, "lik1000", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vr = decodeVoteResponse(t, resp)
	assert.True(t, vr.Liked)
	assert.Equal(t, 1, vr.Likes)
}

// TestDislikeToggleJSONRoundTrip drives the dislike endpoint's JSON
// contract, including the like→dislike switch (mutual exclusivity
// observable over HTTP with one cookie).
func TestDislikeToggleJSONRoundTrip(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1015", "2026-10-05 01:00:00")

	resp := postLike(t, ts, "lik1015", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := likeCookieHeader(t, resp)

	// Dislike switches the held like: neither both-true nor
	// double-counted.
	resp = postDislike(t, ts, "lik1015", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vr := decodeVoteResponse(t, resp)
	assert.False(t, vr.Liked, "dislike releases the like")
	assert.True(t, vr.Disliked)
	assert.Equal(t, 0, vr.Likes)
	assert.Equal(t, 1, vr.Dislikes)

	// Repeat dislike retracts to neutral.
	resp = postDislike(t, ts, "lik1015", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vr = decodeVoteResponse(t, resp)
	assert.False(t, vr.Liked)
	assert.False(t, vr.Disliked)
	assert.Equal(t, 0, vr.Likes)
	assert.Equal(t, 0, vr.Dislikes)

	// And like switches from a held dislike.
	resp = postDislike(t, ts, "lik1015", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = postLike(t, ts, "lik1015", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vr = decodeVoteResponse(t, resp)
	assert.True(t, vr.Liked)
	assert.False(t, vr.Disliked)
	assert.Equal(t, 1, vr.Likes)
	assert.Equal(t, 0, vr.Dislikes)
}

// TestLikeToggleMalformedCookieMintsFresh proves a garbage cookie
// value is ignored (never trusted as identity) and a fresh token is
// issued instead.
func TestLikeToggleMalformedCookieMintsFresh(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1010", "2026-10-05 01:00:00")

	resp := postDislike(t, ts, "lik1010", likeCookieName+"=!!!not-hex!!!", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vr := decodeVoteResponse(t, resp)
	assert.True(t, vr.Disliked, "garbage cookie falls back to a fresh mint")
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

	// No Accept: application/json — the no-JS form path, for BOTH
	// formaction targets the details page's two submit buttons carry.
	for _, kind := range []string{"like", "dislike"} {
		resp := postVote(t, ts, "lik1011", kind, "", "")
		require.Equal(t, http.StatusSeeOther, resp.StatusCode, "%s path", kind)
		assert.Equal(t, "/lik1011", resp.Header.Get("Location"), "redirect returns to the details page")
		require.NotEmpty(t, resp.Cookies(), "cookie is minted on the form path too")
	}
}

func TestLikeToggleNotFoundAndGone(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1012", "2026-10-05 01:00:00", func(img *dbImage) {
		img.Hidden = true
	})

	resp := postLike(t, ts, "zzzzzzz", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "unknown id 404s")

	resp = postDislike(t, ts, "bad!", "", "application/json")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "malformed id 404s on the dislike path too")

	resp = postLike(t, ts, "lik1012", "", "application/json")
	assert.Equal(t, http.StatusGone, resp.StatusCode, "hidden id 410s")
}

// TestLikeToggleSiteMatrix pins the safe-host behavior: voting on an
// image invisible on the requesting site 404s exactly like the details
// page does (no existence leak), while the default host votes it
// normally.
func TestLikeToggleSiteMatrix(t *testing.T) {
	app := newTestApp(t, safeSiteTestConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1020", "2026-10-05 01:00:00", func(img *dbImage) {
		img.Network = ptrStr("efnet") // not in allowed_networks, unknown safety
	})

	// Safe host: invisible row → 404, no vote recorded (dislike path
	// probed too — the two endpoints share the visibility gate).
	for _, kind := range []string{"like", "dislike"} {
		req, err := http.NewRequest("POST", ts.URL+"/lik1020/"+kind, nil)
		require.NoError(t, err)
		req.Host = "safe.example.com"
		req.Header.Set("Accept", "application/json")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s path", kind)
		likes, dislikes, err := dbVoteCounts(app.db, "lik1020")
		require.NoError(t, err)
		assert.Zero(t, likes, "404 path writes no vote")
		assert.Zero(t, dislikes)
	}

	// Default host: same id serves normally.
	resp := postLike(t, ts, "lik1020", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	vr := decodeVoteResponse(t, resp)
	assert.Equal(t, 1, vr.Likes)
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

	for _, path := range []string{"/lik1021/like", "/lik1021/dislike"} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "GET %s", path)
		likes, dislikes, err := dbVoteCounts(app.db, "lik1021")
		require.NoError(t, err)
		assert.Zero(t, likes, "GET performed no vote")
		assert.Zero(t, dislikes)
	}
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

	// The bucket is shared by BOTH endpoints (one handleVoteToggle
	// behind two routes): the already-exhausted IP 429s on the dislike
	// path too — disliking is not a second budget.
	req, err := http.NewRequest("POST", ts.URL+"/lik1022/dislike", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "dislike rides the same per-IP bucket")

	// ...while a DIFFERENT IP keeps liking normally: the bucket is
	// per-IP (keyed like the SSE cap, XFF first), never a shared
	// process-wide budget one client could exhaust for everyone.
	// Each pre-429 hammer request was itself a cookieless like (fresh
	// minted token each), so the baseline is tracked, not guessed.
	before, _, err := dbVoteCounts(app.db, "lik1022")
	require.NoError(t, err)
	resp = postLike(t, ts, "lik1022", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode, "RemoteAddr bucket is independent of the hammered XFF bucket")
	vr := decodeVoteResponse(t, resp)
	assert.Equal(t, before+1, vr.Likes, "the independent bucket's like landed")
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
// toggle fans out image-liked with the post-toggle tallies, and the
// payload shape is exactly {id, likes, dislikes}. A dislike toggle
// publishes the same event.
func TestLikeTogglePublishesImageLiked(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1030", "2026-10-05 01:00:00")

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	resp := postLike(t, ts, "lik1030", "", "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cookie := likeCookieHeader(t, resp)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageLiked, f.name)
	var p imageLikedEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, "lik1030", p.ID)
	assert.Equal(t, 1, p.Likes)
	assert.Equal(t, 0, p.Dislikes)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &raw))
	assert.ElementsMatch(t, []string{"id", "likes", "dislikes"}, mapKeys(raw), "payload fields are exactly id+likes+dislikes")

	// Commit-before-publish: re-querying at event-observation time
	// must already see the vote.
	likes, _, err := dbVoteCounts(app.db, "lik1030")
	require.NoError(t, err)
	assert.Equal(t, 1, likes)

	// A dislike switch publishes again — now the like tally is 0 and
	// the dislike tally is 1.
	resp = postDislike(t, ts, "lik1030", cookie, "application/json")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	f = nextSSEEvent(t, frames)
	require.Equal(t, eventImageLiked, f.name, "dislike toggles publish the same event")
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, 0, p.Likes)
	assert.Equal(t, 1, p.Dislikes)
}

// TestLikeToggleSiteFiltersImageLiked pins the per-site delivery rule:
// a vote on an image invisible on the safe host reaches default-site
// subscribers only — the safe stream must stay silent (no existence
// leak via vote events).
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

	resp := postLike(t, ts, "lik1031", "", "application/json") // default host votes
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
	app.publishImageLiked(&dbImage{ID: "lik1032", Safety: safetyUnknown}, 1, 0)
}

// ---------------------------------------------------------------------------
// Rendering: card counts + details-page buttons
// ---------------------------------------------------------------------------

func TestDbGetGalleryPageHydratesCounts(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImage(t, app, "lik1040", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1041", "2026-10-05 02:00:00")
	for _, tok := range []string{"t1", "t2", "t3"} {
		_, _, _, err := dbToggleVote(app.db, "lik1041", tok, voteLike)
		require.NoError(t, err)
	}
	_, _, _, err := dbToggleVote(app.db, "lik1041", "t3", voteDislike)
	require.NoError(t, err)

	rows, err := dbGetGalleryPage(app.db, "", "", 10, siteCtx{})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	byID := map[string]dbImage{rows[0].ID: rows[0], rows[1].ID: rows[1]}
	assert.Equal(t, 0, byID["lik1040"].LikeCount, "unvoted row hydrates to zero")
	assert.Equal(t, 0, byID["lik1040"].DislikeCount)
	assert.Equal(t, 2, byID["lik1041"].LikeCount, "t3's switch released its like")
	assert.Equal(t, 1, byID["lik1041"].DislikeCount)
}

func TestGalleryCardsShowLikeCounts(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1042", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1043", "2026-10-05 02:00:00")
	insertImage(t, app, "lik1044", "2026-10-05 03:00:00")
	for _, tok := range []string{"t1", "t2"} {
		_, _, _, err := dbToggleVote(app.db, "lik1043", tok, voteLike)
		require.NoError(t, err)
	}
	_, _, _, err := dbToggleVote(app.db, "lik1043", "t3", voteDislike)
	require.NoError(t, err)

	status, body := getPage(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, status)

	// Voted card: right-justified tallies on the time line — the
	// .counts wrapper holds one span per non-zero tally.
	assert.Contains(t, body, `<span class="counts"><span class="likes" data-count="2">&#9829; 2</span><span class="dislikes" data-count="1">&#128148; 1</span></span>`,
		"voted card renders wrapped like+dislike spans")
	// Exactly one of each span exists — zero-tally cards render none.
	assert.Equal(t, 1, strings.Count(body, `class="likes"`), "zero-like cards show no span")
	assert.Equal(t, 1, strings.Count(body, `class="dislikes"`), "zero-dislike cards show no span")
}

func TestSearchCardsShowLikeCounts(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1045", "2026-10-05 01:00:00", func(img *dbImage) {
		img.OriginalPrompt = "shrew parade five"
	})
	_, _, _, err := dbToggleVote(app.db, "lik1045", "t9", voteDislike)
	require.NoError(t, err)

	status, body := getPage(t, ts.URL, "/search?q=shrew")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `<span class="counts"><span class="dislikes" data-count="1">&#128148; 1</span></span>`,
		"search cards carry tallies via the shared partial + hydration")
}

// ---------------------------------------------------------------------------
// Top-rated sort mode (?sort=liked, ranked by net score)
// ---------------------------------------------------------------------------

func TestParseLikedKeysetCursor(t *testing.T) {
	count, createdAt, id, ok := parseLikedKeysetCursor("3~2026-10-05 01:00:00.000~lik1050")
	require.True(t, ok)
	assert.Equal(t, 3, count)
	assert.Equal(t, "2026-10-05 01:00:00.000", createdAt)
	assert.Equal(t, "lik1050", id)

	// Negative scores are legal cursor positions (likes − dislikes can
	// go negative — that is the point of dislikes demoting).
	count, _, _, ok = parseLikedKeysetCursor("-2~2026-10-05 01:00:00.000~lik1050")
	require.True(t, ok, "negative score is a valid cursor")
	assert.Equal(t, -2, count)

	for _, bad := range []string{
		"",                                        // empty
		"~2026-10-05 01:00:00.000~lik1050",        // empty score
		"x~2026-10-05 01:00:00.000~lik1050",       // non-numeric score
		"--1~2026-10-05 01:00:00.000~lik1050",     // malformed sign
		"3~not-a-time~lik1050",                    // bad timestamp
		"3~2026-10-05 01:00:00.000~nope!!",        // bad id
		"3~lik1050",                               // wrong arity
		"3~2026-10-05 01:00:00.000~lik1050~extra", // wrong arity
	} {
		_, _, _, ok := parseLikedKeysetCursor(bad)
		assert.False(t, ok, "cursor %q must be rejected", bad)
	}

	// Zero score is valid (the first row of a zero-score tail is a
	// legitimate cursor position).
	_, _, _, ok = parseLikedKeysetCursor("0~2026-10-05 01:00:00.000~lik1050")
	assert.True(t, ok, "zero score is a valid cursor")
}

func TestDbGetGalleryPageLikedOrderingAndPaging(t *testing.T) {
	app := newTestApp(t, testConfig())
	// Scores (likes − dislikes):
	//   lik1050 = 2 (2 likes),        created 01:00
	//   lik1051 = 0 (unvoted),        created 04:00
	//   lik1052 = 3 (3 likes),        created 03:00
	//   lik1053 = 2 (3 likes, 1 dislike — created NEWER than lik1050,
	//               so the score tie must favor it), created 02:00
	//   lik1054 = -2 (1 like, 3 dislikes), created 05:00
	//   lik1055 = -1 (1 dislike),     created 06:00
	// Expected order: 1052(3), 1053(2), 1050(2), 1051(0), 1055(-1), 1054(-2).
	insertImage(t, app, "lik1050", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1051", "2026-10-05 04:00:00")
	insertImage(t, app, "lik1052", "2026-10-05 03:00:00")
	insertImage(t, app, "lik1053", "2026-10-05 02:00:00")
	insertImage(t, app, "lik1054", "2026-10-05 05:00:00")
	insertImage(t, app, "lik1055", "2026-10-05 06:00:00")
	likeCounts := map[string]int{"lik1050": 2, "lik1052": 3, "lik1053": 3, "lik1054": 1}
	dislikeCounts := map[string]int{"lik1053": 1, "lik1054": 3, "lik1055": 1}
	for id, n := range likeCounts {
		for i := 0; i < n; i++ {
			_, _, _, err := dbToggleVote(app.db, id, fmt.Sprintf("l%d", i), voteLike)
			require.NoError(t, err)
		}
	}
	for id, n := range dislikeCounts {
		for i := 0; i < n; i++ {
			_, _, _, err := dbToggleVote(app.db, id, fmt.Sprintf("d%d", i), voteDislike)
			require.NoError(t, err)
		}
	}

	rows, err := dbGetGalleryPageLiked(app.db, 0, "", "", 10, siteCtx{})
	require.NoError(t, err)
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.ID
		assert.Equal(t, likeCounts[r.ID], r.LikeCount, "rows come back with likes hydrated")
		assert.Equal(t, dislikeCounts[r.ID], r.DislikeCount, "rows come back with dislikes hydrated")
	}
	assert.Equal(t, []string{"lik1052", "lik1053", "lik1050", "lik1051", "lik1055", "lik1054"}, got,
		"net score DESC (dislikes demote, negatives last), then created DESC on ties")

	// Keyset paging with limit 3: page 2 continues exactly where page
	// 1 ended (no dupes, no skips — on static data). The cursor's
	// first component is the SCORE (likes − dislikes of the last row).
	p1, err := dbGetGalleryPageLiked(app.db, 0, "", "", 3, siteCtx{})
	require.NoError(t, err)
	require.Len(t, p1, 3)
	last := p1[2]
	assert.Equal(t, 2, last.LikeCount)
	assert.Equal(t, 0, last.DislikeCount)
	p2, err := dbGetGalleryPageLiked(app.db, last.LikeCount-last.DislikeCount, last.CreatedAt, last.ID, 3, siteCtx{})
	require.NoError(t, err)
	require.Len(t, p2, 3)
	assert.Equal(t, []string{"lik1051", "lik1055", "lik1054"}, []string{p2[0].ID, p2[1].ID, p2[2].ID},
		"cursor excludes everything at-or-above its position")

	// Paging across the negative-score boundary works: after lik1055
	// (score -1), only lik1054 remains.
	p3, err := dbGetGalleryPageLiked(app.db, p2[1].LikeCount-p2[1].DislikeCount, p2[1].CreatedAt, p2[1].ID, 3, siteCtx{})
	require.NoError(t, err)
	require.Len(t, p3, 1)
	assert.Equal(t, "lik1054", p3[0].ID)
}

func TestGalleryLikedSortMarkup(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "lik1060", "2026-10-05 01:00:00")
	insertImage(t, app, "lik1061", "2026-10-05 02:00:00")
	for _, tok := range []string{"t1", "t2"} {
		_, _, _, err := dbToggleVote(app.db, "lik1061", tok, voteLike)
		require.NoError(t, err)
	}

	status, body := getPage(t, ts.URL, "/?sort=liked")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `data-sort="liked"`, "body carries the sort mode for the fragment fetcher")
	assert.Contains(t, body, `href="/?sort=liked" aria-current="page"`, "active sort link is marked")
	assert.Contains(t, body, `<a href="/">newest</a>`, "inactive link carries no aria-current")
	assert.Contains(t, body, ">top rated</a>", "score-ranked mode is labeled honestly")
	assert.Less(t, strings.Index(body, "prompt for lik1061"), strings.Index(body, "prompt for lik1060"),
		"higher-scoring card sorts above the unvoted one")

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
		_, _, _, err := dbToggleVote(app.db, "lik1062", tok, voteLike)
		require.NoError(t, err)
	}

	status, body := getPage(t, ts.URL, "/gallery?sort=liked")
	require.Equal(t, http.StatusOK, status)
	assert.Less(t, strings.Index(body, "lik1062"), strings.Index(body, "lik1064"),
		"fragment honors the score sort")

	// Cursor paging through the fragment endpoint: after the top
	// scored row, the remainder follows.
	cursor := "2~2026-10-05 01:00:00~lik1062"
	status, body = getPage(t, ts.URL, "/gallery?sort=liked&after="+url.QueryEscape(cursor))
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "lik1064")
	assert.NotContains(t, body, "lik1062", "cursor excludes its own row")

	// Negative-score cursors page the demoted tail (the new shape the
	// score sort produces — parseLikedKeysetCursor must accept them).
	status, body = getPage(t, ts.URL, "/gallery?sort=liked&after="+url.QueryEscape("-1~2026-10-05 02:00:00~lik1063"))
	require.Equal(t, http.StatusOK, status, "negative score cursors are valid")
	assert.NotContains(t, body, "lik1063", "cursor excludes its own row")

	// Malformed liked cursor: 400 like the default mode.
	status, _ = getPage(t, ts.URL, "/gallery?sort=liked&after=garbage")
	assert.Equal(t, http.StatusBadRequest, status)

	// Default-mode cursors are NOT valid liked-mode cursors (2 parts,
	// no score) — must 400, not silently mis-parse.
	status, _ = getPage(t, ts.URL, "/gallery?sort=liked&after="+url.QueryEscape("2026-10-05 01:00:00~lik1062"))
	assert.Equal(t, http.StatusBadRequest, status)
}

// getPageCookie is getPage with a Cookie header (the vote-state render
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

	// Anonymous visit: no stance, zero tallies, form posts back to the
	// page via each button's formaction.
	status, body := getPage(t, ts.URL, "/lik1046")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `action="/lik1046/like"`, "no-JS form's implicit action is the like toggle")
	assert.Contains(t, body, `id="like-btn" formaction="/lik1046/like"`, "like button carries its formaction")
	assert.Contains(t, body, `id="dislike-btn" formaction="/lik1046/dislike"`, "dislike button carries its formaction")
	assert.Contains(t, body, `id="like-count">0<`, "like count renders inside the button")
	assert.Contains(t, body, `id="dislike-count">0<`, "dislike count renders inside the button")
	assert.Contains(t, body, `&#128148;`, "dislike button carries the broken-heart glyph")
	assert.Equal(t, 2, strings.Count(body, `aria-pressed="false"`), "both buttons report unpressed")
	assert.NotContains(t, body, `class="like-btn liked"`)
	assert.NotContains(t, body, `class="dislike-btn disliked"`)

	// Liked visit: same page renders the pressed state + count.
	_, _, _, err := dbToggleVote(app.db, "lik1046", "0123456789abcdef0123456789abcdef", voteLike)
	require.NoError(t, err)
	status, body = getPageCookie(t, ts.URL, "/lik1046", likeCookieName+"=0123456789abcdef0123456789abcdef")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `class="like-btn liked"`)
	assert.Contains(t, body, `id="like-count">1<`)
	assert.Contains(t, body, `title="unlike"`)
	assert.NotContains(t, body, `class="dislike-btn disliked"`, "the other stance stays unpressed")

	// Disliked visit: the SAME token's row now reports the dislike.
	_, _, _, err = dbToggleVote(app.db, "lik1046", "0123456789abcdef0123456789abcdef", voteDislike)
	require.NoError(t, err)
	status, body = getPageCookie(t, ts.URL, "/lik1046", likeCookieName+"=0123456789abcdef0123456789abcdef")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `class="dislike-btn disliked"`)
	assert.Contains(t, body, `title="remove dislike"`)
	assert.Contains(t, body, `id="dislike-count">1<`)
	assert.Contains(t, body, `id="like-count">0<`, "switching released the like")
	assert.NotContains(t, body, `class="like-btn liked"`, "mutually exclusive on the rendered page too")
}
