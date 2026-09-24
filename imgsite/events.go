package main

// events.go — SSE hub and the GET /events stream (milestone 4).
//
// One process-wide hub fans published events out to every connected
// /events subscriber. The hub itself runs no goroutines: publish is a
// synchronous fan-out under one mutex (event rate here is single
// digits per minute — uploads and thumbnail completions — so a lock is
// nowhere near contended), and each connection's delivery loop lives
// in its own handler goroutine. Ordering guarantee: events are
// published only AFTER the DB write they describe has committed
// (image-new after the upload INSERT, thumb-ready after the ready
// flip), so a subscriber acting on an event can immediately re-query
// and see the row.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SSE event names (wire "event:" field values). Payload field names
// and types are pinned by the plan and asserted verbatim in
// events_test.go — clients (gallery.js, image.js) and any future
// consumer depend on the exact shapes.
const (
	eventImageNew    = "image-new"
	eventThumbReady  = "thumb-ready"
	eventImageHidden = "image-hidden"
	eventReset       = "reset"
)

// Hub tuning constants. Deliberately not config: these are
// protocol/robustness internals the plan pins (32/128/8/20s), not
// operator policy. The hub fields they seed stay directly injectable
// for tests (heartbeat interval, per-IP cap).
const (
	// sseSubChannelCap is the per-connection outbound buffer. A
	// subscriber whose consumer falls a full buffer behind trips the
	// overflow policy (see dropSubscriberLocked) instead of blocking
	// publish.
	sseSubChannelCap = 32
	// sseRingCap bounds the replay ring (Last-Event-ID reconnects).
	sseRingCap = 128
	// ssePerIPCap bounds concurrent streams per client IP.
	ssePerIPCap = 8
	// sseHeartbeatInterval is the ":hb" comment cadence that keeps
	// intermediaries from reaping an otherwise-idle stream.
	sseHeartbeatInterval = 20 * time.Second
	// sseRetryMs is the reconnect hint sent as the first frame; it
	// tunes the browser EventSource retry delay.
	sseRetryMs = 3000
)

// imageNewEvent is the image-new payload. created_at is RFC3339 UTC
// (APIs/SSE convention; the stored format is second-resolution text).
// page_url is the site-relative details-page link.
type imageNewEvent struct {
	ID             string `json:"id"`
	CreatedAt      string `json:"created_at"`
	OriginalPrompt string `json:"original_prompt"`
	ThumbStatus    string `json:"thumb_status"`
	PageURL        string `json:"page_url"`
}

// thumbReadyEvent is the thumb-ready payload (thumb_status is always
// "ready": failures don't publish — the gallery's existing 404-retry
// path degrades those cards to the original bytes on its own).
type thumbReadyEvent struct {
	ID          string `json:"id"`
	ThumbStatus string `json:"thumb_status"`
}

// imageHiddenEvent is the image-hidden payload (soft delete). The
// DELETE /api/images/<id> endpoint arrives in milestone 7; the payload
// shape and publish path exist now so clients can be written against
// the final event vocabulary.
type imageHiddenEvent struct {
	ID string `json:"id"`
}

// event is one SSE frame. ID is the monotonic sequence number used
// for Last-Event-ID replay; it is 0 only on the overflow sentinel,
// which the delivery loop stamps with the current id (stampReset)
// before anything reaches the wire. Reset is strictly per-client and
// never enters the ring.
type event struct {
	ID   uint64
	Name string
	Data string
}

// subscriber is one connected /events client.
type subscriber struct {
	ch chan event

	// resetPending (guarded by hub.mu): an overflow dropped this
	// subscriber's backlog and queued the reset sentinel; further
	// publishes skip the subscriber until the sentinel is delivered —
	// the full refetch the reset instructs covers those events.
	resetPending bool
}

// sseHub coordinates fan-out, the replay ring, and connection limits.
type sseHub struct {
	mu     sync.Mutex
	subs   map[*subscriber]struct{}
	nextID uint64
	ring   []event
	closed bool
	done   chan struct{}

	// heartbeat is the per-connection ":hb" interval; a field (not the
	// constant) so tests can shrink it instead of sleeping 20s.
	heartbeat time.Duration

	ipMu    sync.Mutex
	ipCount map[string]int
	// ipCap is the per-IP concurrent-stream limit; injectable for tests.
	ipCap int
}

