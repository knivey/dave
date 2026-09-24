package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Milestone-2 details page. All output goes through html/template
// auto-escaping — prompts, reasoning, and workflow JSON are LLM/user text
// and must never hit the page raw. The gallery grid, display-size
// thumbnails, and SSE live-next are later milestones; the main image uses
// the orig URL for now. Milestone 7 adds the OpenGraph block (see
// headExtrasSrc).

// headExtrasSrc defines the <head> additions shared by EVERY full-page
// template set: the favicon link and the two universal OpenGraph tags.
// Parsed into the image, gallery, and search sets so all three render
// identical head markup; og:title/og:description come from view fields
// (every view struct carries OGTitle/OGDescription), which keeps this
// one partial the single place og tags live. The details page layers
// og:image / og:url / twitter:card on top inline — only it has them.
// All values flow through html/template attribute-context escaping
// (quotes → &#34;), so prompts with quotes can never break out of the
// content attributes.
const headExtrasSrc = `{{define "head-extras"}}<link rel="icon" href="/favicon.ico" type="image/svg+xml">
<meta property="og:title" content="{{.OGTitle}}">
<meta property="og:description" content="{{.OGDescription}}">
{{end}}`

var imageTemplate = template.Must(template.New("image").Parse(headExtrasSrc + `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} — {{.ID}}</title>
{{template "head-extras" .}}<meta property="og:image" content="{{.OGImageURL}}">
<meta property="og:url" content="{{.OGPageURL}}">
<meta name="twitter:card" content="summary_large_image">
<script type="module" src="/static/app.js"></script>
<style>
:root { color-scheme: dark; }
body { margin: 0; font-family: system-ui, sans-serif; background: #141416; color: #ddd; line-height: 1.5; }
a { color: #7ab0ff; text-decoration: none; }
a:hover { text-decoration: underline; }
.topnav { display: flex; gap: 1rem; align-items: center; padding: 0.6rem 1rem; background: #1d1d21; border-bottom: 1px solid #2c2c31; }
.topnav .spacer { flex: 1; }
.topnav input[type="search"] { background: #141416; color: #ddd; border: 1px solid #3c3c44; border-radius: 6px; padding: 0.25rem 0.7rem; font: inherit; min-width: min(14rem, 40vw); }
button { background: #2a2a30; color: #ddd; border: 1px solid #3c3c44; border-radius: 6px; padding: 0.25rem 0.7rem; cursor: pointer; font: inherit; }
button:hover { background: #35353d; }
.viewer { position: relative; display: flex; justify-content: center; background: #0d0d0f; }
.viewer img { max-width: 100%; max-height: 85vh; display: block; }
.chevron { position: absolute; top: 0; bottom: 0; width: 18%; display: flex; align-items: center; justify-content: center; font-size: 3rem; color: rgba(255,255,255,0.25); }
.chevron:hover { background: rgba(255,255,255,0.06); color: rgba(255,255,255,0.6); text-decoration: none; }
.chevron.left { left: 0; }
.chevron.right { right: 0; }
main { max-width: 60rem; margin: 0 auto; padding: 1rem; }
h1 { font-size: 1.15rem; margin: 1rem 0 0.25rem; }
details { background: #1d1d21; border: 1px solid #2c2c31; border-radius: 8px; padding: 0.5rem 0.9rem; margin: 0.5rem 0; }
summary { cursor: pointer; font-weight: 600; }
.original { font-size: 1.05rem; background: #1d1d21; border: 1px solid #2c2c31; border-radius: 8px; padding: 0.6rem 0.9rem; white-space: pre-wrap; }
pre { white-space: pre-wrap; word-break: break-word; font-size: 0.85rem; }
table.params { border-collapse: collapse; width: 100%; }
table.params td { border: 1px solid #2c2c31; padding: 0.35rem 0.6rem; vertical-align: top; }
table.params td:first-child { width: 9rem; color: #999; white-space: nowrap; }
.mono { font-family: ui-monospace, monospace; }
.provenance { color: #999; font-size: 0.9rem; }
</style>
</head>
<body data-page="image" data-image-id="{{.ID}}"{{if .AtEnd}} data-at-end="true"{{end}}>
<nav class="topnav">
<a href="/">&larr; gallery</a>
<span class="spacer"></span>
{{/* Plain GET form: Enter navigates to the server-rendered /search
     page. No JS module boots a search layer on the image page, so this
     stays a pure form submit — the keyboard nav in image.js ignores
     keystrokes originating from inputs. */}}
<form action="/search" method="get" role="search">
<input type="search" name="q" placeholder="search prompts…" autocomplete="off" aria-label="Search prompts">
</form>
<button data-copy="{{.AbsOrigURL}}" title="copy direct link">copy link</button>
<a href="{{.OrigURL}}" download>download</a>
</nav>
<div class="viewer">
{{if .HasPrev}}<a class="chevron left" id="nav-prev" href="/{{.PrevID}}" title="newer (← / p)">&#8249;</a>{{end}}
<a href="{{.OrigURL}}"><img src="/{{.ID}}/t/display" onerror="this.onerror=null;this.src='{{.OrigURL}}'" alt="{{.OriginalPrompt}}"></a>
{{if .HasNext}}<a class="chevron right" id="nav-next" href="/{{.NextID}}" title="older (→ / n)">&#8250;</a>{{end}}
</div>
<main>
<h1>{{.Filename}}</h1>
<p class="provenance">{{.CreatedAt}} UTC
{{if .JobID}} &middot; job <span class="mono">{{.JobID}}</span>{{end}}
{{if .WorkflowName}} &middot; {{.WorkflowName}}{{end}}
{{if .ProvenanceChannel}} &middot; {{.ProvenanceChannel}}{{end}}
</p>

<h2>Original prompt</h2>
{{if .OriginalPrompt}}<p class="original">{{.OriginalPrompt}}</p>{{else}}<p class="provenance"><em>(no prompt recorded)</em></p>{{end}}

{{if .EnhancedPrompt}}
<details{{if .EnhancedDiffers}} open{{end}}>
<summary>Enhanced prompt</summary>
<p>{{.EnhancedPrompt}}</p>
</details>
{{end}}

{{if .Reasoning}}
<details>
<summary>Enhancement reasoning</summary>
<pre>{{.Reasoning}}</pre>
</details>
{{end}}

{{if .NegativePrompt}}
<details open>
<summary>Negative prompt</summary>
<p>{{.NegativePrompt}}</p>
</details>
{{end}}

{{if .ParamRows}}
<details open>
<summary>Parameters</summary>
<table class="params">
{{range .ParamRows}}
<tr><td>{{.Label}}</td><td{{if .Mono}} class="mono"{{end}}>{{.Value}}{{if .Copy}} <button data-copy="{{.Value}}" title="copy">&#9112;</button>{{end}}</td></tr>
{{end}}
</table>
</details>
{{end}}

{{if .WorkflowJSON}}
<details>
<summary>Workflow JSON</summary>
<pre>{{.WorkflowJSON}}</pre>
</details>
{{end}}
</main>
<script>
(function () {
	"use strict";
	// Keyboard nav (←/→/n/p, modifier-guarded) moved to image.js: it
	// looks the chevron elements up at keypress time so a live-revealed
	// chevron's href (SSE image-new) is picked up — the closed-over
	// vars here could never see an element created after parse. The
	// copy-link buttons stay: they need no live element references.
	document.querySelectorAll("[data-copy]").forEach(function (btn) {
		btn.addEventListener("click", function () {
			var v = btn.getAttribute("data-copy");
			if (navigator.clipboard && v) {
				navigator.clipboard.writeText(v).then(function () {
					var old = btn.textContent;
					btn.textContent = "copied";
					setTimeout(function () { btn.textContent = old; }, 1200);
				});
			}
		});
	});
})();
</script>
</body>
</html>
`))

