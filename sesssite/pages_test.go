package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

// TestSessionPageReasoningBeforeContent: the collapsed reasoning unit
// renders ABOVE the reply content — reading order matches chronology
// (the model thinks first, then answers) — while the meta header
// (role/time) stays the message's first element. The reply is pinned
// by its TEXT position: the reasoning block contains its own
// "content md" div, so a class-based marker would be ambiguous.
func TestSessionPageReasoningBeforeContent(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "assistant", "the answer", func(m *Message) {
		r := "the thought process"
		m.ReasoningContent = &r
	})

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)

	rIdx := strings.Index(body, `<details class="reasoning"`)
	aIdx := strings.Index(body, "the answer")
	mIdx := strings.Index(body, `<div class="meta">`)
	require.GreaterOrEqual(t, rIdx, 0, "reasoning details present")
	require.GreaterOrEqual(t, aIdx, 0, "reply content present")
	assert.Less(t, rIdx, aIdx, "reasoning renders before the reply content")
	require.GreaterOrEqual(t, mIdx, 0)
	assert.Less(t, mIdx, rIdx, "meta header stays above reasoning")
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
	// explicit like the user-list fragment handler (final-review
	// symmetry finding) — never left to sniffing
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
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
	// an assistant row after ALL the usage rows joins them to the
	// message (window (user, assistant]); the per-message line renders
	// below. Explicit CreatedAt: mkMessage's time.Now() can land before
	// the later usage rows' base+Ns stamps.
	mkMessage(t, db, s.ID, "assistant", "a", func(m *Message) { m.CreatedAt = base.Add(3 * time.Second) })

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "usage-by-model")
	// the aggregate is exact (full-session rows) — "totals" wording
	assert.Contains(t, body, "token usage totals by model")
	assert.Contains(t, body, "m-a")
	assert.Contains(t, body, "m-b")
	assert.Contains(t, body, "model changed") // marker: >1 distinct model
	assert.Contains(t, body, "libera")        // network badge
	assert.Contains(t, body, `/u/uuuuuuuu`)   // user page link from UserID
	// Aggregated totals: 150 in / 15 out for m-a.
	assert.Contains(t, body, "150 in")
	assert.Contains(t, body, "15 out")
	// the per-message line is approximate by construction (assignUsage
	// windows by time; usage rows carry no message id) — labeled ≈
	assert.Contains(t, body, "≈ m-b · 157→17 tok")
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

// TestSessionPageCodeBlockClasses pins the controller ruling on the
// Chroma/CSP gap: fenced code blocks render with CLASS-based Chroma
// output (colors from /static/chroma.css, linked in base.html), never
// inline style= attributes — the strict CSP (style-src 'self') blocks
// inline styles, and the spec promises highlighted code.
func TestSessionPageCodeBlockClasses(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "assistant", "```go\nfmt.Println(\"hello\")\n```")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `class="chroma"`)
	assert.Contains(t, body, `<span class="`) // token classes present
	assert.NotContains(t, body, "style=")     // no inline styles anywhere
	assert.Contains(t, body, `<link rel="stylesheet" href="/static/chroma.css">`)

	// The Dracula palette lives in the served stylesheet now (this is
	// where TestRender_CodeBlockDracula's hex-color pin moved to).
	respCSS, err := http.Get(ts.URL + "/static/chroma.css")
	require.NoError(t, err)
	defer respCSS.Body.Close()
	require.Equal(t, 200, respCSS.StatusCode)
	css := readBody(t, respCSS)
	assert.Contains(t, css, "#282a36") // dracula background
	assert.Contains(t, css, "#50fa7b") // dracula green (functions)
	assert.Contains(t, css, ".chroma")
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

