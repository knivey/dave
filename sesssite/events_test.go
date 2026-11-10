package main

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trimCR strips one line terminator (\n, or \r\n from CRLF-framing
// servers) from the end of a bufio ReadString line — the terminator
// must never ride along into event-name or blank-line comparisons.
func trimCR(s string) string { return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r") }

// readSSE reads the event stream until an event whose name is in want
// completes. SSE events terminate on a blank line; a multi-line payload
// arrives as successive "data: " lines that re-join with \n — exactly
// what the browser's EventSource hands the page as ev.data. Comment
// lines (the ready marker, heartbeats) never dispatch and are skipped.
func readSSE(t *testing.T, body *bufio.Reader, want ...string) (name string, data string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var gotName string
	var lines []string
	for time.Now().Before(deadline) {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read failed: %v", err)
		}
		line = trimCR(line)
		switch {
		case strings.HasPrefix(line, ":"):
			// comment (ready/heartbeat) — no event dispatch
		case strings.HasPrefix(line, "event: "):
			gotName = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			lines = append(lines, strings.TrimPrefix(line, "data: "))
		case line == "":
			for _, w := range want {
				if gotName == w {
					return gotName, strings.Join(lines, "\n")
				}
			}
			gotName, lines = "", nil
		}
	}
	t.Fatal("timed out waiting for SSE event " + strings.Join(want, "/"))
	return "", ""
}

// drainSSEConns registers a cleanup that waits for every handler from
// this test to release its per-IP slot. Handlers exit asynchronously
// after the client closes the response body, the cap counter is a
// package global, and tests in the package share the test server's
// source IP — a stale slot would 503 the next test's connection.
func drainSSEConns(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		assert.Eventually(t, func() bool {
			sseMu.Lock()
			defer sseMu.Unlock()
			return len(sseConns) == 0
		}, 5*time.Second, 20*time.Millisecond, "per-IP SSE slots did not drain")
	})
}

func TestSessionEventsNewMessage(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "user", "first")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events?since=0")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	reader := bufio.NewReader(resp.Body)

	m := mkMessage(t, db, s.ID, "assistant", "fresh reply")
	name, data := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name)
	assert.Contains(t, data, `id="m`+fmtInt(m.ID)+`"`)
	assert.Contains(t, data, "fresh reply")
}

func TestSessionEventsSinceGapFill(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	s := mkSession(t, db, "aaaaaaaa")
	m1 := mkMessage(t, db, s.ID, "user", "before disconnect")
	m2 := mkMessage(t, db, s.ID, "assistant", "while disconnected")

	// Reconnect with since=m1: m2 must still arrive.
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events?since=" + fmtInt(m1.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	name, data := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name)
	assert.Contains(t, data, "while disconnected")
	assert.Contains(t, data, `id="m`+fmtInt(m2.ID)+`"`)
	assert.NotContains(t, data, "before disconnect")
}

func TestSessionEventsUnknownSlug404(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/s/zzzzzzzz/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestUserEventsPing(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "shrew"}).Error)

	resp, err := http.Get(ts.URL + "/u/uuuuuuuu/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	reader := bufio.NewReader(resp.Body)

	uid := int64(1)
	mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid })
	name, _ := readSSE(t, reader, "updated")
	assert.Equal(t, "updated", name)
}

// The SSE wire encoding: multi-line payloads ride one "data: " line
// per payload line (a bare \n would terminate the event early and
// truncate the HTML), empty data still emits its single data line, and
// comments never dispatch.
func TestWriteSSEMultiLine(t *testing.T) {
	var buf bytes.Buffer
	writeSSE(&buf, "messages", "<div>a</div>\n<div>b</div>\n")
	assert.Equal(t, "event: messages\ndata: <div>a</div>\ndata: <div>b</div>\ndata: \n\n", buf.String())
}

func TestWriteSSEEmptyData(t *testing.T) {
	var buf bytes.Buffer
	writeSSE(&buf, "updated", "")
	assert.Equal(t, "event: updated\ndata: \n\n", buf.String())
}

func TestWriteSSEComment(t *testing.T) {
	var buf bytes.Buffer
	writeSSEComment(&buf, "hb")
	assert.Equal(t, ":hb\n", buf.String())
}

