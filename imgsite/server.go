package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// validImageID guards the id-keyed public routes: ids are exactly 7
// base62 chars. Those routes are dispatched manually by
// handleImageRoutes behind the "GET /" catch-all (ServeMux wildcard
// patterns cannot express bounded repetition); validation happens here
// instead, and the exact patterns registered on the mux (/updo,
// /admin/reload, /favicon.ico, /gallery, /search, /search-fragment,
// /events, /api/images/{id}/neighbors, /api/images/{id} DELETE,
// /static/) always win precedence over that catch-all, so a malformed
// id can never shadow a real route.
var validImageID = regexp.MustCompile(`^[0-9A-Za-z]{7}$`).MatchString

// dbGetImageByIDFn is the row-lookup the page/orig handlers use; a package
// var (the repo's standard test seam) so tests can inject DB failures and
// exercise the 500 path.
var dbGetImageByIDFn = dbGetImageByID

// Neighbor-lookup seams for the same reason as dbGetImageByIDFn.
var (
	dbGetNewerImageFn = dbGetNewerImage
	dbGetOlderImageFn = dbGetOlderImage
)

// dbHideImageFn is the soft-delete seam (same reason as
// dbGetImageByIDFn: handler-level failure injection for the 500 path).
var dbHideImageFn = dbHideImage

// lookupImage fetches a row for a handler, mapping errors to HTTP
// responses: a missing row (sql.ErrNoRows, including wrapped variants) is
// a plain 404, while any other DB failure is a logged 500. Flattening
// everything to 404 would hide real breakage from the logs.
func (a *App) lookupImage(w http.ResponseWriter, r *http.Request, id string) (*dbImage, bool) {
	img, err := dbGetImageByIDFn(a.db, id)
	if err == nil {
		return img, true
	}
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return nil, false
	}
	logger.Error("image lookup failed", "id", id, "error", err)
	http.Error(w, "lookup failure", http.StatusInternalServerError)
	return nil, false
}

// App carries the shared state of the running site: hot-swappable config
// (RWMutex-guarded like img-mcp's ToolHandlers), the DB handle, the
// content-addressed store, and the upload rate bucket. thumbs is the
// background thumbnailer, set once by main after NewApp and before
// serving starts — uploads then enqueue through enqueueThumb (a nil-safe
// no-op in tests that don't run the worker). events is the SSE hub,
// attached the same way (main, before serving); the publish helpers are
// nil-safe no-ops when it is unset.
type App struct {
	mu     sync.RWMutex
	config Config

	db    *sqlx.DB
	store *Store

	limiter    *rateLimiter
	configPath string

	thumbs *thumbWorker
	events *sseHub
}

func NewApp(cfg Config, db *sqlx.DB, configPath string) *App {
	return &App{
		config:     cfg,
		db:         db,
		store:      NewStore(cfg.Storage.ResolvedPath, cfg.Storage.ResolvedThumbsPath),
		limiter:    newRateLimiter(cfg.Upload.RatePerMinute),
		configPath: configPath,
	}
}

func (a *App) getConfig() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.config
}

func (a *App) setConfig(cfg Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.config = cfg
}

// setThumbWorker attaches the background thumbnailer (main calls this
// once, before serving). Reads of a.thumbs afterwards are race-free.
func (a *App) setThumbWorker(tw *thumbWorker) {
	a.thumbs = tw
}

// enqueueThumb hands a freshly inserted image to the thumbnailer. The
// upload path must never block or fail on thumbnailing: a nil worker
// (tests) or a full queue simply leaves the row pending for the startup
// re-scan. Uploads drop at most one job at a time, so the drop is
// reported per id (the startup re-scan aggregates its drops instead —
// see rescanPending).
func (a *App) enqueueThumb(id string) {
	if a.thumbs == nil {
		return
	}
	if !a.thumbs.Enqueue(id) {
		loggerThumbs.Warn("thumb queue full; upload job dropped (row stays pending for restart re-scan)", "id", id)
	}
}

type reloadResponse struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings,omitempty"`
	Message  string   `json:"message,omitempty"`
}

