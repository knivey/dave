package main

import (
	"bufio"
	"bytes"
	"fmt"
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

// A gap larger than sseRowsLimit must arrive in successive batches
// (gap fill emits the OLDEST sseRowsLimit; the poller continues from
// the emission watermark) — nothing silently truncated.
func TestSessionEventsBatchingOverCap(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	s := mkSession(t, db, "aaaaaaaa")
	m0 := mkMessage(t, db, s.ID, "user", "anchor")
	for i := 1; i <= sseRowsLimit+100; i++ {
		mkMessage(t, db, s.ID, "assistant", fmt.Sprintf("bulk-%04d", i))
	}

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events?since=" + fmtInt(m0.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	// Batch 1 (gap fill): oldest sseRowsLimit bulk rows.
	name, data := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name)
	assert.Contains(t, data, "bulk-0001")
	assert.Contains(t, data, fmt.Sprintf("bulk-%04d", sseRowsLimit))
	assert.NotContains(t, data, fmt.Sprintf("bulk-%04d", sseRowsLimit+1))
	assert.NotContains(t, data, fmt.Sprintf("bulk-%04d", sseRowsLimit+100))

	// Batch 2 (first tick): the remainder, through the newest row.
	name2, data2 := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name2)
	assert.Contains(t, data2, fmt.Sprintf("bulk-%04d", sseRowsLimit+1))
	assert.Contains(t, data2, fmt.Sprintf("bulk-%04d", sseRowsLimit+100))
	assert.NotContains(t, data2, "bulk-0001")
}

// A transient render failure must not lose rows: the emission
// watermark only advances through successfully rendered rows, so a
// span whose render failed is re-rendered on a later tick. Injected
// by dropping the turn_usage table — readState never touches it (the
// tick keeps running), renderRowsAfter's sessionUsageRows does.
//
// Timing is the point: m2 lands, the outage starts, and a poll tick
// fires INSIDE the outage window (failed render, no event, watermark
// must not advance). Healing delivers nothing new — the next tick must
// re-render the failed span itself. Under the old watermark code the
// failed tick advanced to the DB watermark and the stream stayed
// silent forever (readSSE times out).
func TestSessionEventsRenderFailureSelfHeals(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	s := mkSession(t, db, "aaaaaaaa")
	m1 := mkMessage(t, db, s.ID, "user", "first")

	// Connect with since=m1 (no gap: since == connect-time watermark),
	// so m2 can only arrive through the stream.
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events?since=" + fmtInt(m1.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	// m2 lands while the stream is healthy; the outage starts
	// immediately after — long before the first tick (~1s), so the
	// drop can never race a healthy render of m2.
	m2 := mkMessage(t, db, s.ID, "assistant", "during outage")
	require.NoError(t, db.Migrator().DropTable(&TurnUsage{}))

	// Outage window: a tick fires at ~1s and its render FAILS (the
	// 1.5s margin absorbs handler-start scheduling delay on loaded
	// CI). No messages event is emitted and the watermark stays at m1.
	time.Sleep(1500 * time.Millisecond)

	// Heal — and insert nothing further: the next tick must itself
	// re-render the failed span and deliver m2.
	require.NoError(t, db.AutoMigrate(&TurnUsage{}))
	name, data := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name)
	assert.Contains(t, data, "during outage", "row from the failed-render tick must not be lost")
	assert.Contains(t, data, `id="m`+fmtInt(m2.ID)+`"`)
}

// X-Forwarded-For keys the per-IP cap when the direct peer is a
// configured trusted proxy: two viewers behind one proxy count
// separately (the default config trusts loopback; the httptest server
// connects from 127.0.0.1).
func TestSSECapXFFHonoredBehindTrustedProxy(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{Server: ServerConfig{TrustedProxies: []string{"127.0.0.1"}}})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	mkSession(t, db, "aaaaaaaa")

	held := make([]*http.Response, 0, ssePerIPLimit)
	get := func(xff string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/s/aaaaaaaa/events", nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	// Fill viewer A's budget from behind the proxy.
	for i := 0; i < ssePerIPLimit; i++ {
		resp := get("203.0.113.7")
		require.Equal(t, http.StatusOK, resp.StatusCode, "viewer A conn %d", i)
		held = append(held, resp)
	}
	// Viewer B, same proxy, different XFF: distinct key — not capped.
	// (Never readBody a 200 here: an SSE stream's body is infinite.)
	respB := get("198.51.100.9")
	respB.Body.Close()
	assert.Equal(t, http.StatusOK, respB.StatusCode)

	for _, r := range held {
		r.Body.Close()
	}
}

