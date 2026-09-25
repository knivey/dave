package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertImage inserts a row directly for page/neighbor tests, with sane
// required fields and per-test overrides applied via mutators.
func insertImage(t *testing.T, app *App, id, createdAt string, mutators ...func(*dbImage)) {
	t.Helper()
	seed := int64(111222333444555)
	steps := 8
	cfgv := 1.0
	denoise := 1.0
	loras := `[{"name":"ZIT/PepeZimage.safetensors","strength":0.2}]`
	img := dbImage{
		ID:             id,
		SHA256:         strings.Repeat(strings.ToLower(id[0:1])+id[1:2], 32),
		Filename:       id + ".webp",
		MimeType:       "image/webp",
		SizeBytes:      123456,
		CreatedAt:      createdAt,
		ThumbStatus:    thumbStatusPending,
		OriginalPrompt: "prompt for " + id,
		Seed:           &seed,
		Steps:          &steps,
		Cfg:            &cfgv,
		Denoise:        &denoise,
		Sampler:        &[]string{"euler"}[0],
		Scheduler:      &[]string{"simple"}[0],
		ModelUnet:      &[]string{"krea2_turbo_int8_convrot.safetensors"}[0],
		ModelClip:      &[]string{"qwen3vl_4b_fp8_scaled.safetensors"}[0],
		ModelVae:       &[]string{"qwen_image_vae.safetensors"}[0],
		Loras:          &loras,
		WorkflowJSON:   "{\n  \"58\": {\n    \"class_type\": \"KSampler\"\n  }\n}",
		JobID:          &[]string{"job-" + id}[0],
		Network:        &[]string{"libera"}[0],
		Channel:        &[]string{"#dave"}[0],
		Nick:           &[]string{"knivey"}[0],
		WorkflowName:   &[]string{"zimage-turbo"}[0],
		MetaSource:     metaSourceUploadAndEXIF,
	}
	for _, m := range mutators {
		m(&img)
	}
	require.NoError(t, dbInsertImage(app.db, &img))
}

func getPage(t *testing.T, tsURL, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(tsURL + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func TestDetailsPageRendersMetadata(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	width, height := 1920, 1080
	reasoning := "step one\nstep two"
	insertImage(t, app, "aaaa001", "2026-09-24 03:12:00", func(img *dbImage) {
		img.EnhancedPrompt = "an enhanced version"
		img.Reasoning = reasoning
		img.NegativePrompt = "blurry"
		img.Width, img.Height = &width, &height
	})

	status, body := getPage(t, ts.URL, "/aaaa001")

	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "prompt for aaaa001", "original prompt prominent")
	assert.Contains(t, body, "an enhanced version")
	assert.Contains(t, body, "blurry")
	assert.Contains(t, body, "step one\nstep two", "reasoning preserved inside pre")
	assert.Contains(t, body, "111222333444555", "seed rendered")
	assert.Contains(t, body, "1920\u00d71080", "dimensions")
	assert.Contains(t, body, "120.6 KB", "humanized file size")
	assert.Contains(t, body, "krea2_turbo_int8_convrot.safetensors")
	assert.Contains(t, body, "ZIT/PepeZimage.safetensors")
	assert.Contains(t, body, "(0.2)", "lora strength")
	assert.Contains(t, body, "job-aaaa001")
	assert.Contains(t, body, "zimage-turbo")
	assert.Contains(t, body, "libera #dave by knivey", "provenance composed")
	assert.Contains(t, body, "2026-09-24 03:12:00 UTC")
	assert.Contains(t, body, `href="/aaaa001/orig/aaaa001.webp"`, "link to orig")
	// <pre> content is HTML-escaped by the template; quotes render as &#34;.
	assert.Contains(t, body, "&#34;class_type&#34;: &#34;KSampler&#34;", "pretty-printed workflow JSON rendered (escaped)")
	assert.NotContains(t, body, `id="nav-prev"`, "single image has no prev")
	assert.NotContains(t, body, `id="nav-next"`, "single image has no next")
}

// TestDetailsPageReasoningExpandedByDefault pins the default-expanded
// reasoning section: the <details> element carries the open attribute
// (so it renders expanded on load, no JS involved) while remaining a
// plain native toggle. The workflow JSON viewer keeps its collapsed
// default, and rows without reasoning render no reasoning section.
func TestDetailsPageReasoningExpandedByDefault(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "eeee003", "2026-09-24 05:00:00", func(img *dbImage) {
		img.EnhancedPrompt = "an enhanced version"
		img.Reasoning = "step one\nstep two"
	})
	insertImage(t, app, "eeee004", "2026-09-24 05:01:00")

	t.Run("ReasoningOpenByDefault", func(t *testing.T) {
		body := getPage2(t, ts, "/eeee003")
		assert.Contains(t, body,
			"<details open>\n<summary>Enhancement reasoning</summary>",
			"reasoning section renders with the open attribute")
		assert.Contains(t, body, "step one\nstep two", "reasoning content still present")
		assert.Contains(t, body, "</details>", "still a collapsible details element")
	})

	t.Run("WorkflowJSONStaysCollapsed", func(t *testing.T) {
		body := getPage2(t, ts, "/eeee003")
		assert.Contains(t, body,
			"<details>\n<summary>Workflow JSON</summary>",
			"workflow viewer keeps its default-collapsed state")
	})

	t.Run("NoReasoningRendersNoSection", func(t *testing.T) {
		body := getPage2(t, ts, "/eeee004")
		assert.NotContains(t, body, "Enhancement reasoning")
	})
}

