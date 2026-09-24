package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uploadOne uploads a small PNG and returns the decoded response.
func uploadOne(t *testing.T, ts *httptest.Server) uploadResponse {
	t.Helper()
	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "test.png", data: pngBytes("route test")})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	return decodeUploadResponse(t, resp)
}

func TestOrigRouteServesDirect(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	data := pngBytes("route test")
	ur := uploadOne(t, ts)

	req, err := http.NewRequest("GET", ur.URL, nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	require.Equal(t, http.StatusOK, resp.StatusCode, "direct 200, no redirect hops")
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.NotEmpty(t, resp.Header.Get("Content-Length"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, data, body, "original bytes served verbatim")
}

func TestOrigRouteFilenameMismatch(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	ur := uploadOne(t, ts)

	req, err := http.NewRequest("GET", ts.URL+"/"+ur.ID+"/orig/other.png", nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrigRouteUnknownID(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	req, err := http.NewRequest("GET", ts.URL+"/zzzzzzz/orig/test.png", nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestOrigRouteInvalidIDs(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	for _, id := range []string{"short", "eightchar", "abc12 3", "abc-123"} {
		t.Run(id, func(t *testing.T) {
			req, err := http.NewRequest("GET", ts.URL+"/"+id+"/orig/test.png", nil)
			require.NoError(t, err)
			resp := doReq(t, ts, req)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, "malformed id never reaches the DB")
		})
	}
}

func TestImagePageStub(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	meta := `{"original_prompt":"shrew walkin down main street"}`
	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "shrew.webp", data: pngBytes("s"), meta: meta})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	req, err := http.NewRequest("GET", ur.Page, nil)
	require.NoError(t, err)
	page := doReq(t, ts, req)

	require.Equal(t, http.StatusOK, page.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", page.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", page.Header.Get("Cache-Control"))
	assert.Equal(t, "nosniff", page.Header.Get("X-Content-Type-Options"))

	body, err := io.ReadAll(page.Body)
	require.NoError(t, err)
	html := string(body)
	assert.Contains(t, html, ur.ID)
	assert.Contains(t, html, "shrew.webp")
	assert.Contains(t, html, "shrew walkin down main street")
	assert.Contains(t, html, ur.ID+"/orig/shrew.webp", "links to the original file")
}

func TestImagePageUnknownID(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	req, err := http.NewRequest("GET", ts.URL+"/zzzzzzz", nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestIndexPage(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	req, err := http.NewRequest("GET", ts.URL+"/", nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "test site")
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
}

// TestRouteIsolation pins the routing precedence: exact routes always win
// over the /{id} wildcard, and only well-formed ids reach the page/orig
// handlers. Admin paths, /updo, and junk paths never collide with /{id}.
func TestRouteIsolation(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"GetUpdoNotAnIdPage", http.MethodGet, "/updo", http.StatusNotFound},
		{"GetAdminNotAnIdPage", http.MethodGet, "/admin", http.StatusNotFound},
		{"GetAdminReloadNotRouted", http.MethodGet, "/admin/reload", http.StatusNotFound},
		// The favicon has its own exact-literal route (M7); the
		// id-shaped catch-all would 404 it since "favicon.ico" is 11
		// chars and contains '.'.
		{"GetFaviconServedByLiteralRoute", http.MethodGet, "/favicon.ico", http.StatusOK},
		{"IdTooShort", http.MethodGet, "/abc", http.StatusNotFound},
		{"IdTooLong", http.MethodGet, "/abcdefgh", http.StatusNotFound},
		{"NestedJunkPath", http.MethodGet, "/abc1234/junk", http.StatusNotFound},
		{"PostIdNotAllowed", http.MethodPost, "/abc1234", http.StatusMethodNotAllowed},
		{"IndexStillWorks", http.MethodGet, "/", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, ts.URL+tt.path, nil)
			require.NoError(t, err)
			resp := doReq(t, ts, req)
			assert.Equal(t, tt.want, resp.StatusCode)
		})
	}

	// /updo must keep its auth behavior (not fall through to /{id}).
	resp := doUpload(t, ts, "", uploadParts{hasFile: true, filename: "x.png", data: pngBytes("x")})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
[auth]
api_key = "`+testAPIKey+`"

[site]
title = "before"

[server]
addr = ":8081"
`), 0644))

	cfg, err := loadConfig(cfgPath)
	require.NoError(t, err)
	app := newTestApp(t, cfg)
	app.configPath = cfgPath
	ts := newTestServer(t, app)

	// Change a reloadable field and a non-reloadable one.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
[auth]
api_key = "`+testAPIKey+`"

[site]
title = "after"

[server]
addr = ":9999"
`), 0644))

	req, err := http.NewRequest("POST", ts.URL+"/admin/reload", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", testAPIKey)
	resp := doReq(t, ts, req)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	var rr reloadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&rr))
	assert.Equal(t, "ok", rr.Status)
	assert.Len(t, rr.Warnings, 1)
	assert.Contains(t, rr.Warnings[0], "server.addr")

	got := app.getConfig()
	assert.Equal(t, "after", got.Site.Title, "reloadable field applied")
	assert.Equal(t, ":8081", got.Server.Addr, "non-reloadable field frozen at startup value")
}

