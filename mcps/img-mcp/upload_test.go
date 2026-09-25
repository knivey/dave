package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUploadServer mimics the imgsite upload wire protocol:
//
//	POST /updo (X-API-Key, multipart file+meta) -> 201 + {id,url,page,filename}
//
// Behavior is configurable through the fields so error paths can be
// exercised; every request is recorded for assertions.
type fakeUploadServer struct {
	updoStatus int
	// respondBody overrides the default 201 JSON body (ignored when empty).
	respondBody string

	gotAPIKey       string
	gotMetaRaw      string
	gotFileField    string
	gotFileFilename string
	gotFileContent  []byte

	server *httptest.Server
}

func newFakeUploadServer(t *testing.T) *fakeUploadServer {
	t.Helper()
	f := &fakeUploadServer{
		updoStatus: http.StatusCreated,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/updo", func(w http.ResponseWriter, r *http.Request) {
		f.gotAPIKey = r.Header.Get("X-API-Key")
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.gotMetaRaw = r.FormValue("meta")
		file, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		f.gotFileField = "file"
		f.gotFileFilename = hdr.Filename
		f.gotFileContent, _ = io.ReadAll(file)

		if f.updoStatus != http.StatusCreated {
			w.WriteHeader(f.updoStatus)
			if f.respondBody != "" {
				io.WriteString(w, f.respondBody)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if f.respondBody != "" {
			io.WriteString(w, f.respondBody)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"id":       "aQ3f9xK",
			"url":      f.server.URL + "/aQ3f9xK/orig/test-image.png",
			"page":     f.server.URL + "/aQ3f9xK",
			"filename": "test-image.png",
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// TestUploadImageReturnsPageVerbatim pins the link contract: the PAGE link
// (gallery details page) is what dave pastes to IRC, so when the response
// carries both fields the page wins — returned verbatim, no derivation, no
// re-encoding.
func TestUploadImageReturnsPageVerbatim(t *testing.T) {
	f := newFakeUploadServer(t)
	cfg := Config{Upload: UploadConfig{URL: f.server.URL, APIKey: "secret-key"}}

	// A deliberately odd page URL (different host, query-ish suffix)
	// proves the value is returned verbatim; the differing url proves the
	// page is preferred over it.
	page := "https://img.example.com/aQ3f9xK?from=test"
	direct := "https://img.example.com/aQ3f9xK/orig/2026-09-23-234552__0.webp"
	f.respondBody = `{"id":"aQ3f9xK","url":"` + direct + `","page":"` + page + `","filename":"2026-09-23-234552__0.webp"}`

	got, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png", UploadMeta{})

	require.NoError(t, err)
	assert.Equal(t, page, got, "page link must be preferred over the direct url")
}

// TestUploadImageFallsBackToURLWhenPageEmpty covers older imgsite
// deployments that might answer with an empty page field: the direct url is
// still usable, so the upload falls back to it (with a WARN in the logs)
// instead of failing the job.
func TestUploadImageFallsBackToURLWhenPageEmpty(t *testing.T) {
	f := newFakeUploadServer(t)
	cfg := Config{Upload: UploadConfig{URL: f.server.URL, APIKey: "secret-key"}}

	direct := f.server.URL + "/aQ3f9xK/orig/test-image.png"
	f.respondBody = `{"id":"aQ3f9xK","url":"` + direct + `","page":"","filename":"test-image.png"}`

	got, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png", UploadMeta{})

	require.NoError(t, err)
	assert.Equal(t, direct, got, "empty page must fall back to the direct url")
}

// TestUploadImageURLNotRequiredWhenPagePresent: once the page link is the
// primary contract, a response that omits url entirely (page only) is a
// success, not an error.
func TestUploadImageURLNotRequiredWhenPagePresent(t *testing.T) {
	f := newFakeUploadServer(t)
	cfg := Config{Upload: UploadConfig{URL: f.server.URL, APIKey: "secret-key"}}

	page := "https://img.example.com/aQ3f9xK"
	f.respondBody = `{"id":"aQ3f9xK","page":"` + page + `","filename":"f.png"}`

	got, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png", UploadMeta{})

	require.NoError(t, err)
	assert.Equal(t, page, got)
}

func TestUploadImageSendsAPIKeyFileAndMeta(t *testing.T) {
	f := newFakeUploadServer(t)
	cfg := Config{Upload: UploadConfig{URL: f.server.URL, APIKey: "secret-key"}}
	meta := UploadMeta{
		JobID:          "ed974b6d",
		OriginalPrompt: "a cat",
		Network:        "libera",
		Channel:        "#dave",
		Nick:           "knivey",
	}

	url, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png", meta)

	require.NoError(t, err)
	assert.Equal(t, f.server.URL+"/aQ3f9xK", url, "the page link is the upload result")
	assert.Equal(t, "secret-key", f.gotAPIKey, "upload must authenticate with X-API-Key")
	assert.Equal(t, "file", f.gotFileField)
	assert.Equal(t, "test-image.png", f.gotFileFilename)
	assert.Equal(t, []byte("png-bytes"), f.gotFileContent)

	var gotMeta UploadMeta
	require.NoError(t, json.Unmarshal([]byte(f.gotMetaRaw), &gotMeta), "meta form value must be JSON: %q", f.gotMetaRaw)
	assert.Equal(t, meta, gotMeta, "meta must arrive intact")
}

func TestUploadImageFilenameSanitized(t *testing.T) {
	t.Run("StripsPathComponents", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		_, err := uploadImage(cfg, []byte("png-bytes"), "sub/dir/test-image.png", UploadMeta{})

		require.NoError(t, err)
		assert.Equal(t, "test-image.png", f.gotFileFilename)
	})
	t.Run("EmptyDefaultsToImagePng", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		_, err := uploadImage(cfg, []byte("png-bytes"), "", UploadMeta{})

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename)
	})
	t.Run("DotDotFallsBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		_, err := uploadImage(cfg, []byte("png-bytes"), "..", UploadMeta{})

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "'..' must never reach the site's /orig/ URL path")
	})
	t.Run("ControlCharsFallBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		_, err := uploadImage(cfg, []byte("png-bytes"), "bad\r\n.png", UploadMeta{})

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "control characters would corrupt the multipart part header")
	})
	t.Run("WindowsPathFallsBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		_, err := uploadImage(cfg, []byte("png-bytes"), `C:\Users\img.png`, UploadMeta{})

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "filepath.Base does not split Windows separators on Linux")
	})
	t.Run("NonASCIIFallsBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		_, err := uploadImage(cfg, []byte("png-bytes"), "café.png", UploadMeta{})

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "only printable ASCII is safe to embed in the returned URL")
	})
}