func TestDetailsPageEscapesUntrustedText(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "bbbb002", "2026-09-24 03:12:00", func(img *dbImage) {
		img.OriginalPrompt = "<script>alert(1)</script>"
		img.EnhancedPrompt = "<img src=x onerror=alert(2)>"
		img.WorkflowJSON = `{"x":"</script><script>alert(3)</script>"}`
	})

	status, body := getPage(t, ts.URL, "/bbbb002")

	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", "original prompt escaped")
	assert.Contains(t, body, "&lt;img src=x onerror=alert(2)&gt;", "enhanced prompt escaped")
	assert.NotContains(t, body, "<script>alert(1)", "no raw script injection from prompts")
	assert.NotContains(t, body, "<img src=x onerror", "no raw attr injection")
	assert.NotContains(t, body, "alert(3)</script>", "workflow JSON in pre escaped")
}

// insertThreeImages seeds oldest/middle/newest rows and returns their ids.
func insertThreeImages(t *testing.T, app *App) (oldest, middle, newest string) {
	t.Helper()
	oldest, middle, newest = "ccc0001", "ccc0002", "ccc0003"
	insertImage(t, app, oldest, "2026-09-24 01:00:00")
	insertImage(t, app, middle, "2026-09-24 02:00:00")
	insertImage(t, app, newest, "2026-09-24 03:00:00")
	return
}

// TestDetailsPagePrevNextNavigation pins the keyset chevrons and the
// milestone-4 live hooks: data-image-id always renders (image.js needs
// the current id), data-at-end only on the newest image (the page
// where live arrivals can extend navigation), and the app.js module
// entry is wired so the enhancement layer loads.
func TestDetailsPagePrevNextNavigation(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	oldest, middle, newest := insertThreeImages(t, app)

	assert.Contains(t, getPage2(t, ts, "/"+middle), `src="/static/app.js"`, "module entry wired on the image page")

	t.Run("MiddleHasBoth", func(t *testing.T) {
		body := getPage2(t, ts, "/"+middle)
		assert.Contains(t, body, `id="nav-prev" href="/`+newest+`"`, "prev points to the newer image")
		assert.Contains(t, body, `id="nav-next" href="/`+oldest+`"`, "next points to the older image")
		assert.Contains(t, body, `data-image-id="`+middle+`"`)
		assert.NotContains(t, body, `data-at-end`, "mid-history: live reveal does not apply")
	})
	t.Run("NewestHasNoPrev", func(t *testing.T) {
		body := getPage2(t, ts, "/"+newest)
		assert.NotContains(t, body, `id="nav-prev"`)
		assert.Contains(t, body, `id="nav-next" href="/`+middle+`"`)
		assert.Contains(t, body, `data-image-id="`+newest+`"`)
		assert.Contains(t, body, `data-at-end="true"`, "newest image: image.js subscribes for live arrivals")
	})
	t.Run("OldestHasNoNext", func(t *testing.T) {
		body := getPage2(t, ts, "/"+oldest)
		assert.Contains(t, body, `id="nav-prev" href="/`+middle+`"`)
		assert.NotContains(t, body, `id="nav-next"`)
		assert.NotContains(t, body, `data-at-end`, "oldest is not the arrival end")
	})
}

// getPage2 is getPage returning only the body (assertions here never
// need the status).
func getPage2(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	status, body := getPage(t, ts.URL, path)
	require.Equal(t, http.StatusOK, status)
	return body
}

// TestDetailsPageSameSecondTiebreak pins the id DESC tiebreaker: with
// identical created_at, prev/next must still be well-defined.
func TestDetailsPageSameSecondTiebreak(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	// Same timestamp; id ordering decides: aaa < bbb < ccc.
	insertImage(t, app, "ddd0001", "2026-09-24 04:00:00")
	insertImage(t, app, "ddd0002", "2026-09-24 04:00:00")
	insertImage(t, app, "ddd0003", "2026-09-24 04:00:00")

	_, body := getPage(t, ts.URL, "/ddd0002")

	assert.Contains(t, body, `id="nav-prev" href="/ddd0003"`, "prev = higher id at the same second")
	assert.Contains(t, body, `id="nav-next" href="/ddd0001"`, "next = lower id at the same second")
}

