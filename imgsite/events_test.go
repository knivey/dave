package main

// events_test.go — SSE hub unit tests, /events HTTP tests, and the
// publish wiring (upload → image-new, thumb pipeline → thumb-ready).
// Streaming reads go through frame channels with generous timeouts:
// -race on a contended host can slow even in-memory paths dramatically
// (see the note in TestThumbWorkerRescanPending).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- frame plumbing ---

// sseFrame is one parsed SSE event (or comment/retry hint).
type sseFrame struct {
	id      uint64
	name    string
	data    string
	comment string
	retry   string
}

// openEvents subscribes to /events on the test server and returns a
// channel of parsed frames plus the request's cancel func. The stream
// ends (channel closes) when the caller cancels, the body closes, or
// the server ends the response.
func openEvents(t *testing.T, ts *httptest.Server, header map[string]string, query string) (<-chan sseFrame, *http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/events"+query, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })

	frames := make(chan sseFrame, 64)
	go func() {
		defer close(frames)
		br := bufio.NewReader(resp.Body)
		var f sseFrame
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			switch {
			case line == "":
				if f.id != 0 || f.name != "" || f.data != "" || f.comment != "" || f.retry != "" {
					frames <- f
					f = sseFrame{}
				}
			case strings.HasPrefix(line, "id: "):
				f.id, _ = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
			case strings.HasPrefix(line, "event: "):
				f.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if f.data != "" {
					f.data += "\n"
				}
				f.data += strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "retry: "):
				f.retry = strings.TrimPrefix(line, "retry: ")
			case strings.HasPrefix(line, ":"):
				f.comment = strings.TrimPrefix(line, ":")
			}
		}
	}()
	return frames, resp, cancel
}

func nextSSEFrame(t *testing.T, frames <-chan sseFrame) sseFrame {
	t.Helper()
	select {
	case f, ok := <-frames:
		require.True(t, ok, "stream ended unexpectedly")
		return f
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for an SSE frame")
		return sseFrame{}
	}
}

// nextSSEEvent returns the next frame carrying a real event (non-empty
// name), skipping heartbeat comments and stray retry hints. Necessary
// because events.go writes heartbeats DIRECTLY to the wire — bypassing
// the subscriber channel — so under a -race run stalled past the 20s
// heartbeat interval (contended host; see the file header) a ":hb"
// frame can interleave AHEAD of a concurrently published event and
// derail a name assertion. Deliberate comment/retry assertions
// (TestEventsHeartbeatComment, the connect-probe barriers) keep using
// nextSSEFrame.
func nextSSEEvent(t *testing.T, frames <-chan sseFrame) sseFrame {
	t.Helper()
	for {
		f := nextSSEFrame(t, frames)
		if f.name != "" {
			return f
		}
	}
}

// drainSSEFrames consumes everything currently buffered, keeping only
// frames that carry a real event (non-empty name). Heartbeat comments
// are written directly to the wire and can land in the drain window
// under a -race stall (same reasoning as nextSSEEvent), which would
// otherwise trip the "no further replay frames" Empty assertion.
func drainSSEFrames(t *testing.T, frames <-chan sseFrame) []sseFrame {
	t.Helper()
	var out []sseFrame
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return out
			}
			if f.name != "" {
				out = append(out, f)
			}
		default:
			return out
		}
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- hub unit tests ---

func TestHubFanOutToSubscribers(t *testing.T) {
	h := newSSEHub()
	subs := make([]*subscriber, 3)
	for i := range subs {
		s, replay, covered := h.subscribe(0, false)
		require.True(t, covered)
		require.Empty(t, replay)
		subs[i] = s
	}

	id := h.publish(eventImageNew, imageNewEvent{ID: "aaaa001"})
	require.Equal(t, uint64(1), id)

	for i, s := range subs {
		select {
		case ev := <-s.ch:
			assert.Equal(t, uint64(1), ev.ID, "subscriber %d", i)
			assert.Equal(t, eventImageNew, ev.Name)
			assert.Contains(t, ev.Data, `"aaaa001"`)
		case <-time.After(5 * time.Second):
			t.Fatalf("subscriber %d did not receive the event", i)
		}
	}
}

