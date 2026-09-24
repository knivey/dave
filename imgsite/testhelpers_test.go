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