func TestNeighborsAPI(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	oldest, middle, newest := insertThreeImages(t, app)

	getNeighbors := func(t *testing.T, id string) map[string]any {
		t.Helper()
		status, body := getPage(t, ts.URL, "/api/images/"+id+"/neighbors")
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "no-cache", neighborCacheHeader(t, ts.URL, id))
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &m))
		return m
	}

	t.Run("Middle", func(t *testing.T) {
		m := getNeighbors(t, middle)
		prev := m["prev"].(map[string]any)
		next := m["next"].(map[string]any)
		assert.Equal(t, newest, prev["id"])
		assert.Equal(t, "2026-09-24T03:00:00Z", prev["created_at"], "RFC3339 rendering")
		assert.Equal(t, "prompt for "+newest, prev["original_prompt"])
		assert.Equal(t, oldest, next["id"])
	})
	t.Run("NewestEndIsNull", func(t *testing.T) {
		m := getNeighbors(t, newest)
		assert.Nil(t, m["prev"])
		assert.Equal(t, middle, m["next"].(map[string]any)["id"])
	})
	t.Run("OldestEndIsNull", func(t *testing.T) {
		m := getNeighbors(t, oldest)
		assert.Nil(t, m["next"])
		assert.Equal(t, middle, m["prev"].(map[string]any)["id"])
	})
	t.Run("UnknownID", func(t *testing.T) {
		status, _ := getPage(t, ts.URL, "/api/images/zzzzzzz/neighbors")
		assert.Equal(t, http.StatusNotFound, status)
	})
	t.Run("InvalidID", func(t *testing.T) {
		status, _ := getPage(t, ts.URL, "/api/images/short/neighbors")
		assert.Equal(t, http.StatusNotFound, status)
	})
}

func neighborCacheHeader(t *testing.T, tsURL, id string) string {
	t.Helper()
	resp, err := http.Get(tsURL + "/api/images/" + id + "/neighbors")
	require.NoError(t, err)
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.Header.Get("Cache-Control")
}

func TestNeighborsAndPageSkipHidden(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "eeee001", "2026-09-24 01:00:00")
	insertImage(t, app, "eeee002", "2026-09-24 02:00:00", func(img *dbImage) { img.Hidden = true })
	insertImage(t, app, "eeee003", "2026-09-24 03:00:00")

	// The hidden middle image must be skipped by both surfaces.
	_, body := getPage(t, ts.URL, "/eeee003")
	assert.Contains(t, body, `id="nav-next" href="/eeee001"`, "hidden neighbor skipped by page nav")
	assert.NotContains(t, body, "eeee002")

	m := func() map[string]any {
		status, body := getPage(t, ts.URL, "/api/images/eeee003/neighbors")
		require.Equal(t, http.StatusOK, status)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &m))
		return m
	}()
	assert.Equal(t, "eeee001", m["next"].(map[string]any)["id"])
}

func TestHiddenImagePageReturns410(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "ffff001", "2026-09-24 01:00:00", func(img *dbImage) { img.Hidden = true })

	status, _ := getPage(t, ts.URL, "/ffff001")
	assert.Equal(t, http.StatusGone, status)

	status, _ = getPage(t, ts.URL, "/api/images/ffff001/neighbors")
	assert.Equal(t, http.StatusGone, status)
}

// injectNeighborFailure points both neighbor-lookup seams at an always
// failing function.
func injectNeighborFailure(t *testing.T) {
	t.Helper()
	fail := func(db *sqlx.DB, createdAt, id string) (*dbImage, error) {
		return nil, fmt.Errorf("injected neighbor lookup failure")
	}
	origNewer, origOlder := dbGetNewerImageFn, dbGetOlderImageFn
	dbGetNewerImageFn = fail
	dbGetOlderImageFn = fail
	t.Cleanup(func() {
		dbGetNewerImageFn = origNewer
		dbGetOlderImageFn = origOlder
	})
}

func TestNeighborsAPIDBErrorReturns500(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "abcd001", "2026-09-24 01:00:00")
	injectNeighborFailure(t)

	req, err := http.NewRequest("GET", ts.URL+"/api/images/abcd001/neighbors", nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "neighbor lookup failure must surface as 500, not a null-ended 200")
}

// TestDetailsPageDegradesWhenNeighborLookupFails pins the page's
// degrade-to-missing-links behavior: a DB failure in either neighbor
// direction still renders the page, just without that nav link.
func TestDetailsPageDegradesWhenNeighborLookupFails(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	_, middle, _ := insertThreeImages(t, app)
	injectNeighborFailure(t)

	status, body := getPage(t, ts.URL, "/"+middle)

	require.Equal(t, http.StatusOK, status, "page must still render when neighbor lookups fail")
	assert.NotContains(t, body, `id="nav-prev"`)
	assert.NotContains(t, body, `id="nav-next"`)
	assert.Contains(t, body, "prompt for "+middle, "content unaffected")
}