func TestHubNonSubscriberIsolation(t *testing.T) {
	h := newSSEHub()
	early, _, _ := h.subscribe(0, false)
	h.publish(eventImageNew, imageNewEvent{ID: "aaaa001"})
	h.unsubscribe(early) // disconnects must not affect later subscribers

	late, replay, covered := h.subscribe(0, false)
	require.True(t, covered)
	require.Empty(t, replay, "fresh connect replays nothing")

	h.publish(eventThumbReady, thumbReadyEvent{ID: "bbbb002", ThumbStatus: thumbStatusReady})

	select {
	case ev := <-late.ch:
		assert.Equal(t, eventThumbReady, ev.Name, "late subscriber gets only post-subscription events")
		assert.Equal(t, uint64(2), ev.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("late subscriber missed the live event")
	}
	select {
	case ev := <-late.ch:
		t.Fatalf("unexpected extra frame: %+v", ev)
	default:
	}
}

// TestHubOverflowResetsSlowConsumer pins the overflow policy: 40 rapid
// publishes against a consumer that never reads overflows the cap-32
// buffer; the backlog is dropped and a reset sentinel queued in its
// place, so the first delivered frame after the burst IS the reset
// (stamped at delivery with the current id), and delivery resumes
// normally once the sentinel is stamped.
func TestHubOverflowResetsSlowConsumer(t *testing.T) {
	h := newSSEHub()
	slow, _, _ := h.subscribe(0, false)

	const burst = sseSubChannelCap + 8
	for i := 0; i < burst; i++ {
		h.publish(eventThumbReady, thumbReadyEvent{
			ID:          fmt.Sprintf("id%03d", i),
			ThumbStatus: thumbStatusReady,
		})
	}

	// The drain in dropSubscriberLocked empties the backlog under the
	// same lock as the sentinel send, so the sentinel is the only
	// frame left — nothing partially delivered.
	select {
	case ev := <-slow.ch:
		require.Equal(t, eventReset, ev.Name, "overflow must deliver the reset sentinel, got %+v", ev)
		assert.Zero(t, ev.ID, "sentinel is stamped by the delivery loop, not at enqueue")
	case <-time.After(5 * time.Second):
		t.Fatal("reset never arrived after overflow")
	}

	stamped := h.stampReset(slow)
	assert.Equal(t, uint64(burst), stamped.ID, "reset carries the current last id")
	assert.Equal(t, eventReset, stamped.Name)
	assert.Equal(t, "{}", stamped.Data)

	// After the reset is stamped the subscriber resumes live delivery.
	h.publish(eventImageNew, imageNewEvent{ID: "resumee"})
	select {
	case ev := <-slow.ch:
		assert.Equal(t, eventImageNew, ev.Name)
		assert.Equal(t, uint64(burst+1), ev.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber did not resume delivery after reset")
	}
}

func TestHubReplayFromSince(t *testing.T) {
	h := newSSEHub()
	const k = 10
	for i := 1; i <= k; i++ {
		h.publish(eventImageNew, imageNewEvent{ID: fmt.Sprintf("e%02d", i)})
	}

	sub, replay, covered := h.subscribe(k-5, true)
	require.True(t, covered)
	require.Len(t, replay, 5)
	for i, ev := range replay {
		assert.Equal(t, uint64(k-4+i), ev.ID, "replay is id-ordered from since+1")
		assert.Equal(t, eventImageNew, ev.Name)
		assert.Contains(t, ev.Data, fmt.Sprintf("e%02d", k-4+i), "replay carries the matching payload")
	}

	// Live delivery continues seamlessly after the replay.
	h.publish(eventImageNew, imageNewEvent{ID: "live01"})
	select {
	case ev := <-sub.ch:
		assert.Equal(t, uint64(k+1), ev.ID)
	case <-time.After(5 * time.Second):
		t.Fatal("no live event after replay")
	}
}

func TestHubReplayGapBeyondRingResets(t *testing.T) {
	h := newSSEHub()
	const total = sseRingCap + 20
	for i := 0; i < total; i++ {
		h.publish(eventThumbReady, thumbReadyEvent{ID: "x", ThumbStatus: thumbStatusReady})
	}

	// Gap wider than the retained ring: reset, not a partial replay.
	_, replay, covered := h.subscribe(3, true)
	assert.False(t, covered, "gap beyond the ring must reset")
	assert.Nil(t, replay)

	// Ids this server never issued (client from before a restart): reset.
	_, _, covered = h.subscribe(total+100, true)
	assert.False(t, covered, "since beyond nextID must reset")

	// Exactly current: covered, nothing to replay.
	_, replay, covered = h.subscribe(total, true)
	assert.True(t, covered)
	assert.Empty(t, replay)

	// Exact ring boundary: gap == ring length is still covered.
	_, replay, covered = h.subscribe(uint64(total-sseRingCap), true)
	assert.True(t, covered, "gap exactly the ring length must replay")
	assert.Len(t, replay, sseRingCap)
}

func TestHubFreshConnectNeverReplays(t *testing.T) {
	h := newSSEHub()
	h.publish(eventImageNew, imageNewEvent{ID: "aaaa001"})
	// sinceProvided=false is the no-header/no-param first visit: the
	// page render is the state; replaying at it would duplicate cards.
	_, replay, covered := h.subscribe(0, false)
	require.True(t, covered)
	assert.Empty(t, replay)

	// An explicit ?since=0, by contrast, asks for everything retained.
	_, replay, covered = h.subscribe(0, true)
	require.True(t, covered)
	assert.Len(t, replay, 1)
}

func TestHubShutdownIdempotentAndStopsPublish(t *testing.T) {
	h := newSSEHub()
	sub, _, _ := h.subscribe(0, false)

	h.shutdown()
	h.shutdown() // must not panic (double close)

	select {
	case <-h.done:
	default:
		t.Fatal("done channel must be closed after shutdown")
	}
	assert.Equal(t, uint64(0), h.publish(eventImageNew, imageNewEvent{ID: "late"}), "publish after shutdown is a no-op")
	select {
	case ev := <-sub.ch:
		t.Fatalf("no events may arrive after shutdown, got %+v", ev)
	default:
	}
}

func TestPublishImageHiddenPayloadShape(t *testing.T) {
	h := newSSEHub()
	sub, _, _ := h.subscribe(0, false)

	id := h.publish(eventImageHidden, imageHiddenEvent{ID: "zzzz999"})
	require.Equal(t, uint64(1), id)

	ev := <-sub.ch
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(ev.Data), &raw))
	assert.Equal(t, []string{"id"}, mapKeys(raw), "image-hidden payload is exactly {id}")
	assert.Equal(t, "zzzz999", raw["id"])
}