// injectLookupFailure swaps the handler's row-lookup seam for one that
// always errors, exercising the 500 branch of App.lookupImage.
func injectLookupFailure(t *testing.T) {
	t.Helper()
	orig := dbGetImageByIDFn
	dbGetImageByIDFn = func(db *sqlx.DB, id string) (*dbImage, error) {
		return nil, fmt.Errorf("injected db failure")
	}
	t.Cleanup(func() { dbGetImageByIDFn = orig })
}

func TestImagePageDBErrorReturns500(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	injectLookupFailure(t)

	req, err := http.NewRequest("GET", ts.URL+"/zzzzzzz", nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "DB failure on the page route must surface as 500, not 404")
}

func TestOrigRouteDBErrorReturns500(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	injectLookupFailure(t)

	req, err := http.NewRequest("GET", ts.URL+"/zzzzzzz/orig/test.png", nil)
	require.NoError(t, err)
	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "DB failure on the orig route must surface as 500, not 404")
}

func TestThumbRouteMatrix(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	cfg := testConfig()

	// plain call form: the returned hash is not needed here (the route
	// matrix keys on the image id, not the content hash).
	insertImageWithFile(t, app, "aaaa00a", loadFixture(t, "plain.webp"))
	require.NoError(t, processThumbJob(app.db, app.store, cfg, "aaaa00a"))
	insertImageWithFile(t, app, "bbbb00b", loadFixture(t, "enhanced.webp")) // stays pending
	insertImageWithFile(t, app, "cccc00c", loadFixture(t, "plain.webp"), func(img *dbImage) {
		img.Hidden = true
	})
	require.NoError(t, processThumbJob(app.db, app.store, cfg, "cccc00c"))

	t.Run("ReadyServesImmutableJPEG", func(t *testing.T) {
		for _, size := range []string{"small", "display"} {
			resp := fetchPath(t, ts, "/aaaa00a/t/"+size)
			assert.Equal(t, 200, resp.StatusCode, size)
			assert.Equal(t, "image/jpeg", resp.Header.Get("Content-Type"), size)
			assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"), size)
			assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"), size)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.True(t, bytes.HasPrefix(body, []byte{0xFF, 0xD8}), size)
		}
	})
	t.Run("PendingIs404NoCache", func(t *testing.T) {
		resp := fetchPath(t, ts, "/bbbb00b/t/small")
		assert.Equal(t, 404, resp.StatusCode)
		assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"), "the JS retry path depends on this being re-checked")
	})
	t.Run("HiddenIs410", func(t *testing.T) {
		resp := fetchPath(t, ts, "/cccc00c/t/small")
		assert.Equal(t, http.StatusGone, resp.StatusCode)
	})
	t.Run("BadSizeTokenIs404", func(t *testing.T) {
		resp := fetchPath(t, ts, "/aaaa00a/t/huge")
		assert.Equal(t, 404, resp.StatusCode)
	})
	t.Run("UnknownIDIs404", func(t *testing.T) {
		resp := fetchPath(t, ts, "/zzzzzzz/t/small")
		assert.Equal(t, 404, resp.StatusCode)
	})
	t.Run("InvalidIDIs404", func(t *testing.T) {
		resp := fetchPath(t, ts, "/short/t/small")
		assert.Equal(t, 404, resp.StatusCode)
	})
}

