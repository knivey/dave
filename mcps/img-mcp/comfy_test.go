package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestComfyClientIDDerivation pins the per-job client id contract: ComfyUI
// keys /ws sockets by clientId and routes per-prompt events only to the
// socket registered under the client id the prompt was submitted with. Every
// submit and monitor must derive the same value for the same job (a mismatch
// means a deaf monitor), and the derivation must be stable across restarts
// so recoverRunningJob reconnects under the id the original process used.
func TestComfyClientIDDerivation(t *testing.T) {
	wc := WorkflowConfig{ClientID: "img-mcp"}
	assert.Equal(t, "img-mcp-ab12cd34", comfyClientID(wc, "ab12cd34"),
		"per-job client id is the configured base plus the job id")
}

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
		result, err := monitorComfyGeneration(context.Background(), cfg, "test", "test-prompt-1", "test-job")
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

	result, err := monitorComfyGeneration(context.Background(), cfg, "test", "test-prompt-1", "test-job")
	require.NoError(t, err, "monitor should succeed via polling")
	require.NotNil(t, result.ExecStartedAt, "ExecStartedAt should be parsed from history status")
	require.NotNil(t, result.ExecSuccessAt, "ExecSuccessAt should be parsed from history status")
	assert.EqualValues(t, 1790200000000, *result.ExecStartedAt, "execution_start timestamp (ms)")
	assert.EqualValues(t, 1790200015000, *result.ExecSuccessAt, "execution_success timestamp (ms)")
}

// TestDownloadComfyImageStats pins the download metrics contract: sizes are
// reported exactly, the first request to a host opens a fresh connection and
// the second reuses the pooled one (the keep-alive assumption — if this flips,
// every request is paying TCP setup and the "slow connecting" theory is live).
func TestDownloadComfyImageStats(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 2<<20) // 2 MiB
	mux := http.NewServeMux()
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(payload)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	img := ComfyImage{Filename: "img_00001.png", Subfolder: "", Type: "output"}

	data, st, err := downloadComfyImage(context.Background(), server.URL, img)
	require.NoError(t, err, "first download")
	assert.Len(t, data, len(payload), "downloaded bytes")
	assert.EqualValues(t, len(payload), st.SizeBytes, "SizeBytes")
	assert.True(t, st.FreshConnect, "first request to this host must open a connection")
	assert.GreaterOrEqual(t, st.ConnectMS, int64(0), "ConnectMS")
	assert.GreaterOrEqual(t, st.TotalMS, st.TTFBMS, "total must include time to first byte")
	assert.GreaterOrEqual(t, st.TransferMS, int64(0), "TransferMS")

	_, st2, err := downloadComfyImage(context.Background(), server.URL, img)
	require.NoError(t, err, "second download")
	assert.False(t, st2.FreshConnect, "second request must reuse the pooled connection")
	assert.Zero(t, st2.ConnectMS, "no connect should happen on a reused connection")
}

// TestDownloadComfyImageRespectsContextCancel pins the hang fix: the download
// must abort when its context is cancelled. The old http.Get had no context
// at all — a stalled /view response blocked the worker forever and ignored
// job cancellation.
func TestDownloadComfyImageRespectsContextCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // stall until the client gives up
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, _, err := downloadComfyImage(ctx, server.URL, ComfyImage{Filename: "x.png", Type: "output"})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err, "cancelled download must return an error")
	case <-time.After(3 * time.Second):
		t.Fatal("download ignored context cancellation (hung)")
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want string
	}{
		{name: "zero", in: 0, want: "0B"},
		{name: "bytes", in: 512, want: "512B"},
		{name: "kibibytes", in: 2048, want: "2.0KB"},
		{name: "mebibytes", in: 2<<20 + 300<<10, want: "2.3MB"},
		{name: "gibibytes", in: 3 << 30, want: "3.0GB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, humanBytes(tt.in))
		})
	}
}