func TestWriteSSEEventFraming(t *testing.T) {
	var b strings.Builder
	require.NoError(t, writeSSEEvent(&b, event{ID: 5, Name: eventImageNew, Data: `{"a":1}`}))
	assert.Equal(t, "id: 5\nevent: image-new\ndata: {\"a\":1}\n\n", b.String())

	// Multi-line data is split per spec line (defensive; JSON never
	// emits raw newlines) and zero ids omit the id line.
	var b2 strings.Builder
	require.NoError(t, writeSSEEvent(&b2, event{Name: "x", Data: "l1\nl2"}))
	assert.Equal(t, "event: x\ndata: l1\ndata: l2\n\n", b2.String())
}

func TestParseSinceHeaderThenQuery(t *testing.T) {
	// Header wins (browser-native reconnect mechanism).
	req := httptest.NewRequest(http.MethodGet, "/events?since=7", nil)
	req.Header.Set("Last-Event-ID", "3")
	since, ok := parseSince(req)
	require.True(t, ok)
	assert.Equal(t, uint64(3), since)

	// Query fallback (fresh EventSource objects cannot set headers).
	req = httptest.NewRequest(http.MethodGet, "/events?since=9", nil)
	since, ok = parseSince(req)
	require.True(t, ok)
	assert.Equal(t, uint64(9), since)

	// Neither present: fresh connect.
	req = httptest.NewRequest(http.MethodGet, "/events", nil)
	_, ok = parseSince(req)
	assert.False(t, ok)

	// Garbage in either place is ignored rather than fatal.
	req = httptest.NewRequest(http.MethodGet, "/events?since=abc", nil)
	_, ok = parseSince(req)
	assert.False(t, ok)
}

func TestSSEClientIPForwardedForWins(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	req.RemoteAddr = "192.0.2.10:5555"
	assert.Equal(t, "192.0.2.10", sseClientIP(req))

	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	assert.Equal(t, "203.0.113.7", sseClientIP(req), "first XFF value, rest of the chain dropped")

	req.Header.Set("X-Forwarded-For", "  ")
	assert.Equal(t, "192.0.2.10", sseClientIP(req), "blank XFF falls back to RemoteAddr")
}

// --- /events HTTP tests ---

