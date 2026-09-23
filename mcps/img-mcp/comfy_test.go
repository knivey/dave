package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterruptComfyPrompt_Success(t *testing.T) {
	var receivedBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/interrupt", r.URL.Path, "request path")
		assert.Equal(t, http.MethodPost, r.Method, "request method")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := testConfig(server.URL)
	ctx := context.Background()

	err := interruptComfyPrompt(ctx, cfg, "prompt-abc-123")
	require.NoError(t, err, "unexpected error")

	assert.Equal(t, "prompt-abc-123", receivedBody["prompt_id"], "prompt_id")
}

func TestInterruptComfyPrompt_NetworkError(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := interruptComfyPrompt(ctx, cfg, "prompt-123")
	require.Error(t, err, "expected error for unreachable server")
}

func TestInterruptComfyPrompt_WithContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := testConfig(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := interruptComfyPrompt(ctx, cfg, "prompt-123")
	require.Error(t, err, "expected error due to context cancellation")
}

func TestInterruptComfyPrompt_MultipleInterrupts(t *testing.T) {
	var received []map[string]string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(body, &req)
		mu.Lock()
		received = append(received, req)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := testConfig(server.URL)
	ctx := context.Background()

	interruptComfyPrompt(ctx, cfg, "prompt-1")
	interruptComfyPrompt(ctx, cfg, "prompt-2")
	interruptComfyPrompt(ctx, cfg, "prompt-3")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 3, "interrupt count")
	for i, r := range received {
		expected := fmt.Sprintf("prompt-%d", i+1)
		assert.Equal(t, expected, r["prompt_id"], fmt.Sprintf("interrupt %d prompt_id", i))
	}
}

func TestSubmitComfyPrompt_UsesContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		resp := ComfyPromptResponse{PromptID: "test-id"}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := testConfig(server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := submitComfyPrompt(ctx, cfg, "test", ComfyWorkflow{})
	require.Error(t, err, "expected error due to context cancellation")
}

func TestPrepareComfyWorkflowInjectsPromptNote(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig("http://127.0.0.1:0")
	wc := cfg.Workflows["test"]
	// A realistic prompt node with a clip link, so we can verify the orphan
	// node copies it and looks like a real (merely disconnected) prompt node.
	workflow := map[string]ComfyNode{
		"18": {
			Inputs: map[string]interface{}{"clip_name": "text_encoder.safetensors"},
			Class:  "CLIPLoader",
		},
		"prompt-node": {
			Inputs: map[string]interface{}{"text": "", "clip": []interface{}{"18", 0}},
			Class:  "CLIPTextEncode",
		},
		"output-node": {
			Inputs: map[string]interface{}{"images": []string{"1"}},
			Class:  "SaveImage",
		},
	}
	data, err := json.Marshal(workflow)
	require.NoError(t, err)
	wc.WorkflowPath = filepath.Join(dir, "wf.json")
	require.NoError(t, os.WriteFile(wc.WorkflowPath, data, 0644))
	cfg.Workflows["test"] = wc

	note := `{"prompt":"a cat","llm_generated":false,"job_id":"j_123"}`

	got, err := prepareComfyWorkflow(cfg, "test", "enhanced cat", "", nil, note)
	require.NoError(t, err, "prepareComfyWorkflow")

	node, ok := got[davePromptNoteNodeID]
	require.True(t, ok, "workflow should contain the prompt note node")
	// DESIGN NOTE: this must be a registered core class (e.g. CLIPTextEncode),
	// NOT "Note" — Note is a frontend-only node type and the backend rejects
	// API prompts containing it with missing_node_type.
	assert.Equal(t, "CLIPTextEncode", node.Class, "note node class")
	require.NotNil(t, node.Meta, "note node meta")
	assert.Equal(t, "dave original prompt", node.Meta.Title, "note node title")
	assert.Equal(t, note, node.Inputs["text"], "note node text should be the payload verbatim")
	assert.Equal(t, []interface{}{"18", float64(0)}, node.Inputs["clip"],
		"note node should copy the prompt node's clip link so it looks like a real disconnected prompt node")

	assert.Equal(t, "enhanced cat", got["prompt-node"].Inputs["text"],
		"prompt node should still receive the (possibly enhanced) prompt")
}

