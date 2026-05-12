package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newSummarizerStubServer returns a minimal HTTP server that replies to a
// chat-completions POST with a fixed text body. Used by compaction tests
// that exercise the real LLM call path through the openai SDK without
// hitting an external API. Closes via the returned *httptest.Server.
func newSummarizerStubServer(t *testing.T, replyText string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain body so the SDK's request lifecycle completes cleanly.
		_ = json.NewDecoder(r.Body).Decode(&map[string]any{})

		resp := map[string]any{
			"id":      "stub-cmpl-1",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "stub-model",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": replyText,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     100,
				"completion_tokens": 20,
				"total_tokens":      120,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return server
}