// TestDetailsPageMainImageServesOriginal pins the owner-requested (Sep
// 2026) details-page swap plus its redesign: the main <img> loads the
// ORIGINAL bytes (the display thumb stays an og:image derivative) in a
// container-owned aspect box (the zoom-toggle BUTTON, sized via inline
// aspect-ratio — see the dims test), chevrons flank the image as flex
// siblings instead of absolute strips over it, and the distortion-prone
// 2fedc24 pattern (img-level dual caps + width/height attrs) is gone.
func TestDetailsPageMainImageServesOriginal(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "zooma01", "2026-09-24 03:12:00")

	_, body := getPage(t, ts.URL, "/zooma01")

	assert.Contains(t, body, `src="/zooma01/orig/zooma01.webp"`, "main image uses the original bytes")
	assert.NotContains(t, body, `t/display`, "display thumb is not the page's main image (og:image only, and this pending row falls back to orig there too)")
	assert.Contains(t, body, `<button type="button" class="zoom-toggle`, "zoom toggle button carries the main image")
	assert.Contains(t, body, `aria-haspopup="dialog" aria-expanded="false"`, "toggle advertises the overlay dialog and its closed state (aria-pressed is gone with the in-place toggle semantics)")
	assert.NotContains(t, body, `aria-pressed`, "no in-place zoom state to press")
	assert.Contains(t, body, `<div class="stage">`, "stage wraps the fit-state box between the chevron rails")
	// The bulletproof fit pattern: the BOX owns the caps and the img
	// fills it with contain — the img itself no longer carries the
	// distortion-prone dual max-* caps.
	assert.Contains(t, body, `.zoom-toggle img { display: block; width: 100%; height: 100%; object-fit: contain; }`,
		"contain fit pattern ships (ratio lives on the container, contain letterboxes — cannot distort)")
	assert.NotContains(t, body, `.viewer img {`, "old img-level dual-cap rule is gone (with width/height attrs shipped it clamped both axes independently — the squash bug)")
	// Chevrons outside the image: slim flex rails, not absolute strips.
	assert.Contains(t, body, `flex: none; width: clamp(`, "chevron rule is a flex rail that shrinks via clamp(), never overlaps the image")
	assert.NotContains(t, body, `width: 18%`, "the 18% absolute strip is gone")
	// Pan-zoom apparatus deleted with the overlay redesign.
	assert.NotContains(t, body, `justify-content: safe center`, "safe-center (overflow pan alignment) is gone — nothing overflows the fit box anymore")
	assert.NotContains(t, body, `.viewer.zoomed`, "no in-place zoomed class — zoom is the fullscreen overlay now")
	assert.NotContains(t, body, `<a href="/zooma01/orig/zooma01.webp"><img`, "main image is no longer wrapped in a file link")
	assert.NotContains(t, body, "onerror=", "no inline onerror fallback — the src IS the orig URL")
	assert.Contains(t, body, `<a href="/zooma01/orig/zooma01.webp" download>download</a>`, "download anchor intact")
	assert.Contains(t, body, `<a href="/zooma01/orig/zooma01.webp">open file</a>`, "direct-file anchor beside the download button")
}

// TestDetailsPageZoomOverlayMarkup pins the fullscreen overlay's
// server-rendered half: it ships hidden (no-JS = no zoom, and display
// stays none because the [hidden] rule outranks the author display:
// flex), it is a dialog (role/aria-modal + the toggle's haspopup pair),
// its img reuses the orig URL (browser cache — no refetch), its sizing
// is pure-CSS contain (shrink big / scale up small, letterbox both —
// no JS dimension math), the fade is gated behind
// prefers-reduced-motion, and the body scroll-lock rule ships.
func TestDetailsPageZoomOverlayMarkup(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "zoomb01", "2026-09-24 03:12:00")

	_, body := getPage(t, ts.URL, "/zoomb01")

	assert.Contains(t, body, `<div class="zoom-overlay" id="zoom-overlay" hidden role="dialog" aria-modal="true" aria-label="zoomed image">`,
		"overlay ships server-rendered and hidden")
	assert.Contains(t, body, `<button type="button" class="zoom-close" id="zoom-close" title="close (Esc)" aria-label="close zoomed image">`,
		"visible close affordance inside the overlay")
	assert.Contains(t, body, "<img src=\"/zoomb01/orig/zoomb01.webp\" alt=\"prompt for zoomb01\">\n</div>",
		"overlay img reuses the orig URL and the row's prompt alt")
	assert.Contains(t, body, `.zoom-overlay[hidden] { display: none; }`,
		"hidden rule outranks the author display: flex (otherwise the UA rule is overridden and no-JS ships a visible overlay)")
	assert.Contains(t, body, `.zoom-overlay.open { opacity: 1; visibility: visible; }`,
		"class-based open state (visibility keeps the closed overlay out of the a11y tree and tab order)")
	assert.Contains(t, body, `opacity: 0; visibility: hidden;`, "closed overlay is fully inert")
	assert.Contains(t, body, `position: fixed; inset: 0; z-index: 100;`, "fixed overlay covers the entire page — topnav, chevrons, everything")
	assert.Contains(t, body, `.zoom-overlay img { width: 100%; height: 100%; object-fit: contain; }`,
		"overlay sizing is pure-CSS contain — larger-than-screen shrinks, smaller-than-screen scales up, both letterbox")
	assert.Contains(t, body, `.zoom-overlay { transition: opacity 0.15s ease, visibility 0s linear 0.15s; }`,
		"closing transition delays the visibility flip to the fade's end (fade-out stays visible, then inert)")
	assert.Contains(t, body, `.zoom-overlay.open { transition: opacity 0.15s ease, visibility 0s; }`,
		"opening transition flips visibility instantly — an animated visibility computes hidden at progress 0 and no-ops the open-path focus even a frame later (browser-review finding)")
	assert.Contains(t, body, `@media (prefers-reduced-motion: no-preference)`,
		"fade transitions gated behind prefers-reduced-motion")
	assert.Contains(t, body, `body.zoom-open { overflow: hidden; }`, "page scroll locks behind the open overlay")
	assert.Equal(t, 2, strings.Count(body, "object-fit: contain"), "contain pattern on both the fit-state img and the overlay img")
}