func TestBuildPromptNote(t *testing.T) {
	tests := []struct {
		name string
		job  *Job
		want string
	}{
		{
			name: "user prompt",
			job:  &Job{ID: "j_1", Input: JobInput{Prompt: "a cat"}},
			want: `{"prompt":"a cat","llm_generated":false,"job_id":"j_1"}`,
		},
		{
			name: "llm generated",
			job:  &Job{ID: "j_2", Input: JobInput{Prompt: "a cat", LLMGenerated: true}},
			want: `{"prompt":"a cat","llm_generated":true,"job_id":"j_2"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildPromptNote(tt.job)
			require.NoError(t, err, "buildPromptNote")
			assert.JSONEq(t, tt.want, got)
		})
	}
}

// mockComfyFlowServer stands in for ComfyUI's HTTP+WS API: it records every
// workflow submitted to /prompt, reports a completed history entry for the
// prompt it handed out, serves /view bytes, and accepts+immediately closes
// the /ws monitor socket so monitorComfyGeneration falls back to history.
type mockComfyFlowServer struct {
	server  *httptest.Server
	mu      sync.Mutex
	prompts []ComfyPromptRequest
}

func newMockComfyFlowServer(t *testing.T) *mockComfyFlowServer {
	t.Helper()
	m := &mockComfyFlowServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		var req ComfyPromptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.prompts = append(m.prompts, req)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ComfyPromptResponse{PromptID: "test-prompt-1"})
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ComfyHistoryResponse{
			"test-prompt-1": {
				Outputs: map[string]ComfyOutput{
					"output-node": {
						Images: []ComfyImage{{Filename: "img_00001.png", Subfolder: "", Type: "output"}},
					},
				},
			},
		})
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fakedata"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn.Close()
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(func() { m.server.Close() })
	return m
}

func (m *mockComfyFlowServer) submittedPrompts() []ComfyPromptRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ComfyPromptRequest{}, m.prompts...)
}

func (m *mockComfyFlowServer) URL() string {
	return m.server.URL
}

func TestProcessJobSubmitsPromptNote(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt:       "a cat sitting on a mat",
		LLMGenerated: true,
		OutputFormat: "base64",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	submitted := mockComfy.submittedPrompts()
	require.Len(t, submitted, 1, "exactly one workflow should have been submitted")
	node, ok := submitted[0].Prompt[davePromptNoteNodeID]
	require.True(t, ok, "submitted workflow should contain the prompt note node")
	assert.Equal(t, "CLIPTextEncode", node.Class)

	text, ok := node.Inputs["text"].(string)
	require.True(t, ok, "note node text should be a string")
	var payload promptNotePayload
	require.NoError(t, json.Unmarshal([]byte(text), &payload), "note text should be valid JSON")
	assert.Equal(t, "a cat sitting on a mat", payload.Prompt, "original prompt")
	assert.True(t, payload.LLMGenerated, "llm_generated flag")
	assert.Equal(t, job.ID, payload.JobID, "job id")

	assert.Equal(t, "a cat sitting on a mat", submitted[0].Prompt["prompt-node"].Inputs["text"],
		"prompt node should receive the unenhanced prompt for a plain generate job")
}

// newMockEnhanceServer stands in for an OpenAI-compatible prompt-enhancement
// endpoint; it always returns the same enhanced/negative prompt pair.
func newMockEnhanceServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"model": "enhancer",
			"choices": []map[string]any{{
				"message": map[string]any{
					"content": `{"enhanced_prompt":"a majestic cat, studio lighting, 4k","negative_prompt":"blurry","refused":false,"reason":""}`,
				},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(func() { server.Close() })
	return server
}

func TestProcessJobEnhanceKeepsOriginalPromptInNote(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	enhServer := newMockEnhanceServer(t)

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Enhancements = map[string]EnhancementConfig{
		"default": {
			BaseURL:      enhServer.URL,
			Key:          "test-key",
			Model:        "enhancer",
			SystemPrompt: "enhance",
		},
	}
	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeEnhanceGenerate, "test", JobInput{
		Prompt:       "a cat sitting on a mat",
		LLMGenerated: false,
		OutputFormat: "base64",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	submitted := mockComfy.submittedPrompts()
	require.Len(t, submitted, 1)
	node, ok := submitted[0].Prompt[davePromptNoteNodeID]
	require.True(t, ok, "submitted workflow should contain the prompt note node")
	assert.Equal(t, "CLIPTextEncode", node.Class)

	var payload promptNotePayload
	require.NoError(t, json.Unmarshal([]byte(node.Inputs["text"].(string)), &payload))
	assert.Equal(t, "a cat sitting on a mat", payload.Prompt,
		"note should preserve the original pre-enhancement prompt")
	assert.False(t, payload.LLMGenerated)
	assert.Equal(t, job.ID, payload.JobID)

	assert.Equal(t, "a majestic cat, studio lighting, 4k", submitted[0].Prompt["prompt-node"].Inputs["text"],
		"prompt node should receive the enhanced prompt")
}