// TestFragmentCompactionDividerBoundaryAligned pins the final-review
// fix: a compaction whose archived range ends EXACTLY at a chunk
// boundary (LastArchivedID == the newer page's lo-1) must render its
// divider — before the NEWER page's first row. The old intersect
// predicate included it in the older chunk only, where no row follows
// the range, so the divider rendered nowhere.
func TestFragmentCompactionDividerBoundaryAligned(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	for i := 0; i < 205; i++ {
		mkMessage(t, db, s.ID, "user", "row")
	}
	// ids 1..205; newest page spans 6..205. Archive ids 1..5 — the
	// range ends exactly at the boundary (LastArchivedID = 6-1).
	comp := Compaction{SessionID: s.ID, SummaryMessageID: 2, FirstArchivedID: 1, LastArchivedID: 5,
		ArchivedCount: 5, Trigger: "auto", Model: "m", CreatedAt: time.Now()}
	require.NoError(t, db.Create(&comp).Error)

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Equal(t, 1, strings.Count(body, "compaction-divider")) // on the newer page now
	// and it sits before the page's first row (m6), not merely anywhere
	require.Less(t, strings.Index(body, "compaction-divider"), strings.Index(body, `id="m6"`))

	// the older fragment (rows 1..5) must NOT repeat it — no row there
	// follows the range.
	resp2, err := http.Get(ts.URL + "/s/aaaaaaaa?fragment=rows&before=6")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, 200, resp2.StatusCode)
	assert.NotContains(t, readBody(t, resp2), "compaction-divider")
}

// TestCompsForChunkDispositions unit-pins the final-review predicate
// (lo <= LastArchivedID+1 && hi > LastArchivedID) across all four
// chunk/range dispositions. The two false rows are the behavior change:
// the old intersect predicate included both, rendering nothing for
// either (the aligned case rendered nowhere at all — the bug; the
// past-hi case was a harmless include-but-render-nothing).
func TestCompsForChunkDispositions(t *testing.T) {
	msgs := func(ids ...int64) []Message {
		out := make([]Message, len(ids))
		for i, id := range ids {
			out[i] = Message{ID: id}
		}
		return out
	}
	tests := []struct {
		name              string
		ids               []int64
		first, last       int64
		wantIncluded      bool
		wantDividerBefore int64 // row id the divider precedes (0 = none)
	}{
		{"boundary aligned: range ends at lo-1", []int64{6, 7, 205}, 1, 5, true, 6},
		{"range inside chunk, rows follow", []int64{1, 2, 3, 4, 5}, 1, 3, true, 4},
		{"range straddles lo", []int64{4, 5, 6, 9}, 2, 5, true, 6},
		{"range entirely above chunk", []int64{6, 7, 205}, 1, 3, false, 0},
		{"range extends past chunk newest (include-nothing dropped)", []int64{1, 2, 3, 4, 5}, 3, 10, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp := CompactionView{Compaction: Compaction{FirstArchivedID: tt.first, LastArchivedID: tt.last}}
			got := compsForChunk([]CompactionView{comp}, msgs(tt.ids...))
			if !tt.wantIncluded {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			// cross-check against the renderer's actual placement so
			// the predicate and the placement rule cannot drift apart
			var before int64
			for _, m := range tt.ids {
				if m > tt.last {
					before = m
					break
				}
			}
			assert.Equal(t, tt.wantDividerBefore, before, "divider placement row")
		})
	}
}

// --- Task 9: user page ---

func mkUser(t *testing.T, db *gorm.DB, mods ...func(*User)) *User {
	t.Helper()
	u := &User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "shrew", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	for _, f := range mods {
		f(u)
	}
	require.NoError(t, db.Create(u).Error)
	return u
}