// TestDetailsPageMainImageDimsAspectRatio pins the layout-shift guard
// on the fit-state box: the inline aspect-ratio + max-width pair
// renders only when the row carries BOTH dims (graph-extracted at
// upload or backfilled by the thumb worker); NULL dims fall back to
// the .nodims auto-sizing variant with no inline style at all.
func TestDetailsPageMainImageDimsAspectRatio(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	width, height := 1920, 1080
	insertImage(t, app, "dimsa01", "2026-09-24 03:12:00", func(img *dbImage) {
		img.Width, img.Height = &width, &height
	})
	// Default insertImage leaves Width/Height NULL (dims arrive from
	// graph extraction or the thumb worker's decode backfill).
	insertImage(t, app, "dimsa02", "2026-09-24 03:13:00")

	_, known := getPage(t, ts.URL, "/dimsa01")
	assert.Contains(t, known, `<button type="button" class="zoom-toggle" id="zoom-toggle" aria-haspopup="dialog" aria-expanded="false" title="zoom in" style="aspect-ratio: 1920 / 1080; max-width: min(100%, 1920px, calc(85vh * 1920 / 1080))">`,
		"known dims render the aspect-ratio + three-term width-cap pair on the fit box (pair or nothing — a lone dim would bake a wrong ratio; the calc term is the width at which ratio-height reaches 85vh, so the height cap never binds and the box cannot go misshapen)")

	_, unknown := getPage(t, ts.URL, "/dimsa02")
	// Nothing else on the page renders an inline style or any
	// width="/height= attribute (the viewport meta is unquoted) —
	// absence pins the NULL-dims fallback.
	assert.NotContains(t, unknown, `style="`, "NULL dims ship no inline style")
	assert.NotContains(t, unknown, `width="`, "NULL dims omit the width attribute entirely")
	assert.NotContains(t, unknown, `height="`, "NULL dims omit the height attribute entirely")
	assert.NotContains(t, unknown, `aspect-ratio`, "no ratio reserved while dims are unknown")
	assert.Contains(t, unknown, `class="zoom-toggle nodims"`, "NULL dims take the auto-sizing fallback class")
	assert.Contains(t, unknown, `.zoom-toggle.nodims img { width: auto; height: auto; max-width: 100%; max-height: 85vh; }`,
		"fallback keeps BOTH dimensions auto (safe only because the img carries no attrs — the definite-size variant is the squash bug)")
}

func TestGalleryCardsEscapePrompts(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "aaaa002", "2026-09-24 03:12:00", func(img *dbImage) {
		img.OriginalPrompt = "<script>alert(1)</script> and a really long prompt that goes on"
	})

	_, page := getPage(t, ts.URL, "/")
	_, frag := getPage(t, ts.URL, "/gallery")

	for name, body := range map[string]string{"page": page, "fragment": frag} {
		assert.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", name)
		assert.NotContains(t, body, "<script>alert(1)", name)
	}
}

func TestGallerySnippetClamped(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	cfg := testConfig()
	long := strings.Repeat("word ", cfg.Search.SnippetChars) // way past the clamp
	insertImage(t, app, "aaaa003", "2026-09-24 03:12:00", func(img *dbImage) {
		img.OriginalPrompt = long
	})

	_, body := getPage(t, ts.URL, "/")

	assert.Contains(t, body, "…", "clamped with ellipsis")
	assert.NotContains(t, body, long, "full prompt never shipped to the card")
}

func TestClampSnippet(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"eleven chars", 10, "eleven ch…"},
		{"x", 1, "x"},
		{"xy", 1, "…"},
		{"anything", 0, "anything"},
		{"héllo wörld beyond", 7, "héllo …"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, clampSnippet(tt.in, tt.max), "input %q max %d", tt.in, tt.max)
	}
}