// TestEventsHeadersAndRetryHint doubles as the route-precedence proof:
// reaching this handler at all (not the "GET /" catch-all's 404 for a
// non-id-shaped path) means the exact literal /events pattern won.
func TestEventsHeadersAndRetryHint(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	resp, err := ts.Client().Get(ts.URL + "/events")
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })

	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"), "nginx needs this to stop buffering the stream")
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"), "middleware applies to the stream too")

	// The retry hint must be the first bytes on the wire — flushed out
	// with the headers, not buffered behind the heartbeat.
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "retry: "+strconv.Itoa(sseRetryMs)+"\n", line)
}

func TestEventsWithoutHubReturns503(t *testing.T) {
	app := newTestApp(t, testConfig()) // no hub attached
	ts := newTestServer(t, app)
	resp := fetchPath(t, ts, "/events")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// TestEventsHeadReturns405WithAllow pins the method guard: the mux's
// "GET /events" pattern also matches HEAD, and a HEAD response has no
// body to stream, so it must be refused with 405 plus an Allow header
// advertising GET (RFC 9110 SHOULD).
func TestEventsHeadReturns405WithAllow(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	req, err := http.NewRequest(http.MethodHead, ts.URL+"/events", nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, http.MethodGet, resp.Header.Get("Allow"))
}

func TestEventsHeartbeatComment(t *testing.T) {
	app := newTestApp(t, testConfig())
	hub := newSSEHub()
	hub.heartbeat = 25 * time.Millisecond // injected interval; no 20s sleeps
	app.setEventHub(hub)
	ts := newTestServer(t, app)

	frames, _, _ := openEvents(t, ts, nil, "")
	retry := nextSSEFrame(t, frames)
	assert.Equal(t, strconv.Itoa(sseRetryMs), retry.retry)

	hb := nextSSEFrame(t, frames)
	assert.Equal(t, "hb", hb.comment, "heartbeat is an SSE comment")
	assert.Empty(t, hb.name, "comments are not events")
}

func TestEventsLastEventIDHeaderReplay(t *testing.T) {
	app := newTestApp(t, testConfig())
	hub := newSSEHub()
	app.setEventHub(hub)
	ts := newTestServer(t, app)

	for i := 1; i <= 3; i++ {
		hub.publish(eventImageNew, imageNewEvent{ID: fmt.Sprintf("p%02d", i), CreatedAt: "2026-09-24T03:12:00Z"})
	}

	frames, _, _ := openEvents(t, ts, map[string]string{"Last-Event-ID": "1"}, "")
	nextSSEFrame(t, frames) // retry hint

	f1 := nextSSEEvent(t, frames)
	assert.Equal(t, uint64(2), f1.id)
	assert.Equal(t, eventImageNew, f1.name)
	f2 := nextSSEEvent(t, frames)
	assert.Equal(t, uint64(3), f2.id)

	// Live delivery continues after the replay.
	hub.publish(eventImageNew, imageNewEvent{ID: "live"})
	f3 := nextSSEEvent(t, frames)
	assert.Equal(t, uint64(4), f3.id)
	assert.Contains(t, f3.data, "live")
}

func TestEventsSinceQueryParamReplay(t *testing.T) {
	app := newTestApp(t, testConfig())
	hub := newSSEHub()
	app.setEventHub(hub)
	ts := newTestServer(t, app)

	for i := 1; i <= 3; i++ {
		hub.publish(eventImageNew, imageNewEvent{ID: fmt.Sprintf("q%02d", i)})
	}

	frames, _, _ := openEvents(t, ts, nil, "?since=2")
	nextSSEFrame(t, frames) // retry hint

	f1 := nextSSEEvent(t, frames)
	assert.Equal(t, uint64(3), f1.id, "?since=2 replays only id 3")
	assert.Contains(t, f1.data, "q03")
	drained := drainSSEFrames(t, frames)
	assert.Empty(t, drained, "no further replay frames")
}

func TestEventsSinceBeyondRingSendsReset(t *testing.T) {
	app := newTestApp(t, testConfig())
	hub := newSSEHub()
	app.setEventHub(hub)
	ts := newTestServer(t, app)

	for i := 0; i < sseRingCap+5; i++ {
		hub.publish(eventThumbReady, thumbReadyEvent{ID: "x", ThumbStatus: thumbStatusReady})
	}

	frames, _, _ := openEvents(t, ts, nil, "?since=3")
	nextSSEFrame(t, frames) // retry hint

	f := nextSSEEvent(t, frames)
	assert.Equal(t, eventReset, f.name, "uncoverable gap arrives as reset")
	assert.NotZero(t, f.id, "reset is stamped with the current id")
	assert.Equal(t, "{}", f.data)
}

func TestEventsPerIPCap(t *testing.T) {
	app := newTestApp(t, testConfig())
	hub := newSSEHub()
	hub.ipCap = 2 // injected cap
	app.setEventHub(hub)
	ts := newTestServer(t, app)

	frames1, _, cancel1 := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames1) // established
	frames2, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames2) // established

	resp, err := ts.Client().Get(ts.URL + "/events")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "third stream from the same IP is refused")

	// A forwarded client has its own bucket.
	fwdFrames, fwdResp, _ := openEvents(t, ts, map[string]string{"X-Forwarded-For": "203.0.113.7"}, "")
	assert.Equal(t, http.StatusOK, fwdResp.StatusCode)
	nextSSEFrame(t, fwdFrames)

	// Releasing a slot (cancel → handler returns → deferred releaseIP)
	// admits a new stream from the original IP.
	cancel1()
	require.Eventually(t, func() bool {
		resp, err := ts.Client().Get(ts.URL + "/events")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "released slot must admit a new stream")
}

