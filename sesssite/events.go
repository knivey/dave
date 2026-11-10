package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	ssePollInterval = time.Second
	sseHeartbeat    = 15 * time.Second
	ssePerIPLimit   = 10
	// sseRowsLimit bounds ONE emitted batch, not a whole gap: a larger
	// gap renders as successive batches on following ticks because the
	// emission watermark (lastSent) only advances through the rows a
	// successful render actually sent.
	sseRowsLimit = 500
)

// sseConns counts live event-stream connections per client host. The
// cap is a package global because one process serves every route from
// one DB — all handlers share the budget.
var (
	sseMu    sync.Mutex
	sseConns = map[string]int{}
)

// hostOnly strips the port from RemoteAddr ("host:port", "[v6]:port"),
// returning the host that keys the per-IP cap. A malformed address
// returns as-is.
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// trustedProxy reports whether peer is a configured trusted proxy.
// Entries and the peer are compared as IPs when both parse (so "::1"
// matches "0:0:0:0:0:0:0:1"), else as literal strings. A nil/empty
// list (or a nil config, e.g. hand-built test configs) trusts nothing
// — fail closed.
func (s *Server) trustedProxy(peer string) bool {
	for _, t := range s.cfg.Server.TrustedProxies {
		if pi, ti := net.ParseIP(peer), net.ParseIP(t); pi != nil && ti != nil {
			if pi.Equal(ti) {
				return true
			}
			continue
		}
		if peer == t {
			return true
		}
	}
	return false
}

// sseCapKey resolves the per-IP cap key for a request: X-Forwarded-
// For's first value (the original client, imgsite's clientIP parsing)
// ONLY when the direct peer is a configured trusted proxy — behind a
// reverse proxy every viewer shares the proxy's RemoteAddr, so keying
// on it would give the whole site ssePerIPLimit streams total. An
// untrusted peer's XFF is attacker-controlled and ignored: forged
// headers cannot multiply capacity. No XFF (or a blank one) from a
// trusted proxy keys on the proxy itself — correct for a local proxy
// fronting one viewer.
func (s *Server) sseCapKey(r *http.Request) string {
	peer := hostOnly(r.RemoteAddr)
	if !s.trustedProxy(peer) {
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if first := strings.TrimSpace(xff); first != "" {
			return first
		}
	}
	return peer
}

// acquireSSE takes a connection slot for key, reporting whether one
// was free.
func acquireSSE(key string) bool {
	sseMu.Lock()
	defer sseMu.Unlock()
	if sseConns[key] >= ssePerIPLimit {
		return false
	}
	sseConns[key]++
	return true
}

// releaseSSE returns a slot taken by acquireSSE. Handlers run it via
// defer, so every exit path — client disconnect, write failure, normal
// return — releases exactly once.
func releaseSSE(key string) {
	sseMu.Lock()
	defer sseMu.Unlock()
	sseConns[key]--
	if sseConns[key] <= 0 {
		delete(sseConns, key)
	}
}

// writeSSE emits one named event. The payload is usually multi-line
// (a rendered rows fragment): per the SSE spec each payload line must
// ride its own "data: " line — a bare embedded \n would terminate the
// event early and truncate the HTML. EventSource re-joins the lines
// with \n, so ev.data carries the payload verbatim; the client must
// never re-parse the "data: " prefix itself.
func writeSSE(w io.Writer, name, data string) {
	fmt.Fprintf(w, "event: %s\n", name)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}

// writeSSEComment emits an SSE comment line — the ready marker and the
// heartbeats. Comments never dispatch client events; they exist to
// keep proxies from reaping an otherwise-silent connection.
func writeSSEComment(w io.Writer, comment string) {
	fmt.Fprintf(w, ":%s\n", comment)
}

