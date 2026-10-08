package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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

// newRecordingSummarizerStubServer returns an httptest server that records
// the raw request body of every chat-completions POST (under a mutex) and
// replies with replies[i] on the i-th call (extra calls reuse the last
// entry; no entries → "Summary."). The returned accessor hands back a copy
// of the bodies seen so far. Used by compaction tests that must inspect
// what the summarizer was fed or return different summary text per call.
func newRecordingSummarizerStubServer(t *testing.T, replies ...string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body failed", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		idx := calls
		calls++
		mu.Unlock()

		reply := "Summary."
		if len(replies) > 0 {
			reply = replies[len(replies)-1]
			if idx < len(replies) {
				reply = replies[idx]
			}
		}

		resp := map[string]any{
			"id":      "stub-cmpl-rec",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "stub-model",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": reply,
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
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(bodies))
		copy(out, bodies)
		return out
	}
}

// newBlockingSummarizerStubServer returns an httptest server whose handler
// signals each request's arrival on the returned channel and then BLOCKS
// until release is closed, letting tests mutate session state while a
// summarizer call is genuinely in flight. After release it writes a valid
// chat-completion JSON response so the SDK call completes normally.
func newBlockingSummarizerStubServer(t *testing.T, release <-chan struct{}) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	arrived := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		arrived <- struct{}{}
		<-release

		resp := map[string]any{
			"id":      "stub-cmpl-block",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "stub-model",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "Blocked-path summary.",
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
	return server, arrived
}