func newSSEHub() *sseHub {
	return &sseHub{
		subs:      make(map[*subscriber]struct{}),
		ring:      make([]event, 0, sseRingCap),
		done:      make(chan struct{}),
		ipCount:   make(map[string]int),
		ipCap:     ssePerIPCap,
		heartbeat: sseHeartbeatInterval,
	}
}

// publish marshals payload, assigns the next monotonic id, records the
// event in the replay ring and fans it out to every subscriber. It
// returns the assigned id, or 0 when the hub is closed or marshaling
// failed (both drop the event; live updates are best-effort — the next
// full page load is always correct, so callers don't retry).
func (h *sseHub) publish(name string, payload any) uint64 {
	data, err := json.Marshal(payload)
	if err != nil {
		loggerEvents.Error("marshaling SSE payload", "event", name, "error", err)
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0
	}
	h.nextID++
	ev := event{ID: h.nextID, Name: name, Data: string(data)}
	h.ringAppendLocked(ev)
	for sub := range h.subs {
		h.deliverLocked(sub, ev)
	}
	return h.nextID
}

// deliverLocked hands ev to one subscriber or trips the overflow
// policy when its buffer is full.
func (h *sseHub) deliverLocked(sub *subscriber, ev event) {
	if sub.resetPending {
		// A reset is already queued; events behind an undelivered
		// reset are covered by the full refetch it instructs.
		return
	}
	select {
	case sub.ch <- ev:
	default:
		h.dropSubscriberLocked(sub)
	}
}

// dropSubscriberLocked implements the overflow policy: a subscriber
// whose buffer is full gets its entire queued backlog dropped and a
// reset sentinel enqueued in its place. The next delivery stamps the
// sentinel with the then-current id and writes event:reset — the
// client refetches its current page slice. Simpler than lossless
// backfill and correct: partial delivery after a stall would leave the
// client silently missing whatever was dropped mid-range.
//
// Draining and the sentinel send happen under the same hub mutex as
// every other channel send, so the sentinel send can never block: the
// channel was just emptied (cap >= 1) and no other sender exists. The
// concurrent delivery-loop receiver only ever drains the channel
// further, never fills it.
func (h *sseHub) dropSubscriberLocked(sub *subscriber) {
	for {
		select {
		case <-sub.ch:
			continue
		default:
		}
		break
	}
	sub.resetPending = true
	sub.ch <- event{Name: eventReset}
	loggerEvents.Warn("SSE subscriber overflow; backlog dropped, reset queued",
		"subs", len(h.subs), "last_id", h.nextID)
}

// ringAppendLocked records ev in the bounded replay ring.
//
// DESIGN NOTE (bounded memory): the ring holds the most recent
// sseRingCap events — ~128 frames of a few hundred bytes each, tens of
// KB, capped forever regardless of traffic — purely so reconnecting
// clients can bridge short gaps. A client whose gap exceeds the ring
// (tab asleep too long, or ids from before a restart) gets a reset
// (full refetch) instead: correct-if-blunt, and immune to unbounded
// growth. The slice shift on overflow is O(cap) per publish, which at
// this site's event rate is noise; a circular buffer would only add
// index arithmetic to save nothing measurable.
func (h *sseHub) ringAppendLocked(ev event) {
	if len(h.ring) == sseRingCap {
		copy(h.ring, h.ring[1:])
		h.ring[len(h.ring)-1] = ev
		return
	}
	h.ring = append(h.ring, ev)
}

// subscribe registers a subscriber and computes its replay batch
// atomically. Computing the replay and registering must happen under
// one lock, or events published in between would be neither replayed
// nor delivered live. covered=false means the gap can't be covered by
// the ring — the caller sends a reset instead (the subscriber is still
// registered: after the reset, live delivery continues).
func (h *sseHub) subscribe(since uint64, sinceProvided bool) (*subscriber, []event, bool) {
	sub := &subscriber{ch: make(chan event, sseSubChannelCap)}
	h.mu.Lock()
	defer h.mu.Unlock()
	var replay []event
	covered := true
	if sinceProvided {
		replay, covered = h.replaySinceLocked(since)
	}
	h.subs[sub] = struct{}{}
	return sub, replay, covered
}