// cardsPartialSrc is the shared card-grid partial. Parsed into every
// template set that needs it (gallery page/fragment, search
// page/fragment) so page 1, fetched pages, and search results all
// render identical markup.
//
// Snippet rendering: search hits carry SnippetParts (marker-split
// pieces of FTS5 snippet() output). Every piece is rendered through
// html/template auto-escaping ({{.Text}}); only the literal <mark> tags
// come from the template itself — there is no template.HTML anywhere,
// so untrusted prompt text can never inject markup no matter what the
// snippet markers split around (see search.go's marker design note).
const cardsPartialSrc = `{{define "cards"}}{{range .Cards}}<article class="card" data-id="{{.ID}}">
<a class="card-link" href="/{{.ID}}">
{{if .ThumbFailed}}<img loading="lazy" src="{{.OrigURL}}" alt="{{.PromptSnippet}}">{{else if .ThumbPending}}<img class="pending" loading="lazy" src="{{.ThumbURL}}" data-orig="{{.OrigURL}}" alt="{{.PromptSnippet}}">{{else}}<img loading="lazy" src="{{.ThumbURL}}" alt="{{.PromptSnippet}}">{{end}}
</a>
{{if .SnippetParts}}<p class="prompt">{{range .SnippetParts}}{{if .Hit}}<mark>{{.Text}}</mark>{{else}}{{.Text}}{{end}}{{end}}</p>{{else}}<p class="prompt">{{.PromptSnippet}}</p>{{end}}
<time datetime="{{.CreatedRFC3339}}" data-ts="{{.CreatedRFC3339}}">{{.Timestamp}}</time>
</article>
{{end}}{{if .NoResults}}<p class="no-results">no results for {{.Query}}</p>{{end}}{{if .NothingSearched}}<p class="no-results">nothing searched yet — type a query</p>{{end}}{{if .EmptyGallery}}<p class="no-results">nothing here yet — images appear here as they are generated</p>{{end}}{{if .HasMore}}<div class="sentinel" data-next-cursor="{{.NextCursor}}"></div>{{end}}{{end}}`

