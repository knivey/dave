package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	initLogger(os.TempDir())
	os.Exit(m.Run())
}

const testAPIKey = "test-api-key"

func setupTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := initDB(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err, "initDB")
	t.Cleanup(func() { db.Close() })
	return db
}

func testConfig() Config {
	return Config{
		Server:     ServerConfig{Name: "imgsite", Addr: ":0", BaseURL: ""},
		Database:   DatabaseConfig{Path: "data/imgsite.db"},
		Storage:    StorageConfig{Path: "data/images", ThumbsPath: "data/thumbs"},
		Auth:       AuthConfig{APIKey: testAPIKey},
		Thumbnails: ThumbnailsConfig{SmallWidth: 480, DisplayWidth: 1280, JPEGQuality: 78, Workers: 2, MaxDimension: 8192},
		Upload:     UploadConfig{MaxBytes: 32 << 20, RatePerMinute: 1000},
		Search:     SearchConfig{SnippetChars: 160, PrefixMin: 2},
		Site:       SiteConfig{Title: "test site", Description: "test descriptions"},
	}
}

// newTestApp builds an App whose DB and store live under temp dirs.
func newTestApp(t *testing.T, cfg Config) *App {
	t.Helper()
	dir := t.TempDir()
	cfg.Storage.ResolvedPath = filepath.Join(dir, "images")
	cfg.Storage.ResolvedThumbsPath = filepath.Join(dir, "thumbs")
	cfg.Database.Resolved = filepath.Join(dir, "imgsite.db")
	db := setupTestDB(t)
	return NewApp(cfg, db, filepath.Join(dir, "config.toml"))
}

func newTestServer(t *testing.T, app *App) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(app.buildHandler())
	t.Cleanup(ts.Close)
	return ts
}

// safeSiteTestConfig is testConfig with the safe site configured: host
// safe.example.com (the getPageHost override target), allowed network
// libera. The safe base_url (https://safe.example.com) is what
// upload-response link building switches to for allowed-network
// uploads; query surfaces ignore it (they select by request Host).
func safeSiteTestConfig() Config {
	cfg := testConfig()
	cfg.SafeSite = &SafeSiteConfig{
		Hosts:           []string{"safe.example.com"},
		BaseURL:         "https://safe.example.com",
		AllowedNetworks: []string{"libera"},
	}
	return cfg
}

// getPageHost is getPage with the HTTP Host header overridden — the
// input resolveSite selects the logical site from. httptest servers
// listen on 127.0.0.1; overriding req.Host (not the URL) sends the
// request to the same listener with the chosen Host header, which is
// exactly how the production host split arrives (one process, Host
// routing at the proxy).
func getPageHost(t *testing.T, ts *httptest.Server, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+path, nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// seedSiteMatrix seeds the six-row site-visibility matrix from the
// safe-site plan (task-3 brief). Every row shares the search token
// "gribble" so /search?q=gribble hits them all; placement per row:
//
//	sit0001 libera network, unknown safety — safe-visible by ORIGIN;
//	         tier 1 (original-prompt token match)
//	sit0002 efnet network, unknown safety — safe-INVISIBLE;
//	         tier 1
//	sit0003 NULL network, unknown safety — safe-INVISIBLE (default-deny
//	         for rows without provenance); tier 2 (enhanced-only FTS)
//	sit0004 NULL network, safety='safe' — safe-visible by VERDICT;
//	         tier 2
//	sit0005 efnet network, safety='safe' — verdict outranks the
//	         disallowed origin; tier 3 (substring-only "cowgribble",
//	         invisible to FTS prefix terms → trigram-accelerated scan)
//	sit0006 efnet network, hidden — invisible on BOTH sites; on the
//	         disallowed origin so the 410-first pins (details page,
//	         asset routes) discriminate check order: a site-check-
//	         first bug would 404 it on the safe host, not 410
//
// Resulting visibility: safe host sees {sit0001, sit0004, sit0005};
// default host sees all five non-hidden rows.
func seedSiteMatrix(t *testing.T, app *App) {
	t.Helper()
	insertImage(t, app, "sit0001", "2026-09-26 01:00:00", func(img *dbImage) {
		// Explicit libera, not the insertImage default: this row's
		// whole role is "safe-visible by ORIGIN", so the fixture must
		// not lean on an implicit helper value that a future edit to
		// insertImage could silently change.
		img.Network = ptrStr("libera")
		img.OriginalPrompt = "gribble parade one"
	})
	insertImage(t, app, "sit0002", "2026-09-26 02:00:00", func(img *dbImage) {
		img.Network = ptrStr("efnet")
		img.OriginalPrompt = "gribble parade two"
	})
	insertImage(t, app, "sit0003", "2026-09-26 03:00:00", func(img *dbImage) {
		img.Network = nil
		img.OriginalPrompt = "plain three"
		img.EnhancedPrompt = "gribble enhanced three"
	})
	insertImage(t, app, "sit0004", "2026-09-26 04:00:00", func(img *dbImage) {
		img.Network = nil
		img.Safety = safetySafe
		img.OriginalPrompt = "plain four"
		img.EnhancedPrompt = "gribble enhanced four"
	})
	insertImage(t, app, "sit0005", "2026-09-26 05:00:00", func(img *dbImage) {
		img.Network = ptrStr("efnet")
		img.Safety = safetySafe
		img.OriginalPrompt = "a cowgribble five"
	})
	insertImage(t, app, "sit0006", "2026-09-26 06:00:00", func(img *dbImage) {
		img.Hidden = true
		// efnet, not the helper's default libera: hidden AND
		// disallowed, so HiddenIs410FirstEverywhere (pages) actually
		// discriminates — under a site-check-first order this row
		// would 404 on the safe host instead of 410.
		img.Network = ptrStr("efnet")
		img.OriginalPrompt = "gribble hidden six"
	})
}

// pngBytes sniffs as image/png via magic bytes.
func pngBytes(payload string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), []byte(payload)...)
}

// uploadParts describes a multipart /updo request body. hasFile controls
// whether a file part exists at all (an empty filename or empty data is
// still a present part when hasFile is true).
type uploadParts struct {
	hasFile  bool
	filename string
	data     []byte
	meta     string
}

func multipartBody(t *testing.T, p uploadParts) (io.Reader, string) {
	t.Helper()
	body := &bytes.Buffer{}
	wr := multipart.NewWriter(body)
	if p.hasFile {
		fw, err := wr.CreateFormFile("file", p.filename)
		require.NoError(t, err)
		_, err = fw.Write(p.data)
		require.NoError(t, err)
	}
	if p.meta != "" {
		require.NoError(t, wr.WriteField("meta", p.meta))
	}
	require.NoError(t, wr.Close())
	return body, wr.FormDataContentType()
}

func doUpload(t *testing.T, ts *httptest.Server, key string, p uploadParts) *http.Response {
	t.Helper()
	body, contentType := multipartBody(t, p)
	req, err := http.NewRequest("POST", ts.URL+"/updo", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	return doReq(t, ts, req)
}

func doReq(t *testing.T, ts *httptest.Server, req *http.Request) *http.Response {
	t.Helper()
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeUploadResponse(t *testing.T, resp *http.Response) uploadResponse {
	t.Helper()
	var ur uploadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&ur), "decoding upload response JSON")
	return ur
}