func TestClampWorkflowJSON(t *testing.T) {
	// At/below the bound: byte-identical passthrough (fast path).
	small := "{\n  \"58\": {\n    \"class_type\": \"KSampler\"\n  }\n}"
	assert.Equal(t, small, clampWorkflowJSON(small))

	// One rune past the bound triggers the marker; the cut lands on a
	// rune boundary even for multi-byte content (the result stays
	// valid UTF-8).
	multiByte := strings.Repeat("é", workflowJSONMaxRunes+5)
	got := clampWorkflowJSON(multiByte)
	assert.True(t, utf8.ValidString(got), "cut must land on a rune boundary")
	assert.True(t, strings.HasSuffix(got, workflowJSONTruncationMarker), "truncation marker appended")
	wantRunes := workflowJSONMaxRunes + utf8.RuneCountInString(workflowJSONTruncationMarker)
	assert.Equal(t, wantRunes, utf8.RuneCountInString(got), "clamped to bound + marker")
}

// TestDetailsPageWorkflowJSONTruncationMarker pins the <pre> viewer's
// clamp end-to-end: a pathological workflow JSON renders truncated with
// the explicit marker instead of shipping a multi-megabyte page (the
// raw bytes stay recoverable from the orig file's embedded EXIF).
func TestDetailsPageWorkflowJSONTruncationMarker(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	huge := `{"nodes":[` + strings.Repeat(`"x",`, workflowJSONMaxRunes) + `]}`
	insertImage(t, app, "cccc004", "2026-09-24 03:12:00", func(img *dbImage) {
		img.WorkflowJSON = huge
	})

	status, body := getPage(t, ts.URL, "/cccc004")

	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "…[truncated]", "cut renders the marker")
	assert.NotContains(t, body, huge, "full workflow JSON never shipped to the page")
	assert.Less(t, len(body), len(huge), "page is smaller than the raw JSON")
}

func TestParseKeysetCursor(t *testing.T) {
	createdAt, id, ok := parseKeysetCursor("2026-09-24 03:12:00~aaaa001")
	require.True(t, ok)
	assert.Equal(t, "2026-09-24 03:12:00", createdAt)
	assert.Equal(t, "aaaa001", id)

	for _, bad := range []string{
		"",
		"no-tilde",
		"~",
		"2026-09-24 03:12:00~",
		"~aaaa001",
		"not-a-date~aaaa001",
		"2026-09-24 03:12:00~short",
		"2026-09-24 03:12:00~zzzzzzz", // valid shape, unknown id — parse is shape-only
	} {
		if bad == "2026-09-24 03:12:00~zzzzzzz" {
			continue // shape-valid; documented above
		}
		_, _, ok := parseKeysetCursor(bad)
		assert.False(t, ok, "cursor %q", bad)
	}
}

func TestHumanizeBytes(t *testing.T) {
	tests := map[int64]string{
		0:           "0 B",
		512:         "512 B",
		2048:        "2.0 KB",
		123456:      "120.6 KB",
		32 << 20:    "32.0 MB",
		5 * 1 << 30: "5.0 GB",
	}
	for in, want := range tests {
		assert.Equal(t, want, humanizeBytes(in), "input %d", in)
	}
}

// ---------------------------------------------------------------------------
// Milestone 7: OpenGraph tags + gallery empty state
// ---------------------------------------------------------------------------

func TestDetailsPageOpenGraphTags(t *testing.T) {
	cfg := testConfig()
	cfg.Server.BaseURL = "https://img.example.com"
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)
	width, height := 1920, 1080
	insertImage(t, app, "ogga001", "2026-09-24 03:12:00", func(img *dbImage) {
		img.OriginalPrompt = `prompt with "quotes" & <angles>`
		img.Width, img.Height = &width, &height
	})

	status, body := getPage(t, ts.URL, "/ogga001")

	require.Equal(t, http.StatusOK, status)
	// og:title is the original prompt, attribute-escaped (quotes →
	// &#34; — attribute contexts auto-escape in html/template).
	assert.Contains(t, body, `<meta property="og:title" content="prompt with &#34;quotes&#34; &amp; &lt;angles&gt;">`)
	assert.Contains(t, body,
		`<meta property="og:description" content="zimage-turbo · 1920×1080 · krea2_turbo_int8_convrot.safetensors">`)
	// Thumb is pending on this row: og:image must fall back to the
	// absolute orig URL (/t/display would 404 for an unfurler).
	assert.Contains(t, body, `<meta property="og:image" content="https://img.example.com/ogga001/orig/ogga001.webp">`)
	assert.Contains(t, body, `<meta property="og:url" content="https://img.example.com/ogga001">`)
	assert.Contains(t, body, `<meta name="twitter:card" content="summary_large_image">`)
}

