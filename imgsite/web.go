package main

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// embeddedWeb carries the web/ layer (style.css, app.js, page modules)
// inside the binary so the single-binary deployment story holds. Files
// evolve independently of the binary's immutable routes, so static gets a
// short max-age rather than immutable caching.
//
//go:embed web
var embeddedWeb embed.FS

func staticHandler() http.Handler {
	sub, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		panic("imgsite: embedded web/ missing: " + err.Error())
	}
	fileServer := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No directory listings: http.FileServer would render an index
		// of the embedded tree for directory targets. 404 both
		// trailing-slash requests and any path that resolves to a
		// directory. (ServeMux has already cleaned the URL, so ".."
		// tricks never reach the fs.Stat; a hostile name still just
		// fails the stat and 404s.)
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
		fileServer.ServeHTTP(w, r)
	})
}
