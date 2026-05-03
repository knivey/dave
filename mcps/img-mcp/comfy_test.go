package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