// renderRowsAfter renders the live-transcript rows with id > afterID —
// the same row shape, view pipeline, usage join, and compaction-divider
// chunking as the page and its fragments, so a live append is exactly
// what a reload would show minus what the viewer already has. It
// returns the fragment plus renderedThrough, the newest row id
// actually included (0 when nothing matched) — the caller's emission
// watermark advances ONLY through rows a successful render actually
// sent, which bounds one batch at sseRowsLimit (larger gaps continue
// on following ticks) and self-heals render/query failures.
func (s *Server) renderRowsAfter(sess *Session, afterID int64) (string, int64, error) {
	var msgs []Message
	if err := s.db.
		Where("session_id = ? AND id > ? AND "+liveTranscriptCond, sess.ID, afterID).
		Order("id ASC").Limit(sseRowsLimit).Find(&msgs).Error; err != nil {
		return "", 0, err
	}
	if len(msgs) == 0 {
		return "", 0, nil
	}
	usage, err := sessionUsageRows(s.db, sess.ID)
	if err != nil {
		return "", 0, err
	}
	comps, err := s.fetchCompactionViews(sess.ID)
	if err != nil {
		return "", 0, err
	}
	views := buildMessageViews(msgs, sess.Slug)
	assignUsage(views, msgs, usageFrom(usage, msgs))
	rows, err := renderRowsHTML(views, compsForChunk(comps, msgs))
	if err != nil {
		return "", 0, err
	}
	return string(rows), msgs[len(msgs)-1].ID, nil
}

// sessionEventState is the per-tick session fingerprint, read by four
// one-row indexed queries (plan note: no aggregation tricks). maxID is
// the watermark over ALL of the session's message rows — monotonic, so
// a compaction marking rows superseded can never lower it (the
// live-shape count CAN drop; it is tracked as the page-shape size for
// a future transcript-size signal). status and comps drive the
// 'session' event.
type sessionEventState struct {
	maxID  int64
	count  int64
	status string
	comps  int64
}

func (s *Server) sessionEventState(sess *Session) (sessionEventState, error) {
	var st sessionEventState
	if err := s.db.Model(&Message{}).Where("session_id = ?", sess.ID).
		Select("COALESCE(MAX(id), 0)").Row().Scan(&st.maxID); err != nil {
		return st, err
	}
	if err := s.db.Model(&Message{}).
		Where("session_id = ? AND "+liveTranscriptCond, sess.ID).
		Count(&st.count).Error; err != nil {
		return st, err
	}
	var row Session
	err := s.db.Select("status").Where("id = ?", sess.ID).First(&row).Error
	switch {
	case err == nil:
		st.status = row.Status
	case errors.Is(err, gorm.ErrRecordNotFound):
		// Soft-deleted mid-stream: "" differs from every real status
		// once, then stays stable; the client ignores empty payloads.
	default:
		return st, err
	}
	if err := s.db.Model(&Compaction{}).Where("session_id = ?", sess.ID).Count(&st.comps).Error; err != nil {
		return st, err
	}
	return st, nil
}

// userEventState is the per-tick user fingerprint: the newest session
// by (last_active, id) — the MAX(last_active) row with id as the
// tiebreak, scanned into the struct so the timestamp round-trips
// through the same driver path every tick — plus the session count.
// GORM model queries (not raw SQL) so the sessions' soft-delete filter
// applies: a soft-deleted session must not pin the user page's state.
type userEventState struct {
	maxActive time.Time
	maxID     int64
	count     int64
}

func (s *Server) userEventState(userID int64) (userEventState, error) {
	var st userEventState
	var latest Session
	err := s.db.Where("user_id = ?", userID).Order("last_active DESC, id DESC").First(&latest).Error
	switch {
	case err == nil:
		st.maxActive, st.maxID = latest.LastActive, latest.ID
	case errors.Is(err, gorm.ErrRecordNotFound):
		// No sessions (or all soft-deleted): the zero state is a valid,
		// comparable state.
	default:
		return st, err
	}
	if err := s.db.Model(&Session{}).Where("user_id = ?", userID).Count(&st.count).Error; err != nil {
		return st, err
	}
	return st, nil
}

// sseHeaders + firstFlush are the shared stream setup of both event
// handlers. X-Accel-Buffering follows imgsite's events handler: nginx
// buffers proxied responses by default, which silently stalls SSE —
// it honors this header from the upstream with zero proxy config.
func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
}

