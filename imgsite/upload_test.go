package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUploadAuth(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"NoKey", ""},
		{"WrongKey", "wrong-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t, testConfig())
			ts := newTestServer(t, app)

			resp := doUpload(t, ts, tt.key, uploadParts{hasFile: true, filename: "test.png", data: pngBytes("x")})

			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
}

func TestUploadHappyPath(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	data := pngBytes("hello world")

	resp := doUpload(t, ts, testAPIKey, uploadParts{
		hasFile:  true,
		filename: "2026-09-23-234552__0.webp",
		data:     data,
		meta:     `{"job_id":"ed974b6d","original_prompt":"shrew walkin down main street"}`,
	})

	require.Equal(t, http.StatusCreated, resp.StatusCode)

	ur := decodeUploadResponse(t, resp)
	assert.Regexp(t, `^[0-9A-Za-z]{7}$`, ur.ID)
	assert.Equal(t, "2026-09-23-234552__0.webp", ur.Filename, "filename echoed back")
	assert.Equal(t, ts.URL+"/"+ur.ID, ur.Page, "page is absolute, derived from request")
	assert.Equal(t, ts.URL+"/"+ur.ID+"/orig/2026-09-23-234552__0.webp", ur.URL, "url is the absolute direct link")

	// Row landed with the right hash, sniffed MIME, and upload provenance.
	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err, "row must exist for the returned id")
	sum := sha256.Sum256(data)
	assert.Equal(t, hex.EncodeToString(sum[:]), img.SHA256)
	assert.Equal(t, "image/png", img.MimeType, "MIME comes from magic bytes, not the .webp name")
	assert.Equal(t, int64(len(data)), img.SizeBytes)
	assert.Equal(t, "2026-09-23-234552__0.webp", img.Filename)
	assert.Equal(t, thumbStatusPending, img.ThumbStatus)
	assert.Equal(t, metaSourceUpload, img.MetaSource)
	assert.False(t, img.Hidden)
	assert.Nil(t, img.Width, "width stays NULL until the thumbnails milestone")
	assert.Nil(t, img.Height)
	assert.Equal(t, "shrew walkin down main street", img.OriginalPrompt)
	assert.Equal(t, "ed974b6d", ptrValue(img.JobID))
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}$`, img.CreatedAt, "sortable UTC timestamp format (ms precision — seconds order randomly within a second)")

	// File landed at the content address.
	_, err = os.Stat(app.store.OriginalPath(img.SHA256))
	require.NoError(t, err, "stored original file exists")
}

func TestUploadBaseURLFromConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Server.BaseURL = "https://img.example.com/"
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "test.png", data: pngBytes("x")})

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)
	assert.Equal(t, "https://img.example.com/"+ur.ID, ur.Page, "trailing slash trimmed from configured base_url")
	assert.Equal(t, "https://img.example.com/"+ur.ID+"/orig/test.png", ur.URL)
}

// mergeFixtureWebP embeds a production-shaped graph (note node + sampler +
// ZeroOut negative + latent) in a synthetic webp.
func mergeFixtureWebP(t *testing.T) []byte {
	t.Helper()
	graph := `{"56":{"class_type":"CLIPTextEncode","inputs":{"text":"exif enhanced"}}` +
		`,"57":{"class_type":"EmptyLatentImage","inputs":{"width":64,"height":32}}` +
		`,"58":{"class_type":"KSampler","inputs":{"seed":123,"steps":4,"cfg":1.5,"denoise":1,"sampler_name":"euler","scheduler":"simple","positive":["56",0],"negative":["57",0]}}` +
		`,"63":{"class_type":"ConditioningZeroOut","inputs":{"conditioning":["56",0]}}` +
		`,"dave_original_prompt":{"class_type":"CLIPTextEncode","inputs":{"text":"{\"prompt\":\"exif orig\",\"llm_generated\":true,\"job_id\":\"j-exif\"}"}}}`
	return buildWebP(buildTIFF("prompt:"+graph+"\x00", false), true)
}

func TestUploadMergePolicy(t *testing.T) {
	exifData := mergeFixtureWebP(t)

	// Graph embedded in the fixture (asserted once here so the table below
	// can be trusted).
	_, _, found := extractEmbeddedWorkflows(exifData)
	require.True(t, found, "fixture setup: embedded workflow must be found")

	tests := []struct {
		name       string
		data       []byte
		meta       string
		wantOrig   string
		wantEnh    string
		wantJob    string
		wantLLM    bool
		wantWidth  *int
		wantSource string
	}{
		{
			name:       "ExifOnly",
			data:       exifData,
			wantOrig:   "exif orig",
			wantEnh:    "exif enhanced",
			wantJob:    "j-exif",
			wantLLM:    true,
			wantWidth:  intPtr(64),
			wantSource: "exif",
		},
		{
			name:       "ExifOnlyPlusProvenanceMeta",
			data:       exifData,
			meta:       `{"network":"libera","channel":"#dave","nick":"knivey","workflow_name":"zimage"}`,
			wantOrig:   "exif orig",
			wantEnh:    "exif enhanced",
			wantJob:    "j-exif",
			wantLLM:    true,
			wantWidth:  intPtr(64),
			wantSource: "upload+exif",
		},
		{
			name:       "MetaOnlyNoExif",
			data:       pngBytes("no embedded metadata"),
			meta:       `{"job_id":"j-meta","original_prompt":"meta orig","enhanced_prompt":"meta enhanced","llm_generated":true}`,
			wantOrig:   "meta orig",
			wantEnh:    "meta enhanced",
			wantJob:    "j-meta",
			wantLLM:    true,
			wantWidth:  nil,
			wantSource: "upload",
		},
		{
			name:       "BothConflictingExifWins",
			data:       exifData,
			meta:       `{"job_id":"j-meta","original_prompt":"meta orig","llm_generated":false}`,
			wantOrig:   "exif orig",
			wantEnh:    "exif enhanced",
			wantJob:    "j-exif",
			wantLLM:    true,
			wantWidth:  intPtr(64),
			wantSource: "upload+exif",
		},
		{
			name:       "Neither",
			data:       pngBytes("no embedded metadata"),
			wantOrig:   "",
			wantEnh:    "",
			wantJob:    "",
			wantLLM:    false,
			wantWidth:  nil,
			wantSource: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t, testConfig())
			ts := newTestServer(t, app)

			resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "x.png", data: tt.data, meta: tt.meta})
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			ur := decodeUploadResponse(t, resp)

			img, err := dbGetImageByID(app.db, ur.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantOrig, img.OriginalPrompt)
			assert.Equal(t, tt.wantEnh, img.EnhancedPrompt)
			assert.Equal(t, tt.wantJob, ptrValue(img.JobID))
			assert.Equal(t, tt.wantLLM, img.LLMGenerated)
			assert.Equal(t, tt.wantSource, img.MetaSource)
			if tt.wantWidth == nil {
				assert.Nil(t, img.Width)
			} else {
				require.NotNil(t, img.Width)
				assert.Equal(t, *tt.wantWidth, *img.Width)
			}
			if tt.wantSource == "exif" || tt.wantSource == "upload+exif" {
				require.NotNil(t, img.Seed)
				assert.EqualValues(t, 123, *img.Seed)
				assert.NotEmpty(t, img.WorkflowJSON, "graph-derived workflow JSON stored on EXIF merges")
			} else {
				assert.Nil(t, img.Seed)
				assert.Empty(t, img.WorkflowJSON)
			}

			// Provenance always comes from meta regardless of side.
			if tt.name == "ExifOnlyPlusProvenanceMeta" {
				assert.Equal(t, "libera", ptrValue(img.Network))
				assert.Equal(t, "#dave", ptrValue(img.Channel))
				assert.Equal(t, "knivey", ptrValue(img.Nick))
				assert.Equal(t, "zimage", ptrValue(img.WorkflowName))
			}
		})
	}
}

func TestUploadUnparseableEmbeddedWorkflowFallsBackToMeta(t *testing.T) {
	// webp with a prompt: payload that is not valid JSON.
	badPayload := "{definitely not json"
	webp := buildWebP(buildTIFF("prompt:"+badPayload+"\x00", false), true)
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{
		hasFile: true, filename: "x.png", data: webp,
		meta: `{"job_id":"j-meta","original_prompt":"meta orig"}`,
	})

	require.Equal(t, http.StatusCreated, resp.StatusCode, "EXIF parse failure must never fail the upload")
	ur := decodeUploadResponse(t, resp)

	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, "meta orig", img.OriginalPrompt)
	assert.Equal(t, "j-meta", ptrValue(img.JobID))
	assert.Equal(t, metaSourceUpload, img.MetaSource)
	assert.Nil(t, img.Seed, "graph-derived fields stay blank without usable EXIF")
}

func TestUploadExtractionPopulatesRow(t *testing.T) {
	// End-to-end: uploading the real production fixture populates the full
	// row straight off the EXIF.
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	data := loadFixture(t, "enhanced.webp")
	meta := `{"network":"libera","channel":"#dave","nick":"knivey","job_id":"ed974b6d","original_prompt":"shrew walkin down main street","llm_generated":false}`

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "shrew.webp", data: data, meta: meta})

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, "shrew walkin down main street", img.OriginalPrompt)
	assert.Equal(t, "ed974b6d", ptrValue(img.JobID))
	assert.Len(t, img.Reasoning, 677)
	assert.Equal(t, fixtureEnhanced528, img.EnhancedPrompt)
	assert.Empty(t, img.NegativePrompt)
	require.NotNil(t, img.Seed)
	assert.EqualValues(t, 5702895218061442231, *img.Seed)
	require.NotNil(t, img.Width)
	assert.Equal(t, 1920, *img.Width)
	require.NotNil(t, img.Height)
	assert.Equal(t, 1080, *img.Height)
	assert.Equal(t, "krea2_turbo_int8_convrot.safetensors", ptrValue(img.ModelUnet))
	assert.Equal(t, "libera", ptrValue(img.Network), "provenance from meta")
	assert.Equal(t, metaSourceUploadAndEXIF, img.MetaSource)
	assert.Contains(t, img.WorkflowJSON, "5702895218061442231")
}

func TestUploadFilenameEscapedInURL(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "weird name?.png", data: pngBytes("x")})

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)
	assert.Equal(t, ts.URL+"/"+ur.ID+"/orig/weird%20name%3F.png", ur.URL, "filename is URL-escaped in the direct link")
}

func TestUploadTooLargeContentLength(t *testing.T) {
	cfg := testConfig()
	cfg.Upload.MaxBytes = 1024
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "big.png", data: pngBytes(strings.Repeat("x", 4096))})

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestUploadTooLargeChunkedBody(t *testing.T) {
	cfg := testConfig()
	cfg.Upload.MaxBytes = 1024
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)

	// Wrap the body so the client cannot report Content-Length; the
	// MaxBytesReader guard must catch it during parsing.
	body, contentType := multipartBody(t, uploadParts{hasFile: true, filename: "big.png", data: pngBytes(strings.Repeat("x", 4096))})
	raw, err := io.ReadAll(body)
	require.NoError(t, err)
	req, err := http.NewRequest("POST", ts.URL+"/updo", struct{ io.Reader }{bytes.NewReader(raw)})
	require.NoError(t, err)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-API-Key", testAPIKey)

	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestUploadRateLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Upload.RatePerMinute = 1 // burst of 1
	app := newTestApp(t, cfg)
	ts := newTestServer(t, app)

	first := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "a.png", data: pngBytes("a")})
	second := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "b.png", data: pngBytes("b")})

	assert.Equal(t, http.StatusCreated, first.StatusCode)
	assert.Equal(t, http.StatusTooManyRequests, second.StatusCode)
	assert.NotEmpty(t, second.Header.Get("Retry-After"))
}

func TestUploadEmptyFileRejected(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "empty.png", data: []byte{}})

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadNoBodyRejected(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	req, err := http.NewRequest("POST", ts.URL+"/updo", nil)
	require.NoError(t, err)
	req.Header.Set("X-API-Key", testAPIKey)

	resp := doReq(t, ts, req)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadMissingFileFieldRejected(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{meta: `{"job_id":"j"}`})

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadBadMetaJSONRejected(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "x.png", data: pngBytes("x"), meta: "{not json"})

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadFilenameSanitization(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"StripsPathComponents", "sub/dir/test-image.png", "test-image.png"},
		{"DotDotFallsBack", "..", "image.png"},
		{"DotFallsBack", ".", "image.png"},
		{"WindowsPathFallsBack", `C:\Users\img.png`, "image.png"},
		{"NonASCIIFallsBack", "café.png", "image.png"},
		{"PlainNameKept", "2026-09-23-234552__0.webp", "2026-09-23-234552__0.webp"},
		{"SpacesAndQuerySurvive", "weird name?.png", "weird name?.png"},
	}
	// NOTE: the empty-filename and raw-control-character rules exist in
	// sanitizeUploadFilename (see TestSanitizeUploadFilenameMatchesImgMCP)
	// but cannot be produced by Go's own multipart writer/reader pair —
	// an empty filename makes the parser treat the part as a plain value,
	// and control characters get percent-encoded on the wire — so they
	// are not exercised end-to-end here.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newTestApp(t, testConfig())
			ts := newTestServer(t, app)

			resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: tt.filename, data: pngBytes("x")})

			require.Equal(t, http.StatusCreated, resp.StatusCode)
			ur := decodeUploadResponse(t, resp)
			assert.Equal(t, tt.want, ur.Filename, "response echoes the sanitized filename")

			img, err := dbGetImageByID(app.db, ur.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.want, img.Filename, "row stores the sanitized filename")
		})
	}
}

func TestUploadMetaLandsInDB(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	meta := `{
		"job_id": "ed974b6d",
		"original_prompt": "shrew walkin down main street",
		"enhanced_prompt": "A cute anthropomorphic brown shrew",
		"negative_prompt": "blurry",
		"reasoning": "The user described a shrew.",
		"llm_generated": true,
		"workflow_name": "zimage-turbo",
		"network": "libera",
		"channel": "#dave",
		"nick": "knivey"
	}`

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "x.png", data: pngBytes("x"), meta: meta})

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, "ed974b6d", ptrValue(img.JobID))
	assert.Equal(t, "shrew walkin down main street", img.OriginalPrompt)
	assert.Equal(t, "A cute anthropomorphic brown shrew", img.EnhancedPrompt)
	assert.Equal(t, "blurry", img.NegativePrompt)
	assert.Equal(t, "The user described a shrew.", img.Reasoning)
	assert.True(t, img.LLMGenerated)
	assert.Equal(t, "zimage-turbo", ptrValue(img.WorkflowName))
	assert.Equal(t, "libera", ptrValue(img.Network))
	assert.Equal(t, "#dave", ptrValue(img.Channel))
	assert.Equal(t, "knivey", ptrValue(img.Nick))
}

func TestUploadMetaUnknownFieldsIgnored(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	resp := doUpload(t, ts, testAPIKey, uploadParts{
		hasFile: true, filename: "x.png", data: pngBytes("x"),
		meta: `{"original_prompt":"a cat","future_field":{"nested":true},"another":"x"}`,
	})

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, "a cat", img.OriginalPrompt)
}

func TestUploadDedupeSharesFileNotRow(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	data := pngBytes("identical bytes")

	first := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "one.png", data: data})
	second := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "two.png", data: data})

	require.Equal(t, http.StatusCreated, first.StatusCode)
	require.Equal(t, http.StatusCreated, second.StatusCode)
	ur1 := decodeUploadResponse(t, first)
	ur2 := decodeUploadResponse(t, second)

	assert.NotEqual(t, ur1.ID, ur2.ID, "each upload is its own gallery entry")

	img1, err := dbGetImageByID(app.db, ur1.ID)
	require.NoError(t, err)
	img2, err := dbGetImageByID(app.db, ur2.ID)
	require.NoError(t, err)
	assert.Equal(t, img1.SHA256, img2.SHA256, "same bytes, same hash")

	// Exactly one stored file serves both rows.
	path := app.store.OriginalPath(img1.SHA256)
	_, err = os.Stat(path)
	require.NoError(t, err)
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "content-addressed store writes duplicate content only once")
}

func TestGenerateImageIDFormat(t *testing.T) {
	exists := func(string) (bool, error) { return false, nil }

	for i := 0; i < 500; i++ {
		id, err := generateImageID(exists)
		require.NoError(t, err)
		require.Regexp(t, `^[0-9A-Za-z]{7}$`, id)
	}
}

func TestGenerateImageIDCollisionRetry(t *testing.T) {
	calls := 0
	exists := func(string) (bool, error) {
		calls++
		return calls <= 2, nil // first two candidates are taken
	}

	id, err := generateImageID(exists)

	require.NoError(t, err)
	assert.Regexp(t, `^[0-9A-Za-z]{7}$`, id)
	assert.Equal(t, 3, calls, "two collisions then success")
}

func TestGenerateImageIDExhausted(t *testing.T) {
	calls := 0
	exists := func(string) (bool, error) {
		calls++
		return true, nil
	}

	id, err := generateImageID(exists)

	require.Error(t, err)
	assert.Empty(t, id)
	assert.Equal(t, imageIDAttempts, calls, "bounded retry: exactly maxIDAttempts tries")
}

func TestUploadNonImageStoredAsOctetStream(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	htmlish := []byte("<html><body><script>alert(1)</script></body></html>")

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "evil.html", data: htmlish})

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	assert.Equal(t, "application/octet-stream", img.MimeType, "non-image sniff must never be stored as text/html")

	// The orig route serves the hardened type (plus the global nosniff).
	req, err := http.NewRequest("GET", ur.URL, nil)
	require.NoError(t, err)
	ores := doReq(t, ts, req)
	require.Equal(t, http.StatusOK, ores.StatusCode)
	assert.Equal(t, "application/octet-stream", ores.Header.Get("Content-Type"))
	assert.Equal(t, "nosniff", ores.Header.Get("X-Content-Type-Options"))
	body, err := io.ReadAll(ores.Body)
	require.NoError(t, err)
	assert.Equal(t, htmlish, body, "bytes still served faithfully")
}

func TestIsAllowedImageMIME(t *testing.T) {
	for _, mt := range []string{"image/webp", "image/png", "image/jpeg"} {
		assert.True(t, isAllowedImageMIME(mt), mt)
	}
	// gif/bmp are decodable-sniff image types the thumbnailer cannot
	// decode — they must NOT pass the allowlist.
	for _, mt := range []string{"text/html; charset=utf-8", "text/plain; charset=utf-8", "application/pdf", "application/zip", "image/svg+xml", "image/gif", "image/bmp", ""} {
		assert.False(t, isAllowedImageMIME(mt), mt)
	}
}

// TestUploadEscapedFilenameRoundTrip pins the URL escaping contract: the
// response's url field must be GETtable verbatim for any
// sanitizer-legal filename (space, %, ?, & all survive the round trip).
func TestUploadEscapedFilenameRoundTrip(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	data := pngBytes("roundtrip")
	filename := "spaced & 100% done?.png"

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: filename, data: data})

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)
	assert.Equal(t, filename, ur.Filename)
	assert.Contains(t, ur.URL, "%25", "the literal % in the filename must be escaped in the URL")

	// GET the url field exactly as handed to the client.
	req, err := http.NewRequest("GET", ur.URL, nil)
	require.NoError(t, err)
	ores := doReq(t, ts, req)

	require.Equal(t, http.StatusOK, ores.StatusCode)
	body, err := io.ReadAll(ores.Body)
	require.NoError(t, err)
	assert.Equal(t, data, body, "orig route serves byte-identical content")
}

func TestDeriveBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(r *http.Request)
		want    string
	}{
		{
			name:    "PlainHTTP",
			prepare: func(r *http.Request) { r.Host = "img.example.com" },
			want:    "http://img.example.com",
		},
		{
			name: "ForwardedProtoHTTPS",
			prepare: func(r *http.Request) {
				r.Host = "img.example.com"
				r.Header.Set("X-Forwarded-Proto", "https")
			},
			want: "https://img.example.com",
		},
		{
			name: "ForwardedProtoListUsesFirst",
			prepare: func(r *http.Request) {
				r.Host = "img.example.com"
				r.Header.Set("X-Forwarded-Proto", "https, http")
			},
			want: "https://img.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", "/updo", nil)
			require.NoError(t, err)
			tt.prepare(req)

			assert.Equal(t, tt.want, deriveBaseURL(req))
		})
	}
}

func TestCheckAPIKey(t *testing.T) {
	assert.True(t, checkAPIKey("secret", "secret"))
	assert.False(t, checkAPIKey("secret", "wrong"))
	assert.False(t, checkAPIKey("secret", ""))
	assert.False(t, checkAPIKey("", ""), "no configured key denies everything")
}

func TestRateLimiterSwap(t *testing.T) {
	rl := newRateLimiter(1)

	assert.True(t, rl.allow(), "bucket starts full (burst = 1)")
	assert.False(t, rl.allow())

	rl.setPerMinute(1000)
	for i := 0; i < 10; i++ {
		assert.True(t, rl.allow(), "swapped bucket refills at the new rate")
	}
}

func TestSanitizeUploadFilenameMatchesImgMCP(t *testing.T) {
	// Same rule set as mcps/img-mcp's sanitizeUploadFilename; keep in sync.
	tests := map[string]string{
		"sub/dir/test-image.png": "test-image.png",
		"":                       "image.png",
		"..":                     "image.png",
		"bad\r\n.png":            "image.png",
		`C:\Users\img.png`:       "image.png",
		"café.png":               "image.png",
		"plain-name_1.webp":      "plain-name_1.webp",
	}
	for in, want := range tests {
		assert.Equal(t, want, sanitizeUploadFilename(in), "input %q", in)
	}
}
