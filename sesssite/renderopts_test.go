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

// newHideServer builds a server with [render] hide_system_prompts on.
// Returns the server, session, the system-prompt row, the user row, and
// the assistant row — in insertion order.
func newHideServer(t *testing.T) (*httptest.Server, *Session, Message, Message, Message) {
	t.Helper()
	db := newTestDB(t)
	srv := NewServer(db, &Config{Render: RenderConfig{HideSystemPrompts: true}})
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)

	s := mkSession(t, db, "aaaaaaaa")
	sys := mkMessage(t, db, s.ID, "system", "SECRET-PROMPT-INSTRUCTIONS you must obey")
	// Compaction over the system prompt row: its summary is ALSO a
	// system row and must stay visible through the divider even though
	// the row itself is filtered from the transcript.
	summary := mkMessage(t, db, s.ID, "system", "SUMMARY-OF-EARLIER-TALK about shrews")
	comp := Compaction{SessionID: s.ID, SummaryMessageID: summary.ID,
		FirstArchivedID: sys.ID, LastArchivedID: summary.ID,
		ArchivedCount: 2, Trigger: "auto", Model: "m", CreatedAt: time.Now()}
	require.NoError(t, db.Create(&comp).Error)
	user := mkMessage(t, db, s.ID, "user", "VISIBLE-USER-TEXT hello")
	asst := mkMessage(t, db, s.ID, "assistant", "VISIBLE-ASSISTANT-TEXT hi")
	return ts, s, sys, user, asst
}

// TestSessionPageHideSystemPrompts: with [render] hide_system_prompts,
// system prompt rows vanish from the transcript while the compaction
// divider's inline summary (a system row itself) keeps rendering and
// user/assistant content is untouched.
func TestSessionPageHideSystemPrompts(t *testing.T) {
	ts, s, _, _, _ := newHideServer(t)

	resp, err := http.Get(ts.URL + "/s/" + s.Slug)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)

	assert.NotContains(t, body, "SECRET-PROMPT-INSTRUCTIONS", "system prompt must not render")
	assert.NotContains(t, body, "role-system", "no system row renders as a transcript row")
	assert.Contains(t, body, "compaction-divider")
	assert.Contains(t, body, "about shrews", "divider's inline summary content stays")
	assert.Contains(t, body, "VISIBLE-USER-TEXT")
	assert.Contains(t, body, "VISIBLE-ASSISTANT-TEXT")
}

// TestRawEndpointHideSystemPrompts: the option 404s raw fetches of
// system rows (hiding the transcript while leaving raw readable would
// be pointless) and leaves non-system rows byte-for-byte.
func TestRawEndpointHideSystemPrompts(t *testing.T) {
	ts, s, sys, user, _ := newHideServer(t)

	resp, err := http.Get(ts.URL + "/s/" + s.Slug + "/m/" + fmtInt(sys.ID) + "/raw")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode, "system row raw must 404 when hidden")

	resp, err = http.Get(ts.URL + "/s/" + s.Slug + "/m/" + fmtInt(user.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "VISIBLE-USER-TEXT hello", readBody(t, resp))
}

// TestRawEndpointSystemRowsServedWhenUnhidden: default (option off)
// keeps serving system-row raw — the default guard.
func TestRawEndpointSystemRowsServedWhenUnhidden(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	sys := mkMessage(t, db, s.ID, "system", "PROMPT-RAW")

	resp, err := http.Get(ts.URL + "/s/" + s.Slug + "/m/" + fmtInt(sys.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "PROMPT-RAW", readBody(t, resp))
}

// TestSessionFragmentHideSystemPrompts: the older-rows fragment path
// filters system rows too.
func TestSessionFragmentHideSystemPrompts(t *testing.T) {
	ts, s, _, _, asst := newHideServer(t)

	resp, err := http.Get(ts.URL + "/s/" + s.Slug + "?fragment=rows&before=" + fmtInt(asst.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.NotContains(t, body, "SECRET-PROMPT-INSTRUCTIONS")
	assert.Contains(t, body, "VISIBLE-USER-TEXT")
	assert.True(t, strings.Contains(body, "about shrews"), "fragment divider summary stays")
}

// TestRenderRowsAfterSkipsSystemRows: the SSE path filters system rows
// (unit-level, no timing). The emission watermark must still advance
// THROUGH skipped rows — they will never render later, so holding the
// watermark back would re-scan them forever.
func TestRenderRowsAfterSkipsSystemRows(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{Render: RenderConfig{HideSystemPrompts: true}})
	s := mkSession(t, db, "aaaaaaaa")
	sys := mkMessage(t, db, s.ID, "system", "SECRET-PROMPT-INSTRUCTIONS")
	user := mkMessage(t, db, s.ID, "user", "LIVE-USER-TEXT")

	frag, through, err := srv.renderRowsAfter(s, sys.ID-1)
	require.NoError(t, err)
	assert.Contains(t, frag, "LIVE-USER-TEXT")
	assert.NotContains(t, frag, "SECRET-PROMPT-INSTRUCTIONS")
	assert.Equal(t, user.ID, through, "watermark advances through the skipped system row")
}

// TestRenderRowsAfterSystemOnlyBatch: a span whose every row is system
// (the post-compaction shape: fresh prompt + summary, tail copies
// excluded by liveTranscriptCond) renders EMPTY while the watermark
// still advances — the caller's contract is "empty fragment, no event,
// advance" (see the tick-loop guard in events.go).
func TestRenderRowsAfterSystemOnlyBatch(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{Render: RenderConfig{HideSystemPrompts: true}})
	s := mkSession(t, db, "aaaaaaaa")
	sys1 := mkMessage(t, db, s.ID, "system", "PROMPT-ONE")
	sys2 := mkMessage(t, db, s.ID, "system", "PROMPT-TWO")

	frag, through, err := srv.renderRowsAfter(s, sys1.ID-1)
	require.NoError(t, err)
	assert.Empty(t, frag, "system-only span renders empty")
	assert.Equal(t, sys2.ID, through, "watermark advances through the skipped span")
}