func TestDetailsPageOpenGraphVariants(t *testing.T) {
	cfg := testConfig()
	cfg.Server.BaseURL = "https://img.example.com"
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)

	t.Run("ReadyThumbUsesDisplayURL", func(t *testing.T) {
		insertImage(t, app, "oggb001", "2026-09-24 03:12:00", func(img *dbImage) {
			img.ThumbStatus = thumbStatusReady
		})
		_, body := getPage(t, ts.URL, "/oggb001")
		assert.Contains(t, body, `<meta property="og:image" content="https://img.example.com/oggb001/t/display">`)
	})
	t.Run("FailedThumbFallsBackToOrig", func(t *testing.T) {
		insertImage(t, app, "oggc001", "2026-09-24 03:12:00", func(img *dbImage) {
			img.ThumbStatus = thumbStatusFailed
		})
		_, body := getPage(t, ts.URL, "/oggc001")
		assert.Contains(t, body, `<meta property="og:image" content="https://img.example.com/oggc001/orig/oggc001.webp">`)
	})
	t.Run("TitleClampedAndFallsBackToFilename", func(t *testing.T) {
		long := strings.Repeat("w", 300)
		insertImage(t, app, "oggd001", "2026-09-24 03:12:00", func(img *dbImage) {
			img.OriginalPrompt = long
		})
		_, body := getPage(t, ts.URL, "/oggd001")
		want := `<meta property="og:title" content="` + strings.Repeat("w", 79) + `…">`
		assert.Contains(t, body, want, "og:title clamped to 80 runes including the ellipsis")
		// The clamp protects the HEAD only — the body's prompt panel
		// still shows the full prompt by design.
		head := body[:strings.Index(body, "</head>")]
		assert.NotContains(t, head, long, "full prompt never shipped into the head")

		insertImage(t, app, "ogge001", "2026-09-24 03:12:00", func(img *dbImage) {
			img.OriginalPrompt = ""
		})
		_, body = getPage(t, ts.URL, "/ogge001")
		assert.Contains(t, body, `<meta property="og:title" content="ogge001.webp">`, "no prompt: filename is the title")
	})
	t.Run("BaseURLDerivedFromRequest", func(t *testing.T) {
		// Empty server.base_url: absolute og URLs derive from the
		// request host — the same rule as the upload response and the
		// copy-link button.
		app2 := newTestApp(t, testConfig())
		ts2 := newTestServer(t, app2)
		insertImage(t, app2, "oggf001", "2026-09-24 03:12:00")
		_, body := getPage(t, ts2.URL, "/oggf001")
		assert.Contains(t, body, `<meta property="og:url" content="`+ts2.URL+`/oggf001">`)
		assert.Contains(t, body, `<meta property="og:image" content="`+ts2.URL+`/oggf001/orig/oggf001.webp">`)
	})
}

func TestGalleryAndSearchPagesOpenGraph(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "oggh001", "2026-09-24 03:12:00")

	_, gallery := getPage(t, ts.URL, "/")
	assert.Contains(t, gallery, `<meta property="og:title" content="test site">`)
	assert.Contains(t, gallery, `<meta property="og:description" content="test descriptions">`)

	// Search page: og:title carries the query (matching the page
	// title format), og:description the site description. The query
	// is attribute-escaped like every other untrusted value.
	_, search := getPage(t, ts.URL, "/search?q="+url.QueryEscape(`q with "quotes"`))
	assert.Contains(t, search, `<meta property="og:title" content="search: q with &#34;quotes&#34; — test site">`)
	assert.Contains(t, search, `<meta property="og:description" content="test descriptions">`)
	assert.NotContains(t, search, `<meta property="og:image"`, "basic og only on list pages")

	// Fragments carry no head at all.
	_, frag := getPage(t, ts.URL, "/gallery")
	assert.NotContains(t, frag, "og:title")
}

func TestGalleryEmptyState(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	status, body := getPage(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, status, "empty gallery still renders")
	assert.Contains(t, body, "nothing here yet", "friendly server-rendered empty state")
	assert.Contains(t, body, `class="no-results"`, "reuses the styled no-results class")
	assert.NotContains(t, body, "no results for", "distinct from the search no-results note")

	// Once an image exists the state is gone — and it never appears on
	// search no-results (that has its own message) or empty fragments.
	insertImage(t, app, "empt001", "2026-09-24 03:12:00")
	_, body = getPage(t, ts.URL, "/")
	assert.NotContains(t, body, "nothing here yet")

	_, search := getPage(t, ts.URL, "/search?q=zzzznothing")
	assert.NotContains(t, search, "nothing here yet")
	assert.Contains(t, search, "no results for zzzznothing")

	app2 := newTestApp(t, testConfig())
	ts2 := newTestServer(t, app2)
	insertImage(t, app2, "empt002", "2026-09-24 03:12:00")
	_, frag := getPage(t, ts2.URL, "/gallery?after="+url.QueryEscape(formatKeysetCursor("2026-09-24 03:12:00", "empt002")))
	assert.NotContains(t, frag, "nothing here yet", "end-of-scroll fragment is not an empty gallery")
}

