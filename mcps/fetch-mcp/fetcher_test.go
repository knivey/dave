package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPaginate(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		startIndex int
		maxLength  int
		expected   string
		truncated  bool
	}{
		{
			name:       "full content fits",
			content:    "hello world",
			startIndex: 0,
			maxLength:  100,
			expected:   "hello world",
			truncated:  false,
		},
		{
			name:       "truncated",
			content:    "hello world",
			startIndex: 0,
			maxLength:  5,
			expected:   "hello",
			truncated:  true,
		},
		{
			name:       "start index past content",
			content:    "hello",
			startIndex: 10,
			maxLength:  100,
			expected:   "",
			truncated:  false,
		},
		{
			name:       "start in middle",
			content:    "hello world",
			startIndex: 6,
			maxLength:  100,
			expected:   "world",
			truncated:  false,
		},
		{
			name:       "start in middle truncated",
			content:    "hello world",
			startIndex: 6,
			maxLength:  3,
			expected:   "wor",
			truncated:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := paginate(tt.content, tt.startIndex, tt.maxLength)
			assert.Equal(t, tt.expected, got)
			assert.Equal(t, tt.truncated, truncated)
		})
	}
}

func TestNextIndex(t *testing.T) {
	assert.Equal(t, 10, nextIndex(0, 10, 100))
	assert.Equal(t, 20, nextIndex(10, 10, 100))
	assert.Equal(t, 0, nextIndex(90, 10, 100))
	assert.Equal(t, 0, nextIndex(95, 10, 100))
}

func TestIsHTML(t *testing.T) {
	assert.True(t, isHTML("text/html; charset=utf-8", nil))
	assert.True(t, isHTML("application/xhtml+xml", nil))
	assert.True(t, isHTML("text/plain", []byte("<html><body>hello</body></html>")))
	assert.False(t, isHTML("application/json", []byte(`{"key":"value"}`)))
	assert.False(t, isHTML("text/plain", []byte(`just some text`)))
}

func TestExtractTitle(t *testing.T) {
	assert.Equal(t, "My Page", extractTitle([]byte("<html><head><title>My Page</title></head></html>")))
	assert.Equal(t, "My Page", extractTitle([]byte("<html><head><TITLE>My Page</TITLE></head></html>")))
	assert.Equal(t, "", extractTitle([]byte("<html><body>no title</body></html>")))
	assert.Equal(t, "", extractTitle([]byte("")))
}

func TestNewFetcherBadProxy(t *testing.T) {
	cfg := Config{
		Fetch: FetchConfig{
			ProxyURL:     "://bad-url",
			Timeout:      30 * time.Second,
			MaxRedirects: 10,
			UserAgent:    "test",
		},
		Cache:       CacheConfig{DSN: "memcache://"},
		Readability: ReadabilityConfig{},
	}
	mdCache := NewMarkdownCache(5 * time.Minute)
	_, err := NewFetcher(cfg, mdCache)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parsing proxy URL")
}

func TestFetcherIntegration(t *testing.T) {
	htmlPage := `<!DOCTYPE html>
<html>
<head><title>Test Page</title></head>
<body><h1>Hello World</h1><p>This is some content.</p></body>
</html>`

	plainText := "just some plain text here"
	jsonBody := `{"key": "value", "number": 42}`

	mux := http.NewServeMux()
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(htmlPage))
	})
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(plainText))
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(jsonBody))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	cfg := Config{
		Fetch: FetchConfig{
			Timeout:      10 * time.Second,
			MaxRedirects: 10,
			UserAgent:    "test-fetcher",
		},
		Cache:       CacheConfig{DSN: "memcache://"},
		Readability: ReadabilityConfig{},
	}
	mdCache := NewMarkdownCache(5 * time.Minute)
	fetcher, err := NewFetcher(cfg, mdCache)
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("HTML page conversion", func(t *testing.T) {
		result, err := fetcher.Fetch(ctx, server.URL+"/html", 0, 20000)
		require.NoError(t, err)
		assert.Equal(t, "Test Page", result.Title)
		assert.Contains(t, result.Markdown, "Hello World")
		assert.Contains(t, result.Markdown, "This is some content")
		assert.False(t, result.IsRawContent)
		assert.False(t, result.Truncated)
		assert.Equal(t, 0, result.NextIndex)
	})

	t.Run("Markdown cache hit on second call", func(t *testing.T) {
		freshCache := NewMarkdownCache(5 * time.Minute)
		freshFetcher, err := NewFetcher(cfg, freshCache)
		require.NoError(t, err)

		result1, err := freshFetcher.Fetch(ctx, server.URL+"/html", 0, 20000)
		require.NoError(t, err)
		assert.False(t, result1.FromMarkdownCache)

		result2, err := freshFetcher.Fetch(ctx, server.URL+"/html", 0, 20000)
		require.NoError(t, err)
		assert.True(t, result2.FromMarkdownCache)
		assert.Equal(t, result1.Title, result2.Title)
		assert.Equal(t, result1.Markdown, result2.Markdown)
	})

	t.Run("Plain text passthrough", func(t *testing.T) {
		result, err := fetcher.Fetch(ctx, server.URL+"/text", 0, 20000)
		require.NoError(t, err)
		assert.Equal(t, plainText, result.Markdown)
		assert.True(t, result.IsRawContent)
		assert.False(t, result.Truncated)
		assert.Equal(t, 0, result.NextIndex)
	})

	t.Run("JSON passthrough", func(t *testing.T) {
		result, err := fetcher.Fetch(ctx, server.URL+"/json", 0, 20000)
		require.NoError(t, err)
		assert.Equal(t, jsonBody, result.Markdown)
		assert.True(t, result.IsRawContent)
	})

	t.Run("Pagination with small maxLength", func(t *testing.T) {
		result, err := fetcher.Fetch(ctx, server.URL+"/text", 0, 5)
		require.NoError(t, err)
		assert.True(t, result.Truncated)
		assert.Equal(t, 5, len(result.Markdown))
		assert.Greater(t, result.NextIndex, 0)
		assert.Equal(t, 0, result.StartIndex)
	})

	t.Run("Rejects non-http(s) schemes", func(t *testing.T) {
		_, err := fetcher.Fetch(ctx, "file:///etc/passwd", 0, 20000)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported URL scheme")

		_, err = fetcher.Fetch(ctx, "ftp://example.com/file", 0, 20000)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported URL scheme")
	})
}
