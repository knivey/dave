package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enhancementRecorder captures decoded request bodies per API path.
type enhancementRecorder struct {
	mu                sync.Mutex
	chatRequests      []map[string]any
	responsesRequests []map[string]any
}

func (r *enhancementRecorder) chat() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any{}, r.chatRequests...)
}

func (r *enhancementRecorder) responses() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any{}, r.responsesRequests...)
}

// newEnhancementStubServer emulates an OpenAI-compatible API serving both
// /v1/chat/completions and /v1/responses. Every request body is recorded;
// replies embed the canned EnhancementResponse on both paths. The Responses
// reply includes a reasoning item with two summary entries before the
// message item, mirroring real reasoning-model output ordering.
func newEnhancementStubServer(t *testing.T, reply EnhancementResponse) (*httptest.Server, *enhancementRecorder) {
	t.Helper()
	rec := &enhancementRecorder{}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))
		rec.mu.Lock()
		rec.chatRequests = append(rec.chatRequests, decoded)
		rec.mu.Unlock()

		content, _ := json.Marshal(reply)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatc-1","object":"chat.completion","created":1,"model":"stub",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	})

	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))
		rec.mu.Lock()
		rec.responsesRequests = append(rec.responsesRequests, decoded)
		rec.mu.Unlock()

		content, _ := json.Marshal(reply)
		output := `[` +
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"step one"},{"type":"summary_text","text":"step two"}]},` +
			`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + string(content) + `,"annotations":[]}]}` +
			`]`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"stub",` +
			`"output":` + output + `,` +
			`"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"output_tokens_details":{"reasoning_tokens":1}}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func testEnhanceConfig(serverURL string, responsesAPI bool, effort string) Config {
	return Config{
		Enhancements: map[string]EnhancementConfig{
			"default": {
				BaseURL:         serverURL + "/v1",
				Key:             "test-key",
				Model:           "stub-model",
				SystemPrompt:    "enhance the prompt",
				Timeout:         10,
				ResponsesAPI:    responsesAPI,
				ReasoningEffort: effort,
			},
		},
	}
}

func TestEnhancePromptChatCompletionsSendsReasoningEffort(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a majestic cat", NegativePrompt: "blurry"}
	srv, rec := newEnhancementStubServer(t, reply)

	result, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, false, "low"), "default", "a cat")
	require.NoError(t, err)
	assert.Equal(t, "a majestic cat", result.EnhancedPrompt)
	assert.Equal(t, "blurry", result.NegativePrompt)

	requests := rec.chat()
	require.Len(t, requests, 1)
	assert.Equal(t, "low", requests[0]["reasoning_effort"], "effort must be sent as top-level reasoning_effort")
	assert.Equal(t, "stub-model", requests[0]["model"])
	assert.NotContains(t, requests[0], "reasoning", "Chat Completions must not carry a reasoning object")
}

func TestEnhancePromptResponsesReturnsReasoningSummary(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a majestic cat", NegativePrompt: "blurry"}
	srv, _ := newEnhancementStubServer(t, reply)

	result, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, ""), "default", "a cat")
	require.NoError(t, err)
	assert.Equal(t, "a majestic cat", result.EnhancedPrompt)
	// The stub's reasoning item carries two summary entries ("step one",
	// "step two"); production concatenates them with no separator.
	assert.Equal(t, "step onestep two", result.Reasoning,
		"responses path must surface the reasoning summary for the prompt note")
}

func TestEnhancePromptChatCompletionsHasNoReasoning(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a majestic cat"}
	srv, _ := newEnhancementStubServer(t, reply)

	result, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, false, ""), "default", "a cat")
	require.NoError(t, err)
	assert.Empty(t, result.Reasoning,
		"chat completions exposes no reasoning summaries; payload stays compact")
}

func TestEnhancePromptChatCompletionsOmitsReasoningEffortWhenEmpty(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a cat"}
	srv, rec := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, false, ""), "default", "a cat")
	require.NoError(t, err)

	requests := rec.chat()
	require.Len(t, requests, 1)
	assert.NotContains(t, requests[0], "reasoning_effort", "empty effort must omit reasoning_effort entirely")
}

func TestEnhancePromptResponsesAPIRequestShape(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a majestic cat", NegativePrompt: "blurry"}
	srv, rec := newEnhancementStubServer(t, reply)

	result, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, "high"), "default", "a cat")
	require.NoError(t, err)
	assert.Equal(t, "a majestic cat", result.EnhancedPrompt)
	assert.Empty(t, rec.chat(), "responses_api must not touch chat completions")

	requests := rec.responses()
	require.Len(t, requests, 1)
	body := requests[0]
	assert.Equal(t, "stub-model", body["model"])
	require.Contains(t, body, "reasoning", "non-empty effort must send reasoning")
	assert.Equal(t, "high", body["reasoning"].(map[string]any)["effort"])

	input, ok := body["input"].([]any)
	require.True(t, ok, "input must be an item list")
	require.Len(t, input, 2)
	sysMsg, ok := input[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "system", sysMsg["role"])
	assert.Equal(t, "enhance the prompt", sysMsg["content"])
	userMsg, ok := input[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", userMsg["role"])
	assert.Equal(t, "a cat", userMsg["content"])

	text, ok := body["text"].(map[string]any)
	require.True(t, ok, "structured output must go in text.format")
	format, ok := text["format"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "json_schema", format["type"])
	assert.Equal(t, "prompt_enhancement", format["name"])
	assert.Equal(t, true, format["strict"])
	schema, ok := format["schema"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", schema["type"])
}

func TestEnhancePromptResponsesAPIOmitsReasoningWhenEmpty(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a cat"}
	srv, rec := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, ""), "default", "a cat")
	require.NoError(t, err)

	requests := rec.responses()
	require.Len(t, requests, 1)
	assert.NotContains(t, requests[0], "reasoning", "empty effort must omit the reasoning object entirely")
}

func TestEnhancePromptResponsesAPINoMessageOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"stub",` +
			`"output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hmm"}]}],` +
			`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, ""), "default", "a cat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no message output")
}

func TestEnhancePromptResponsesAPIRefused(t *testing.T) {
	reply := EnhancementResponse{Refused: true, Reason: "policy"}
	srv, _ := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, "low"), "default", "a cat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enhancement refused: policy")
}

func TestEnhancePromptChatCompletionsRefused(t *testing.T) {
	reply := EnhancementResponse{Refused: true, Reason: "policy"}
	srv, _ := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, false, ""), "default", "a cat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enhancement refused: policy")
}
