package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPageServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	db := newTestDB(t)
	srv := NewServer(db, &Config{Server: ServerConfig{BaseURL: "https://s.example.com"}})
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)
	return srv, ts
}

func TestSessionPageRenders(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa", func(x *Session) { x.UserID = ptrInt64(1) })
	mkMessage(t, db, s.ID, "user", "hello **world**")
	m := mkMessage(t, db, s.ID, "assistant", "hi there")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "hello <strong>world</strong>")
	assert.Contains(t, body, `id="m`)
	assert.Contains(t, body, "/s/aaaaaaaa/m/"+fmtInt(m.ID)+"/raw")
}

func TestSlugValidation(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	for _, slug := range []string{"short", "toolongslug1", "with/slash", "SYMBOLS!", "aaaaaaaa ", ""} {
		resp, err := http.Get(ts.URL + "/s/" + slug)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, 404, resp.StatusCode, "slug %q", slug)
	}
}

func TestSessionPageSoftDeleted404(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	require.NoError(t, db.Delete(&Session{}, s.ID).Error) // soft delete

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestSessionPageTailCopiesNotDoublyRendered(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "user", "unique-text-xyz")
	mkMessage(t, db, s.ID, "user", "unique-text-xyz", func(m *Message) { c := int64(9); m.SourceCompactionID = &c })

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 1, strings.Count(readBody(t, resp), "unique-text-xyz"))
}

func TestRawEndpointByteForByte(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	stored := "\x02bold\x02 **not strong** `code` http://x.example/"
	m := mkMessage(t, db, s.ID, "user", stored)

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/m/" + fmtInt(m.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, stored, readBody(t, resp)) // byte-for-byte, IRC codes intact
}

func TestRawEndpointMulticontentAndToolCalls(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mc := `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://i.example/x.png"}}]`
	mMC := mkMessage(t, db, s.ID, "user", "", func(m *Message) { m.MultiContent = &mc })
	tc := `[{"id":"call_1","function":{"name":"gen","arguments":"{}"}}]`
	mTC := mkMessage(t, db, s.ID, "assistant", "", func(m *Message) { m.ToolCalls = &tc })

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/m/" + fmtInt(mMC.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, mc, readBody(t, resp)) // raw multicontent JSON verbatim

	resp, err = http.Get(ts.URL + "/s/aaaaaaaa/m/" + fmtInt(mTC.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, tc, readBody(t, resp)) // raw tool_calls JSON verbatim
}

func TestRawEndpointWrongSession404(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s1 := mkSession(t, db, "aaaaaaaa")
	s2 := mkSession(t, db, "bbbbbbbb")
	m := mkMessage(t, db, s2.ID, "user", "x")
	resp, err := http.Get(ts.URL + "/s/" + s1.Slug + "/m/" + fmtInt(m.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestRawEndpointMalformedID404(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	mkSession(t, db, "aaaaaaaa")
	for _, id := range []string{"notanumber", "-1", "99999999999999999999"} {
		resp, err := http.Get(ts.URL + "/s/aaaaaaaa/m/" + id + "/raw")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, 404, resp.StatusCode, "id %q", id)
	}
}

func TestOlderRowsFragment(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	for i := 0; i < 5; i++ {
		mkMessage(t, db, s.ID, "user", "row")
	}
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa?fragment=rows&before=4")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `id="m3"`)
	assert.NotContains(t, body, `id="m4"`)
}

func TestLanding(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "^link")
}

func TestSecurityHeaders(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "x")

	for _, path := range []string{"/", "/s/aaaaaaaa", "/s/aaaaaaaa?fragment=rows&before=" + fmtInt(m.ID), "/s/aaaaaaaa/m/" + fmtInt(m.ID) + "/raw", "/static/style.css"} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, "default-src 'none'; img-src * data:; style-src 'self'; script-src 'self'; connect-src 'self'",
			resp.Header.Get("Content-Security-Policy"), "path %s", path)
		assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"), "path %s", path)
		assert.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"), "path %s", path)
	}
}

func TestNoStoreCaching(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "x")

	for _, path := range []string{"/s/aaaaaaaa", "/s/aaaaaaaa?fragment=rows&before=" + fmtInt(m.ID), "/s/aaaaaaaa/m/" + fmtInt(m.ID) + "/raw"} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"), "path %s", path)
	}
}

func TestStaticServed(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/static/style.css")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/css; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Contains(t, readBody(t, resp), ".msg")

	// No directory listing, no missing-file body.
	for _, path := range []string{"/static/", "/static/nope.css", "/static/../config.toml"} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, 404, resp.StatusCode, "path %s", path)
	}
}

