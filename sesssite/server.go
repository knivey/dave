package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"gorm.io/gorm"
)

// staticFS embeds the site's static assets (style.css now; session.js
// arrives with the live-update task) so the single-binary deployment
// story holds — imgsite's embeddedWeb precedent.
//
//go:embed static
var staticFS embed.FS

// Server is the sesssite HTTP layer: a read-only GORM handle over
// dave's DB plus the loaded config. Handlers only ever issue SELECTs.
type Server struct {
	db  *gorm.DB
	cfg *Config
}

func NewServer(db *gorm.DB, cfg *Config) *Server { return &Server{db: db, cfg: cfg} }

func (s *Server) Run(addr string) error { return http.ListenAndServe(addr, s.mux()) }

// mux builds the route table. Go 1.22+ method patterns give 405s for
// free and {slug}/{id} capture PathValues; "{$}" pins the landing to
// the exact root so unknown paths fall through to the mux's own 404.
// The /events routes are the SSE live-update streams (events.go).
func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /{$}", secure(s.handleLanding))
	m.HandleFunc("GET /static/", secure(s.handleStatic))
	m.HandleFunc("GET /s/{slug}", secure(s.handleSessionPage))
	m.HandleFunc("GET /s/{slug}/m/{id}/raw", secure(s.handleRawMessage))
	m.HandleFunc("GET /s/{slug}/events", secure(s.handleSessionEvents))
	m.HandleFunc("GET /u/{slug}", secure(s.handleUserPage))
	m.HandleFunc("GET /u/{slug}/events", secure(s.handleUserEvents))
	return m
}

// secure wraps a handler with the site-wide security headers (spec,
// "Error handling & edges"). The CSP is XSS defense in depth on top of
// html/template escaping: img-src must allow * and data: because
// conversation images arrive as both remote URLs and inline base64
// data URIs. Exact header per the plan.
func secure(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; img-src * data:; style-src 'self'; script-src 'self'; connect-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next(w, r)
	}
}

// handleStatic serves the embedded assets under /static/. No directory
// listings (imgsite's staticHandler pattern): embedded directories and
// trailing slashes 404 instead of indexing. ServeMux has already
// cleaned the URL path, so traversal never reaches fs.Stat.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, "static missing", http.StatusInternalServerError)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/static/")
	if rel == "" || strings.HasSuffix(rel, "/") {
		http.NotFound(w, r)
		return
	}
	if st, statErr := fs.Stat(sub, path.Clean(rel)); statErr != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.StripPrefix("/static/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}