// TestEventsConnectionClosesOnHubShutdown asserts the graceful-stop
// path: hub.shutdown() releases every stream (the handler returns,
// the response ends) — which is what lets http.Server.Shutdown
// terminate despite /events being active requests that never complete
// on their own. Run under -race, this also exercises the hub's
// concurrency for leaks.
func TestEventsConnectionClosesOnHubShutdown(t *testing.T) {
	app := newTestApp(t, testConfig())
	hub := newSSEHub()
	app.setEventHub(hub)
	ts := newTestServer(t, app)

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint: connection fully established

	hub.shutdown()

	select {
	case _, ok := <-frames:
		assert.False(t, ok, "stream must END (EOF), not deliver more frames")
	case <-time.After(30 * time.Second):
		t.Fatal("stream did not close after hub shutdown")
	}
}

// --- wiring tests ---

// TestUploadPublishesImageNew drives the full upload path with a live
// subscription open: the image-new event must arrive with the exact
// payload shape — and only AFTER the INSERT committed, proven by the
// details page answering 200 the moment the event is received.
func TestUploadPublishesImageNew(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	resp := doUpload(t, ts, testAPIKey, uploadParts{
		hasFile:  true,
		filename: "shrew.webp",
		data:     loadFixture(t, "plain.webp"),
		meta:     `{"original_prompt":"shrew walkin down main street"}`,
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageNew, f.name)

	var p imageNewEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, ur.ID, p.ID)
	assert.Equal(t, "/"+ur.ID, p.PageURL)
	assert.Equal(t, thumbStatusPending, p.ThumbStatus)
	_, err := time.Parse(time.RFC3339, p.CreatedAt)
	require.NoError(t, err, "created_at must be RFC3339, got %q", p.CreatedAt)

	// The event mirrors the merged row exactly. The fixture's embedded
	// workflow carries its own original_prompt, which WINS the merge
	// over the meta field per the policy — so compare against the row,
	// not the meta input.
	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, img.OriginalPrompt, p.OriginalPrompt, "event prompt mirrors the merged row")
	require.NotEmpty(t, p.OriginalPrompt, "fixture guarantees a prompt")

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &raw))
	assert.Equal(t, []string{"created_at", "id", "original_prompt", "page_url", "thumb_status"},
		mapKeys(raw), "payload fields are exactly the planned set")

	// Commit-before-publish: the row is visible the instant the event
	// is observed.
	page := fetchPath(t, ts, "/"+ur.ID)
	assert.Equal(t, http.StatusOK, page.StatusCode)
}

// TestThumbPipelinePublishesThumbReady drives the thumbnail pipeline
// synchronously through the worker wrapper (tw.process) with main's
// onThumbReady wiring in place: thumb-ready must arrive after the DB
// flip to ready, with the exact payload shape.
func TestThumbPipelinePublishesThumbReady(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{
		hasFile:  true,
		filename: "small.png",
		data:     smallPNG(t, 640, 400), // fast under -race
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	// Main's wiring: the worker publishes through the App seam. The
	// default no-op is replaced exactly the way main.go does it.
	tw := newThumbWorker(app.getConfig, app.db, app.store)
	tw.onThumbReady = app.publishThumbReady

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	tw.process(ur.ID) // synchronous: pipeline + DB flip + publish

	// Ordering: the row is ready by the time the event is observed.
	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, thumbStatusReady, img.ThumbStatus)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventThumbReady, f.name)
	var p thumbReadyEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, ur.ID, p.ID)
	assert.Equal(t, thumbStatusReady, p.ThumbStatus)

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(f.data), &raw))
	assert.Equal(t, []string{"id", "thumb_status"}, mapKeys(raw))
}

