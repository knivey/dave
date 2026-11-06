package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// servePhotoPage stands up a server that mirrors the structure of the
// production photo-detail pages that motivated the images field (see session
// 2993): preload links, a lightbox anchor wrapping the main image, metadata
// tables, and a filmstrip with lazy data-URI placeholders.
func servePhotoPage(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/photo_page.html")
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, string(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchHTMLPageImagesField(t *testing.T) {
	srv := servePhotoPage(t)
	f := newTestFetcher(t)

	res, err := f.Fetch(context.Background(), srv.URL+"/AAA1/_DSC1234", 0, 20000)
	require.NoError(t, err)
	assert.Equal(t, []string{
		srv.URL + "/prev/full/256.jpg",
		srv.URL + "/next/full/256.jpg",
		srv.URL + "/AAA1/full/256.jpg",
		srv.URL + "/thumb1/small.webp",
	}, res.Images)
	// The page content itself is still the metadata the model always saw.
	assert.Contains(t, res.Markdown, "JPG")
}

func TestFetchImagesSurviveMarkdownCache(t *testing.T) {
	srv := servePhotoPage(t)

	fresh := newTestFetcher(t)
	res1, err := fresh.Fetch(context.Background(), srv.URL+"/AAA1/_DSC1234", 0, 20000)
	require.NoError(t, err)
	require.False(t, res1.FromMarkdownCache)

	res2, err := fresh.Fetch(context.Background(), srv.URL+"/AAA1/_DSC1234", 0, 20000)
	require.NoError(t, err)
	require.True(t, res2.FromMarkdownCache)
	assert.Equal(t, res1.Images, res2.Images)
}

func TestFetchRawContentHasNoImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "https://example.com/not-a-real-harvest.jpg is just text")
	}))
	defer srv.Close()

	f := newTestFetcher(t)
	res, err := f.Fetch(context.Background(), srv.URL, 0, 20000)
	require.NoError(t, err)
	assert.Empty(t, res.Images)
}

func TestFetchMarkdownElidesDataURIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><body><p>Lazy filmstrip:</p><img src="data:image/webp;base64,UklGRhIAAABXRUJQVlA4">`+
			`<img src="/real.jpg"></body></html>`)
	}))
	defer srv.Close()

	cfg := Config{
		Fetch: FetchConfig{
			Timeout:      10 * time.Second,
			MaxRedirects: 10,
			MaxBodySize:  20 * 1024 * 1024,
			UserAgent:    "test-fetcher",
		},
		Cache:       CacheConfig{DSN: "memcache://"},
		Readability: ReadabilityConfig{Disabled: true},
	}
	f, err := NewFetcher(cfg, NewMarkdownCache(5*time.Minute))
	require.NoError(t, err)

	res, err := f.Fetch(context.Background(), srv.URL, 0, 20000)
	require.NoError(t, err)
	assert.Contains(t, res.Markdown, "data URI elided")
	assert.NotContains(t, res.Markdown, "base64")
	// The data: placeholder must never leak into the images field either.
	assert.Equal(t, []string{srv.URL + "/real.jpg"}, res.Images)

	// Pin the elide-BEFORE-cache ordering: a markdown-cache hit must serve
	// the elided form, never raw base64.
	res2, err := f.Fetch(context.Background(), srv.URL, 0, 20000)
	require.NoError(t, err)
	require.True(t, res2.FromMarkdownCache)
	assert.Contains(t, res2.Markdown, "data URI elided")
	assert.NotContains(t, res2.Markdown, "base64")
}

func TestHandleFetchOutputCarriesImages(t *testing.T) {
	initTestLogger()
	srv := servePhotoPage(t)
	h := newTestToolHandlers(t)

	_, out, err := h.handleFetch(context.Background(), nil, FetchInput{URL: srv.URL + "/AAA1/_DSC1234"})
	require.NoError(t, err)
	assert.Equal(t, []string{
		srv.URL + "/prev/full/256.jpg",
		srv.URL + "/next/full/256.jpg",
		srv.URL + "/AAA1/full/256.jpg",
		srv.URL + "/thumb1/small.webp",
	}, out.Images)
}