func TestStaticRoute(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	t.Run("StyleSheet", func(t *testing.T) {
		resp := fetchPath(t, ts, "/static/style.css")
		require.Equal(t, 200, resp.StatusCode)
		assert.Equal(t, "text/css; charset=utf-8", resp.Header.Get("Content-Type"))
		assert.Equal(t, "public, max-age=3600", resp.Header.Get("Cache-Control"))
		assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	})
	t.Run("Module", func(t *testing.T) {
		resp := fetchPath(t, ts, "/static/gallery.js")
		require.Equal(t, 200, resp.StatusCode)
		assert.Equal(t, "text/javascript; charset=utf-8", resp.Header.Get("Content-Type"))
	})
	t.Run("MissingIs404", func(t *testing.T) {
		resp := fetchPath(t, ts, "/static/nope.js")
		assert.Equal(t, 404, resp.StatusCode)
	})
	t.Run("NoDirectoryListing", func(t *testing.T) {
		// Both the embedded root and any trailing-slash target must 404
		// instead of rendering http.FileServer's directory index.
		for _, p := range []string{"/static/", "/static/app.js/"} {
			resp := fetchPath(t, ts, p)
			assert.Equal(t, 404, resp.StatusCode, p)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.NotContains(t, string(body), "gallery.js", "no file index in %s", p)
		}
	})
}

// insertFiveImages seeds rows with distinct timestamps, newest first
// ordering dddd005 > dddd004 > ... > dddd001, one hidden.
func insertFiveImages(t *testing.T, app *App) {
	t.Helper()
	for i := 5; i >= 1; i-- {
		id := "dddd00" + string(rune('0'+i)) // 7-char base62
		createdAt := time.Date(2026, 9, 24, 0, i, 0, 0, time.UTC).Format(dbTimeFormat)
		insertImage(t, app, id, createdAt, func(img *dbImage) {
			img.OriginalPrompt = "prompt for " + id
			if i == 3 {
				img.Hidden = true
			}
		})
	}
}

func TestGalleryPageRendersCards(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertFiveImages(t, app)

	resp := fetchPath(t, ts, "/")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)

	assert.Contains(t, html, `data-page="gallery"`)
	assert.Contains(t, html, `src="/dddd005/t/small"`, "cards use the small thumb")
	assert.Contains(t, html, `href="/dddd004"`)
	assert.NotContains(t, html, "dddd003", "hidden image filtered from the gallery")
	assert.Contains(t, html, "2026-09-24 00:05:00.000 UTC", "server-rendered timestamp fallback (ms-precision fixture)")
	assert.Contains(t, html, `data-ts="2026-09-24T00:05:00Z"`, "RFC3339 hook for JS localization")
	assert.NotContains(t, html, `class="sentinel"`, "no sentinel without a full page")
}