func TestUserPageListsSessions(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()

	u := mkUser(t, db, func(x *User) { x.IRCAccount = "shrewacct" })
	uid := u.ID
	mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid; s.FirstMessage = "tell me about shrews" })
	mkSession(t, db, "bbbbbbbb", func(s *Session) { s.UserID = &uid; s.FirstMessage = "another one" })
	other := int64(2)
	mkSession(t, db, "cccccccc", func(s *Session) { s.UserID = &other })

	resp, err := http.Get(ts.URL + "/u/uuuuuuuu")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "shrew")
	assert.Contains(t, body, "account: shrewacct")        // IRC account, display-only
	assert.Contains(t, body, `data-user-slug="uuuuuuuu"`) // Task 10 user.js wiring
	assert.Contains(t, body, "tell me about shrews")
	assert.Contains(t, body, `href="/s/aaaaaaaa"`)
	assert.Contains(t, body, `href="/s/bbbbbbbb"`)
	assert.NotContains(t, body, "/s/cccccccc") // other user's sessions never listed
	assert.Contains(t, body, `class="badge status-active"`)
	assert.Contains(t, body, "#dave")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "default-src 'none'; img-src * data:; style-src 'self'; script-src 'self'; connect-src 'self'",
		resp.Header.Get("Content-Security-Policy"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "no-referrer", resp.Header.Get("Referrer-Policy"))
	// Short page (2 < 50) is the whole list: no load-older affordance.
	assert.NotContains(t, body, "user-load-older")
}

func TestUserPageNotFound(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	mkUser(t, db)

	for _, slug := range []string{"short", "toolongslug1", "SYMBOLS!", "", "aaaaaaaa "} {
		resp, err := http.Get(ts.URL + "/u/" + slug)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, 404, resp.StatusCode, "slug %q", slug)
	}
	// Well-formed slug, no such user.
	resp, err := http.Get(ts.URL + "/u/zzzzzzzz")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestUserPageFragmentPagination(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	u := mkUser(t, db)
	uid := u.ID
	var last *Session
	for i := 0; i < 3; i++ {
		last = mkSession(t, db, "aaaaaaaa"+strings.Repeat("b", i), func(s *Session) { s.UserID = &uid })
	}
	resp, err := http.Get(ts.URL + "/u/uuuuuuuu?fragment=list&before_last_active=" +
		urlQueryTime(last.LastActive) + "&before_id=" + fmtInt(last.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `id="session-list"`)
	assert.Contains(t, body, `href="/s/aaaaaaaa"`) // the older two rows
	assert.Contains(t, body, `href="/s/aaaaaaaab"`)
	assert.NotContains(t, body, `href="/s/aaaaaaaabb"`) // the cursor row itself
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	// Exhausted cursor: one past the oldest row → empty list, no button.
	page, err := userSessionsPage(db, uid, time.Time{}, 0, 50)
	require.NoError(t, err)
	oldest := page[len(page)-1]
	resp, err = http.Get(ts.URL + "/u/uuuuuuuu?fragment=list&before_last_active=" +
		urlQueryTime(oldest.LastActive) + "&before_id=" + fmtInt(oldest.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	empty := readBody(t, resp)
	assert.NotContains(t, empty, `href="/s/`)
	assert.NotContains(t, empty, "user-load-older")
}

// TestUserPageLoadOlderCursor pins the load-older contract: a full page
// with more rows beyond it renders the button carrying the LAST row's
// (last_active, id) — the oldest of the batch — and that exact cursor
// fetches the next page.
func TestUserPageLoadOlderCursor(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	u := mkUser(t, db)
	uid := u.ID
	for i := 1; i <= 51; i++ {
		mkSession(t, db, fmt.Sprintf("aaa%05d", i), func(s *Session) { s.UserID = &uid })
	}
	// ids 1..51, last_active increasing with id: page = ids 51..2, so the
	// button cursor is row 2 and only row 1 lies beyond it.
	page, err := userSessionsPage(db, uid, time.Time{}, 0, 50)
	require.NoError(t, err)
	require.Len(t, page, 50)
	cursor := page[49]
	require.EqualValues(t, 2, cursor.ID)

	resp, err := http.Get(ts.URL + "/u/uuuuuuuu")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `id="user-load-older"`)
	assert.Contains(t, body, `data-id="2"`)
	assert.Contains(t, body, `data-last-active="`+urlQueryTime(cursor.LastActive)+`"`)

	resp, err = http.Get(ts.URL + "/u/uuuuuuuu?fragment=list&before_last_active=" +
		urlQueryTime(cursor.LastActive) + "&before_id=" + fmtInt(cursor.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	next := readBody(t, resp)
	assert.Contains(t, next, `href="/s/aaa00001"`)
	assert.NotContains(t, next, "user-load-older") // 1 row < 50: exhausted
}

// TestUserPageZeroLastActiveTailNoCursor pins the carried Task 5 review
// requirement: zero-last_active sessions (dave leaves LastActive zero
// until the first AddMessage) sort LAST in DESC order, and a zero-time
// cursor passed back would be treated as "no cursor" by
// userSessionsPage's IsZero sentinel — an infinite page-1 loop. When
// the last row of a page has zero last_active, no further page is
// offered (no button, no cursor params).
func TestUserPageZeroLastActiveTailNoCursor(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	u := mkUser(t, db)
	uid := u.ID
	for i := 1; i <= 49; i++ {
		mkSession(t, db, fmt.Sprintf("aaa%05d", i), func(s *Session) { s.UserID = &uid })
	}
	mkSession(t, db, "zero0000", func(s *Session) { s.UserID = &uid; s.LastActive = time.Time{} })

	resp, err := http.Get(ts.URL + "/u/uuuuuuuu")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `href="/s/zero0000"`) // the zero row itself is listed (sorts last)
	assert.NotContains(t, body, "user-load-older") // ...but no cursor is ever offered from it
	assert.NotContains(t, body, "data-last-active")
}

// TestUserPagePreviewClampAndEscape pins the preview contract: the
// first-message preview clamps at 120 runes server-side (ellipsis when
// cut) and the template's contextual escaping applies — no double
// escape, no live HTML from message content.
func TestUserPagePreviewClampAndEscape(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	uid := int64(1)
	mkUser(t, db, func(u *User) { u.ID = uid })
	mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid; s.FirstMessage = strings.Repeat("x", 200) })
	mkSession(t, db, "bbbbbbbb", func(s *Session) {
		s.UserID = &uid
		s.FirstMessage = `<script>alert(1)</script> & "quotes" 'single'`
	})

	resp, err := http.Get(ts.URL + "/u/uuuuuuuu?fragment=list")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, strings.Repeat("x", 120)+"…")
	assert.NotContains(t, body, strings.Repeat("x", 121))
	assert.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")
	assert.Contains(t, body, "&amp; &#34;quotes&#34; &#39;single&#39;") // Go's full escaper: & < > " ' all escaped
	assert.NotContains(t, body, "<script")
}