// replaySinceLocked returns the ring events with ID > since, or
// ok=false when the gap can't be covered. Ring ids are contiguous
// (nextID increments once per publish and every publish enters the
// ring), so the retained window is exactly
// [nextID-len(ring)+1 .. nextID].
func (h *sseHub) replaySinceLocked(since uint64) ([]event, bool) {
	if since >= h.nextID {
		if since > h.nextID {
			// Client remembers ids this server never issued: ids from
			// before a restart. Nothing to replay against — reset.
			return nil, false
		}
		return nil, true // exactly current: nothing to replay
	}
	gap := h.nextID - since
	if gap > uint64(len(h.ring)) {
		return nil, false // gap exceeds the retained history: reset
	}
	out := make([]event, gap)
	copy(out, h.ring[uint64(len(h.ring))-gap:])
	return out, true
}

func (h *sseHub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
}

// resetEvent builds the connect-time reset (replay gap beyond the
// ring) carrying the current last id.
func (h *sseHub) resetEvent() event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return event{ID: h.nextID, Name: eventReset, Data: "{}"}
}

// stampReset fills in the overflow sentinel's id (the current last id,
// so the client's Last-Event-ID jumps past the dropped range and a
// post-reset reconnect doesn't replay events the refetch already
// covered) and clears the subscriber's reset mark.
func (h *sseHub) stampReset(sub *subscriber) event {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub.resetPending = false
	return event{ID: h.nextID, Name: eventReset, Data: "{}"}
}

// shutdown closes every stream: idempotent, and publish becomes a
// no-op. Called before http.Server.Shutdown in main — /events handlers
// are active requests that never finish on their own, and Shutdown
// waits for active requests, so releasing the streams here is what
// lets graceful shutdown terminate at all.
func (h *sseHub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
}

// acquireIP/releaseIP maintain the per-IP connection count. A separate
// mutex: this is the connect/disconnect path, never the publish path.
func (h *sseHub) acquireIP(ip string) bool {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	if h.ipCount[ip] >= h.ipCap {
		return false
	}
	h.ipCount[ip]++
	return true
}

func (h *sseHub) releaseIP(ip string) {
	h.ipMu.Lock()
	defer h.ipMu.Unlock()
	if n := h.ipCount[ip]; n <= 1 {
		delete(h.ipCount, ip)
	} else {
		h.ipCount[ip] = n - 1
	}
}

// sseClientIP resolves the per-connection cap key. X-Forwarded-For's
// first value wins when present: the site is expected to sit behind
// the operator's own reverse proxy, which appends the real client
// there. Trusting XFF is a deployment assumption, not a security
// boundary — a directly-exposed server lets clients forge the header
// and dodge the cap — but the cap is a cheap guard against runaway
// tabs/scripts, not an authentication mechanism. Without XFF, the
// RemoteAddr host part is used.
func sseClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if first := strings.TrimSpace(xff); first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// parseSince extracts the replay cursor. The Last-Event-ID header wins
// when present and parseable: that is the browser-native mechanism
// (EventSource sends it automatically on its own retries, and the
// server's replay ring answers it). The ?since=N query param is the
// fallback for deliberately fresh EventSource objects — sse.js's
// visibilitychange reopen constructs a new one, which cannot set
// headers, so it carries the last seen id in the URL instead. Both
// paths hit the same ring replay.
func parseSince(r *http.Request) (uint64, bool) {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if id, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			return id, true
		}
	}
	if v := r.URL.Query().Get("since"); v != "" {
		if id, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			return id, true
		}
	}
	return 0, false
}