// handleSessionEvents streams live updates for a session page:
// 'messages' events (rendered row HTML for ids beyond the viewer's
// position), 'session' events on status or compaction-count change,
// and heartbeat comments. Polling is per-connection at 1s — messages
// land at turn granularity, which the spec accepts — and each tick is
// four one-row indexed queries plus (only when something changed) one
// render pass, so an idle tab costs almost nothing.
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	// The mux's GET pattern also matches HEAD; a HEAD has no body to
	// stream and would pin a connection (and a poller) forever —
	// imgsite's events handler guards the same way.
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "stream required", http.StatusMethodNotAllowed)
		return
	}
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.lookupSession(w, r, slug)
	if !ok {
		return
	}
	// Resolve the cap key ONCE — acquire and the deferred release must
	// never disagree about which viewer they counted.
	capKey := s.sseCapKey(r)
	if !acquireSSE(capKey) {
		http.Error(w, "too many event-stream connections", http.StatusServiceUnavailable)
		return
	}
	defer releaseSSE(capKey)

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// First read before any bytes go out so a query failure can still
	// answer with a real status code.
	last, err := s.sessionEventState(sess)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	sseHeaders(w)

	since := int64(0)
	fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)

	// lastSent is the emission watermark: the newest row id whose
	// rendering has actually been SENT on this stream. It starts at the
	// viewer's claimed position (since) — or the connect-time DB
	// watermark for a since-less fresh page — and advances ONLY through
	// rows a successful render actually sent. That makes sseRowsLimit a
	// true per-batch bound (a larger gap continues on following ticks)
	// and lets a failed render retry next tick instead of silently
	// skipping the span (the DB watermark `last` must NEVER gate
	// emission — advancing past unsent rows loses them forever).
	lastSent := last.maxID
	if since > 0 {
		lastSent = since
	}

	// Gap fill: a reconnecting viewer sends ?since={last id it has};
	// everything newer is emitted immediately — before the first poll
	// tick — so the disconnect window is invisible. A gap larger than
	// sseRowsLimit continues batch-by-batch on the ticks below.
	if since > 0 && since < last.maxID {
		if frag, through, err := s.renderRowsAfter(sess, lastSent); err == nil && through > 0 {
			writeSSE(w, "messages", frag)
			lastSent = through
		}
	}
	writeSSEComment(w, "ready")
	fl.Flush()

	tick := time.NewTicker(ssePollInterval)
	defer tick.Stop()
	heart := time.NewTicker(sseHeartbeat)
	defer heart.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heart.C:
			writeSSEComment(w, "hb")
			fl.Flush()
		case <-tick.C:
			cur, err := s.sessionEventState(sess)
			if err != nil {
				continue // transient read failure: keep `last`, retry next tick
			}
			if cur.maxID > lastSent {
				frag, through, err := s.renderRowsAfter(sess, lastSent)
				if err == nil && through > 0 {
					writeSSE(w, "messages", frag)
					lastSent = through
				}
				// render failure or an empty render: lastSent unchanged —
				// the next tick re-renders the same span
			}
			if cur.status != last.status || cur.comps != last.comps {
				payload, _ := json.Marshal(map[string]any{"status": cur.status})
				writeSSE(w, "session", string(payload))
			}
			last = cur
			fl.Flush()
		}
	}
}

// handleUserEvents streams an 'updated' ping whenever the user's
// session set changes — a new session, a bumped last_active (dave
// touches it every turn), a soft delete. Payloads stay empty because
// the list fragment IS the state: the page refetches it on ping.
func (s *Server) handleUserEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "stream required", http.StatusMethodNotAllowed)
		return
	}
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	u, ok := s.lookupUser(w, r, slug)
	if !ok {
		return
	}
	capKey := s.sseCapKey(r)
	if !acquireSSE(capKey) {
		http.Error(w, "too many event-stream connections", http.StatusServiceUnavailable)
		return
	}
	defer releaseSSE(capKey)

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	last, err := s.userEventState(u.ID)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	sseHeaders(w)

	writeSSEComment(w, "ready")
	fl.Flush()

	tick := time.NewTicker(ssePollInterval)
	defer tick.Stop()
	heart := time.NewTicker(sseHeartbeat)
	defer heart.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heart.C:
			writeSSEComment(w, "hb")
			fl.Flush()
		case <-tick.C:
			cur, err := s.userEventState(u.ID)
			if err != nil {
				continue
			}
			if !cur.maxActive.Equal(last.maxActive) || cur.maxID != last.maxID || cur.count != last.count {
				writeSSE(w, "updated", "")
			}
			last = cur
			fl.Flush()
		}
	}
}
