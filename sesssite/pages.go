package main

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strconv"

	"gorm.io/gorm"
)

// pageSize is the transcript chunk size (spec: latest 200 messages
// render initially; "load older" fetches previous 200-message chunks,
// keyset by id DESC).
const pageSize = 200

// pageFuncs mirrors rowsTpl's funcmap (render.go): safeImageURL must be
// attached to any template that renders image URLs directly.
var pageFuncs = template.FuncMap{"safeImageURL": safeImageURL}

// templatesFS is embedded in render.go; the page templates parse from
// the same FS. base.html carries the skeleton; session.html and
// landing.html define "content" (and may override "title"/"head").
var baseTpl = template.Must(template.New("base.html").
	Funcs(pageFuncs).
	ParseFS(templatesFS, "templates/base.html"))

var sessionTpl = template.Must(template.Must(baseTpl.Clone()).
	ParseFS(templatesFS, "templates/session.html"))

var landingTpl = template.Must(template.Must(baseTpl.Clone()).
	ParseFS(templatesFS, "templates/landing.html"))

// modelUsage is one per-model aggregate for the header's collapsed
// usage details (spec: aggregate token usage per model).
type modelUsage struct {
	Model            string
	Turns            int
	PromptTokens     int
	CompletionTokens int
}

// sessionPageData feeds session.html. Rows is pre-rendered rows HTML
// (renderRowsHTML). OldestID is the oldest message id on the page —
// session.js's keyset cursor for the next older-rows fragment.
type sessionPageData struct {
	Session      *Session
	Rows         template.HTML
	HasOlder     bool
	OldestID     int64
	BaseURL      string
	UserSlug     string
	MessageCount int64
	UsageByModel []modelUsage
	ModelChanged bool
}