func TestBuildOGDescription(t *testing.T) {
	width, height := 1024, 768
	unet := "model.safetensors"
	workflow := "zimage-turbo"
	tests := []struct {
		name string
		img  dbImage
		want string
	}{
		{"Full", dbImage{WorkflowName: &workflow, Width: &width, Height: &height, ModelUnet: &unet},
			"zimage-turbo · 1024×768 · model.safetensors"},
		{"NoDims", dbImage{WorkflowName: &workflow, ModelUnet: &unet},
			"zimage-turbo · model.safetensors"},
		{"OnlyDims", dbImage{Width: &width, Height: &height}, "1024×768"},
		{"OneDimOnly", dbImage{Width: &width}, ""},
		{"Empty", dbImage{}, ""},
		{"Clamped", dbImage{ModelUnet: &[]string{strings.Repeat("m", 500)}[0]},
			strings.Repeat("m", 199) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, buildOGDescription(&tt.img))
		})
	}
}

func TestDetailsPageSearchForm(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImageWithFile(t, app, "frm0001", pngBytes("f"), func(img *dbImage) {
		img.OriginalPrompt = "form test"
	})

	resp, err := http.Get(ts.URL + "/frm0001")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	// The topnav search bar: a plain GET form to /search — Enter
	// navigates to the server-rendered search page, no JS involved.
	assert.Contains(t, html, `<form action="/search" method="get" role="search">`)
	assert.Contains(t, html, `type="search"`, "the CSS keys on type=search — a type= text refactor must not pass silently")
	assert.Contains(t, html, `name="q"`)
	assert.Contains(t, html, `aria-label="Search prompts"`)
}

// ---------------------------------------------------------------------------
// Render-time SSE cursor (body[data-last-event])
// ---------------------------------------------------------------------------

// TestPagesEmbedRenderEventCursor pins the render→subscribe race fix's
// server half: every full page that boots a live-updating snapshot —
// gallery, search results (live via the "+N new" pill), and the image
// details page (live next-button) — embeds the hub's current event id
// as body[data-last-event], and the embedded value tracks the hub as
// events are published. 0 is a meaningful cursor (a hub with no events
// yet: replay everything published since the render) and must render,
// not be dropped as a zero value.
func TestPagesEmbedRenderEventCursor(t *testing.T) {
	app := newTestApp(t, testConfig())
	hub := newSSEHub()
	app.setEventHub(hub)
	ts := newTestServer(t, app)
	insertImage(t, app, "curt001", "2026-09-24 03:12:00")

	// Fresh hub: cursor 0 renders (nil-pointer-free meaningful zero).
	_, body := getPage(t, ts.URL, "/")
	assert.Contains(t, body, `data-last-event="0"`, "gallery embeds the zero cursor")

	hub.publish(eventImageNew, imageNewEvent{ID: "curt001"})
	hub.publish(eventThumbReady, thumbReadyEvent{ID: "curt001"})

	_, body = getPage(t, ts.URL, "/")
	assert.Contains(t, body, `data-last-event="2"`, "gallery cursor tracks the hub id at render time")

	_, body = getPage(t, ts.URL, "/search?q=prompt")
	assert.Contains(t, body, `data-last-event="2"`, "search results page carries the cursor (pill updates)")

	_, body = getPage(t, ts.URL, "/curt001")
	assert.Contains(t, body, `data-last-event="2"`, "details page carries the cursor (live next-button)")

	// One more publish and a re-render proves the value is live, not a
	// stale constant baked at startup.
	hub.publish(eventImageHidden, imageHiddenEvent{ID: "curt001"})
	_, body = getPage(t, ts.URL, "/")
	assert.Contains(t, body, `data-last-event="3"`)

	// Fragments omit the cursor even WITH a hub wired: they render only
	// the "cards" partial (no body tag) — they are swapped into a page
	// whose SSE stream already exists, so the cursor belongs to the
	// page body, not the fragment. Pins the field comment's contract.
	for _, path := range []string{"/gallery", "/search-fragment?q=prompt"} {
		_, frag := getPage(t, ts.URL, path)
		assert.NotContains(t, frag, "data-last-event", "fragment %s must never carry the cursor", path)
	}
}

// TestPagesWithoutHubOmitEventCursor pins the nil-hub fallback: pages
// rendered without an attached hub (every test app that never wires
// one; production always does before serving) omit the attribute
// entirely — the client's first connect then stays live-only — and
// nothing panics. Fragments never carry it: they are swapped into a
// page whose SSE stream already exists, so the cursor belongs to the
// page body, not the fragment.
func TestPagesWithoutHubOmitEventCursor(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertImage(t, app, "curn001", "2026-09-24 03:12:00")

	for _, path := range []string{"/", "/search?q=prompt", "/curn001", "/gallery"} {
		_, body := getPage(t, ts.URL, path)
		assert.NotContains(t, body, "data-last-event", "path %s must omit the cursor without a hub (fragments always)", path)
	}
}