// handleEvents serves GET /events — the SSE stream.
//
// Route precedence: registered as the exact literal pattern
// "GET /events", which always wins over the "GET /" catch-all (see
// handleImageRoutes' DESIGN NOTE) — an /events request must never fall
// into id-shape dispatch.
func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	hub := a.events
	if hub == nil {
		// Tests that never attach a hub; production always does (main,
		// before serving). Distinguishable from a 404 so a wiring bug
		// can't hide as "unknown image id".
		http.Error(w, "events unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		// The mux's GET pattern also matches HEAD; a head request has
		// no body to stream and would pin a connection forever.
		// Allow advertises the one supported method (RFC 9110 SHOULD).
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "stream required", http.StatusMethodNotAllowed)
		return
	}

	// Cheap per-IP guard before any streaming state is set up.
	ip := sseClientIP(r)
	if !hub.acquireIP(ip) {
		http.Error(w, "too many event streams from this address", http.StatusTooManyRequests)
		return
	}
	defer hub.releaseIP(ip)

	// Flushing is how headers and frames reach the client promptly;
	// NewResponseController reaches the underlying Flusher through the
	// loggingResponseWriter's Unwrap (see its comment in server.go).
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// X-Content-Type-Options: nosniff arrives via the outer middleware.

	// Retry hint first, flushed with the headers: it tunes the
	// browser's reconnect delay and doubles as proof-of-liveness for
	// proxies that want early bytes.
	if _, err := fmt.Fprintf(w, "retry: %d\n\n", sseRetryMs); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	since, sinceProvided := parseSince(r)
	sub, replay, covered := hub.subscribe(since, sinceProvided)
	defer hub.unsubscribe(sub)

	if !covered {
		if err := writeSSEEvent(w, hub.resetEvent()); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	} else if len(replay) > 0 {
		for _, ev := range replay {
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}

	ticker := time.NewTicker(hub.heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-hub.done:
			// Hub shutdown (graceful stop): the deferred unsubscribe
			// runs and the connection closes with the handler.
			return
		case <-ticker.C:
			// Heartbeat comment: ignored by EventSource, keeps
			// proxies and load balancers from reaping the stream.
			if _, err := io.WriteString(w, ":hb\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case ev := <-sub.ch:
			if ev.Name == eventReset {
				// Overflow sentinel: stamp the current id so the
				// client's Last-Event-ID jumps past the dropped
				// range, then instruct the full refetch.
				ev = hub.stampReset(sub)
			}
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

// writeSSEEvent frames one event per the SSE wire format:
//
//	id: <n>
//	event: <name>
//	data: <payload line(s)>
//
// The id line is omitted for zero ids (the pre-stamp sentinel never
// reaches this writer). Data is split per line to satisfy the spec's
// line-oriented fields — json.Marshal never emits raw newlines, but
// the splitter keeps the writer honest for any future payload source.
func writeSSEEvent(w io.Writer, ev event) error {
	var b strings.Builder
	if ev.ID != 0 {
		fmt.Fprintf(&b, "id: %d\n", ev.ID)
	}
	if ev.Name != "" {
		fmt.Fprintf(&b, "event: %s\n", ev.Name)
	}
	for _, line := range strings.Split(ev.Data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// toRFC3339 renders the stored second-resolution UTC timestamp as
// RFC3339 (API/SSE convention), passing the raw value through when
// parsing fails so a bad row degrades instead of vanishing.
func toRFC3339(ts string) string {
	if t, err := time.Parse(dbTimeFormat, ts); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ts
}

// setEventHub attaches the SSE hub (main calls this once, before
// serving starts — reads of a.events afterwards are race-free,
// mirroring setThumbWorker). Tests that exercise /events attach a hub
// explicitly; publishes are nil-hub-safe no-ops otherwise.
func (a *App) setEventHub(h *sseHub) {
	a.events = h
}

// publishImageNew fans out image-new for a freshly inserted row. Call
// only AFTER the INSERT committed: subscribers act on this event by
// prepending cards and fetching neighbors, which must already see the
// row.
func (a *App) publishImageNew(img *dbImage) {
	if a.events == nil {
		return
	}
	// The payload's prompt is clamped to search.snippet_chars — the
	// same clamp gallery cards get. The ring retains the last 128
	// events verbatim, so an unclamped multi-KB prompt would sit in
	// hub memory 128 times over (and ride every replay); the full
	// text remains on the row and the details page.
	snippetChars := a.getConfig().Search.SnippetChars
	a.events.publish(eventImageNew, imageNewEvent{
		ID:             img.ID,
		CreatedAt:      toRFC3339(img.CreatedAt),
		OriginalPrompt: clampSnippet(img.OriginalPrompt, snippetChars),
		ThumbStatus:    img.ThumbStatus,
		PageURL:        "/" + img.ID,
	})
}

// publishThumbReady fans out thumb-ready. The worker calls this (via
// its onThumbReady seam) only after processThumbJob flipped the row to
// ready and that UPDATE committed — the ordering a subscriber's
// shimmer→thumb swap depends on.
func (a *App) publishThumbReady(id string) {
	if a.events == nil {
		return
	}
	a.events.publish(eventThumbReady, thumbReadyEvent{
		ID:          id,
		ThumbStatus: thumbStatusReady,
	})
}

// publishImageHidden fans out image-hidden (soft delete → clients drop
// the card). No caller yet: the DELETE /api/images/<id> endpoint is
// milestone 7 and will call this after the hidden=1 UPDATE commits —
// wired now so the event vocabulary clients are built against is
// final, and testable end-to-end today (events_test.go).
func (a *App) publishImageHidden(id string) {
	if a.events == nil {
		return
	}
	a.events.publish(eventImageHidden, imageHiddenEvent{ID: id})
}