// galleryPageSrc is the full gallery page (the root template of the
// gallery set). The header carries the search box: a plain GET form to
// /search so searching works with no JS at all (search.js only adds
// debounced fragment-swapping on top).
const galleryPageSrc = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
{{template "head-extras" .}}<link rel="stylesheet" href="/static/style.css">
<script type="module" src="/static/app.js"></script>
</head>
<body data-page="gallery" data-gallery-title="{{.Title}}">
<header class="site">
<div class="head-row">
<div>
<h1>{{.Title}}</h1>
<p>{{.Description}}</p>
</div>
<form id="search-form" action="/search" method="get" role="search">
<input id="search-box" type="search" name="q" value="{{.Query}}" placeholder="search prompts…" autocomplete="off" aria-label="Search prompts">
</form>
</div>
</header>
<main>
<div id="grid">
{{template "cards" .}}
</div>
</main>
</body>
</html>
`

// searchPageSrc is the full search results page (the root template of
// the search set): the same chrome and grid as the gallery, with the
// query prefilled into the search box and the URL shareable as
// /search?q=…. data-page stays "gallery" deliberately — the search
// page IS the gallery in results mode as far as the JS modules are
// concerned (infinite scroll, SSE pill, thumb swap all run here).
const searchPageSrc = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<meta name="description" content="{{.Description}}">
{{template "head-extras" .}}<link rel="stylesheet" href="/static/style.css">
<script type="module" src="/static/app.js"></script>
</head>
<body data-page="gallery" data-gallery-title="{{.SiteTitle}}">
<header class="site">
<div class="head-row">
<div>
<h1>{{.SiteTitle}}</h1>
<p>{{.Description}}</p>
</div>
<form id="search-form" action="/search" method="get" role="search">
<input id="search-box" type="search" name="q" value="{{.Query}}" placeholder="search prompts…" autocomplete="off" aria-label="Search prompts">
</form>
</div>
</header>
<main>
<div id="grid">
{{template "cards" .}}
</div>
</main>
</body>
</html>
`

// galleryTemplates holds the gallery page set; searchTemplates the
// search page set. Both parse the shared head extras and cards partial
// so the fragment endpoints render byte-identical cards and every full
// page carries the same favicon + basic og block.
var galleryTemplates = template.Must(template.New("gallery").Parse(headExtrasSrc + cardsPartialSrc + galleryPageSrc))
var searchTemplates = template.Must(template.New("search").Parse(headExtrasSrc + cardsPartialSrc + searchPageSrc))

type galleryCard struct {
	ID             string
	ThumbURL       string
	OrigURL        string
	PromptSnippet  string
	Timestamp      string
	CreatedRFC3339 string
	ThumbPending   bool
	ThumbFailed    bool
	// SnippetParts, when non-nil, replaces the plain prompt preview
	// with a highlighted match snippet (search results only).
	SnippetParts []snippetPart
}

