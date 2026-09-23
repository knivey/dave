package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUploadServer mimics the img.zkpq.ca upload wire protocol:
//
//	POST /updo             -> 303 + Location: /<id>/<slug>
//	GET  /<id>/orig/<file> -> 307 + Location: /file/<hash>/<file>
//
// Behavior is configurable through the fields so error paths can be
// exercised; every request is recorded for assertions.
type fakeUploadServer struct {
	updoStatus   int
	updoLocation string
	origStatus   int
	origLocation string

	gotToirc        string
	gotFileField    string
	gotFileFilename string
	gotFileContent  []byte
	gotOrigPath     string

	server *httptest.Server
}

func newFakeUploadServer(t *testing.T) *fakeUploadServer {
	t.Helper()
	f := &fakeUploadServer{
		updoStatus:   http.StatusSeeOther,
		updoLocation: "/4F4/_test-image",
		origStatus:   http.StatusTemporaryRedirect,
		origLocation: "/file/abc123/test-image.png",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/updo", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.gotToirc = r.FormValue("toirc")
		file, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		f.gotFileField = "file"
		f.gotFileFilename = hdr.Filename
		f.gotFileContent, _ = io.ReadAll(file)
		w.Header().Set("Location", f.updoLocation)
		w.WriteHeader(f.updoStatus)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.gotOrigPath = r.URL.Path
		w.Header().Set("Location", f.origLocation)
		w.WriteHeader(f.origStatus)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func TestUploadImageReturnsOrigURL(t *testing.T) {
	f := newFakeUploadServer(t)
	cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

	url, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png")

	require.NoError(t, err)
	assert.Equal(t, f.server.URL+"/4F4/orig/test-image.png", url)
}

func TestUploadImageSendsToircDisabled(t *testing.T) {
	f := newFakeUploadServer(t)
	cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

	url, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png")

	require.NoError(t, err)
	assert.NotEmpty(t, url)
	assert.Equal(t, "0", f.gotToirc, "upload must explicitly disable the site's IRC announcement")
	assert.Equal(t, "file", f.gotFileField)
	assert.Equal(t, "test-image.png", f.gotFileFilename)
	assert.Equal(t, []byte("png-bytes"), f.gotFileContent)
}

func TestUploadImageFilenameSanitized(t *testing.T) {
	t.Run("StripsPathComponents", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		url, err := uploadImage(cfg, []byte("png-bytes"), "sub/dir/test-image.png")

		require.NoError(t, err)
		assert.Equal(t, "test-image.png", f.gotFileFilename)
		assert.Equal(t, f.server.URL+"/4F4/orig/test-image.png", url)
	})
	t.Run("EmptyDefaultsToImagePng", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		url, err := uploadImage(cfg, []byte("png-bytes"), "")

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename)
		assert.Equal(t, f.server.URL+"/4F4/orig/image.png", url)
	})
	t.Run("DotDotFallsBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		url, err := uploadImage(cfg, []byte("png-bytes"), "..")

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "'..' must never reach the derived /orig/ URL path")
		assert.Equal(t, f.server.URL+"/4F4/orig/image.png", url)
	})
	t.Run("ControlCharsFallBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		url, err := uploadImage(cfg, []byte("png-bytes"), "bad\r\n.png")

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "control characters would corrupt the multipart part header")
		assert.Equal(t, f.server.URL+"/4F4/orig/image.png", url)
	})
	t.Run("WindowsPathFallsBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		url, err := uploadImage(cfg, []byte("png-bytes"), `C:\Users\img.png`)

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "filepath.Base does not split Windows separators on Linux")
		assert.Equal(t, f.server.URL+"/4F4/orig/image.png", url)
	})
	t.Run("NonASCIIFallsBack", func(t *testing.T) {
		f := newFakeUploadServer(t)
		cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

		url, err := uploadImage(cfg, []byte("png-bytes"), "café.png")

		require.NoError(t, err)
		assert.Equal(t, "image.png", f.gotFileFilename, "only printable ASCII is safe to embed in the derived URL")
		assert.Equal(t, f.server.URL+"/4F4/orig/image.png", url)
	})
}

func TestUploadImageAbsoluteLocation(t *testing.T) {
	f := newFakeUploadServer(t)
	f.updoLocation = f.server.URL + "/4F5/_abs-image"
	cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

	url, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png")

	require.NoError(t, err)
	assert.Equal(t, f.server.URL+"/4F5/orig/test-image.png", url)
	assert.Equal(t, "/4F5/orig/test-image.png", f.gotOrigPath, "the validation GET must hit the derived orig path")
}

func TestUploadImageErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *fakeUploadServer)
		errMsg string
	}{
		{
			name:   "UpdoNotRedirect",
			mutate: func(f *fakeUploadServer) { f.updoStatus = http.StatusOK },
			errMsg: "unexpected status from upload",
		},
		{
			name:   "UpdoMissingLocation",
			mutate: func(f *fakeUploadServer) { f.updoLocation = "" },
			errMsg: "upload response missing Location",
		},
		{
			name:   "OrigNotFound",
			mutate: func(f *fakeUploadServer) { f.origStatus = http.StatusNotFound },
			errMsg: "unexpected status",
		},
		{
			name:   "OrigMissingLocation",
			mutate: func(f *fakeUploadServer) { f.origLocation = "" },
			errMsg: "missing Location",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeUploadServer(t)
			tt.mutate(f)
			cfg := Config{Upload: UploadConfig{URL: f.server.URL}}

			url, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png")

			require.Error(t, err)
			assert.Empty(t, url)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
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

func TestUploadImageServerDown(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // nothing listens anymore

	cfg := Config{Upload: UploadConfig{URL: url}}

	got, err := uploadImage(cfg, []byte("png-bytes"), "test-image.png")

	require.Error(t, err)
	assert.Empty(t, got)
}