func TestMBPerSecond(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		ms    int64
		want  float64
	}{
		{name: "2MiB in half a second", bytes: 2 << 20, ms: 500, want: 4.0},
		{name: "zero ms clamps to 1ms", bytes: 1 << 20, ms: 0, want: 1000},
		{name: "negative ms clamps to 1ms", bytes: 1 << 20, ms: -5, want: 1000},
		{name: "no bytes", bytes: 0, ms: 100, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, mbPerSecond(tt.bytes, tt.ms), 0.0001)
		})
	}
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

	_, err := submitComfyPrompt(ctx, cfg, "test", ComfyWorkflow{}, "test-job")
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
		name      string
		job       *Job
		reasoning string
		nsfw      bool
		want      string
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
		{
			name:      "enhancement reasoning",
			job:       &Job{ID: "j_3", Input: JobInput{Prompt: "a cat"}},
			reasoning: "the user wants a cat",
			want:      `{"prompt":"a cat","llm_generated":false,"job_id":"j_3","enhancement_reasoning":"the user wants a cat"}`,
		},
		{
			name: "nsfw first pass flagged",
			job:  &Job{ID: "j_4", Input: JobInput{Prompt: "a cat"}},
			nsfw: true,
			want: `{"prompt":"a cat","llm_generated":false,"job_id":"j_4","nsfw":true}`,
		},
		{
			name: "nsfw false omits the key",
			job:  &Job{ID: "j_5", Input: JobInput{Prompt: "a cat"}},
			nsfw: false,
			want: `{"prompt":"a cat","llm_generated":false,"job_id":"j_5"}`,
		},
		{
			name:      "nsfw flagged alongside reasoning",
			job:       &Job{ID: "j_6", Input: JobInput{Prompt: "a cat"}},
			reasoning: "the user wants a cat",
			nsfw:      true,
			want:      `{"prompt":"a cat","llm_generated":false,"job_id":"j_6","enhancement_reasoning":"the user wants a cat","nsfw":true}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildPromptNote(tt.job, tt.reasoning, tt.nsfw)
			require.NoError(t, err, "buildPromptNote")
			assert.JSONEq(t, tt.want, got)
			// JSONEq is key-set equality, but pin the omitempty shape raw
			// too: the flag must appear ONLY as "nsfw":true, never an
			// explicit false (absent = no signal is the tri-state rule).
			if tt.nsfw {
				assert.Contains(t, got, `"nsfw":true`)
			} else {
				assert.NotContains(t, got, "nsfw")
			}
		})
	}
}

// TestBuildPromptNoteTruncatesReasoning pins the cap on embedded reasoning:
// the note rides inside every generated image's metadata permanently, so a
// pathological rambling summary must not bloat all images.
func TestBuildPromptNoteTruncatesReasoning(t *testing.T) {
	job := &Job{ID: "j_t", Input: JobInput{Prompt: "a cat"}}

	exact := strings.Repeat("x", maxPromptNoteReasoningRunes)
	got, err := buildPromptNote(job, exact, false)
	require.NoError(t, err, "buildPromptNote")
	var payload promptNotePayload
	require.NoError(t, json.Unmarshal([]byte(got), &payload))
	assert.Equal(t, exact, payload.EnhancementReasoning,
		"reasoning at exactly the cap must pass through untouched")

	over := strings.Repeat("y", maxPromptNoteReasoningRunes+50)
	got, err = buildPromptNote(job, over, false)
	require.NoError(t, err, "buildPromptNote")
	require.NoError(t, json.Unmarshal([]byte(got), &payload))
	assert.Equal(t, strings.Repeat("y", maxPromptNoteReasoningRunes)+promptNoteTruncationMarker,
		payload.EnhancementReasoning,
		"reasoning over the cap must be truncated with a marker")

	// Multi-byte safety: truncation must land on a rune boundary so the
	// embedded JSON never carries invalid UTF-8. The leading ASCII byte
	// matters: with pure 2-byte runes a naive byte-slice at the (even) cap
	// would coincidentally cut on a boundary — "a" + é's pushes the cap
	// offset to an odd position, mid-rune, so only rune-aware truncation
	// passes. (A byte-split rune would surface as U+FFFD after the JSON
	// round-trip and fail the equality below.)
	multibyte := "a" + strings.Repeat("é", maxPromptNoteReasoningRunes+10)
	got, err = buildPromptNote(job, multibyte, false)
	require.NoError(t, err, "buildPromptNote")
	require.NoError(t, json.Unmarshal([]byte(got), &payload))
	want := "a" + strings.Repeat("é", maxPromptNoteReasoningRunes-1) + promptNoteTruncationMarker
	assert.Equal(t, want, payload.EnhancementReasoning,
		"truncation must cut on rune boundaries")
}

// mockComfyFlowServer stands in for ComfyUI's HTTP+WS API: it records every
// workflow submitted to /prompt, reports a completed history entry for the
// prompt it handed out, serves /view bytes, and accepts+immediately closes
// the /ws monitor socket so monitorComfyGeneration falls back to history.
type mockComfyFlowServer struct {
	server      *httptest.Server
	mu          sync.Mutex
	prompts     []ComfyPromptRequest
	wsClientIDs []string
	// viewData, when non-nil, is what /view serves instead of "fakedata"
	// (e.g. a webp with an embedded workflow for EXIF-recovery tests).
	viewData []byte
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
		m.mu.Lock()
		data := m.viewData
		m.mu.Unlock()
		if data == nil {
			data = []byte("fakedata")
		}
		w.Write(data)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.wsClientIDs = append(m.wsClientIDs, r.URL.Query().Get("clientId"))
		m.mu.Unlock()
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

// serveViewData makes /view return data (nil restores "fakedata").
func (m *mockComfyFlowServer) serveViewData(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.viewData = data
}

func (m *mockComfyFlowServer) wsClientIDList() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.wsClientIDs...)
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
	assert.Empty(t, payload.EnhancementReasoning,
		"plain generate jobs must not carry an enhancement_reasoning field")
	// Struct decoding can't distinguish an absent key from "" — pin the
	// serialized shape too: omitempty must keep the key out entirely.
	assert.NotContains(t, text, "enhancement_reasoning",
		"raw note JSON must omit the enhancement_reasoning key for plain generate jobs")
	assert.NotContains(t, text, "nsfw",
		"plain generate jobs have no first pass: the nsfw key must be absent, not false")

	assert.Equal(t, "a cat sitting on a mat", submitted[0].Prompt["prompt-node"].Inputs["text"],
		"prompt node should receive the unenhanced prompt for a plain generate job")

	// Per-job client id: submit and the monitor websocket must both use the
	// derived id (ComfyUI routes per-prompt events only to the submitting
	// client's socket, and evicts same-id ws reconnects — which matters once
	// max_workers > 1 lets monitors overlap).
	wantClientID := comfyClientID(wc, job.ID)
	assert.Equal(t, wantClientID, submitted[0].ClientID,
		"prompt submission must carry the per-job client id")
	assert.Contains(t, mockComfy.wsClientIDList(), wantClientID,
		"monitor websocket must connect under the same per-job client id as the submission")
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
	assert.Empty(t, payload.EnhancementReasoning,
		"chat completions enhancement returns no reasoning summaries")
	// The stub's reply carries no nsfw flag (absent = no signal), so the
	// enhanced-but-unflagged note must omit the key entirely.
	assert.NotContains(t, node.Inputs["text"].(string), "nsfw",
		"an unflagged enhancement must not bake nsfw:false — absent is the no-signal shape")

	assert.Equal(t, "a majestic cat, studio lighting, 4k", submitted[0].Prompt["prompt-node"].Inputs["text"],
		"prompt node should receive the enhanced prompt")
}

// TestProcessJobEnhanceEmbedsReasoningInNote runs an enhance_generate job
// against a Responses-API enhancement stub that emits reasoning summaries,
// and asserts the summary lands in the prompt note node's payload — the
// provenance chain is: enhancePrompt → processJob → buildPromptNote →
// dave_original_prompt node (embedded in the image by the save node).
func TestProcessJobEnhanceEmbedsReasoningInNote(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	enhServer, _ := newEnhancementStubServer(t, EnhancementResponse{
		EnhancedPrompt: "a majestic cat, studio lighting, 4k",
		NegativePrompt: "blurry",
	})

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Enhancements = map[string]EnhancementConfig{
		"default": {
			BaseURL:      enhServer.URL + "/v1",
			Key:          "test-key",
			Model:        "enhancer",
			SystemPrompt: "enhance",
			Timeout:      10,
			ResponsesAPI: true,
		},
	}
	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeEnhanceGenerate, "test", JobInput{
		Prompt:       "a cat sitting on a mat",
		OutputFormat: "base64",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	submitted := mockComfy.submittedPrompts()
	require.Len(t, submitted, 1)
	node, ok := submitted[0].Prompt[davePromptNoteNodeID]
	require.True(t, ok, "submitted workflow should contain the prompt note node")

	var payload promptNotePayload
	require.NoError(t, json.Unmarshal([]byte(node.Inputs["text"].(string)), &payload))
	assert.Equal(t, "step onestep two", payload.EnhancementReasoning,
		"note should embed the enhancement model's reasoning summary")
	assert.Equal(t, "a cat sitting on a mat", payload.Prompt,
		"note should still preserve the original pre-enhancement prompt")
}

func TestWorkflowEnhancementInstructions(t *testing.T) {
	tests := []struct {
		name     string
		workflow map[string]ComfyNode
		want     string
	}{
		{
			name: "node with instructions",
			workflow: map[string]ComfyNode{
				"prompt-node": {Inputs: map[string]interface{}{"text": ""}, Class: "CLIPTextEncode"},
				daveEnhancementInstructionsNodeID: {
					Inputs: map[string]interface{}{"text": "lean photorealistic, avoid anime"},
					Class:  daveEnhancementInstructionsNodeID,
				},
			},
			want: "lean photorealistic, avoid anime",
		},
		{
			name: "no instructions node",
			workflow: map[string]ComfyNode{
				"prompt-node": {Inputs: map[string]interface{}{"text": ""}, Class: "CLIPTextEncode"},
			},
			want: "",
		},
		{
			name: "empty text",
			workflow: map[string]ComfyNode{
				daveEnhancementInstructionsNodeID: {
					Inputs: map[string]interface{}{"text": ""},
					Class:  daveEnhancementInstructionsNodeID,
				},
			},
			want: "",
		},
		{
			name: "non-string text",
			workflow: map[string]ComfyNode{
				daveEnhancementInstructionsNodeID: {
					Inputs: map[string]interface{}{"text": 42},
					Class:  daveEnhancementInstructionsNodeID,
				},
			},
			want: "",
		},
		{
			name: "missing inputs map",
			workflow: map[string]ComfyNode{
				daveEnhancementInstructionsNodeID: {Class: daveEnhancementInstructionsNodeID},
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := testConfig("http://127.0.0.1:0")
			wc := cfg.Workflows["test"]
			data, err := json.Marshal(tt.workflow)
			require.NoError(t, err)
			wc.WorkflowPath = filepath.Join(dir, "wf.json")
			require.NoError(t, os.WriteFile(wc.WorkflowPath, data, 0644))
			cfg.Workflows["test"] = wc

			assert.Equal(t, tt.want, workflowEnhancementInstructions(cfg, "test"))
		})
	}
}

func TestWorkflowEnhancementInstructionsUnknownWorkflowAndUnreadableFile(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	assert.Equal(t, "", workflowEnhancementInstructions(cfg, "nope"),
		"unknown workflow must yield empty instructions, not an error")

	wc := cfg.Workflows["test"]
	wc.WorkflowPath = filepath.Join(t.TempDir(), "missing.json")
	cfg.Workflows["test"] = wc
	assert.Equal(t, "", workflowEnhancementInstructions(cfg, "test"),
		"unreadable workflow file must yield empty instructions")
}

func TestPrepareComfyWorkflowStripsEnhancementInstructions(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig("http://127.0.0.1:0")
	wc := cfg.Workflows["test"]
	workflow := map[string]ComfyNode{
		"prompt-node": {Inputs: map[string]interface{}{"text": ""}, Class: "CLIPTextEncode"},
		"output-node": {Inputs: map[string]interface{}{"images": []string{"1"}}, Class: "SaveImage"},
		daveEnhancementInstructionsNodeID: {
			Inputs: map[string]interface{}{"text": "lean photorealistic, avoid anime"},
			Class:  daveEnhancementInstructionsNodeID,
		},
	}
	data, err := json.Marshal(workflow)
	require.NoError(t, err)
	wc.WorkflowPath = filepath.Join(dir, "wf.json")
	require.NoError(t, os.WriteFile(wc.WorkflowPath, data, 0644))
	cfg.Workflows["test"] = wc

	got, err := prepareComfyWorkflow(cfg, "test", "a cat", "", nil, "{}")
	require.NoError(t, err, "prepareComfyWorkflow")

	assert.NotContains(t, got, daveEnhancementInstructionsNodeID,
		"instructions node must be stripped before submission — ComfyUI validates class_type registration on every node, even disconnected ones")
}

// TestProcessJobEnhanceUsesWorkflowInstructions runs an enhance_generate job
// whose workflow file carries a dave_enhancement_instructions node and
// asserts the full chain: processJob extracts the instructions,
// enhancePrompt appends them to the enhancement system prompt on a new
// line, and prepareComfyWorkflow strips the node before submission.
func TestProcessJobEnhanceUsesWorkflowInstructions(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	enhServer, rec := newEnhancementStubServer(t, EnhancementResponse{
		EnhancedPrompt: "a majestic cat, studio lighting, 4k",
		NegativePrompt: "blurry",
	})

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	workflow := map[string]ComfyNode{
		"prompt-node": {Inputs: map[string]interface{}{"text": ""}, Class: "CLIPTextEncode"},
		"output-node": {Inputs: map[string]interface{}{"images": []string{"1"}}, Class: "SaveImage"},
		daveEnhancementInstructionsNodeID: {
			Inputs: map[string]interface{}{"text": "lean photorealistic, avoid anime"},
			Class:  daveEnhancementInstructionsNodeID,
		},
	}
	data, err := json.Marshal(workflow)
	require.NoError(t, err)
	dir := t.TempDir()
	wc.WorkflowPath = filepath.Join(dir, "wf.json")
	require.NoError(t, os.WriteFile(wc.WorkflowPath, data, 0644))
	cfg.Workflows["test"] = wc
	cfg.Enhancements = map[string]EnhancementConfig{
		"default": {
			BaseURL:      enhServer.URL + "/v1",
			Key:          "test-key",
			Model:        "enhancer",
			SystemPrompt: "enhance",
			Timeout:      10,
		},
	}
	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeEnhanceGenerate, "test", JobInput{
		Prompt:       "a cat sitting on a mat",
		OutputFormat: "base64",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	requests := rec.chat()
	require.Len(t, requests, 1)
	messages, ok := requests[0]["messages"].([]any)
	require.True(t, ok, "chat request must carry a messages array")
	require.Len(t, messages, 2)
	sysMsg, ok := messages[0].(map[string]any)
	require.True(t, ok, "first message must be an object")
	assert.Equal(t, "system", sysMsg["role"])
	assert.Equal(t, "enhance\nlean photorealistic, avoid anime", sysMsg["content"],
		"workflow instructions must be appended after the system prompt on a new line")

	submitted := mockComfy.submittedPrompts()
	require.Len(t, submitted, 1)
	assert.NotContains(t, submitted[0].Prompt, daveEnhancementInstructionsNodeID,
		"instructions node must be stripped from the submitted workflow")
}
