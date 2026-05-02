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
)

func TestInterruptComfyPrompt_Success(t *testing.T) {
	var receivedBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/interrupt" {
			t.Errorf("expected path /api/interrupt, got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := testConfig(server.URL)
	ctx := context.Background()

	err := interruptComfyPrompt(ctx, cfg, "prompt-abc-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedBody["prompt_id"] != "prompt-abc-123" {
		t.Errorf("expected prompt_id 'prompt-abc-123', got %q", receivedBody["prompt_id"])
	}
}

func TestInterruptComfyPrompt_NetworkError(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := interruptComfyPrompt(ctx, cfg, "prompt-123")
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
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
	if err == nil {
		t.Fatal("expected error due to context cancellation")
	}
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
	if len(received) != 3 {
		t.Fatalf("expected 3 interrupts, got %d", len(received))
	}
	for i, r := range received {
		expected := fmt.Sprintf("prompt-%d", i+1)
		if r["prompt_id"] != expected {
			t.Errorf("interrupt %d: expected prompt_id %q, got %q", i, expected, r["prompt_id"])
		}
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
	if err == nil {
		t.Fatal("expected error due to context cancellation")
	}
}
