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
	"text/template"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupNoticesDefaults(t *testing.T) {
	t.Helper()
	var n NoticesConfig
	setNoticesDefaults(&n)
	configMu.Lock()
	config.Notices = n
	configMu.Unlock()
}

func TestExecuteToolCalls_SingleToolSendsCallNotice(t *testing.T) {
	chatContextsMap = make(map[string]ChatContext)
	setupNoticesDefaults(t)
	mcpServersMu.Lock()
	origToolMap := mcpToolToServer
	origServers := mcpServers
	mcpToolToServer = map[string]string{"tool_a": "serverA"}
	mcpServers = map[string]*MCPServer{"serverA": {}}
	mcpServersMu.Unlock()
	defer func() {
		mcpServersMu.Lock()
		mcpToolToServer = origToolMap
		mcpServers = origServers
		mcpServersMu.Unlock()
	}()

	verbose := true
	outputCh := make(chan string, 20)
	cr := &chatRunner{
		cfg:      AIConfig{ToolVerbose: &verbose},
		network:  Network{Name: "testnet"},
		channel:  "#test",
		nick:     "test",
		ctxKey:   "testnet#testtest",
		logger:   logxi.New("test"),
		ctx:      context.Background(),
		outputCh: outputCh,
	}

	toolCalls := []ToolCall{
		{ID: "tc1", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}},
	}

	go cr.executeToolCalls(nil, toolCalls)

	var msgs []string
	timeout := time.After(2 * time.Second)
	for len(msgs) < 1 {
		select {
		case m := <-outputCh:
			msgs = append(msgs, m)
		case <-timeout:
			t.Fatal("timed out waiting for IRC output")
		}
	}

	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "tool_a")
	assert.Contains(t, msgs[0], "serverA")
}

func TestExecuteToolCalls_MultipleToolsSendsCallMultiNotice(t *testing.T) {
	chatContextsMap = make(map[string]ChatContext)
	setupNoticesDefaults(t)
	mcpServersMu.Lock()
	origToolMap := mcpToolToServer
	origServers := mcpServers
	mcpToolToServer = map[string]string{
		"tool_a": "serverA",
		"tool_b": "serverB",
	}
	mcpServers = map[string]*MCPServer{"serverA": {}, "serverB": {}}
	mcpServersMu.Unlock()
	defer func() {
		mcpServersMu.Lock()
		mcpToolToServer = origToolMap
		mcpServers = origServers
		mcpServersMu.Unlock()
	}()

	verbose := true
	outputCh := make(chan string, 20)
	cr := &chatRunner{
		cfg:      AIConfig{ToolVerbose: &verbose},
		network:  Network{Name: "testnet"},
		channel:  "#test",
		nick:     "test",
		ctxKey:   "testnet#testtest",
		logger:   logxi.New("test"),
		ctx:      context.Background(),
		outputCh: outputCh,
	}

	toolCalls := []ToolCall{
		{ID: "tc1", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}},
		{ID: "tc2", Function: FunctionCall{Name: "tool_b", Arguments: "{}"}},
	}

	go cr.executeToolCalls(nil, toolCalls)

	var msgs []string
	timeout := time.After(2 * time.Second)
	for len(msgs) < 1 {
		select {
		case m := <-outputCh:
			msgs = append(msgs, m)
		case <-timeout:
			t.Fatal("timed out waiting for IRC output")
		}
	}

	require.Len(t, msgs, 1, "expected single batched notification, got %d: %v", len(msgs), msgs)
	assert.Contains(t, msgs[0], "tool_a")
	assert.Contains(t, msgs[0], "tool_b")
	assert.Contains(t, msgs[0], "serverA")
	assert.Contains(t, msgs[0], "serverB")
}

func TestExecuteToolCalls_MultipleWithBuiltinOnlySendsMCP(t *testing.T) {
	chatContextsMap = make(map[string]ChatContext)
	setupNoticesDefaults(t)
	mcpServersMu.Lock()
	origToolMap := mcpToolToServer
	origServers := mcpServers
	mcpToolToServer = map[string]string{
		"tool_a": "serverA",
	}
	mcpServers = map[string]*MCPServer{"serverA": {}}
	mcpServersMu.Unlock()
	defer func() {
		mcpServersMu.Lock()
		mcpToolToServer = origToolMap
		mcpServers = origServers
		mcpServersMu.Unlock()
	}()

	verbose := true
	outputCh := make(chan string, 20)
	cr := &chatRunner{
		cfg:      AIConfig{ToolVerbose: &verbose},
		network:  Network{Name: "testnet"},
		channel:  "#test",
		nick:     "test",
		ctxKey:   "testnet#testtest",
		logger:   logxi.New("test"),
		ctx:      context.Background(),
		outputCh: outputCh,
	}

	toolCalls := []ToolCall{
		{ID: "tc1", Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j1","tool_name":"t","server_name":"s"}`}},
		{ID: "tc2", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}},
	}

	go cr.executeToolCalls(nil, toolCalls)

	var msgs []string
	timeout := time.After(2 * time.Second)
	for len(msgs) < 1 {
		select {
		case m := <-outputCh:
			msgs = append(msgs, m)
		case <-timeout:
			t.Fatal("timed out waiting for IRC output")
		}
	}

	require.Len(t, msgs, 1, "expected single notification for MCP tool, got %d: %v", len(msgs), msgs)
	assert.Contains(t, msgs[0], "tool_a")
	assert.NotContains(t, msgs[0], "register_background_job")
}

func TestRenderAPIUser(t *testing.T) {
	tests := []struct {
		name     string
		template string
		nick     string
		channel  string
		network  string
		expected string
	}{
		{
			name:     "simple nick",
			template: "{{.Nick}}",
			nick:     "alice",
			expected: "alice",
		},
		{
			name:     "network and nick",
			template: "dave/{{.Network}}/{{.Nick}}",
			nick:     "bob",
			network:  "libera",
			expected: "dave/libera/bob",
		},
		{
			name:     "all fields",
			template: "irc:{{.Network}}:{{.Channel}}:{{.Nick}}",
			nick:     "carol",
			channel:  "#dev",
			network:  "testnet",
			expected: "irc:testnet:#dev:carol",
		},
		{
			name:     "with bot nick",
			template: "{{.BotNick}}-{{.Nick}}",
			nick:     "dave",
			expected: "testbot-dave",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := template.New("test").Parse(tt.template)
			require.NoError(t, err)

			cr := &chatRunner{
				cfg:     AIConfig{apiUserTmpl: tmpl},
				nick:    tt.nick,
				channel: tt.channel,
				network: Network{Name: tt.network, Nick: "testbot"},
				ctx:     context.Background(),
				logger:  logxi.New("test"),
			}

			result := cr.renderAPIUser()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRenderAPIUser_NoTemplate(t *testing.T) {
	cr := &chatRunner{
		cfg:     AIConfig{},
		nick:    "alice",
		channel: "#test",
		network: Network{Name: "testnet"},
		ctx:     context.Background(),
		logger:  logxi.New("test"),
	}
	assert.Equal(t, "", cr.renderAPIUser())
}

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
