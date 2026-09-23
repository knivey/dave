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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// silentComfyServer simulates the production failure mode observed on
// ComfyUI 0.33.3: the monitor's websocket connects and receives ComfyUI's
// initial status message, but the completion events never arrive (delayed or
// dropped push delivery). The history endpoint flips from empty to a
// completed entry shortly after the test starts, so only a monitor that
// polls history can observe the completion.
type silentComfyServer struct {
	server *httptest.Server
	ready  atomic.Bool

	// interruptDelay parks Cancel()'s interrupt call, reproducing the window
	// where the job context is already cancelled but Cancel() has not yet
	// set StatusCancelled.
	interruptDelay time.Duration
	// wsConnected is closed on the first /ws connection.
	wsConnected chan struct{}
	// promptAccepted is closed on the first /prompt submission.
	promptAccepted chan struct{}
}

func newSilentComfyServer(t *testing.T, readyAfter time.Duration) *silentComfyServer {
	t.Helper()
	s := &silentComfyServer{
		wsConnected:    make(chan struct{}),
		promptAccepted: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		var req ComfyPromptRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		closeOnce(s.promptAccepted)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ComfyPromptResponse{PromptID: "test-prompt-1"})
	})
	mux.HandleFunc("/api/interrupt", func(w http.ResponseWriter, r *http.Request) {
		if s.interruptDelay > 0 {
			time.Sleep(s.interruptDelay)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !s.ready.Load() {
			_ = json.NewEncoder(w).Encode(ComfyHistoryResponse{})
			return
		}
		// Mirrors ComfyUI 0.33.x history shape exactly: status.messages is an
		// array of ["event", {timestamp, prompt_id}] JSON tuples (NOT objects
		// — the first mock of this used objects and hid a decode bug that only
		// real ComfyUI exposed).
		entry := ComfyHistoryEntry{
			Outputs: map[string]ComfyOutput{
				"output-node": {
					Images: []ComfyImage{{Filename: "img_00001.png", Subfolder: "", Type: "output"}},
				},
			},
			Status: &ComfyHistoryStatus{
				Messages: [][]json.RawMessage{
					{json.RawMessage(`"execution_start"`), json.RawMessage(`{"timestamp":1790200000000,"prompt_id":"test-prompt-1"}`)},
					{json.RawMessage(`"execution_success"`), json.RawMessage(`{"timestamp":1790200015000,"prompt_id":"test-prompt-1"}`)},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(ComfyHistoryResponse{"test-prompt-1": entry})
	})
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fakedata"))
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		closeOnce(s.wsConnected)
		// Real ComfyUI sends one status message on connect; after that this
		// socket deliberately stays open and silent — no executing/executed/
		// completion events are ever delivered.
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"status","data":{}}`))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(func() { s.server.Close() })
	if readyAfter > 0 {
		time.AfterFunc(readyAfter, func() { s.ready.Store(true) })
	}
	return s
}

func closeOnce(ch chan struct{}) {
	closeOnceMu.Lock()
	defer closeOnceMu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

var closeOnceMu sync.Mutex

// TestMonitorDetectsCompletionByPollingWhenWebsocketSilent pins the fix for
// delayed generation-completion detection: when ComfyUI's websocket push
// never arrives, the monitor must still notice the completed prompt within
// roughly one poll interval of the history entry appearing — not wait for
// the workflow timeout to fire the read-deadline fallback.
func TestMonitorDetectsCompletionByPollingWhenWebsocketSilent(t *testing.T) {
	readyAfter := 300 * time.Millisecond
	silent := newSilentComfyServer(t, readyAfter)

	cfg := testConfig(silent.server.URL)
	wc := cfg.Workflows["test"]
	// Small but well above the expected poll-based detection time: with the
	// bug, the monitor only exits via this read deadline (~10s), which the
	// elapsed assertion below rejects.
	wc.Timeout = 10
	cfg.Workflows["test"] = wc

	type monitorOutcome struct {
		result ComfyResult
		err    error
	}
	outcomeCh := make(chan monitorOutcome, 1)
	start := time.Now()
	go func() {
		result, err := monitorComfyGeneration(context.Background(), cfg, "test", "test-prompt-1")
		outcomeCh <- monitorOutcome{result: result, err: err}
	}()

	select {
	case outcome := <-outcomeCh:
		require.NoError(t, outcome.err, "monitor should succeed once history is ready")
		assert.Len(t, outcome.result.Images, 1, "downloaded images")
		elapsed := time.Since(start)
		maxElapsed := readyAfter + 4*time.Second
		assert.Less(t, elapsed, maxElapsed,
			"completion should be detected via polling shortly after history is ready; took %v", elapsed)
	case <-time.After(60 * time.Second):
		t.Fatal("monitor never returned")
	}
}

// TestMonitorReturnsComfyExecutionTimestamps pins the observability contract:
// when the history entry carries ComfyUI's execution_start/execution_success
// status messages, the monitor's result must expose them so the "generation
// detected" log line can separate "comfyui was slow to finish" from
// "img-mcp was slow to notice".
func TestMonitorReturnsComfyExecutionTimestamps(t *testing.T) {
	silent := newSilentComfyServer(t, 200*time.Millisecond)

	cfg := testConfig(silent.server.URL)
	wc := cfg.Workflows["test"]
	wc.Timeout = 10
	cfg.Workflows["test"] = wc

	result, err := monitorComfyGeneration(context.Background(), cfg, "test", "test-prompt-1")
	require.NoError(t, err, "monitor should succeed via polling")
	require.NotNil(t, result.ExecStartedAt, "ExecStartedAt should be parsed from history status")
	require.NotNil(t, result.ExecSuccessAt, "ExecSuccessAt should be parsed from history status")
	assert.EqualValues(t, 1790200000000, *result.ExecStartedAt, "execution_start timestamp (ms)")
	assert.EqualValues(t, 1790200015000, *result.ExecSuccessAt, "execution_success timestamp (ms)")
}

// TestCheckComfyOutputStatusMessageRobustness pins the history decode against
// real ComfyUI 0.33.x status-message shapes: decode failures of the enclosing
// response turn every poll into "not found" (a live incident showed this as
// "generation timed out"), so malformed or unfamiliar tuples must be skipped,
// never fatal.
func TestCheckComfyOutputStatusMessageRobustness(t *testing.T) {
	const outputJSON = `"outputs":{"output-node":{"images":[{"filename":"img_00001.png","subfolder":"","type":"output"}]}}`
	tests := []struct {
		name        string
		historyBody string
		wantFound   bool
		wantStart   *int64
		wantSuccess *int64
	}{
		{
			name: "full real message set",
			historyBody: `{"test-prompt-1":{` + outputJSON + `,"status":{"status_str":"success","completed":true,"messages":[
				["execution_start",{"prompt_id":"test-prompt-1","timestamp":1790200000000}],
				["execution_cached",{"nodes":["57","60","61"],"prompt_id":"test-prompt-1","timestamp":1790200000003}],
				["execution_success",{"prompt_id":"test-prompt-1","timestamp":1790200015000}]
			]}}}`,
			wantFound:   true,
			wantStart:   ptrInt64(1790200000000),
			wantSuccess: ptrInt64(1790200015000),
		},
		{
			name: "execution_error with traceback strings",
			historyBody: `{"test-prompt-1":{` + outputJSON + `,"status":{"status_str":"error","completed":false,"messages":[
				["execution_start",{"prompt_id":"test-prompt-1","timestamp":1790200000000}],
				["execution_error",{"prompt_id":"test-prompt-1","exception_message":"boom","traceback":["  File x","    y"],"current_inputs":{"text":["a"]}}]
			]}}}`,
			wantFound: true,
			wantStart: ptrInt64(1790200000000),
		},
		{
			name: "malformed tuples ignored, valid ones still parsed",
			historyBody: `{"test-prompt-1":{` + outputJSON + `,"status":{"messages":[
				["execution_start","execution_start",{"timestamp":1}],
				[123,{"timestamp":2}],
				["execution_success","not-an-object"],
				["execution_start",{"prompt_id":"test-prompt-1","timestamp":1790200000000}],
				["execution_success",{"prompt_id":"test-prompt-1","timestamp":1790200015000}]
			]}}}`,
			wantFound:   true,
			wantStart:   ptrInt64(1790200000000),
			wantSuccess: ptrInt64(1790200015000),
		},
		{
			name:        "status null",
			historyBody: `{"test-prompt-1":{` + outputJSON + `,"status":null}}`,
			wantFound:   true,
		},
		{
			name:        "empty history",
			historyBody: `{}`,
			wantFound:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.historyBody)
			})
			mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("fakedata"))
			})
			server := httptest.NewServer(mux)
			t.Cleanup(func() { server.Close() })

			cfg := testConfig(server.URL)
			result, found := checkComfyOutput(context.Background(), cfg, cfg.Workflows["test"], server.URL, "test-prompt-1")
			assert.Equal(t, tt.wantFound, found, "found")
			if !tt.wantFound {
				return
			}
			assert.Len(t, result.Images, 1, "downloaded images")
			if tt.wantStart != nil {
				require.NotNil(t, result.ExecStartedAt, "ExecStartedAt")
				assert.EqualValues(t, *tt.wantStart, *result.ExecStartedAt, "execution_start ts")
			}
			if tt.wantSuccess != nil {
				require.NotNil(t, result.ExecSuccessAt, "ExecSuccessAt")
				assert.EqualValues(t, *tt.wantSuccess, *result.ExecSuccessAt, "execution_success ts")
			}
		})
	}
}

func ptrInt64(v int64) *int64 { return &v }

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
	assert.NotContains(t, node.Inputs, "clip",
		"orphan should be text-only even when the prompt node has a clip link — unreachable nodes are not input-validated")

	assert.Equal(t, "enhanced cat", got["prompt-node"].Inputs["text"],
		"prompt node should still receive the (possibly enhanced) prompt")
}

func TestPrepareComfyWorkflowMissingPromptNode(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig("http://127.0.0.1:0")
	wc := cfg.Workflows["test"]
	// Workflow whose nodes do NOT include the configured prompt node.
	workflow := map[string]ComfyNode{
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

	_, err = prepareComfyWorkflow(cfg, "test", "a cat", "", nil, "{}")
	require.Error(t, err, "missing prompt node must be an error, not a panic")
	assert.Contains(t, err.Error(), "prompt node")
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
	// mustWriteWorkflow's prompt node has no clip input: the orphan must
	// degrade to text-only (still accepted — unreachable nodes are not
	// input-validated).
	assert.NotContains(t, node.Inputs, "clip", "orphan should omit clip when the prompt node has none")

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
