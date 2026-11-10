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
	// sseRowsLimit bounds one emitted batch. Rows arrive at turn
	// granularity; 500 covers the largest single-turn burst many times
	// over while keeping a pathological burst from rendering an
	// unbounded fragment.
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

// acquireSSE takes a per-host connection slot, reporting whether one
// was free.
func acquireSSE(r *http.Request) bool {
	host := hostOnly(r.RemoteAddr)
	sseMu.Lock()
	defer sseMu.Unlock()
	if sseConns[host] >= ssePerIPLimit {
		return false
	}
	sseConns[host]++
	return true
}

// releaseSSE returns a slot taken by acquireSSE. Handlers run it via
// defer, so every exit path — client disconnect, write failure, normal
// return — releases exactly once.
func releaseSSE(r *http.Request) {
	host := hostOnly(r.RemoteAddr)
	sseMu.Lock()
	defer sseMu.Unlock()
	sseConns[host]--
	if sseConns[host] <= 0 {
		delete(sseConns, host)
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

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// renderRowsAfter renders the live-transcript rows with id > afterID —
// the same row shape, view pipeline, usage join, and compaction-divider
// chunking as the page and its fragments, so a live append is exactly
// what a reload would show minus what the viewer already has. An empty
// result renders as "" so callers can skip the event entirely.
func (s *Server) renderRowsAfter(sess *Session, afterID int64) (string, error) {
	var msgs []Message
	if err := s.db.
		Where("session_id = ? AND id > ? AND superseded = false AND source_compaction_id IS NULL", sess.ID, afterID).
		Order("id ASC").Limit(sseRowsLimit).Find(&msgs).Error; err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", nil
	}
	usage, err := sessionUsageRows(s.db, sess.ID)
	if err != nil {
		return "", err
	}
	comps, err := s.fetchCompactionViews(sess.ID)
	if err != nil {
		return "", err
	}
	views := buildMessageViews(msgs, sess.Slug)
	assignUsage(views, msgs, usageFrom(usage, msgs))
	rows, err := renderRowsHTML(views, compsForChunk(comps, msgs))
	if err != nil {
		return "", err
	}
	return string(rows), nil
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
		Where("session_id = ? AND superseded = false AND source_compaction_id IS NULL", sess.ID).
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
	if !acquireSSE(r) {
		http.Error(w, "too many event-stream connections", http.StatusServiceUnavailable)
		return
	}
	defer releaseSSE(r)

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

	// Gap fill: a reconnecting viewer sends ?since={last id it has};
	// everything newer is emitted immediately — before the first poll
	// tick — so the disconnect window is invisible.
	if since > 0 && since < last.maxID {
		if frag, err := s.renderRowsAfter(sess, since); err == nil && frag != "" {
			writeSSE(w, "messages", frag)
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
			if cur.maxID > last.maxID {
				// maxI64: a viewer whose since sits above the DB's
				// watermark (its newest row was superseded as a
				// compaction tail-copy) keeps its own position as the
				// emission floor.
				if frag, err := s.renderRowsAfter(sess, maxI64(last.maxID, since)); err == nil && frag != "" {
					writeSSE(w, "messages", frag)
				}
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
	if !acquireSSE(r) {
		http.Error(w, "too many event-stream connections", http.StatusServiceUnavailable)
		return
	}
	defer releaseSSE(r)

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