// TestPublishImageHiddenDirect pins the publish helper's payload shape
// in isolation (the endpoint path — DELETE /api/images/<id>, ordering
// after the UPDATE commit — is covered by
// TestDeletePublishesImageHiddenAfterCommit in server_test.go).
func TestPublishImageHiddenDirect(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	app.publishImageHidden("zzzz999")

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageHidden, f.name)
	var p imageHiddenEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, "zzzz999", p.ID)
}

// TestPublishesAreNilHubSafe keeps the existing-test contract: apps
// without a hub (every other test in the package) must not panic on
// the upload-path publish calls.
func TestPublishesAreNilHubSafe(t *testing.T) {
	app := newTestApp(t, testConfig())
	assert.NotPanics(t, func() {
		app.publishImageNew(&dbImage{ID: "aaaa001"})
		app.publishThumbReady("aaaa001")
		app.publishImageHidden("aaaa001")
	})
}

// TestUploadPublishesClampedPrompt pins the SSE payload's prompt clamp:
// search.snippet_chars bounds original_prompt in the event — and thus
// in the 128-entry replay ring, where an unclamped multi-KB prompt
// would be retained 128 times over. The row keeps the full text; only
// the wire payload is clamped.
func TestUploadPublishesClampedPrompt(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	// Raw PNG carries no embedded workflow, so the meta prompt wins
	// the merge — full control over the stored prompt length.
	long := strings.Repeat("word ", 200) // 1000 runes >> snippet_chars
	resp := doUpload(t, ts, testAPIKey, uploadParts{
		hasFile:  true,
		filename: "long.png",
		data:     smallPNG(t, 640, 400),
		meta:     `{"original_prompt":"` + long + `"}`,
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageNew, f.name)
	var p imageNewEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, ur.ID, p.ID)

	snippetChars := app.getConfig().Search.SnippetChars
	assert.Equal(t, clampSnippet(long, snippetChars), p.OriginalPrompt,
		"event prompt is the row's prompt clamped to snippet_chars")
	assert.Less(t, len([]rune(p.OriginalPrompt)), len([]rune(long)), "payload is smaller than the stored prompt")
	assert.True(t, strings.HasSuffix(p.OriginalPrompt, "…"), "clamped payload ends in ellipsis")

	// The row itself is untouched by the clamp.
	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, long, img.OriginalPrompt, "row keeps the full prompt")
}

// TestConcurrentPublishAndDeliver hammers the hub from several
// publisher goroutines with live subscribers — the -race detector's
// main course for the fan-out path. The burst is sized to fit the
// subscriber buffers exactly (4×8 = 32 = sseSubChannelCap), so every
// send succeeds and the delivered count is deterministic: any loss
// here would mean a real bug, not an overflow-policy drop.
func TestConcurrentPublishAndDeliver(t *testing.T) {
	h := newSSEHub()
	const subsN = 4
	subs := make([]*subscriber, subsN)
	for i := range subs {
		s, _, _ := h.subscribe(0, false)
		subs[i] = s
	}

	const publishers = 4
	const perPublisher = 8 // 4*8 == sseSubChannelCap: no overflow by construction
	var delivered atomic.Int64
	var readers sync.WaitGroup
	for _, s := range subs {
		s := s
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range s.ch {
				delivered.Add(1)
			}
		}()
	}
	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				h.publish(eventThumbReady, thumbReadyEvent{ID: "x", ThumbStatus: thumbStatusReady})
			}
		}()
	}
	wg.Wait()

	require.Eventually(t, func() bool {
		return delivered.Load() == int64(publishers*perPublisher*subsN)
	}, 30*time.Second, 50*time.Millisecond,
		"every subscriber receives every event of the burst")

	// Shutdown stops publishers; channels belong to the hub (it never
	// closes them itself), so the test closes them to end the reader
	// ranges — safe because publish is a no-op once closed=true.
	h.shutdown()
	for _, s := range subs {
		close(s.ch)
	}
	readers.Wait()
}
