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
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func runFilter(t *testing.T, input string, oneByte bool) string {
	t.Helper()
	var src io.Reader = io.NopCloser(strings.NewReader(input))
	if oneByte {
		src = iotest.OneByteReader(src)
	}
	f := newSSECommentFilter(io.NopCloser(src))
	out, err := io.ReadAll(f)
	require.NoError(t, err)
	return string(out)
}

// The exact production keepalive: comment line + blank line, both removed.
func TestSSEFilterDropsKeepaliveComment(t *testing.T) {
	assert.Equal(t,
		"data: x\n\n",
		runFilter(t, ": \n\ndata: x\n\n", false),
	)
}

// A bare ":" comment (llama.cpp's exact form) with no trailing space.
func TestSSEFilterDropsBareColonComment(t *testing.T) {
	assert.Equal(t, "", runFilter(t, ":\n\n", false))
}

// Standalone blank lines dispatch data-less events — dropped too.
func TestSSEFilterDropsDataLessBlankLines(t *testing.T) {
	assert.Equal(t, "data: x\n\n", runFilter(t, "\n\ndata: x\n\n", false))
}

// A comment between the data line and its dispatch blank line must not
// break the event: data passes, comment drops, blank passes.
func TestSSEFilterCommentInsideEvent(t *testing.T) {
	assert.Equal(t,
		"data: x\n\n",
		runFilter(t, "data: x\n: keepalive\n\n", false),
	)
}

// Well-formed streams are byte-identical, CRLF terminators included.
func TestSSEFilterPreservesCRLF(t *testing.T) {
	assert.Equal(t,
		"data: x\r\n\r\n",
		runFilter(t, "data: x\r\n\r\n", false),
	)
}

// Multi-line data fields survive intact.
func TestSSEFilterPreservesMultiLineData(t *testing.T) {
	in := "data: a\ndata: b\n\n"
	assert.Equal(t, in, runFilter(t, in, false))
}

// Unknown field lines (event:, id:, retry:) still arm the dispatch blank.
func TestSSEFilterEventFieldArmsBlankLine(t *testing.T) {
	in := "event: foo\n\n"
	assert.Equal(t, in, runFilter(t, in, false))
}

// Byte-at-a-time delivery exercises the partial-line buffering.
func TestSSEFilterOneByteAtATime(t *testing.T) {
	assert.Equal(t,
		"data: hello\n\n",
		runFilter(t, ": comment\n\ndata: hello\n\n", true),
	)
}

// A comment at EOF without a trailing newline is dropped.
func TestSSEFilterCommentAtEOF(t *testing.T) {
	assert.Equal(t, "data: x\n\n", runFilter(t, "data: x\n\n: dangling", false))
}

// A partial non-comment line at EOF passes through.
func TestSSEFilterPartialLineAtEOF(t *testing.T) {
	assert.Equal(t, "data: x\n\nno-newline", runFilter(t, "data: x\n\nno-newline", false))
}

// Non-EOF read errors surface after the flush.
func TestSSEFilterPropagatesReadError(t *testing.T) {
	f := newSSECommentFilter(io.NopCloser(iotest.ErrReader(fmt.Errorf("boom"))))
	_, err := io.ReadAll(f)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// The transport only wraps text/event-stream bodies: a JSON response
// comes through byte-identical (the captureBody path depends on this).
func TestTransportLeavesNonSSEBodyUntouched(t *testing.T) {
	payload := `{"choices":[{"message":{"content":"hi"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, payload)
	}))
	defer server.Close()

	transport := newDaveTransport(nil, nil)
	resp, err := transport.RoundTrip(&http.Request{
		Method: http.MethodPost,
		URL:    mustURL(t, server.URL+"/v1/chat/completions"),
		Body:   io.NopCloser(strings.NewReader(`{}`)),
		Header: http.Header{"Content-Type": []string{"application/json"}},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, string(body))
	var v map[string]any
	require.NoError(t, json.Unmarshal(body, &v), "non-SSE bodies must not be filtered")
}