// handleLanding renders the landing page: what the site is, and that
// pages are unlisted — get yours from the bot via ^link (spec).
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := landingTpl.Execute(&buf, nil); err != nil {
		http.Error(w, "render failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// lookupSession maps slug → session for a handler, flattening errors:
// a missing row — or a soft-deleted one, via GORM's DeletedAt filter —
// is a plain 404 (no existence leak), anything else is a 500
// (imgsite's lookupImage pattern: never swallow real breakage).
func (s *Server) lookupSession(w http.ResponseWriter, r *http.Request, slug string) (*Session, bool) {
	sess, err := sessionBySlug(s.db, slug)
	if err == nil {
		return sess, true
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	http.Error(w, "lookup failure", http.StatusInternalServerError)
	return nil, false
}

// handleSessionPage serves /s/{slug}. The slug is validated BEFORE any
// DB hit — malformed lookups are a plain 404 with no query cost and no
// existence leak (spec, "Error handling & edges"). ?fragment=rows
// serves the older-rows fragment; anything else the full page.
func (s *Server) handleSessionPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.lookupSession(w, r, slug)
	if !ok {
		return
	}
	if r.URL.Query().Get("fragment") == "rows" {
		s.serveRowsFragment(w, r, sess, slug)
		return
	}
	s.serveSessionPage(w, r, sess, slug)
}

// serveRowsFragment answers ?fragment=rows&before={id}: the previous
// pageSize chunk (keyset id DESC) as bare rows HTML for session.js to
// prepend. Fragments carry no usage lines — assignUsage windows rows
// against the previous assistant message, which may sit OUTSIDE the
// chunk, so joining here would mis-attribute the chunk's first
// assistant row.
func (s *Server) serveRowsFragment(w http.ResponseWriter, r *http.Request, sess *Session, slug string) {
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil {
		http.Error(w, "bad before", http.StatusBadRequest)
		return
	}
	msgs, err := sessionMessagesPage(s.db, sess.ID, before, pageSize)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	comps, err := s.fetchCompactionViews(sess.ID)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	rows, err := renderRowsHTML(buildMessageViews(msgs, slug), compsForChunk(comps, msgs))
	if err != nil {
		http.Error(w, "render failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(rows))
}

// serveSessionPage renders the full session page: header (badges,
// dates, message count, per-model usage details), the latest message
// chunk, and the load-older affordance when older rows exist.
func (s *Server) serveSessionPage(w http.ResponseWriter, r *http.Request, sess *Session, slug string) {
	msgs, err := sessionMessagesPage(s.db, sess.ID, 0, pageSize)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	usage, err := sessionUsageRows(s.db, sess.ID)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	comps, err := s.fetchCompactionViews(sess.ID)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	var count int64
	if err := s.db.Model(&Message{}).
		Where("session_id = ? AND superseded = false AND source_compaction_id IS NULL", sess.ID).
		Count(&count).Error; err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}

	views := buildMessageViews(msgs, slug)
	assignUsage(views, msgs, usageFrom(usage, msgs))
	rows, err := renderRowsHTML(views, compsForChunk(comps, msgs))
	if err != nil {
		http.Error(w, "render failure", http.StatusInternalServerError)
		return
	}

	// Keyset probe: a full page may have older rows beyond it — fetch
	// one row below the page's oldest id to decide whether the
	// load-older button renders.
	var oldest int64
	hasOlder := false
	if len(msgs) > 0 {
		oldest = msgs[0].ID
		if len(msgs) == pageSize {
			older, err := sessionMessagesPage(s.db, sess.ID, oldest, 1)
			if err != nil {
				http.Error(w, "query failure", http.StatusInternalServerError)
				return
			}
			hasOlder = len(older) > 0
		}
	}

	byModel := aggregateUsageByModel(usage)
	data := sessionPageData{
		Session:      sess,
		Rows:         rows,
		HasOlder:     hasOlder,
		OldestID:     oldest,
		BaseURL:      s.cfg.Server.BaseURL,
		UserSlug:     s.userSlugFor(sess.UserID),
		MessageCount: count,
		UsageByModel: byModel,
		ModelChanged: len(byModel) > 1,
	}
	var buf bytes.Buffer
	if err := sessionTpl.Execute(&buf, data); err != nil {
		http.Error(w, "render failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// handleRawMessage serves the EXACT stored content byte-for-byte as
// text/plain — no markdown, no HTML, no IRC-code translation (spec,
// "Raw view (first-class)"). Rows whose content is empty because the
// payload lives elsewhere serve that raw JSON instead: multicontent
// parts first, then tool_calls. The lookup is scoped to the slug's
// session, so cross-session message ids 404.
func (s *Server) handleRawMessage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.lookupSession(w, r, slug)
	if !ok {
		return
	}
	m, err := sessionMessageByID(s.db, sess.ID, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	}
	body := m.Content
	if body == "" && m.MultiContent != nil {
		body = *m.MultiContent
	}
	if body == "" && m.ToolCalls != nil {
		body = *m.ToolCalls
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(body))
}

// fetchCompactionViews wraps the session's compaction rows as views;
// renderRowsHTML fills SummaryHTML when the summary message is among
// the rendered rows.
func (s *Server) fetchCompactionViews(sessionID int64) ([]CompactionView, error) {
	comps, err := sessionCompactions(s.db, sessionID)
	if err != nil {
		return nil, err
	}
	views := make([]CompactionView, len(comps))
	for i, c := range comps {
		views[i] = CompactionView{Compaction: c}
	}
	return views, nil
}

// compsForChunk filters compactions to those whose archived id range
// [FirstArchivedID, LastArchivedID] intersects the chunk's rows, so
// each divider renders in exactly ONE chunk — the one whose rows touch
// the archived range. Without the filter, renderRowsHTML's
// "divider before the first row after the range" placement repeats the
// divider at the top of EVERY chunk lying entirely above the boundary
// (a Task 7 review finding: fragments + the initial page are chunks of
// one stream; the top-of-page fallback exists for single-shot renders
// only). Harmless no-op case: a range intersecting a chunk whose rows
// are all ≤ LastArchivedID renders nothing — the divider belongs to a
// newer chunk.
func compsForChunk(comps []CompactionView, msgs []Message) []CompactionView {
	if len(msgs) == 0 || len(comps) == 0 {
		return nil
	}
	lo, hi := msgs[0].ID, msgs[len(msgs)-1].ID
	var out []CompactionView
	for _, c := range comps {
		if c.LastArchivedID >= lo && c.FirstArchivedID <= hi {
			out = append(out, c)
		}
	}
	return out
}

// usageFrom drops usage rows strictly older than the page's oldest
// message. assignUsage windows rows by (previous assistant, this
// assistant] with a zero lower bound for the first assistant row on
// the page; pre-page rows belong to messages outside the window set
// and would otherwise inflate that row by the whole pre-page history.
func usageFrom(usage []TurnUsage, msgs []Message) []TurnUsage {
	if len(msgs) == 0 {
		return nil
	}
	since := msgs[0].CreatedAt
	for i, u := range usage {
		if !u.CreatedAt.Before(since) {
			return usage[i:]
		}
	}
	return nil
}

// aggregateUsageByModel folds the session's turn_usage rows into
// per-model totals, first-seen order (rows arrive created_at ASC).
// Powers the header's collapsed usage details and the model-changed
// marker (spec: derived from turn_usage.model).
func aggregateUsageByModel(usage []TurnUsage) []modelUsage {
	var out []modelUsage
	idx := map[string]int{}
	for _, u := range usage {
		i, ok := idx[u.Model]
		if !ok {
			i = len(out)
			idx[u.Model] = i
			out = append(out, modelUsage{Model: u.Model})
		}
		out[i].Turns++
		out[i].PromptTokens += u.PromptTokens
		out[i].CompletionTokens += u.CompletionTokens
	}
	return out
}

// userSlugFor resolves the header's user-page link (Task 9's /u/{slug}).
// Best-effort: a dangling user_id renders no link rather than a broken
// one.
func (s *Server) userSlugFor(userID *int64) string {
	if userID == nil {
		return ""
	}
	var u User
	if err := s.db.Select("slug").Where("id = ?", *userID).First(&u).Error; err != nil {
		return ""
	}
	return u.Slug
}