// From an UNTRUSTED peer, XFF is ignored: forged headers cannot
// multiply capacity (explicit empty trusted list = never trust XFF).
func TestSSECapXFFIgnoredFromUntrustedPeer(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{Server: ServerConfig{TrustedProxies: []string{}}})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	drainSSEConns(t)
	mkSession(t, db, "aaaaaaaa")

	held := make([]*http.Response, 0, ssePerIPLimit)
	get := func(xff string) *http.Response {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/s/aaaaaaaa/events", nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-For", xff)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	// ssePerIPLimit conns, each forging a DIFFERENT XFF — all must
	// count against the one real peer address.
	for i := 0; i < ssePerIPLimit; i++ {
		resp := get(fmt.Sprintf("10.0.0.%d", i+1))
		require.Equal(t, http.StatusOK, resp.StatusCode, "conn %d", i)
		held = append(held, resp)
	}
	respNext := get("10.0.0.99")
	readBody(t, respNext) // 503: finite error body, safe to drain
	respNext.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, respNext.StatusCode)

	for _, r := range held {
		r.Body.Close()
	}
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

// Source-level pin (TestSessionJSReconnectContract's pattern): the
// code-copy affordances — a hover-revealed button on every <pre>
// (paste-site parity) and click-to-copy inline code — must survive
// future edits. The MutationObserver is load-bearing: SSE-appended and
// load-older-prepended rows arrive after DOMContentLoaded, so a
// one-shot listener would leave their code blocks unwired. The button
// payload is stashed at wire time (dataset.copyText) BEFORE the
// button's own text pollutes pre.textContent.
func TestSessionJSCopyWiring(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/static/session.js")
	require.NoError(t, err)
	body := readBody(t, resp)
	resp.Body.Close()
	assert.Contains(t, body, "wireCopyButtons")
	assert.Contains(t, body, "copy-btn")
	assert.Contains(t, body, "dataset.copyText")
	assert.Contains(t, body, "MutationObserver")
	assert.Contains(t, body, "navigator.clipboard")
	// inline code copies; pre>code (chroma blocks) does not double-fire
	assert.Contains(t, body, `ev.target.closest("code")`)
	assert.Contains(t, body, `code.closest("pre")`)
	// toolpair replay filter: consumed results must not resurrect as
	// orphan rows on reconnect (only skipped when PAIRED already)
	assert.Contains(t, body, `.toolresult[data-callid]`)
	assert.Contains(t, body, `.toolpair[data-callid=`)

	css, err := http.Get(ts.URL + "/static/style.css")
	require.NoError(t, err)
	cssBody := readBody(t, css)
	css.Body.Close()
	assert.Contains(t, cssBody, ".copy-btn")
	assert.Contains(t, cssBody, "#msgs pre { position: relative; }")
	assert.Contains(t, cssBody, "code.copied")
}

// Source-level pin (TestSessionJSReconnectContract's pattern): user.js's
// cursor-adoption contract must survive future edits. The live-refetch
// path replaces the list with page 1 — it must ALSO adopt the refreshed
// load-older button state, or the visible button keeps a stale
// (last_active, id) cursor and load-older fetches past rows between
// page 1's tail and that cursor. The append path's href-keyed dedupe is
// the second net (session.js's replay filter).
func TestUserJSCursorAdoption(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/static/user.js")
	require.NoError(t, err)
	body := readBody(t, resp)
	resp.Body.Close()
	assert.Contains(t, body, "CURSOR-ADOPTION CONTRACT")
	// refetch adopts the fragment's button, not just its rows
	assert.Contains(t, body, "adoptOlder(fresh);")
	assert.Contains(t, body, "older.dataset.lastActive = fresh.dataset.lastActive")
	assert.Contains(t, body, "older.hidden = false")
	// append dedupe keys rows by session href
	assert.Contains(t, body, "li.session-row > a")
	assert.Contains(t, body, "seen.has(a.getAttribute(\"href\"))")
}