func TestGalleryFragmentKeyset(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertFiveImages(t, app)

	fragment := func(t *testing.T, cursor string) string {
		t.Helper()
		path := "/gallery"
		if cursor != "" {
			path += "?after=" + url.QueryEscape(cursor)
		}
		resp := fetchPath(t, ts, path)
		require.Equal(t, 200, resp.StatusCode)
		assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	t.Run("FirstPageIsNewestFirst", func(t *testing.T) {
		html := fragment(t, "")
		assert.Less(t, strings.Index(html, "dddd005"), strings.Index(html, "dddd004"), "newest first")
		assert.Less(t, strings.Index(html, "dddd004"), strings.Index(html, "dddd002"), "keyset order")
		assert.NotContains(t, html, "dddd003", "hidden filtered")
	})

	// Cursor = newest visible row (dddd005 at 00:05): next page must
	// exclude it and the boundary is not duplicated.
	t.Run("AfterBoundaryExcludesCursorRow", func(t *testing.T) {
		html := fragment(t, formatKeysetCursor("2026-09-24 00:05:00", "dddd005"))
		assert.NotContains(t, html, "dddd005", "cursor row never repeated")
		assert.Contains(t, html, "dddd004")
		assert.Contains(t, html, "dddd002")
	})

	t.Run("MiddleCursorSplitsPages", func(t *testing.T) {
		html := fragment(t, formatKeysetCursor("2026-09-24 00:04:00", "dddd004"))
		assert.NotContains(t, html, "dddd005")
		assert.NotContains(t, html, "dddd004")
		assert.Contains(t, html, "dddd002")
	})

	t.Run("MalformedCursorIs400", func(t *testing.T) {
		resp := fetchPath(t, ts, "/gallery?after=garbage")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestGallerySameSecondTiebreak(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	// Same timestamp; id DESC is the tiebreaker: e > m > a ordering.
	insertImage(t, app, "tttt001", "2026-09-24 06:00:00", func(img *dbImage) { img.OriginalPrompt = "oldest" })
	insertImage(t, app, "tttt002", "2026-09-24 06:00:00", func(img *dbImage) { img.OriginalPrompt = "middle" })
	insertImage(t, app, "tttt003", "2026-09-24 06:00:00", func(img *dbImage) { img.OriginalPrompt = "newest" })

	resp := fetchPath(t, ts, "/gallery?after="+url.QueryEscape(formatKeysetCursor("2026-09-24 06:00:00", "tttt003")))
	require.Equal(t, 200, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	html := string(body)
	assert.NotContains(t, html, "tttt003", "cursor row excluded via id tiebreak")
	assert.Contains(t, html, "tttt002")
	assert.Contains(t, html, "tttt001")
	assert.Less(t, strings.Index(html, "tttt002"), strings.Index(html, "tttt001"))
}

func TestAdminReloadBadKey(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	req, err := http.NewRequest("POST", ts.URL+"/admin/reload", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", "wrong")
	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminReloadInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("[site]\ntitle = \"no key\"\n"), 0644))

	cfg := testConfig() // current config is valid; the file on disk is not
	app := newTestApp(t, cfg)
	app.configPath = cfgPath
	before := app.getConfig()

	resp := app.doReload()

	assert.Equal(t, "error", resp.Status)
	assert.NotEmpty(t, resp.Message)
	assert.Equal(t, before, app.getConfig(), "failed reload leaves running config untouched")
}

// TestThumbWidthReloadStillServes pins the M3 review fix: thumbnail
// widths are reloadable, but derivative files are keyed by
// generation-time width. After a width reload the pre-existing ready
// row must still serve its OLD-width file (glob fallback), while a new
// row generates at the NEW widths.
func TestThumbWidthReloadStillServes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeCfg := func(smallWidth, displayWidth int) {
		t.Helper()
		require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
[auth]
api_key = "%s"

[thumbnails]
small_width = %d
display_width = %d
`, testAPIKey, smallWidth, displayWidth)), 0644))
	}

	writeCfg(480, 1280)
	cfg, err := loadConfig(cfgPath)
	require.NoError(t, err)
	app := newTestApp(t, cfg)
	app.configPath = cfgPath
	ts := newTestServer(t, app)

	// Ready row generated at the original widths (640x400 input ->
	// 480x300 small derivative).
	insertImageWithFile(t, app, "aaaa00d", smallPNG(t, 640, 400))
	require.NoError(t, processThumbJob(app.db, app.store, cfg, "aaaa00d"))

	writeCfg(360, 1024)
	resp := app.doReload()
	require.Equal(t, "ok", resp.Status)
	reloaded := app.getConfig()
	require.Equal(t, 360, reloaded.Thumbnails.SmallWidth, "widths are reloadable")

	// Before the fix this 404'd forever: no -360.jpg exists for the old
	// row and the startup re-scan only handles pending rows. Now the
	// fallback serves the stale-width file with immutable caching.
	small := fetchPath(t, ts, "/aaaa00d/t/small")
	require.Equal(t, 200, small.StatusCode)
	assert.Equal(t, "public, max-age=31536000, immutable", small.Header.Get("Cache-Control"))
	body, err := io.ReadAll(small.Body)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(body, []byte{0xFF, 0xD8}), "served bytes are a real JPEG")
	w, h := mustDecodeDims(t, body)
	assert.Equal(t, 480, w, "serves the generation-time width file")
	assert.Equal(t, 300, h)

	// A new row generates at the reloaded widths and serves those.
	newHash := insertImageWithFile(t, app, "bbbb00e", smallPNG(t, 640, 400))
	require.NoError(t, processThumbJob(app.db, app.store, reloaded, "bbbb00e"))
	for _, width := range []int{360, 1024} {
		_, err := os.Stat(app.store.ThumbPath(newHash, width))
		require.NoError(t, err, "new row derivative at reloaded width %d", width)
	}
	small2 := fetchPath(t, ts, "/bbbb00e/t/small")
	require.Equal(t, 200, small2.StatusCode)
	body2, err := io.ReadAll(small2.Body)
	require.NoError(t, err)
	w2, h2 := mustDecodeDims(t, body2)
	assert.Equal(t, 360, w2, "new row serves the reloaded small width")
	assert.Equal(t, 225, h2, "640x400 -> 360x225 at the new width")
}

// TestThumbWidthChangeWarning pins the single-WARN helper used by
// doReload when widths change across a reload.
func TestThumbWidthChangeWarning(t *testing.T) {
	smallChange := testConfig()
	smallChange.Thumbnails.SmallWidth = 360

	displayChange := testConfig()
	displayChange.Thumbnails.DisplayWidth = 1024

	tests := []struct {
		name   string
		oldCfg Config
		newCfg Config
		want   string
	}{
		{"UnchangedIsEmpty", testConfig(), testConfig(), ""},
		{"SmallWidthChanged", testConfig(), smallChange, "small_width 480 -> 360"},
		{"DisplayWidthChanged", testConfig(), displayChange, "display_width 1280 -> 1024"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := thumbWidthChangeWarning(tt.oldCfg, tt.newCfg)
			if tt.want == "" {
				assert.Empty(t, got)
				return
			}
			assert.Contains(t, got, tt.want)
			assert.Contains(t, got, "existing entries keep their generated sizes")
		})
	}
}

// ---------------------------------------------------------------------------
// Milestone 7: favicon + soft delete
// ---------------------------------------------------------------------------

func TestFaviconRoute(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	resp := fetchPath(t, ts, "/favicon.ico")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "image/svg+xml", resp.Header.Get("Content-Type"))
	assert.Equal(t, "public, max-age=86400", resp.Header.Get("Cache-Control"), "short cache: the glyph is code")
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(body, []byte("<svg")), "serves an inline SVG")
	assert.Contains(t, string(body), "</svg>")

	// Every full page template links the icon through the shared
	// head-extras partial (gallery, search, and the details page).
	insertImage(t, app, "favc001", "2026-09-24 03:12:00")
	for name, path := range map[string]string{
		"gallery": "/",
		"search":  "/search?q=x",
		"image":   "/favc001",
	} {
		_, page := getPage(t, ts.URL, path)
		assert.Contains(t, page, `<link rel="icon" href="/favicon.ico" type="image/svg+xml">`, name)
	}
}

// doDelete issues DELETE /api/images/<id> with the given API key ("" =
// no header).
func doDelete(t *testing.T, ts *httptest.Server, key, id string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("DELETE", ts.URL+"/api/images/"+id, nil)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	return doReq(t, ts, req)
}

func TestDeleteImageHandlerMatrix(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	ur := uploadOne(t, ts)

	t.Run("MissingKeyIs401", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, doDelete(t, ts, "", ur.ID).StatusCode)
	})
	t.Run("WrongKeyIs401", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, doDelete(t, ts, "wrong-key", ur.ID).StatusCode)
	})
	t.Run("UnknownIDIs404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, doDelete(t, ts, testAPIKey, "zzzzzzz").StatusCode)
	})
	t.Run("InvalidIDIs404", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, doDelete(t, ts, testAPIKey, "short").StatusCode)
	})

	t.Run("FirstDeleteIs200Hidden", func(t *testing.T) {
		resp := doDelete(t, ts, testAPIKey, ur.ID)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var body struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		assert.Equal(t, ur.ID, body.ID)
		assert.Equal(t, "hidden", body.Status)

		img, err := dbGetImageByID(app.db, ur.ID)
		require.NoError(t, err)
		assert.True(t, img.Hidden, "row is hidden=1 after the delete")
	})
	t.Run("SecondDeleteIs410", func(t *testing.T) {
		// Documented idempotency contract: a repeat delete does not
		// succeed again — the row is already gone from public view.
		assert.Equal(t, http.StatusGone, doDelete(t, ts, testAPIKey, ur.ID).StatusCode)
	})
	t.Run("FilesStayOnDisk", func(t *testing.T) {
		// Soft delete only: content-addressed storage is shared by
		// dedupe, so the bytes are never removed.
		img, err := dbGetImageByID(app.db, ur.ID)
		require.NoError(t, err)
		_, err = os.Stat(app.store.OriginalPath(img.SHA256))
		assert.NoError(t, err, "original file still on disk after soft delete")
	})
}

func TestDeleteDBErrorReturns500(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	ur := uploadOne(t, ts)

	orig := dbHideImageFn
	dbHideImageFn = func(db *sqlx.DB, id string) (bool, error) {
		return false, fmt.Errorf("injected hide failure")
	}
	t.Cleanup(func() { dbHideImageFn = orig })

	assert.Equal(t, http.StatusInternalServerError, doDelete(t, ts, testAPIKey, ur.ID).StatusCode)
}

// TestDeletePublishesImageHiddenAfterCommit pins the milestone-4/7
// ordering rule: the SSE event fires only after the hidden=1 UPDATE
// committed, so by the time a subscriber sees it, the row is already
// hidden to any re-query.
func TestDeletePublishesImageHiddenAfterCommit(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	frames, _, _ := openEvents(t, ts, nil, "")
	nextSSEFrame(t, frames) // retry hint

	ur := uploadOne(t, ts)
	// The upload publishes image-new first; consume it so the next
	// frame assertion really targets the delete's event.
	up := nextSSEEvent(t, frames)
	require.Equal(t, eventImageNew, up.name)

	resp := doDelete(t, ts, testAPIKey, ur.ID)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	f := nextSSEEvent(t, frames)
	require.Equal(t, eventImageHidden, f.name)
	var p imageHiddenEvent
	require.NoError(t, json.Unmarshal([]byte(f.data), &p))
	assert.Equal(t, ur.ID, p.ID)

	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.True(t, img.Hidden, "row already hidden when the event is observable")

	// A repeat delete must not re-publish (the guarded UPDATE lost).
	assert.Equal(t, http.StatusGone, doDelete(t, ts, testAPIKey, ur.ID).StatusCode)
	drainSSEFrames(t, frames)
	// Heartbeat comments may interleave (wire-direct; see
	// nextSSEEvent) — only a second NAMED event would be a bug.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case f := <-frames:
			if f.name != "" {
				t.Fatalf("no second image-hidden event may arrive, got %+v", f)
			}
		case <-deadline:
			return
		}
	}
}

// TestDeletedImageExcludedEverywhere drives the full soft-delete
// surface through the endpoint: every public read of the row is either
// excluded (gallery, fragment, search) or 410 (page, orig, thumb,
// neighbors).
func TestDeletedImageExcludedEverywhere(t *testing.T) {
	app := newTestApp(t, testConfig())
	app.setEventHub(newSSEHub())
	ts := newTestServer(t, app)

	meta := `{"original_prompt":"needle pending deletion"}`
	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "del.webp", data: pngBytes("del"), meta: meta})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	// Sanity: visible before the delete.
	status, gallery := getPage(t, ts.URL, "/")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, gallery, ur.ID)
	status, search := getPage(t, ts.URL, "/search?q=needle")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, search, ur.ID)

	require.Equal(t, http.StatusOK, doDelete(t, ts, testAPIKey, ur.ID).StatusCode)

	t.Run("PageIs410", func(t *testing.T) {
		status, _ := getPage(t, ts.URL, "/"+ur.ID)
		assert.Equal(t, http.StatusGone, status)
	})
	t.Run("OrigIs410", func(t *testing.T) {
		req, err := http.NewRequest("GET", ts.URL+"/"+ur.ID+"/orig/"+ur.Filename, nil)
		require.NoError(t, err)
		assert.Equal(t, http.StatusGone, doReq(t, ts, req).StatusCode)
	})
	t.Run("ThumbIs410", func(t *testing.T) {
		assert.Equal(t, http.StatusGone, fetchPath(t, ts, "/"+ur.ID+"/t/small").StatusCode)
	})
	t.Run("NeighborsIs410", func(t *testing.T) {
		assert.Equal(t, http.StatusGone, fetchPath(t, ts, "/api/images/"+ur.ID+"/neighbors").StatusCode)
	})
	t.Run("GalleryExcludes", func(t *testing.T) {
		status, body := getPage(t, ts.URL, "/")
		require.Equal(t, http.StatusOK, status)
		assert.NotContains(t, body, ur.ID)
	})
	t.Run("GalleryFragmentExcludes", func(t *testing.T) {
		status, body := getPage(t, ts.URL, "/gallery")
		require.Equal(t, http.StatusOK, status)
		assert.NotContains(t, body, ur.ID)
	})
	t.Run("SearchExcludes", func(t *testing.T) {
		// The FTS row keeps its tokens (no prompt columns touched), so
		// this exercises the hidden=0 join-back filtering for real.
		status, body := getPage(t, ts.URL, "/search?q=needle")
		require.Equal(t, http.StatusOK, status)
		assert.NotContains(t, body, ur.ID)
		assert.Contains(t, body, "no results for needle", "empty state after the only hit vanished")
	})
}

func TestAdminReextractHealsGGUFRows(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	// A row in the exact state the production bug left behind: the GGUF
	// workflow parsed at upload, but the loader exact-match dropped the
	// unet/clip while VAE (core class) and provenance survived.
	insertImageWithFile(t, app, "gguf0001", pngBytes("gguf"),
		func(img *dbImage) {
			img.WorkflowJSON = ggufGraph()
			// Deliberately WRONG stored prompt: the re-extract must heal
			// it from the note node AND keep FTS in sync through the
			// images_fts_au trigger dance (first prompt UPDATE in the
			// codebase — pinned here).
			img.OriginalPrompt = "wrong prompt stored"
			img.JobID = ptrStr("gguf0001")
			network, channel, nick := "libera", "#dave", "knivey"
			img.Network, img.Channel, img.Nick = &network, &channel, &nick
			img.WorkflowName = ptrStr("qwenHD")
			img.ModelVae = ptrStr("qwen_image_vae.safetensors")
			img.MetaSource = "upload+exif"
		},
		func(img *dbImage) { img.CreatedAt = "2026-09-24 16:10:00" },
	)
	// A row whose stored workflow_json no longer parses must be skipped
	// with its stored metadata intact.
	insertImageWithFile(t, app, "broken01", pngBytes("broken"),
		func(img *dbImage) {
			img.WorkflowJSON = "{not json"
			img.OriginalPrompt = "keep me"
		},
	)

	// Auth required.
	resp, err := http.Post(ts.URL+"/admin/reextract", "application/json", nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/reextract", nil)
	req.Header.Set("X-API-Key", testAPIKey)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Considered int `json:"considered"`
		Updated    int `json:"updated"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, 2, out.Considered)
	assert.Equal(t, 1, out.Updated, "unparseable row is skipped, not counted")

	healed, err := dbGetImageByID(app.db, "gguf0001")
	require.NoError(t, err)
	assert.Equal(t, "qwen-image-2512-Q8_0.gguf", ptrValue(healed.ModelUnet),
		"re-extract must heal the GGUF unet — the production bug")
	assert.Equal(t, "Qwen2.5-VL-7B-Instruct-UD-Q8_K_XL.gguf", ptrValue(healed.ModelClip))
	assert.Equal(t, "qwen_image_vae.safetensors", ptrValue(healed.ModelVae))
	assert.Equal(t, "libera", ptrValue(healed.Network), "provenance round-trips")
	assert.Equal(t, "#dave", ptrValue(healed.Channel))
	assert.Equal(t, "knivey", ptrValue(healed.Nick))
	assert.Equal(t, "qwenHD", ptrValue(healed.WorkflowName))
	assert.Equal(t, "a shrew on main street", healed.OriginalPrompt, "note-node prompt preserved")
	require.NotNil(t, healed.Width)
	assert.Equal(t, 1920, *healed.Width)
	assert.False(t, healed.Hidden, "visibility untouched")

	skipped, err := dbGetImageByID(app.db, "broken01")
	require.NoError(t, err)
	assert.Equal(t, "keep me", skipped.OriginalPrompt, "unparseable row keeps stored metadata")

	// FTS stayed in sync through the prompt rewrite: the healed prompt
	// matches, the stale one doesn't.
	sresp, err := http.Get(ts.URL + "/search-fragment?q=shrew")
	require.NoError(t, err)
	sbody, err := io.ReadAll(sresp.Body)
	sresp.Body.Close()
	require.NoError(t, err)
	assert.Contains(t, string(sbody), "gguf0001", "healed prompt must be searchable")
	sresp, err = http.Get(ts.URL + "/search-fragment?q=wrong")
	require.NoError(t, err)
	sbody, err = io.ReadAll(sresp.Body)
	sresp.Body.Close()
	require.NoError(t, err)
	assert.NotContains(t, string(sbody), "gguf0001", "stale pre-heal tokens must be gone from the index")
}

func ptrStr(s string) *string { return &s }