// doReload re-reads the config file, applies the reloadable set and
// reports non-reloadable changes as warnings. Shared by the SIGHUP handler
// and POST /admin/reload.
func (a *App) doReload() reloadResponse {
	oldCfg := a.getConfig()
	newCfg, warnings, err := reloadConfigFromFile(a.configPath, oldCfg)
	if err != nil {
		return reloadResponse{Status: "error", Message: err.Error()}
	}
	a.setConfig(newCfg)
	a.limiter.setPerMinute(newCfg.Upload.RatePerMinute)
	if wn := thumbWidthChangeWarning(oldCfg, newCfg); wn != "" {
		logger.Warn(wn)
	}
	resp := reloadResponse{Status: "ok"}
	if len(warnings) > 0 {
		resp.Warnings = warnings
	}
	return resp
}

// thumbWidthChangeWarning returns the single WARN message logged when a
// reload changes the thumbnail widths, or "" when they did not change.
// The widths ARE reloadable — new generations pick them up immediately —
// but existing ready entries keep their generated sizes forever (rows
// are terminal; nothing regenerates them), so their files are served
// through the width-fallback lookup in Store.FindThumbPath. Informational
// rather than a restart requirement, hence logged directly instead of
// riding the API response's non-reloadable warnings list.
func thumbWidthChangeWarning(oldCfg, newCfg Config) string {
	if oldCfg.Thumbnails.SmallWidth == newCfg.Thumbnails.SmallWidth &&
		oldCfg.Thumbnails.DisplayWidth == newCfg.Thumbnails.DisplayWidth {
		return ""
	}
	return fmt.Sprintf(
		"thumbnail widths changed: new generations use the new widths; existing entries keep their generated sizes (small_width %d -> %d, display_width %d -> %d)",
		oldCfg.Thumbnails.SmallWidth, newCfg.Thumbnails.SmallWidth,
		oldCfg.Thumbnails.DisplayWidth, newCfg.Thumbnails.DisplayWidth)
}