type galleryView struct {
	Title       string
	SiteTitle   string // gallery/search h1 + data-gallery-title (search pages: the SITE title, not the page title)
	Description string
	Query       string // current search query ("" = live gallery mode)
	NoResults   bool   // query present, zero hits — renders the empty-state note
	// NothingSearched marks an empty-query search page with zero hits
	// (runSearch short-circuits unsanitizable input) — the friendly
	// "nothing searched yet" note instead of a bare grid. Search pages
	// only; the gallery uses EmptyGallery.
	NothingSearched bool
	// EmptyGallery marks a query-less gallery with zero rows — the
	// friendly "nothing here yet" state (set by the page handler only;
	// an empty beyond-the-end fragment is a normal end of scroll, not
	// an empty gallery).
	EmptyGallery bool
	Cards        []galleryCard
	HasMore      bool
	NextCursor   string

	// OGTitle/OGDescription feed the shared head-extras partial
	// (favicon + basic og tags): the site title/description on the
	// gallery, "search: q — site" + site description on search pages.
	OGTitle       string
	OGDescription string
}

// galleryPageSize is the number of cards per keyset page; handlers fetch
// one extra row to detect has-more.
const galleryPageSize = 48

// handleGalleryPage renders GET / — the full gallery page (first page of
// cards, no-JS complete; infinite scroll is progressive enhancement).
func (a *App) handleGalleryPage(w http.ResponseWriter, r *http.Request) {
	cfg := a.getConfig()
	rows, err := dbGetGalleryPage(a.db, "", "", galleryPageSize+1)
	if err != nil {
		logger.Error("gallery page query failed", "error", err)
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	}
	view := buildGalleryView(cfg, rows)
	// Server-rendered empty state (M7): a fresh site with no uploads
	// still gets a friendly page. Set here, not in buildGalleryView,
	// because the same builder serves the /gallery fragment — an empty
	// fragment past the last row is the normal end of infinite scroll,
	// not "nothing here yet".
	view.EmptyGallery = len(view.Cards) == 0

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-cache: live content, SSE prepend arrives in milestone 4.
	w.Header().Set("Cache-Control", "no-cache")
	if err := galleryTemplates.Execute(w, view); err != nil {
		logger.Error("rendering gallery page", "error", err)
	}
}

// handleGalleryFragment renders GET /gallery?after=<cursor> — the next
// page as an HTML fragment (the same "cards" partial), consumed by
// gallery.js's infinite scroll. Fragment-only by design: the page is
// fully functional without JS and a JSON shape would duplicate the card
// markup contract for no consumer.
func (a *App) handleGalleryFragment(w http.ResponseWriter, r *http.Request) {
	var afterCreatedAt, afterID string
	if after := r.URL.Query().Get("after"); after != "" {
		var ok bool
		afterCreatedAt, afterID, ok = parseKeysetCursor(after)
		if !ok {
			http.Error(w, "malformed cursor", http.StatusBadRequest)
			return
		}
	}
	cfg := a.getConfig()
	rows, err := dbGetGalleryPage(a.db, afterCreatedAt, afterID, galleryPageSize+1)
	if err != nil {
		logger.Error("gallery fragment query failed", "error", err)
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	}
	view := buildGalleryView(cfg, rows)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := galleryTemplates.ExecuteTemplate(w, "cards", view); err != nil {
		logger.Error("rendering gallery fragment", "error", err)
	}
}

func buildGalleryView(cfg Config, rows []dbImage) galleryView {
	v := galleryView{
		Title:         cfg.Site.Title,
		SiteTitle:     cfg.Site.Title,
		Description:   cfg.Site.Description,
		OGTitle:       cfg.Site.Title,
		OGDescription: cfg.Site.Description,
	}
	if len(rows) > galleryPageSize {
		v.HasMore = true
		rows = rows[:galleryPageSize]
	}
	for i := range rows {
		v.Cards = append(v.Cards, galleryCardFromImage(cfg, &rows[i]))
	}
	if v.HasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		v.NextCursor = formatKeysetCursor(last.CreatedAt, last.ID)
	}
	return v
}

