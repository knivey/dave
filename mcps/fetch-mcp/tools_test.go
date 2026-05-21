package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	logxi "github.com/mgutz/logxi/v1"
)

func initTestLogger() {
	if logger == nil {
		logger = logxi.NewLogger(&discardWriter{}, "fetch-mcp-test")
		logger.SetLevel(logxi.LevelAll)
	}
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestToolHandlers(t *testing.T) *ToolHandlers {
	t.Helper()
	initTestLogger()
	cfg := Config{
		Fetch: FetchConfig{
			Timeout:      10 * time.Second,
			MaxRedirects: 10,
			MaxBodySize:  20 * 1024 * 1024,
			UserAgent:    "test-handler",
		},
		Cache:       CacheConfig{DSN: "memcache://"},
		Readability: ReadabilityConfig{},
	}
	h, err := NewToolHandlers(cfg)
	require.NoError(t, err)
	return h
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><head><title>Test</title></head><body><p>Hello World</p></body></html>")
	})
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("x", 50000))
	})
	mux.HandleFunc("/short", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "short content")
	})
	return httptest.NewServer(mux)
}

func TestHandleFetch_EmptyURL(t *testing.T) {
	h := newTestToolHandlers(t)
	_, _, err := h.handleFetch(context.Background(), &mcp.CallToolRequest{}, FetchInput{URL: ""})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "url is required")
}

func TestHandleFetch_MaxLengthDefaults(t *testing.T) {
	tests := []struct {
		name            string
		maxLength       int
		expectNextIndex int
		expectTruncated bool
	}{
		{name: "zero defaults to 20000", maxLength: 0, expectNextIndex: 20000, expectTruncated: true},
		{name: "negative defaults to 20000", maxLength: -5, expectNextIndex: 20000, expectTruncated: true},
		{name: "over cap clamps to 100000", maxLength: 200000, expectNextIndex: 0, expectTruncated: false},
		{name: "valid value passes through", maxLength: 5000, expectNextIndex: 5000, expectTruncated: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestToolHandlers(t)
			srv := newTestServer(t)
			defer srv.Close()

			_, out, err := h.handleFetch(
				context.Background(),
				&mcp.CallToolRequest{},
				FetchInput{URL: srv.URL + "/text", MaxLength: tt.maxLength},
			)
			require.NoError(t, err)
			assert.Equal(t, tt.expectTruncated, out.Truncated)
			assert.Equal(t, tt.expectNextIndex, out.NextIndex)
		})
	}
}

func TestHandleFetch_ContentTypeSelection(t *testing.T) {
	h := newTestToolHandlers(t)
	srv := newTestServer(t)
	defer srv.Close()

	t.Run("HTML returns markdown content type", func(t *testing.T) {
		_, out, err := h.handleFetch(
			context.Background(),
			&mcp.CallToolRequest{},
			FetchInput{URL: srv.URL + "/html"},
		)
		require.NoError(t, err)
		assert.Equal(t, "markdown", out.ContentType)
		assert.Contains(t, out.Content, "Hello World")
	})

	t.Run("plain text returns raw content type", func(t *testing.T) {
		_, out, err := h.handleFetch(
			context.Background(),
			&mcp.CallToolRequest{},
			FetchInput{URL: srv.URL + "/short"},
		)
		require.NoError(t, err)
		assert.Equal(t, "raw", out.ContentType)
	})
}

func TestHandleFetch_TruncationMessage(t *testing.T) {
	h := newTestToolHandlers(t)
	srv := newTestServer(t)
	defer srv.Close()

	_, out, err := h.handleFetch(
		context.Background(),
		&mcp.CallToolRequest{},
		FetchInput{URL: srv.URL + "/text", MaxLength: 100},
	)
	require.NoError(t, err)
	assert.True(t, out.Truncated)
	assert.Contains(t, out.Content, "<pagination>Content truncated. Call again with start_index=")
	assert.Contains(t, out.Content, "</pagination>")
	assert.Equal(t, 100, out.StartIndex+out.NextIndex)
}

func TestHandleFetch_NoTruncationWhenContentFits(t *testing.T) {
	h := newTestToolHandlers(t)
	srv := newTestServer(t)
	defer srv.Close()

	_, out, err := h.handleFetch(
		context.Background(),
		&mcp.CallToolRequest{},
		FetchInput{URL: srv.URL + "/short"},
	)
	require.NoError(t, err)
	assert.False(t, out.Truncated)
	assert.NotContains(t, out.Content, "Content truncated")
	assert.Equal(t, 0, out.NextIndex)
}