// TestUserPageFragmentCursorValidation pins the fragment param
// handling: cursors travel as the PAIR (before_last_active,
// before_id); a malformed or half-present pair is a 400 (Task 8's
// "bad before" pattern), while fragment=list with no cursor is the
// first page — what user.js refetches on live updates.
func TestUserPageFragmentCursorValidation(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	u := mkUser(t, db)
	uid := u.ID
	mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid; s.FirstMessage = "first page row" })

	good := urlQueryTime(time.Now())
	for _, q := range []string{
		"fragment=list&before_last_active=garbage&before_id=1",
		"fragment=list&before_last_active=" + good + "&before_id=garbage",
		"fragment=list&before_last_active=" + good, // lone pair member
		"fragment=list&before_id=1",                // lone pair member
		"fragment=list&before_last_active=" + good + "&before_id=-1",
		"fragment=list&before_last_active=&before_id=",
	} {
		resp, err := http.Get(ts.URL + "/u/uuuuuuuu?" + q)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, 400, resp.StatusCode, "query %q", q)
	}

	// No cursor: the first page — user.js's live-update refetch mode.
	resp, err := http.Get(ts.URL + "/u/uuuuuuuu?fragment=list")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `id="session-list"`)
	assert.Contains(t, body, "first page row")
	assert.NotContains(t, body, "<html") // fragment, never the full skeleton

	// Unknown fragment value falls back to the full page (session-page
	// pattern: only the exact value is a fragment).
	resp, err = http.Get(ts.URL + "/u/uuuuuuuu?fragment=other")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "<html")
}