// galleryCardFromImage builds the shared card shape from one row.
// Callers layer search-specific presentation (snippet parts, tier-2
// prompt preview) on top.
func galleryCardFromImage(cfg Config, img *dbImage) galleryCard {
	created := img.CreatedAt
	rfc3339 := created
	if t, err := time.Parse(dbTimeFormat, created); err == nil {
		rfc3339 = t.UTC().Format(time.RFC3339)
	}
	return galleryCard{
		ID:             img.ID,
		ThumbURL:       "/" + img.ID + "/t/small",
		OrigURL:        "/" + img.ID + "/orig/" + url.PathEscape(img.Filename),
		PromptSnippet:  clampSnippet(img.OriginalPrompt, cfg.Search.SnippetChars),
		Timestamp:      created + " UTC",
		CreatedRFC3339: rfc3339,
		ThumbPending:   img.ThumbStatus == thumbStatusPending,
		ThumbFailed:    img.ThumbStatus == thumbStatusFailed,
	}
}

// formatKeysetCursor builds the plan's cursor wire format:
// "YYYY-MM-DD HH:MM:SS~<id>" (URL-encoded as one component by callers).
func formatKeysetCursor(createdAt, id string) string {
	return createdAt + "~" + id
}

// parseKeysetCursor validates and splits a cursor; LastIndex guards
// against ids ever containing '~' (they cannot today: [0-9A-Za-z]{7}).
func parseKeysetCursor(s string) (createdAt, id string, ok bool) {
	i := strings.LastIndexByte(s, '~')
	if i <= 0 || i >= len(s)-1 {
		return "", "", false
	}
	createdAt, id = s[:i], s[i+1:]
	if _, err := time.Parse(dbTimeFormat, createdAt); err != nil {
		return "", "", false
	}
	if !validImageID(id) {
		return "", "", false
	}
	return createdAt, id, true
}

// clampSnippet truncates to maxRunes (including the ellipsis) so gallery
// cards stay one-liner-ish regardless of prompt length.
func clampSnippet(s string, maxRunes int) string {
	if maxRunes <= 0 || len(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return "…"
	}
	return string(r[:maxRunes-1]) + "…"
}

// workflowJSONMaxRunes bounds the workflow JSON rendered into the
// details page's <pre> viewer. Real ComfyUI graphs are a few KB; the
// bound exists only so a pathological row (or a hand-crafted upload)
// can't ship a multi-megabyte HTML page. Generous (128 KiB of runes)
// so no legitimate graph ever trips it. The raw bytes are not lost:
// the full graph stays embedded in the orig file's EXIF, downloadable
// via /<id>/orig/<filename>.
const workflowJSONMaxRunes = 128 << 10

// workflowJSONTruncationMarker mirrors img-mcp's prompt-note marker
// style: visually distinct, greppable.
const workflowJSONTruncationMarker = "\n…[truncated]"

// clampWorkflowJSON caps the rendered workflow JSON at
// workflowJSONMaxRunes runes, appending the truncation marker when it
// had to cut. Rune-counted so the cut always lands on a rune boundary.
func clampWorkflowJSON(s string) string {
	if len(s) <= workflowJSONMaxRunes {
		// Fast path: rune count is always <= byte count, so a
		// short-enough byte length needs no rune walk.
		return s
	}
	r := []rune(s)
	if len(r) <= workflowJSONMaxRunes {
		return s
	}
	return string(r[:workflowJSONMaxRunes]) + workflowJSONTruncationMarker
}

type paramRow struct {
	Label string
	Value string
	Copy  bool
	Mono  bool
}

type loraView struct {
	Name     string
	Strength string
}

type imageView struct {
	Title string
	ID    string

	Filename       string
	CreatedAt      string
	OriginalPrompt string
	OrigURL        string
	// AbsOrigURL is the absolute direct link handed to the copy-link
	// button (the page URL's scheme+host + orig path).
	AbsOrigURL string

	EnhancedPrompt  string
	EnhancedDiffers bool
	Reasoning       string
	NegativePrompt  string

	ParamRows []paramRow

	JobID             string
	WorkflowName      string
	ProvenanceChannel string

	WorkflowJSON string

	HasPrev bool
	PrevID  string
	HasNext bool
	NextID  string

	// OpenGraph (M7): og:title is the clamped original prompt (filename
	// fallback), og:description the params summary, og:image an
	// ABSOLUTE display-thumb URL (orig URL when the thumb isn't ready —
	// /t/ 404s until the worker flips the row), og:url the absolute
	// page URL. Absolute because unfurlers (Discord, etc.) resolve
	// og:image against og:url at best and drop relative URLs at worst.
	OGTitle       string
	OGDescription string
	OGImageURL    string
	OGPageURL     string

	// AtEnd marks "viewing the newest image" — no prev (newer)
	// neighbor exists, so live arrivals can only ever extend this
	// page's navigation; image.js keys its SSE subscription off
	// data-at-end. Naming note: this is the end in ARRIVAL order,
	// which the plan's prose calls "next is null"; in this
	// codebase's keyset naming the arrival direction is prev
	// (newer) — see handleNeighbors.
	AtEnd bool
}

