package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeResponsesAPIResponse(id, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"object": "response",
		"model":  "test-model",
		"output": []any{
			map[string]any{
				"type":   "message",
				"role":   "assistant",
				"id":     "msg_" + id,
				"status": "completed",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": text,
					},
				},
			},
		},
	}
}

func TestRunTurnResponses_ConcurrentSerialization(t *testing.T) {
	chatContextsMap = make(map[string]ChatContext)
	contextLastActive = make(map[string]int64)

	var (
		mu                   sync.Mutex
		prevIDs              []string
		callCount            int32
		unblockFirst         = make(chan struct{})
		firstRequestReceived = make(chan struct{})
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)

		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)

		var prevID string
		if raw, ok := body["previous_response_id"]; ok {
			json.Unmarshal(raw, &prevID)
		}

		mu.Lock()
		prevIDs = append(prevIDs, prevID)
		mu.Unlock()

		if count == 1 {
			close(firstRequestReceived)
			<-unblockFirst
		}

		respID := fmt.Sprintf("resp-%d", count)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(makeResponsesAPIResponse(respID, fmt.Sprintf("response %d", count)))
	}))
	defer server.Close()

	ctxKey := "testnet#101shrew"
	cfg := AIConfig{
		Model:              "test-model",
		ResponsesAPI:       true,
		PreviousResponseID: true,
		MaxHistory:         20,
		Timeout:            10 * time.Second,
	}

	chatContextsMap[ctxKey] = ChatContext{
		Messages:   []ChatMessage{{Role: RoleSystem, Content: "test"}},
		Config:     cfg,
		ResponseID: "resp-initial",
	}

	makeRunner := func() *chatRunner {
		client := openai.NewClient(
			option.WithAPIKey("test-key"),
			option.WithBaseURL(server.URL+"/v1"),
		)
		transport := newDaveTransport(nil, nil)
		return &chatRunner{
			openaiClient: &client,
			transport:    transport,
			httpClient:   &http.Client{Transport: transport},
			cfg:          cfg,
			network:      Network{Name: "testnet"},
			channel:      "#101",
			nick:         "shrew",
			ctxKey:       ctxKey,
			logger:       logxi.New("test"),
			ctx:          context.Background(),
			outputCh:     make(chan string, 100),
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		messages := GetContext(ctxKey).Messages
		messages = append(messages, ChatMessage{Role: RoleUser, Content: "msg 1"})
		AddContext(cfg, ctxKey, ChatMessage{Role: RoleUser, Content: "msg 1"}, "testnet", "#101", "shrew")
		runner := makeRunner()
		runner.runTurn(messages)
	}()

	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		messages := GetContext(ctxKey).Messages
		messages = append(messages, ChatMessage{Role: RoleSystem, Content: "bg job result"})
		AddContext(cfg, ctxKey, ChatMessage{Role: RoleSystem, Content: "bg job result"}, "testnet", "#101", "shrew")
		runner := makeRunner()
		runner.runTurn(messages)
	}()

	<-firstRequestReceived
	time.Sleep(100 * time.Millisecond)
	unblockFirst <- struct{}{}

	wg.Wait()

	mu.Lock()
	ids := make([]string, len(prevIDs))
	copy(ids, prevIDs)
	mu.Unlock()

	require.Len(t, ids, 2, "expected 2 API calls")
	assert.Equal(t, "resp-initial", ids[0], "first request prevID")
	assert.Equal(t, "resp-1", ids[1], "second request prevID (should use first response's ID)")
}

func TestRunTurnResponses_DifferentCtxKeysParallel(t *testing.T) {
	chatContextsMap = make(map[string]ChatContext)
	contextLastActive = make(map[string]int64)

	var (
		mu        sync.Mutex
		prevIDs   []string
		callCount int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&callCount, 1)

		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)

		var prevID string
		if raw, ok := body["previous_response_id"]; ok {
			json.Unmarshal(raw, &prevID)
		}

		mu.Lock()
		prevIDs = append(prevIDs, prevID)
		mu.Unlock()

		respID := fmt.Sprintf("resp-%d", count)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(makeResponsesAPIResponse(respID, fmt.Sprintf("response %d", count)))
	}))
	defer server.Close()

	cfg := AIConfig{
		Model:              "test-model",
		ResponsesAPI:       true,
		PreviousResponseID: true,
		MaxHistory:         20,
		Timeout:            10 * time.Second,
	}

	key1 := "testnet#101alice"
	key2 := "testnet#101bob"

	chatContextsMap[key1] = ChatContext{
		Messages:   []ChatMessage{{Role: RoleSystem, Content: "test"}},
		Config:     cfg,
		ResponseID: "resp-alice",
	}
	chatContextsMap[key2] = ChatContext{
		Messages:   []ChatMessage{{Role: RoleSystem, Content: "test"}},
		Config:     cfg,
		ResponseID: "resp-bob",
	}

	makeRunner := func(ctxKey, nick string) *chatRunner {
		client := openai.NewClient(
			option.WithAPIKey("test-key"),
			option.WithBaseURL(server.URL+"/v1"),
		)
		transport := newDaveTransport(nil, nil)
		return &chatRunner{
			openaiClient: &client,
			transport:    transport,
			httpClient:   &http.Client{Transport: transport},
			cfg:          cfg,
			network:      Network{Name: "testnet"},
			channel:      "#101",
			nick:         nick,
			ctxKey:       ctxKey,
			logger:       logxi.New("test"),
			ctx:          context.Background(),
			outputCh:     make(chan string, 100),
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		AddContext(cfg, key1, ChatMessage{Role: RoleUser, Content: "msg"}, "testnet", "#101", "alice")
		messages := GetContext(key1).Messages
		runner := makeRunner(key1, "alice")
		runner.runTurn(messages)
	}()

	go func() {
		defer wg.Done()
		AddContext(cfg, key2, ChatMessage{Role: RoleUser, Content: "msg"}, "testnet", "#101", "bob")
		messages := GetContext(key2).Messages
		runner := makeRunner(key2, "bob")
		runner.runTurn(messages)
	}()

	wg.Wait()

	mu.Lock()
	ids := make([]string, len(prevIDs))
	copy(ids, prevIDs)
	mu.Unlock()

	require.Len(t, ids, 2, "expected 2 API calls")

	found := make(map[string]bool)
	for _, id := range ids {
		found[id] = true
	}
	assert.True(t, found["resp-alice"], "missing prevID %q in %v", "resp-alice", ids)
	assert.True(t, found["resp-bob"], "missing prevID %q in %v", "resp-bob", ids)
}