func TestSessionPageHeaderUsageByModel(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	u := User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "alice", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, db.Create(&u).Error)
	s := mkSession(t, db, "aaaaaaaa", func(x *Session) { x.UserID = &u.ID })
	mkMessage(t, db, s.ID, "user", "q")
	base := time.Now()
	for _, row := range []TurnUsage{
		{SessionID: s.ID, Model: "m-a", PromptTokens: 100, CompletionTokens: 10, CreatedAt: base},
		{SessionID: s.ID, Model: "m-a", PromptTokens: 50, CompletionTokens: 5, CreatedAt: base.Add(time.Second)},
		{SessionID: s.ID, Model: "m-b", PromptTokens: 7, CompletionTokens: 2, CreatedAt: base.Add(2 * time.Second)},
	} {
		require.NoError(t, db.Create(&row).Error)
	}

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "usage-by-model")
	assert.Contains(t, body, "m-a")
	assert.Contains(t, body, "m-b")
	assert.Contains(t, body, "model changed") // marker: >1 distinct model
	assert.Contains(t, body, "libera")        // network badge
	assert.Contains(t, body, `/u/uuuuuuuu`)   // user page link from UserID
	// Aggregated totals: 150 in / 15 out for m-a.
	assert.Contains(t, body, "150 in")
	assert.Contains(t, body, "15 out")
}

// TestSessionPageSystemPromptBody pins carried fix 1 (Task 7 review):
// the collapsed system-prompt details must carry the (escaped) raw
// content — system rows get no markdown render, so interpolating only
// ContentHTML left the details body empty.
func TestSessionPageSystemPromptBody(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "system", "SYS-PROMPT-XYZ <b>not html</b> & more")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "SYS-PROMPT-XYZ")
	assert.Contains(t, body, "&lt;b&gt;not html&lt;/b&gt; &amp; more") // escaped, never live HTML
}

// TestSessionPageNoBlankImageSrc pins carried fix 2 (Task 7 review): a
// multicontent image part whose URL fails the safeImageURL allowlist
// renders NO img element at all, never src="".
func TestSessionPageNoBlankImageSrc(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mc := `[{"type":"image_url","image_url":{"url":"https://img.example.com/x.png"}},` +
		`{"type":"image_url","image_url":{"url":"javascript:alert(1)"}}]`
	mkMessage(t, db, s.ID, "user", "pics", func(m *Message) { m.MultiContent = &mc })

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Equal(t, 1, strings.Count(body, "<img"))
	assert.Contains(t, body, `src="https://img.example.com/x.png"`)
	assert.NotContains(t, body, `src=""`)
}

// TestSessionPageHasOlderButton pins the pagination affordance: the
// button renders (with the oldest loaded id as its keyset cursor) only
// when older rows exist beyond the initial 200-message page.
func TestSessionPageHasOlderButton(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	for i := 0; i < 205; i++ {
		mkMessage(t, db, s.ID, "user", "row")
	}
	// ids 1..205: newest-200 page spans 6..205 → oldest loaded = 6.
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `id="load-older"`)
	assert.Contains(t, body, `data-oldest="6"`)
	assert.Contains(t, body, `id="m205"`)

	s2 := mkSession(t, db, "bbbbbbbb")
	mkMessage(t, db, s2.ID, "user", "only")
	resp2, err := http.Get(ts.URL + "/s/bbbbbbbb")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.NotContains(t, readBody(t, resp2), "load-older")
}

// TestFragmentCompactionDividerChunked pins carried fix 3 (Task 7
// review): a compaction divider renders in exactly ONE chunk — the one
// whose rows touch the archived range — never at the top of every chunk
// above the boundary (renderRowsHTML's top-of-page fallback).
func TestFragmentCompactionDividerChunked(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	for i := 0; i < 205; i++ {
		mkMessage(t, db, s.ID, "user", "row")
	}
	// Archive ids 1..3 (summary at 2): the range lies entirely in the
	// OLDER chunk; the newest-200 page (6..205) must not show it.
	comp := Compaction{SessionID: s.ID, SummaryMessageID: 2, FirstArchivedID: 1, LastArchivedID: 3,
		ArchivedCount: 3, Trigger: "auto", Model: "m", CreatedAt: time.Now()}
	require.NoError(t, db.Create(&comp).Error)

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.NotContains(t, readBody(t, resp), "compaction-divider") // not the newer chunk

	resp, err = http.Get(ts.URL + "/s/aaaaaaaa?fragment=rows&before=6")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Equal(t, 1, strings.Count(body, "compaction-divider")) // exactly once, in the owning chunk
	assert.Contains(t, body, `id="m4"`)                           // divider sits before the first row past the range
}