// buildImageView assembles the template view from a row plus its keyset
// neighbors (prev = newer, next = older). Display formatting (humanized
// sizes, lora strings, param rows) happens here so the template stays
// logic-free and everything untrusted flows through auto-escaping only.
func buildImageView(cfg Config, img *dbImage, prev, next *dbImage, absBase string) imageView {
	v := imageView{
		Title:          cfg.Site.Title,
		ID:             img.ID,
		Filename:       img.Filename,
		CreatedAt:      img.CreatedAt,
		OriginalPrompt: img.OriginalPrompt,
		OrigURL:        "/" + img.ID + "/orig/" + url.PathEscape(img.Filename),
		AbsOrigURL:     absBase + "/" + img.ID + "/orig/" + url.PathEscape(img.Filename),
		EnhancedPrompt: img.EnhancedPrompt,
		Reasoning:      img.Reasoning,
		NegativePrompt: img.NegativePrompt,
		JobID:          ptrValue(img.JobID),
		WorkflowName:   ptrValue(img.WorkflowName),
		WorkflowJSON:   clampWorkflowJSON(img.WorkflowJSON),
	}
	v.EnhancedDiffers = v.EnhancedPrompt != "" && v.EnhancedPrompt != v.OriginalPrompt

	if n := ptrValue(img.Network); n != "" {
		v.ProvenanceChannel = n
	}
	if c := ptrValue(img.Channel); c != "" {
		if v.ProvenanceChannel != "" {
			v.ProvenanceChannel += " "
		}
		v.ProvenanceChannel += c
	}
	if nk := ptrValue(img.Nick); nk != "" {
		if v.ProvenanceChannel != "" {
			v.ProvenanceChannel += " "
		}
		v.ProvenanceChannel += "by " + nk
	}

	v.ParamRows = buildParamRows(img)

	// OpenGraph fields (M7). absBase is the same base the copy-link
	// button uses (configured server.base_url, else request-derived —
	// the exact rules the upload response uses), so all absolute URLs
	// on the page agree on the host.
	v.OGTitle = clampSnippet(img.OriginalPrompt, ogTitleMaxChars)
	if v.OGTitle == "" {
		v.OGTitle = clampSnippet(img.Filename, ogTitleMaxChars)
	}
	v.OGDescription = buildOGDescription(img)
	v.OGPageURL = absBase + "/" + img.ID
	if img.ThumbStatus == thumbStatusReady {
		v.OGImageURL = absBase + "/" + img.ID + "/t/display"
	} else {
		// pending/failed: /t/display would 404 and an unfurler would
		// render no preview at all — the original bytes always serve.
		v.OGImageURL = v.AbsOrigURL
	}

	if prev != nil {
		v.HasPrev, v.PrevID = true, prev.ID
	}
	if next != nil {
		v.HasNext, v.NextID = true, next.ID
	}
	v.AtEnd = prev == nil
	return v
}