func TestUploadImageErrors(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		respondBody string
		errMsg      string
	}{
		{
			name:   "UnauthorizedMentionsAPIKey",
			status: http.StatusUnauthorized,
			errMsg: "upload.api_key",
		},
		{
			name:   "Forbidden",
			status: http.StatusForbidden,
			errMsg: "unexpected status from upload: 403",
		},
		{
			name:   "ServerError",
			status: http.StatusInternalServerError,
			errMsg: "unexpected status from upload: 500",
		},
		{
			name:        "TooManyRequestsIncludesBodySnippet",
			status:      http.StatusTooManyRequests,
			respondBody: "rate limit exceeded",
			errMsg:      "unexpected status from upload: 429: rate limit exceeded",
		},
		{
			name:        "CreatedButNotJSON",
			status:      http.StatusCreated,
			respondBody: "not-json",
			errMsg:      "parsing upload response",
		},
		{
			name:        "CreatedButMissingPageAndURL",
			status:      http.StatusCreated,
			respondBody: `{"id":"aQ3f9xK","filename":"f.png"}`,
			errMsg:      "upload response missing page and url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeUploadServer(t)
			f.updoStatus = tt.status
			f.respondBody = tt.respondBody
			cfg := Config{Upload: UploadConfig{URL: f.server.URL, APIKey: "secret-key"}}

			url, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png", UploadMeta{})

			require.Error(t, err)
			assert.Empty(t, url)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

// TestUploadMetaMarshaling pins the omitempty contract with imgsite: empty
// fields drop out of the JSON entirely (the site distinguishes "absent"
// from present-but-empty for llm_generated cross-checks), and a populated
// meta serializes every field with the documented key names.
func TestUploadMetaMarshaling(t *testing.T) {
	t.Run("ZeroValueIsEmptyObject", func(t *testing.T) {
		data, err := json.Marshal(UploadMeta{})
		require.NoError(t, err)
		assert.Equal(t, "{}", string(data))
	})

	t.Run("EmptyStringsAndFalseAreOmitted", func(t *testing.T) {
		meta := UploadMeta{JobID: "ed974b6d"} // everything else zero
		data, err := json.Marshal(meta)
		require.NoError(t, err)
		assert.JSONEq(t, `{"job_id":"ed974b6d"}`, string(data))
	})

	t.Run("AllFieldsPresent", func(t *testing.T) {
		meta := UploadMeta{
			JobID:          "ed974b6d",
			OriginalPrompt: "a cat",
			EnhancedPrompt: "a fluffy cat, cinematic",
			NegativePrompt: "blurry",
			Reasoning:      "the user asked for a cat",
			LLMGenerated:   true,
			WorkflowName:   "zimage",
			Network:        "libera",
			Channel:        "#dave",
			Nick:           "knivey",
		}
		data, err := json.Marshal(meta)
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"job_id": "ed974b6d",
			"original_prompt": "a cat",
			"enhanced_prompt": "a fluffy cat, cinematic",
			"negative_prompt": "blurry",
			"reasoning": "the user asked for a cat",
			"llm_generated": true,
			"workflow_name": "zimage",
			"network": "libera",
			"channel": "#dave",
			"nick": "knivey"
		}`, string(data))
	})
}

func TestElapsedMs(t *testing.T) {
	base := time.Date(2026, 9, 23, 5, 52, 4, 0, time.UTC)
	tests := []struct {
		name string
		from time.Time
		to   time.Time
		want int64
	}{
		{"FromNeverFired", time.Time{}, base.Add(1500 * time.Millisecond), 0},
		{"ToNeverFired", base, time.Time{}, 0},
		{"BothNeverFired", time.Time{}, time.Time{}, 0},
		{"InvertedPair", base.Add(time.Second), base, 0},
		{"PositiveDelta", base, base.Add(2844 * time.Millisecond), 2844},
		{"SubMillisecondTruncates", base, base.Add(900 * time.Microsecond), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, elapsedMs(tt.from, tt.to))
		})
	}
}

func TestSnippet(t *testing.T) {
	assert.Equal(t, "", snippet("", 10))
	assert.Equal(t, "short", snippet("short", 10))
	assert.Equal(t, "0123456789…", snippet("0123456789a", 10),
		"long bodies must clamp, not flood the job error string")
	assert.Equal(t, "0123456789", snippet("0123456789", 10), "exactly-max stays whole")

	// The cut must land on a rune boundary: slicing 12 é's at byte 10
	// would split the 6th é in half and produce invalid UTF-8, which then
	// gets logged and pasted to IRC. The limit counts runes now.
	t.Run("MultibyteRuneTruncatedWhole", func(t *testing.T) {
		got := snippet(strings.Repeat("é", 12), 10)
		assert.Equal(t, strings.Repeat("é", 10)+"…", got)
		assert.True(t, utf8.ValidString(got), "snippet must never emit half a rune")
	})
	t.Run("ExactlyMaxRunesStayWhole", func(t *testing.T) {
		s := strings.Repeat("é", 10) // 20 bytes > 10, but only 10 runes
		assert.Equal(t, s, snippet(s, 10), "byte length over the limit must not truncate a max-rune string")
	})
	t.Run("MixedWidthCutBacksOffToBoundary", func(t *testing.T) {
		// 12 runes ("é" + 11 ASCII chars), 13 bytes: a byte cut at 10 would
		// land cleanly here, but rune semantics still keep exactly 10 runes.
		got := snippet("é0123456789a", 10)
		assert.Equal(t, "é012345678…", got)
		assert.True(t, utf8.ValidString(got))
	})
}

func TestUploadImageServerDown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // nothing listens anymore

	cfg := Config{Upload: UploadConfig{URL: url}}

	got, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png", UploadMeta{})

	require.Error(t, err)
	assert.Empty(t, got)
}

// truncatedBodyTransport returns a 201 whose body dies mid-read, simulating
// a mid-body transport failure after the server already sent its headers.
type truncatedBodyTransport struct{ err error }

func (tt truncatedBodyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	// Enough bytes to look like the start of a plausible JSON body, then
	// the read blows up — io.ReadAll returns partial data plus the error.
	body := io.MultiReader(
		strings.NewReader(`{"id":"aQ3f9xK","url":"https://img.example.com/`),
		iotest.ErrReader(tt.err),
	)
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     http.Header{},
		Body:       io.NopCloser(body),
	}, nil
}

// TestUploadImageBodyReadError pins that a mid-body transport failure on an
// otherwise-201 response surfaces as "reading upload response", not as a
// confusing JSON-parse error on the truncated bytes.
func TestUploadImageBodyReadError(t *testing.T) {
	boomboom := errors.New("connection reset mid-body")
	orig := uploadHTTPClient
	uploadHTTPClient = &http.Client{Transport: truncatedBodyTransport{err: boomboom}}
	t.Cleanup(func() { uploadHTTPClient = orig })

	// The transport intercepts everything, so the URL never needs a listener.
	cfg := Config{Upload: UploadConfig{URL: "http://img.example.invalid", APIKey: "secret-key"}}

	got, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png", UploadMeta{})

	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "reading upload response")
	assert.Contains(t, err.Error(), "connection reset mid-body", "the transport error must be wrapped, not swallowed")
}
