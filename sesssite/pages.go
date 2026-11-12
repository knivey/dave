package main

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"gorm.io/gorm"
)

// pageSize is the transcript chunk size (spec: latest 200 messages
// render initially; "load older" fetches previous 200-message chunks,
// keyset by id DESC).
const pageSize = 200

// pageFuncs mirrors rowsTpl's funcmap (render.go): safeImageURL must be
// attached to any template that renders image URLs directly.
// runesClamp serves the user page's first-message previews.
var pageFuncs = template.FuncMap{"safeImageURL": safeImageURL, "runesClamp": runesClamp}

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

var userTpl = template.Must(template.Must(baseTpl.Clone()).
	ParseFS(templatesFS, "templates/user.html"))

// runesClamp truncates s to at most max runes, appending an ellipsis
// when it actually cut something (user-page first-message previews;
// a byte-slice cut would split multibyte characters). The template's
// contextual escaping applies to the result — this never pre-escapes.
func runesClamp(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

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
	// ClonedFromURL is the source session's page link when the session
	// is a clone AND the source still resolves (exists, not
	// soft-deleted, has a slug); "" renders an unlinked badge.
	ClonedFromURL string
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

// applyHideSystemPrompts fills compaction summaries from the full view
// set, then drops system rows when [render] hide_system_prompts is on.
// Call AFTER assignUsage (the usage join walks views and msgs
// index-parallel) and with comps already chunk-filtered — the summary
// fill must see the summary rows before they're dropped.
func (s *Server) applyHideSystemPrompts(views []MessageView, comps []CompactionView) []MessageView {
	if !s.cfg.Render.HideSystemPrompts {
		return views
	}
	fillCompactionSummaries(comps, views)
	return dropSystemViews(views)
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
	views := buildMessageViews(msgs, slug, s.cfg.Render.viewOptsFor())
	applyAuthors(views, msgs, s.authorsFor(msgs))
	chunkComps := compsForChunk(comps, msgs)
	views = pairToolCalls(s.applyHideSystemPrompts(views, chunkComps))
	rows, err := renderRowsHTML(views, chunkComps)
	if err != nil {
		http.Error(w, "render failure", http.StatusInternalServerError)
		return
	}
	// Explicit like serveUserListFragment: the fragment is bare row
	// HTML meant for innerHTML adoption, never a standalone document.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		Where("session_id = ? AND "+liveTranscriptCond, sess.ID).
		Count(&count).Error; err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}

	views := buildMessageViews(msgs, slug, s.cfg.Render.viewOptsFor())
	assignUsage(views, msgs, usageFrom(usage, msgs))
	applyAuthors(views, msgs, s.authorsFor(msgs))
	chunkComps := compsForChunk(comps, msgs)
	views = pairToolCalls(s.applyHideSystemPrompts(views, chunkComps))
	rows, err := renderRowsHTML(views, chunkComps)
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
		Session:       sess,
		Rows:          rows,
		HasOlder:      hasOlder,
		OldestID:      oldest,
		BaseURL:       s.cfg.Server.BaseURL,
		UserSlug:      s.userSlugFor(sess.UserID),
		MessageCount:  count,
		UsageByModel:  byModel,
		ModelChanged:  len(byModel) > 1,
		ClonedFromURL: s.clonedFromURL(sess),
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
	// hide_system_prompts hides system rows everywhere — a raw endpoint
	// that still served them would make the option pointless.
	if s.cfg.Render.HideSystemPrompts && m.Role == "system" {
		http.NotFound(w, r)
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

// compsForChunk filters compactions to the ONE whose divider belongs
// in this chunk: renderRowsHTML places a compaction's divider before
// the first row with ID > LastArchivedID, so the owning chunk is the
// one containing that boundary row. The predicate
// lo <= LastArchivedID+1 && hi > LastArchivedID says exactly that —
// the boundary row is at or after this chunk's first row, and some row
// of the chunk follows the range. Two edge dispositions fall out:
//
//   - ALIGNED boundary (LastArchivedID == lo-1): INCLUDED — the
//     divider renders before this chunk's first row. The old
//     intersect predicate (range ∩ chunk ≠ ∅) excluded it here and
//     included it in the OLDER chunk instead, where no row follows the
//     range — the divider rendered NOWHERE (final-review finding:
//     205 live messages, newest page ids 6..205, range ending at 5).
//   - Range extending past the chunk's newest row (hi <=
//     LastArchivedID): EXCLUDED — the old predicate included it, but
//     renderRowsHTML renders nothing (no row > LastArchivedID); the
//     divider belongs to a newer chunk.
//
// Without any filter, renderRowsHTML's placement repeats the divider
// at the top of EVERY chunk lying entirely above the boundary (a Task
// 7 review finding: fragments + the initial page are chunks of one
// stream; the top-of-page fallback exists for single-shot renders
// only). Consecutive chunks' windows [lo-1, hi-1] partition the id
// line, so exactly one chunk claims each boundary.
func compsForChunk(comps []CompactionView, msgs []Message) []CompactionView {
	if len(msgs) == 0 || len(comps) == 0 {
		return nil
	}
	lo, hi := msgs[0].ID, msgs[len(msgs)-1].ID
	var out []CompactionView
	for _, c := range comps {
		if lo <= c.LastArchivedID+1 && hi > c.LastArchivedID {
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

// authorsFor resolves the distinct message authors' nicks in one
// query (per-message attribution display). Best effort: nil on query
// failure — rows fall back to the bare role label.
func (s *Server) authorsFor(msgs []Message) map[int64]string {
	seen := make(map[int64]bool)
	ids := make([]int64, 0, 4)
	for i := range msgs {
		if msgs[i].UserID != nil && !seen[*msgs[i].UserID] {
			seen[*msgs[i].UserID] = true
			ids = append(ids, *msgs[i].UserID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var users []User
	if err := s.db.Select("id", "current_nick").Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil
	}
	m := make(map[int64]string, len(users))
	for _, u := range users {
		m[u.ID] = u.CurrentNick
	}
	return m
}

// clonedFromURL resolves a clone's source session page link, best
// effort: "" when the session isn't a clone, or the source row is
// gone/soft-deleted/slug-less (the badge then renders unlinked — the
// nick still tells the story).
func (s *Server) clonedFromURL(sess *Session) string {
	if sess.ClonedFromID == nil {
		return ""
	}
	var src Session
	if err := s.db.Select("slug").Where("id = ?", *sess.ClonedFromID).First(&src).Error; err != nil {
		return ""
	}
	if !validSlug(src.Slug) {
		return ""
	}
	return "/s/" + src.Slug
}

// userPageSize is the user page's session-list page (spec: keyset by
// last_active DESC, 50 per page).
const userPageSize = 50

// userPageData feeds user.html. Sessions is the current page NEWEST
// first; LastActive/LastID describe the page's LAST row — the oldest
// of the batch — as user.js's "load older" keyset cursor (Task 10).
type userPageData struct {
	User       *User
	Sessions   []Session
	HasOlder   bool
	LastActive time.Time
	LastID     int64
}

// handleUserPage serves /u/{slug}: the user's session list, keyset
// paginated by (last_active DESC, id DESC). The slug is validated
// BEFORE any DB hit (session-page pattern). ?fragment=list serves the
// bare list fragment — with no cursor the first page (user.js's
// live-update refetch), with the (before_last_active, before_id) pair
// the next page for load-older. Any other fragment value (or none) is
// the full page.
func (s *Server) handleUserPage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	u, ok := s.lookupUser(w, r, slug)
	if !ok {
		return
	}
	if r.URL.Query().Get("fragment") == "list" {
		s.serveUserListFragment(w, r, u)
		return
	}
	s.serveUserPage(w, r, u)
}

// lookupUser is lookupSession's twin for the users table: missing row
// is a plain 404, anything else a 500.
func (s *Server) lookupUser(w http.ResponseWriter, r *http.Request, slug string) (*User, bool) {
	u, err := userBySlug(s.db, slug)
	if err == nil {
		return u, true
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	http.Error(w, "lookup failure", http.StatusInternalServerError)
	return nil, false
}

// parseUserCursor reads the (before_last_active, before_id) keyset
// pair from the query. The pair travels together or not at all: a lone
// member cannot form a cursor, and a malformed value is a plain 400
// (Task 8's "bad before" pattern). A well-formed zero time is passed
// through to userSessionsPage's IsZero sentinel (first page); the
// load-older button never emits one — see buildUserPageData.
func parseUserCursor(q url.Values) (time.Time, int64, error) {
	if !q.Has("before_last_active") && !q.Has("before_id") {
		return time.Time{}, 0, nil
	}
	if !q.Has("before_last_active") || !q.Has("before_id") {
		return time.Time{}, 0, errors.New("incomplete cursor pair")
	}
	t, err := time.Parse(time.RFC3339Nano, q.Get("before_last_active"))
	if err != nil {
		return time.Time{}, 0, errors.New("bad before_last_active")
	}
	id, err := strconv.ParseInt(q.Get("before_id"), 10, 64)
	if err != nil || id < 0 {
		return time.Time{}, 0, errors.New("bad before_id")
	}
	return t, id, nil
}

// serveUserListFragment answers ?fragment=list: the session rows (plus
// the load-older button when more pages exist) as bare HTML — never
// the full skeleton.
func (s *Server) serveUserListFragment(w http.ResponseWriter, r *http.Request, u *User) {
	beforeActive, beforeID, err := parseUserCursor(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sessions, err := userSessionsPage(s.db, u.ID, beforeActive, beforeID, userPageSize)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	data, err := s.buildUserPageData(u, sessions)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := userTpl.ExecuteTemplate(&buf, "user-list", data); err != nil {
		http.Error(w, "render failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// serveUserPage renders the full user page: identity header and the
// first page of sessions.
func (s *Server) serveUserPage(w http.ResponseWriter, r *http.Request, u *User) {
	sessions, err := userSessionsPage(s.db, u.ID, time.Time{}, 0, userPageSize)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	data, err := s.buildUserPageData(u, sessions)
	if err != nil {
		http.Error(w, "query failure", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := userTpl.Execute(&buf, data); err != nil {
		http.Error(w, "render failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// buildUserPageData assembles the template data including the
// load-older decision. The cursor is the page's LAST row (newest-first
// page ⇒ the oldest of the batch). Two guards:
//
//   - A ZERO last_active row (dave leaves LastActive zero until the
//     first AddMessage) must never carry the cursor: zero rows sort
//     last in DESC order, and a zero time passed back is
//     userSessionsPage's "no cursor" IsZero sentinel — the button
//     would refetch page 1 forever. A page ending on a zero row
//     offers no further pages (carried Task 5 review requirement);
//     everything that could follow it is zero rows anyway.
//   - The one-row probe only runs on a FULL page — a short page is
//     exhausted by definition (session page's HasOlder pattern).
func (s *Server) buildUserPageData(u *User, sessions []Session) (userPageData, error) {
	data := userPageData{User: u, Sessions: sessions}
	if len(sessions) == 0 {
		return data, nil
	}
	last := sessions[len(sessions)-1]
	data.LastActive = last.LastActive
	data.LastID = last.ID
	if last.LastActive.IsZero() || len(sessions) < userPageSize {
		return data, nil
	}
	older, err := userSessionsPage(s.db, u.ID, last.LastActive, last.ID, 1)
	if err != nil {
		return data, err
	}
	data.HasOlder = len(older) > 0
	return data, nil
}