func buildParamRows(img *dbImage) []paramRow {
	var rows []paramRow
	add := func(label, value string, copy, mono bool) {
		if value != "" {
			rows = append(rows, paramRow{Label: label, Value: value, Copy: copy, Mono: mono})
		}
	}
	if img.Seed != nil {
		add("seed", strconv.FormatInt(*img.Seed, 10), true, true)
	}
	if img.Steps != nil {
		add("steps", strconv.Itoa(*img.Steps), false, true)
	}
	if img.Cfg != nil {
		add("cfg", trimFloat(*img.Cfg), false, true)
	}
	if img.Denoise != nil {
		add("denoise", trimFloat(*img.Denoise), false, true)
	}
	add("sampler", joinNonEmpty(ptrValue(img.Sampler), ptrValue(img.Scheduler), "/"), false, true)
	if img.Width != nil && img.Height != nil {
		add("dimensions", strconv.Itoa(*img.Width)+"\u00d7"+strconv.Itoa(*img.Height)+" px", false, true)
	}
	add("file size", humanizeBytes(img.SizeBytes), false, false)
	add("format", img.MimeType, false, true)
	add("model (unet)", ptrValue(img.ModelUnet), false, true)
	add("model (clip)", ptrValue(img.ModelClip), false, true)
	add("model (vae)", ptrValue(img.ModelVae), false, true)
	for _, l := range parseLoras(img.Loras) {
		row := l.Name
		if l.Strength != "" {
			row += " (" + l.Strength + ")"
		}
		add("lora", row, false, true)
	}
	return rows
}

// OpenGraph clamp sizes. og:title at ~80 chars keeps link previews
// one-or-two-line; og:description is a params summary, not prose, so
// the clamp only guards against absurd model-name strings.
const (
	ogTitleMaxChars       = 80
	ogDescriptionMaxChars = 200
)

// buildOGDescription composes the details page's og:description params
// summary — "<workflow> · <WxH> · <unet model>" — from whatever the row
// carries, dropping missing pieces and clamping the join. Empty pieces
// can't happen (each guard checks the field), and a fully-empty row
// yields "" (unfurlers fall back to page text on their own).
func buildOGDescription(img *dbImage) string {
	var parts []string
	if wf := ptrValue(img.WorkflowName); wf != "" {
		parts = append(parts, wf)
	}
	if img.Width != nil && img.Height != nil {
		parts = append(parts, strconv.Itoa(*img.Width)+"\u00d7"+strconv.Itoa(*img.Height))
	}
	if m := ptrValue(img.ModelUnet); m != "" {
		parts = append(parts, m)
	}
	return clampSnippet(strings.Join(parts, " \u00b7 "), ogDescriptionMaxChars)
}

func joinNonEmpty(a, b, sep string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + sep + b
	}
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func parseLoras(lorasJSON *string) []loraView {
	if lorasJSON == nil || *lorasJSON == "" {
		return nil
	}
	var entries []struct {
		Name     string   `json:"name"`
		Strength *float64 `json:"strength"`
	}
	if err := json.Unmarshal([]byte(*lorasJSON), &entries); err != nil {
		return nil
	}
	out := make([]loraView, 0, len(entries))
	for _, e := range entries {
		v := loraView{Name: e.Name}
		if e.Strength != nil {
			v.Strength = trimFloat(*e.Strength)
		}
		out = append(out, v)
	}
	return out
}

func (a *App) handleImagePage(w http.ResponseWriter, r *http.Request, id string) {
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

	// Server-rendered keyset neighbors: prev = newer, next = older, both
	// hidden-filtered. Failures degrade to missing links (page still
	// renders); NoRows is the normal end-of-gallery case.
	prev, err := dbGetNewerImageFn(a.db, img.CreatedAt, img.ID)
	if err != nil {
		logger.Error("neighbor lookup failed", "id", id, "dir", "prev", "error", err)
		prev = nil
	}
	next, err := dbGetOlderImageFn(a.db, img.CreatedAt, img.ID)
	if err != nil {
		logger.Error("neighbor lookup failed", "id", id, "dir", "next", "error", err)
		next = nil
	}

	view := buildImageView(a.getConfig(), img, prev, next, a.absBaseFromRequest(r))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// HTML stays no-cache for liveness (the live next-button arrives with
	// the SSE milestone).
	w.Header().Set("Cache-Control", "no-cache")
	if err := imageTemplate.Execute(w, view); err != nil {
		logger.Error("rendering image page", "id", id, "error", err)
	}
}

// absBaseFromRequest builds the absolute base for page-embedded absolute
// links (copy-link button): the configured server.base_url when set,
// otherwise derived from the request — same rules as upload-response URL
// derivation, so both paths agree on what the host is.
func (a *App) absBaseFromRequest(r *http.Request) string {
	if cfgBase := strings.TrimRight(a.getConfig().Server.BaseURL, "/"); cfgBase != "" {
		return cfgBase
	}
	return deriveBaseURL(r)
}