func (a *App) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	cfg := a.getConfig()
	if !checkAPIKey(cfg.Auth.APIKey, r.Header.Get(apiKeyHeader)) {
		logger.Warn("admin reload rejected: bad or missing api key", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	resp := a.doReload()
	if resp.Status == "error" {
		logger.Error("config reload failed", "error", resp.Message)
	} else {
		logger.Info("config reloaded")
		for _, wn := range resp.Warnings {
			logger.Warn("non-reloadable field changed", "warning", wn)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Error("writing admin reload response", "error", err)
	}
}

// handleAdminReextract re-runs workflow-graph extraction for every stored
// workflow_json using the CURRENT extraction rules. This exists because
// extraction is code that improves: e.g. the GGUF loader variants
// (UnetLoaderGGUF/CLIPLoaderGGUF) were missed by the original exact-match
// rules (production: i.shrews.xyz/Xr8HDUL, Sep 2026) — re-extract heals
// existing rows without re-uploading. The merge policy is reused verbatim:
// fresh-EXIF wins graph-derived fields, stored provenance and safety
// verdicts round-trip (never overwritten), and prompt-field mismatches
// WARN + prefer the fresh side. It also heals rows whose upload meta was
// incomplete: EMPTY provenance (network/channel/nick) and an 'unknown'
// safety backfill from the note payload the img-mcp EXIF rewrite bakes
// into the workflow. Only metadata columns are rewritten (visibility,
// thumbs, file identity, and timestamps are untouched); no SSE is
// published (cards change under the user on next load, which is
// acceptable for an admin-triggered maintenance action).
func (a *App) handleAdminReextract(w http.ResponseWriter, r *http.Request) {
	if !checkAPIKey(a.getConfig().Auth.APIKey, r.Header.Get(apiKeyHeader)) {
		logger.Warn("admin re-extract rejected: bad or missing api key", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := dbGetImagesWithWorkflow(a.db)
	if err != nil {
		logger.Error("re-extract: loading rows failed", "error", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	updated := 0
	for i := range rows {
		img := &rows[i]
		md, exifOK := ExtractMetadata(img.WorkflowJSON)
		if !exifOK {
			// Should not happen (these rows parsed at upload time), but a
			// workflow_json that no longer parses must not destroy stored
			// metadata: skip it and leave the row as-is.
			logger.Warn("re-extract: stored workflow_json no longer parses; skipping", "id", img.ID)
			continue
		}
		merged := mergeUploadMetadata(rowToUploadMeta(img), md, true)
		applyMergedMetadata(img, merged)
		if err := dbUpdateImageMetadata(a.db, img); err != nil {
			logger.Error("re-extract: update failed", "id", img.ID, "error", err)
			continue
		}
		updated++
	}

	logger.Info("re-extract complete", "considered", len(rows), "updated", updated)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]int{
		"considered": len(rows),
		"updated":    updated,
	}); err != nil {
		logger.Error("writing admin re-extract response", "error", err)
	}
}

// handleOrigFile serves the original bytes DIRECTLY from the content
// address — no redirect hops. Unknown id or filename mismatch is a 404.
// Ids are never reused and the content behind a row never changes, so the
// response is immutable year-long cacheable. Dispatched manually by
// handleImageRoutes (see its DESIGN NOTE).
func (a *App) handleOrigFile(w http.ResponseWriter, r *http.Request, id, filename string) {
	if !validImageID(id) {
		http.NotFound(w, r)
		return
	}
	img, ok := a.lookupImage(w, r, id)
	if !ok {
		return
	}
	if img.Hidden {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	if filename != img.Filename {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(a.store.OriginalPath(img.SHA256))
	if err != nil {
		if os.IsNotExist(err) {
			// Row without a stored file: DB/store drift, not a client error.
			logger.Error("stored original missing", "id", id, "sha256", img.SHA256)
			http.NotFound(w, r)
			return
		}
		logger.Error("opening original", "id", id, "error", err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", img.MimeType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	// ServeContent handles Content-Length, Range and HEAD for us; the
	// pre-set Content-Type (from the stored, magic-byte-sniffed value)
	// is respected and not re-sniffed.
	http.ServeContent(w, r, "", time.Time{}, f)
}

// neighborSummary is the wire shape of the neighbors API: enough for a
// client to render a link card without another round trip.
type neighborSummary struct {
	ID             string `json:"id"`
	CreatedAt      string `json:"created_at"`
	OriginalPrompt string `json:"original_prompt"`
}

// handleNeighbors serves GET /api/images/<id>/neighbors — the live
// next-button data source (milestone 4 fetches this once per image-new
// SSE event). prev is the newer neighbor, next the older one, both keyset
// (created_at DESC, id DESC) and hidden-filtered; either end is null.
func (a *App) handleNeighbors(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validImageID(id) {
		http.NotFound(w, r)
		return
	}
	img, ok := a.lookupImage(w, r, id)
	if !ok {
		return
	}
	if img.Hidden {
		http.Error(w, "gone", http.StatusGone)
		return
	}

	resp := struct {
		Prev *neighborSummary `json:"prev"`
		Next *neighborSummary `json:"next"`
	}{}
	if prev, err := dbGetNewerImageFn(a.db, img.CreatedAt, img.ID); err != nil {
		logger.Error("neighbor lookup failed", "id", id, "dir", "prev", "error", err)
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	} else if prev != nil {
		resp.Prev = summarizeNeighbor(prev)
	}
	if next, err := dbGetOlderImageFn(a.db, img.CreatedAt, img.ID); err != nil {
		logger.Error("neighbor lookup failed", "id", id, "dir", "next", "error", err)
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	} else if next != nil {
		resp.Next = summarizeNeighbor(next)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Error("writing neighbors response", "id", id, "error", err)
	}
}

// summarizeNeighbor renders RFC3339 created_at (APIs/SSE convention; the
// stored format is UTC text, ms or legacy-seconds precision) and falls
// back to the raw stored string if parsing ever fails.
func summarizeNeighbor(img *dbImage) *neighborSummary {
	created := img.CreatedAt
	if t, ok := parseDBTime(created); ok {
		created = t.UTC().Format(time.RFC3339)
	}
	return &neighborSummary{
		ID:             img.ID,
		CreatedAt:      created,
		OriginalPrompt: img.OriginalPrompt,
	}
}

// handleImageRoutes manually dispatches the id-keyed public routes:
//
//	/                     -> gallery page
//	/<id>                 -> details page
//	/<id>/orig/<filename> -> original bytes
//	/<id>/t/<size>        -> thumbnail bytes
//
// DESIGN NOTE: these cannot be ServeMux wildcard patterns. A pattern like
// GET /{id}/orig/{filename} starts with a wildcard segment, so it overlaps
// every other multi-segment literal-rooted pattern (e.g. /static/{rest...}
// vs /{id}/orig/{filename} both match "/static/orig/filename" with no
// specificity winner — ServeMux panics at registration). Dispatching here
// instead keeps full control of the [0-9A-Za-z]{7} shape check and leaves
// exact routes (/updo, /gallery, /events, /api/..., /static/) to the mux,
// where they always win precedence over the "GET /" catch-all.
func (a *App) handleImageRoutes(w http.ResponseWriter, r *http.Request) {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(segs) == 1 && segs[0] == "":
		a.handleGalleryPage(w, r)
	case len(segs) == 1:
		a.handleImagePage(w, r, segs[0])
	case len(segs) == 3 && segs[1] == "orig":
		a.handleOrigFile(w, r, segs[0], segs[2])
	case len(segs) == 3 && segs[1] == "t":
		a.handleThumb(w, r, segs[0], segs[2])
	default:
		http.NotFound(w, r)
	}
}

// handleThumb serves GET /<id>/t/<size>. The size token is "small" or
// "display" — NOT the raw pixel widths from config (small deviation from
// the plan's "(480|1280)" notation): tokens stay URL-stable if the
// configured widths ever change or hot-reload, and the on-disk file is
// keyed by the width that was resolved at generation time. Resolution
// therefore goes through Store.FindThumbPath: the current-config width
// file first, then any existing <hash>-*.jpg — otherwise a width reload
// would 404 every pre-existing ready row forever (see FindThumbPath's
// DESIGN NOTE). A pending or failed thumbnail is a 404 with
// Cache-Control: no-cache so the client's retry (image reload / JS
// swap) actually re-checks.
func (a *App) handleThumb(w http.ResponseWriter, r *http.Request, id, sizeToken string) {
	if !validImageID(id) {
		http.NotFound(w, r)
		return
	}
	cfg := a.getConfig()
	var width int
	switch sizeToken {
	case "small":
		width = cfg.Thumbnails.SmallWidth
	case "display":
		width = cfg.Thumbnails.DisplayWidth
	default:
		http.NotFound(w, r)
		return
	}

	img, ok := a.lookupImage(w, r, id)
	if !ok {
		return
	}
	if img.Hidden {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	if img.ThumbStatus != thumbStatusReady {
		// pending | failed: retryable-by-design miss.
		w.Header().Set("Cache-Control", "no-cache")
		http.Error(w, "thumbnail not ready", http.StatusNotFound)
		return
	}

	path, ok := a.store.FindThumbPath(img.SHA256, width)
	if !ok {
		// ready row without any derivative file is DB/store drift;
		// surface as a retryable miss and log for the operator.
		logger.Error("ready thumbnail missing on disk", "id", id, "width", width)
		w.Header().Set("Cache-Control", "no-cache")
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// deleted between the stat in FindThumbPath and this open —
			// same drift bucket as above.
			logger.Error("ready thumbnail missing on disk", "id", id, "width", width)
			w.Header().Set("Cache-Control", "no-cache")
			http.NotFound(w, r)
			return
		}
		logger.Error("opening thumbnail", "id", id, "error", err)
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// handleDeleteImage implements DELETE /api/images/<id> — the admin soft
// delete (milestone 7), authenticated exactly like /updo and
// /admin/reload (constant-time X-API-Key compare). Response matrix:
//
//	401 bad/missing key · 404 unknown or malformed id · 500 DB failure
//	200 hidden by this request · 410 already hidden
//
// DESIGN NOTES:
//   - Soft delete only — files stay on disk. Storage is content
//     addressed and dedupe lets N rows share one file, so deleting
//     bytes would break sibling rows; see dbHideImage.
//   - Idempotency is deliberately NOT "second delete succeeds": a
//     repeat (or racing) DELETE gets 410 Gone. The guard inside the
//     UPDATE elects exactly one winner, so the event is published
//     exactly once and the loser can distinguish "someone else already
//     hid it" from failure without either request erroring.
//   - image-hidden publishes only AFTER the UPDATE commits (same
//     ordering rule as image-new after the upload INSERT): a client
//     dropping the card may immediately re-query and must not see the
//     row. Nil-hub-safe (tests without a hub).
func (a *App) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	if !checkAPIKey(a.getConfig().Auth.APIKey, r.Header.Get(apiKeyHeader)) {
		logger.Warn("image delete rejected: bad or missing api key", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if !validImageID(id) {
		http.NotFound(w, r)
		return
	}
	// Distinguish 404 (unknown id) from 410 (known but already hidden)
	// up front; the UPDATE's own guard re-checks under race.
	if _, ok := a.lookupImage(w, r, id); !ok {
		return
	}
	hid, err := dbHideImageFn(a.db, id)
	if err != nil {
		logger.Error("hiding image failed", "id", id, "error", err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}
	if !hid {
		// Lost the race (or a repeat request): the row was already
		// hidden. Do not re-publish — subscribers already dropped the
		// card for the winner's event.
		http.Error(w, "gone", http.StatusGone)
		return
	}
	a.publishImageHidden(id)

	w.Header().Set("Content-Type", "application/json")
	resp := struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}{ID: id, Status: "hidden"}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Error("writing delete response", "id", id, "error", err)
		return
	}
	logger.Info("image hidden (soft delete; files retained)", "id", id)
}

// faviconSVG is the inline site favicon: a tiny framed-picture glyph
// in the gallery's dark palette and link accent. Inlined in the source
// (not web/) because it is served from the root as /favicon.ico —
// keeping it out of /static/ avoids a redirect-y <link> and keeps the
// browser's default request path answered by one literal route.
// Invariant: this SVG is a compile-time constant and must stay
// script-free (no <script>, <foreignObject>, or event-handler
// attributes) — it is served inline as image/svg+xml, and while
// nosniff + the explicit Content-Type prevent type confusion, only
// constant bytes make inline serving safe.
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">` +
	`<rect width="32" height="32" rx="7" fill="#1d1d21"/>` +
	`<circle cx="12.5" cy="11.5" r="3.2" fill="#7ab0ff"/>` +
	`<path d="M5 24.2 13.4 15l4.8 5 3.4-3.6 5.4 5.9v.7a2.4 2.4 0 0 1-2.4 2.4H7.4A2.4 2.4 0 0 1 5 23z" fill="#4a6fb5"/>` +
	`</svg>`

// handleFavicon serves GET /favicon.ico. Registered as an exact
// literal so it always wins precedence over the "GET /" catch-all —
// "favicon.ico" is not a valid image id (11 chars, contains '.'), so
// the id dispatcher would 404 it (TestRouteIsolation pins this route
// table fact). Short cache: the glyph is code and may change with the
// binary, and a day-long re-check is free.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(faviconSVG))
}

// buildHandler wires the routes. Exact/literal patterns ("/updo",
// "/admin/reload", "/favicon.ico", "/gallery", "/search",
// "/search-fragment", "/events", "/api/images/{id}/neighbors",
// "/static/") always win precedence over the "GET /" catch-all, whose
// dispatcher (handleImageRoutes) owns the id-keyed routes and their
// [0-9A-Za-z]{7} shape validation. /events and the search routes MUST
// stay exact literals: "events" (6) and "search" (6) are not valid
// image ids, so letting them fall into the catch-all would 404 them.
// "favicon.ico" (11 chars, contains '.') is likewise id-invalid, and
// the DELETE /api/images/{id} wildcard cannot overlap the GET-only
// catch-all.
func (a *App) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.handleImageRoutes)
	mux.HandleFunc("GET /favicon.ico", handleFavicon)
	mux.HandleFunc("GET /gallery", a.handleGalleryFragment)
	mux.HandleFunc("GET /search", a.handleSearchPage)
	mux.HandleFunc("GET /search-fragment", a.handleSearchFragment)
	mux.HandleFunc("GET /events", a.handleEvents)
	mux.HandleFunc("POST /updo", a.handleUpload)
	mux.HandleFunc("POST /admin/reload", a.handleAdminReload)
	mux.HandleFunc("POST /admin/reextract", a.handleAdminReextract)
	mux.HandleFunc("GET /api/images/{id}/neighbors", a.handleNeighbors)
	mux.HandleFunc("DELETE /api/images/{id}", a.handleDeleteImage)
	mux.HandleFunc("GET /static/", staticHandler().ServeHTTP)

	var handler http.Handler = mux
	handler = nosniffMiddleware(handler)
	handler = requestLogMiddleware(handler)
	return handler
}

// nosniffMiddleware sets X-Content-Type-Options: nosniff on every
// response — HTML pages and image bytes alike.
func nosniffMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

// loggingResponseWriter captures the status code for request logging. It
// deliberately implements Unwrap instead of http.Flusher: streaming
// handlers (SSE, milestone 4) must flush via
// http.NewResponseController(w).Flush(), which reaches the underlying
// Flusher through Unwrap — a direct Flush method on this wrapper would
// bypass the status-capture contract.
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
