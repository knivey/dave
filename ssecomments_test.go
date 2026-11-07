package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The exact production wire pattern, relay-captured 2026-11-07 (conv
// relay-1794012151): an SSE COMMENT keepalive (":\n\n") arrives after a
// period of stream silence. llama.cpp emits one per 30s of stream silence
// (spec-legal — comments must be ignored by SSE consumers); the openai-go
// ssestream ignores the comment LINE but then dispatches the following
// blank line as a DATA-LESS event and json.Unmarshal([]byte("")) aborts
// the whole stream with "unexpected end of JSON input". This killed every
// cold-cache streaming turn whose prompt processing exceeded ~30s of wire
// silence on type = "llama" services (prod incidents: sessions 2890,
// 2908, 3002, 3007 ×2) because a fresh child's 21k-token prefill takes
// ~30.5s with zero SSE bytes flowing — while warm-cache turns prefill in
// ~1s and never hit the keepalive, curl doesn't parse SSE at all, and the
// webui's proper SSE parser ignores comments.
//
// The fix: daveTransport strips data-less SSE lines (comments and
// empty-buffer blank lines) from text/event-stream bodies before the SDK
// sees them. This test drives the REAL SDK client through the transport
// and must complete despite the keepalive.
func TestStreamSurvivesSSEKeepaliveComment(t *testing.T) {
	stream := ": \n\n" + // the 30s keepalive comment (as llama.cpp sends it)
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"survived\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer server.Close()

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("k"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)

	params := openai.ChatCompletionNewParams{
		Model: shared.ChatModel("m"),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("hi"),
		},
	}
	s := client.Chat.Completions.NewStreaming(context.Background(), params)
	content := ""
	chunks := 0
	for s.Next() {
		chunks++
		if len(s.Current().Choices) > 0 {
			content += s.Current().Choices[0].Delta.Content
		}
	}
	require.NoError(t, s.Err(),
		"the SSE keepalive comment must not kill the stream (the openai-go bug this transport now filters)")
	assert.Equal(t, 2, chunks)
	assert.Equal(t, "survived", content)
}

// The same comment, this time arriving BETWEEN real events mid-stream
// (servers may interleave keepalives at any silence).
func TestStreamSurvivesMidStreamSSEComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, ": keepalive comment mid-stream\n\n")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("k"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	params := openai.ChatCompletionNewParams{
		Model: shared.ChatModel("m"),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("hi"),
		},
	}
	s := client.Chat.Completions.NewStreaming(context.Background(), params)
	content := ""
	for s.Next() {
		if len(s.Current().Choices) > 0 {
			content += s.Current().Choices[0].Delta.Content
		}
	}
	require.NoError(t, s.Err())
	assert.Equal(t, "ab", content)
}