// A message whose rendered content spans lines must arrive COMPLETE:
// the row HTML itself is multi-line, and the markdown render adds
// more. This pins the end-to-end pairing of writeSSE's line-per-data
// encoding with readSSE's spec-compliant re-join (what EventSource
// hands the page as ev.data).
func TestSessionEventsMultiLineMessagePayload(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	s := mkSession(t, db, "aaaaaaaa")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events?since=0")
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	m := mkMessage(t, db, s.ID, "assistant", "para one\n\npara two\n\n> quoted line")
	name, data := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name)
	assert.Contains(t, data, `id="m`+fmtInt(m.ID)+`"`)
	assert.Contains(t, data, "para one")
	assert.Contains(t, data, "para two")
	assert.Contains(t, data, "quoted line")
	assert.Contains(t, data, "\n", "payload must be multi-line")
}

func TestSessionEventsStatusChange(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	s := mkSession(t, db, "aaaaaaaa")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	require.NoError(t, db.Model(&Session{}).Where("id = ?", s.ID).Update("status", "completed").Error)
	name, data := readSSE(t, reader, "session")
	assert.Equal(t, "session", name)
	assert.Contains(t, data, `"status":"completed"`)
}

// The mux's GET pattern also matches HEAD; a HEAD would pin a poller
// forever on a bodyless response.
func TestSessionEventsHeadRejected(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	mkSession(t, db, "aaaaaaaa")

	req, err := http.NewRequest(http.MethodHead, ts.URL+"/s/aaaaaaaa/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestSessionEventsPerIPCap(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	mkSession(t, db, "aaaaaaaa")

	// Every test connection shares the httptest server's source IP:
	// ssePerIPLimit open streams, then the next is refused.
	held := make([]*http.Response, 0, ssePerIPLimit)
	for i := 0; i < ssePerIPLimit; i++ {
		resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "connection %d", i)
		held = append(held, resp)
	}
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events")
	require.NoError(t, err)
	readBody(t, resp)
	resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	for _, r := range held {
		r.Body.Close()
	}
}

// The pages must actually wire the live-update scripts and expose the
// ids the scripts target (source-level pin, imgsite's JS-pinning
// style): #status-badge for session.js, the user.js script tag, and
// both scripts served from the embedded static FS.
func TestLiveUpdateWiringRendered(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	mkSession(t, db, "aaaaaaaa")
	uid := int64(1)
	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "shrew"}).Error)
	mkSession(t, db, "bbbbbbbb", func(x *Session) { x.UserID = &uid })

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	body := readBody(t, resp)
	resp.Body.Close()
	assert.Contains(t, body, `<script src="/static/session.js" data-slug="aaaaaaaa" defer></script>`)
	assert.Contains(t, body, `id="status-badge"`)

	resp2, err := http.Get(ts.URL + "/u/uuuuuuuu")
	require.NoError(t, err)
	body2 := readBody(t, resp2)
	resp2.Body.Close()
	assert.Contains(t, body2, `<script src="/static/user.js" defer></script>`)
	assert.Contains(t, body2, `id="session-list"`)
}

func TestStaticServesScripts(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	for _, name := range []string{"session.js", "user.js"} {
		resp, err := http.Get(ts.URL + "/static/" + name)
		require.NoError(t, err)
		body := readBody(t, resp)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, name)
		assert.Contains(t, body, "EventSource", name)
	}
}

// Source-level pin (imgsite's JS-pinning style): the reconnect
// contract must survive future edits. EventSource auto-reconnects with
// the CONSTRUCTED URL — a since frozen at page load — and the server's
// gap-fill would then replay every row since load, so the script must
// take over the reconnect with a fresh since and filter replayed ids.
func TestSessionJSReconnectContract(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/static/session.js")
	require.NoError(t, err)
	body := readBody(t, resp)
	resp.Body.Close()
	assert.Contains(t, body, "RECONNECT CONTRACT")
	assert.Contains(t, body, "EventSource.CONNECTING")
	assert.Contains(t, body, "es.close()")
	assert.Contains(t, body, "connect();")
	// the idempotency second net: skip elements whose id already exists
	assert.Contains(t, body, "document.getElementById(el.id)")
	// reconnects must re-derive since from the live DOM, not a constant
	assert.Contains(t, body, "events?since=\" + lastId()")
}
