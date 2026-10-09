package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"text/template"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupSessionWithResponseID(t *testing.T, responseID string) int64 {
	t.Helper()
	db := setupTestDB(t)
	_ = db

	sid, err := sessionMgr.CreateSession("testnet", "#101", ensureTestUser(t, "testnet", "shrew"), "testcmd", "testservice", "testmodel")
	require.NoError(t, err)
	if responseID != "" {
		// "test-model" matches the cfg.Model used by this helper's callers'
		// runners, so the response_model chain guard lets chaining proceed.
		require.NoError(t, sessionMgr.UpdateResponseID(sid, &responseID, "test-model"))
	}

	return sid
}

func TestExecuteToolCalls_SingleToolSendsCallNotice(t *testing.T) {
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
		logger:   logxi.New("test"),
		ctx:      context.Background(),
		outputCh: outputCh,
	}

	toolCalls := []ToolCall{
		{ID: "tc1", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}},
	}

	go cr.executeToolCalls(newTurnContext(0, nil), toolCalls)

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
		logger:   logxi.New("test"),
		ctx:      context.Background(),
		outputCh: outputCh,
	}

	toolCalls := []ToolCall{
		{ID: "tc1", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}},
		{ID: "tc2", Function: FunctionCall{Name: "tool_b", Arguments: "{}"}},
	}

	go cr.executeToolCalls(newTurnContext(0, nil), toolCalls)

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
	setupNoticesDefaults(t)
	configMu.Lock()
	config.HiddenTools = []string{"register_background_job", "ban_user", "check_ban_history"}
	configMu.Unlock()
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
		logger:   logxi.New("test"),
		ctx:      context.Background(),
		outputCh: outputCh,
	}

	toolCalls := []ToolCall{
		{ID: "tc1", Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j1","tool_name":"t","server_name":"s"}`}},
		{ID: "tc2", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}},
	}

	go cr.executeToolCalls(newTurnContext(0, nil), toolCalls)

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
		name      string
		template  string
		nick      string
		channel   string
		network   string
		sessionID int64
		expected  string
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
		{
			name:      "with session id",
			template:  "irc:{{.Nick}}:s{{.SessionID}}",
			nick:      "erin",
			sessionID: 1234,
			expected:  "irc:erin:s1234",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := template.New("test").Parse(tt.template)
			require.NoError(t, err)

			cr := &chatRunner{
				cfg:       AIConfig{apiUserTmpl: tmpl},
				nick:      tt.nick,
				channel:   tt.channel,
				network:   Network{Name: tt.network, Nick: "testbot"},
				sessionID: tt.sessionID,
				ctx:       context.Background(),
				logger:    logxi.New("test"),
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

func TestAPIIdentityMatrix(t *testing.T) {
	tmpl, err := template.New("api_user").Parse("user-v")
	require.NoError(t, err)

	tests := []struct {
		name         string
		baseURL      string
		responsesAPI bool
		want         apiIdentity
	}{
		{
			name:    "openai chat completions",
			baseURL: "https://api.openai.com/v1",
			want:    apiIdentity{SafetyID: "user-v", CacheKey: "user-v"},
		},
		{
			name:         "openai responses",
			baseURL:      "https://api.openai.com/v1",
			responsesAPI: true,
			want:         apiIdentity{SafetyID: "user-v", CacheKey: "user-v"},
		},
		{
			name:    "xai chat completions",
			baseURL: "https://api.x.ai/v1",
			want:    apiIdentity{SafetyID: "user-v"},
		},
		{
			name:         "xai responses",
			baseURL:      "https://api.x.ai/v1",
			responsesAPI: true,
			want:         apiIdentity{SafetyID: "user-v", CacheKey: "user-v"},
		},
		{
			name:    "openrouter chat completions",
			baseURL: "https://openrouter.ai/api/v1",
			want:    apiIdentity{User: "user-v"},
		},
		{
			name:         "openrouter responses",
			baseURL:      "https://openrouter.ai/api/v1",
			responsesAPI: true,
			want:         apiIdentity{SafetyID: "user-v"},
		},
		{
			name:    "unknown provider chat completions",
			baseURL: "https://llm.example.com/v1",
			want:    apiIdentity{User: "user-v"},
		},
		{
			name:         "unknown provider responses",
			baseURL:      "https://llm.example.com/v1",
			responsesAPI: true,
			want:         apiIdentity{User: "user-v"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &chatRunner{
				cfg:     AIConfig{apiUserTmpl: tmpl},
				baseURL: tt.baseURL,
				nick:    "alice",
				channel: "#chan",
				network: Network{Name: "testnet"},
				ctx:     context.Background(),
				logger:  logxi.New("test"),
			}

			assert.Equal(t, tt.want, cr.apiIdentity(tt.responsesAPI))
		})
	}

	t.Run("no template sends nothing", func(t *testing.T) {
		cr := &chatRunner{
			cfg:     AIConfig{},
			baseURL: "https://api.openai.com/v1",
			ctx:     context.Background(),
			logger:  logxi.New("test"),
		}
		assert.Equal(t, apiIdentity{}, cr.apiIdentity(true))
		assert.Equal(t, apiIdentity{}, cr.apiIdentity(false))
	})
}

func TestProviderServiceDetection(t *testing.T) {
	tests := []struct {
		name           string
		baseURL        string
		wantOpenAI     bool
		wantGrok       bool
		wantOpenRouter bool
	}{
		{name: "openai", baseURL: "https://api.openai.com/v1", wantOpenAI: true},
		{name: "openai subdomain", baseURL: "https://eu.api.openai.com/v1", wantOpenAI: true},
		{name: "azure-like host is not openai", baseURL: "https://foo.openai.azure.com/v1"},
		{name: "grok", baseURL: "https://api.x.ai/v1", wantGrok: true},
		{name: "grok subdomain", baseURL: "https://api.staging.x.ai/v1", wantGrok: true},
		{name: "xai lookalike is not grok", baseURL: "https://x.ai.example.com/v1"},
		{name: "openrouter", baseURL: "https://openrouter.ai/api/v1", wantOpenRouter: true},
		{name: "openrouter subdomain", baseURL: "https://api.openrouter.ai/v1", wantOpenRouter: true},
		{name: "invalid url", baseURL: "://bad"},
		{name: "empty", baseURL: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantOpenAI, isOpenAIService(tt.baseURL), "isOpenAIService")
			assert.Equal(t, tt.wantGrok, isGrokService(tt.baseURL), "isGrokService")
			assert.Equal(t, tt.wantOpenRouter, isOpenRouterService(tt.baseURL), "isOpenRouterService")
		})
	}
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

// makeResponsesAPIToolCallResponse builds a Responses API payload whose
// output carries a single function_call item, driving the tool-call loop.
func makeResponsesAPIToolCallResponse(id, name, args string) map[string]any {
	return map[string]any{
		"id":     id,
		"object": "response",
		"model":  "test-model",
		"output": []any{
			map[string]any{
				"type":      "function_call",
				"id":        "fc_" + id,
				"call_id":   "call_" + id,
				"name":      name,
				"arguments": args,
				"status":    "completed",
			},
		},
	}
}

// makeResponsesRunner builds a chatRunner pointed at a Responses API test
// server, for tests that drive runTurn end to end.
func makeResponsesRunner(t *testing.T, serverURL string, sid int64, cfg AIConfig) *chatRunner {
	t.Helper()
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(serverURL+"/v1"),
	)
	transport := newDaveTransport(nil, nil)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	return &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		cfg:          cfg,
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		userID:       ensureTestUser(t, "testnet", "shrew"),
		sessionID:    sid,
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     make(chan string, 100),
	}
}

// parseRecordedRequestBody decodes a captured Responses API request body
// into its previous_response_id ("" when absent) and input items. Must run
// on the test goroutine, not the HTTP handler goroutine.
func parseRecordedRequestBody(t *testing.T, raw string) (prevID string, input []json.RawMessage) {
	t.Helper()
	var body map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &body))
	if rawID, ok := body["previous_response_id"]; ok {
		require.NoError(t, json.Unmarshal(rawID, &prevID))
	}
	if rawInput, ok := body["input"]; ok {
		require.NoError(t, json.Unmarshal(rawInput, &input))
	}
	return prevID, input
}

func TestRunTurnResponses_ConcurrentSerialization(t *testing.T) {
	setupSessionWithResponseID(t, "resp-initial")

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

	cfg := AIConfig{
		Model:              "test-model",
		ResponsesAPI:       true,
		PreviousResponseID: true,

		Timeout: 10 * time.Second,
	}

	session, _ := sessionMgr.GetActiveSession("testnet", "#101", ensureTestUser(t, "testnet", "shrew"))
	require.NotNil(t, session)
	shrewUserID := ensureTestUser(t, "testnet", "shrew")

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
			userID:       shrewUserID,
			sessionID:    session.ID,
			logger:       logxi.New("test"),
			ctx:          context.Background(),
			outputCh:     make(chan string, 100),
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		sessionMgr.AddMessage(session.ID, ChatMessage{Role: RoleUser, Content: "msg 1"})
		messages, _ := sessionMgr.GetMessages(session.ID)
		runner := makeRunner()
		runner.runTurn(newTurnContext(runner.sessionID, messages))
	}()

	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		sessionMgr.AddMessage(session.ID, ChatMessage{Role: RoleSystem, Content: "bg job result"})
		messages, _ := sessionMgr.GetMessages(session.ID)
		runner := makeRunner()
		runner.runTurn(newTurnContext(runner.sessionID, messages))
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
	setupTestDB(t)

	cfg := AIConfig{
		Model:              "test-model",
		ResponsesAPI:       true,
		PreviousResponseID: true,

		Timeout: 10 * time.Second,
	}

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

	sid1, err := sessionMgr.CreateSession("testnet", "#101", ensureTestUser(t, "testnet", "alice"), "testcmd", "svc", "model")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid1, strPtrOrNil("resp-alice"), "test-model"))

	sid2, err := sessionMgr.CreateSession("testnet", "#101", ensureTestUser(t, "testnet", "bob"), "testcmd", "svc", "model")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid2, strPtrOrNil("resp-bob"), "test-model"))

	makeRunner := func(sessionID int64, nick string, userID int64) *chatRunner {
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
			userID:       userID,
			sessionID:    sessionID,
			logger:       logxi.New("test"),
			ctx:          context.Background(),
			outputCh:     make(chan string, 100),
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		sessionMgr.AddMessage(sid1, ChatMessage{Role: RoleUser, Content: "msg"})
		messages, _ := sessionMgr.GetMessages(sid1)
		runner := makeRunner(sid1, "alice", ensureTestUser(t, "testnet", "alice"))
		runner.runTurn(newTurnContext(sid1, messages))
	}()

	go func() {
		defer wg.Done()
		sessionMgr.AddMessage(sid2, ChatMessage{Role: RoleUser, Content: "msg"})
		messages, _ := sessionMgr.GetMessages(sid2)
		runner := makeRunner(sid2, "bob", ensureTestUser(t, "testnet", "bob"))
		runner.runTurn(newTurnContext(sid2, messages))
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

// TestRunTurnResponsesSkipsChainOnModelChange covers the Layer 1 chain guard:
// a stored response produced by a different model must never be chained —
// cross-model previous_response_id either errors with wording we may not
// recognize or silently drops the prior assistant history (no error, so no
// recovery would be possible). The guard falls back to full history, exactly
// like an expired chain. Legacy NULL response_model rows still chain and rely
// on the isResponseIDError net.
func TestRunTurnResponsesSkipsChainOnModelChange(t *testing.T) {
	setupTestDB(t)

	var (
		mu      sync.Mutex
		prevIDs []string
		inputs  []int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)

		var prevID string
		if raw, ok := body["previous_response_id"]; ok {
			json.Unmarshal(raw, &prevID)
		}
		var input []json.RawMessage
		json.Unmarshal(body["input"], &input)

		mu.Lock()
		prevIDs = append(prevIDs, prevID)
		inputs = append(inputs, len(input))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(makeResponsesAPIResponse("resp-new", "ok"))
	}))
	defer server.Close()

	seed := func(t *testing.T, responseModel *string) *Session {
		t.Helper()
		sid, err := sessionMgr.CreateSession("testnet", "#101", ensureTestUser(t, "testnet", "shrew"), "testcmd", "svc", "m")
		require.NoError(t, err)
		require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sid).
			Updates(map[string]interface{}{"response_id": "resp-old", "response_model": responseModel}).Error)
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "one"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "two"}))
		s, err := sessionMgr.GetSession(sid)
		require.NoError(t, err)
		return s
	}

	runTurn := func(t *testing.T, session *Session, model string) (prevID string, inputLen int) {
		t.Helper()
		cfg := AIConfig{
			Model:              model,
			ResponsesAPI:       true,
			PreviousResponseID: true,

			Timeout: 10 * time.Second,
		}
		client := openai.NewClient(
			option.WithAPIKey("test-key"),
			option.WithBaseURL(server.URL+"/v1"),
		)
		transport := newDaveTransport(nil, nil)
		runner := &chatRunner{
			openaiClient: &client,
			transport:    transport,
			httpClient:   &http.Client{Transport: transport},
			cfg:          cfg,
			network:      Network{Name: "testnet"},
			channel:      "#101",
			nick:         "shrew",
			userID:       ensureTestUser(t, "testnet", "shrew"),
			sessionID:    session.ID,
			logger:       logxi.New("test"),
			ctx:          context.Background(),
			outputCh:     make(chan string, 100),
		}
		messages, err := sessionMgr.GetMessages(session.ID)
		require.NoError(t, err)
		runner.runTurn(newTurnContext(runner.sessionID, messages))

		mu.Lock()
		defer mu.Unlock()
		require.NotEmpty(t, prevIDs, "expected an API call")
		return prevIDs[len(prevIDs)-1], inputs[len(inputs)-1]
	}

	t.Run("model change skips chain and sends full history", func(t *testing.T) {
		s := seed(t, strPtrOrNil("old-model"))
		prevID, inputLen := runTurn(t, s, "new-model")
		assert.Empty(t, prevID, "previous_response_id must be omitted on model change")
		assert.Equal(t, 3, inputLen, "full history (system + 2 user messages) must be sent when the chain is skipped")
	})

	t.Run("matching model chains last message only", func(t *testing.T) {
		s := seed(t, strPtrOrNil("same-model"))
		prevID, inputLen := runTurn(t, s, "same-model")
		assert.Equal(t, "resp-old", prevID, "matching model must chain")
		assert.Equal(t, 1, inputLen, "chained turn sends only the new message")
	})

	t.Run("legacy null response_model still chains", func(t *testing.T) {
		s := seed(t, nil)
		prevID, _ := runTurn(t, s, "any-model")
		assert.Equal(t, "resp-old", prevID, "NULL response_model (legacy) must still chain")
	})
}

// TestRunTurnResponsesEmptyRetryDropsChainAndCorrects pins the Responses API
// empty-retry behavior: the first (chained) attempt returns a reasoning-only
// response; the retry must drop previous_response_id (handleResponseIDSave
// already cleared the stored id — chaining the old head on top of a
// full-history resend would duplicate context), send the full history plus
// the persisted correction (design D7), and a successful retry
// re-establishes the chain.
func TestRunTurnResponsesEmptyRetryDropsChainAndCorrects(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "shrew")
	sid, err := sessionMgr.CreateSession("testnet", "#101", userID, "testcmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sid).
		Updates(map[string]interface{}{"response_id": "resp-old", "response_model": "m"}).Error)
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "one"}))

	emptyReasoningOnly := func(id string) map[string]any {
		return map[string]any{
			"id": id, "object": "response", "model": "m",
			"output": []any{map[string]any{
				"type": "reasoning", "id": "rs_" + id,
				"summary": []any{map[string]any{"type": "summary_text", "text": "answer stranded in reasoning"}},
			}},
		}
	}

	var (
		mu       sync.Mutex
		prevIDs  []string
		inputs   []string
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		// No require/assert here: testify FailNow must not run off the
		// handler goroutine; a malformed body simply yields empty fields.
		_ = json.Unmarshal(raw, &body)

		var prevID string
		if raw, ok := body["previous_response_id"]; ok {
			json.Unmarshal(raw, &prevID)
		}
		inputJSON, _ := json.Marshal(body["input"])

		mu.Lock()
		n := len(prevIDs)
		prevIDs = append(prevIDs, prevID)
		inputs = append(inputs, string(inputJSON))
		requests = append(requests, string(raw))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			json.NewEncoder(w).Encode(emptyReasoningOnly("resp-empty"))
			return
		}
		json.NewEncoder(w).Encode(makeResponsesAPIResponse("resp-good", "recovered answer"))
	}))
	defer server.Close()

	cfg := AIConfig{
		Model:              "m",
		ResponsesAPI:       true,
		PreviousResponseID: true,
		RetryOnEmpty:       intPtr(1),

		Timeout: 10 * time.Second,
	}
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL+"/v1"),
	)
	transport := newDaveTransport(nil, nil)
	outputCh := make(chan string, 100)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	runner := &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		cfg:          cfg,
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		userID:       userID,
		sessionID:    sid,
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
	}
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	runner.runTurn(newTurnContext(sid, messages))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, prevIDs, 2, "expected initial attempt + 1 retry, got %d", len(prevIDs))
	assert.Equal(t, "resp-old", prevIDs[0], "initial attempt chains the stored response")
	assert.Empty(t, prevIDs[1], "retry must drop previous_response_id (full-history resend would duplicate context)")
	assert.NotContains(t, inputs[1], "resp-old")
	assert.Contains(t, requests[1], "EMPTY response", "retry must carry the correction")
	assert.Contains(t, inputs[1], `"role":"system"`, "retry input must include the correction payload under the default Knob 1 role")
	assert.Contains(t, inputs[1], "automated notice", "correction must be marked as not-from-the-user")
	assert.Contains(t, inputs[1], "one", "retry must resend the full history")

	lines := drainOutput(t, outputCh, 4, time.Second)
	assert.Contains(t, strings.Join(lines, "\n"), "recovered answer")

	// The successful retry re-establishes the chain in the DB.
	s, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, s.ResponseID)
	assert.Equal(t, "resp-good", *s.ResponseID)
}

// TestRunTurnResponsesDisabledPrevIDSendsNoID reproduces the OpenRouter
// incident (2026-10-09, session 1806): a command with responses_api = true
// but previous_response_id = false still attached the session's stored
// response_id to the request. OpenRouter's stateless Responses proxy
// rejects any previous_response_id with 400 invalid_prompt, so the stored
// id must only ever be sent when chaining is actually enabled.
func TestRunTurnResponsesDisabledPrevIDSendsNoID(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	// The stored id is a real OpenRouter generation id, saved by an earlier
	// successful turn (handleResponseIDSave stores every response id).
	sid := setupSessionWithResponseID(t, "gen-1791535207-prodIncident")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "one"}))

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(makeResponsesAPIResponse("resp-ok", "fine"))
	}))
	defer server.Close()

	cfg := AIConfig{
		Model:              "test-model",
		ResponsesAPI:       true,
		PreviousResponseID: false,
		Timeout:            10 * time.Second,
	}
	runner := makeResponsesRunner(t, server.URL, sid, cfg)
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	runner.runTurn(newTurnContext(sid, messages))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1, "expected exactly one API call")
	prevID, input := parseRecordedRequestBody(t, bodies[0])
	assert.Empty(t, prevID, "previous_response_id must NOT be sent when the command disables it")
	assert.Len(t, input, 2, "full history (system + user) must be sent when chaining is disabled")
}

// TestRunTurnResponsesDisabledPrevIDToolLoopSendsNoID pins the tool-call
// round of the disabled-chaining path: the follow-up request after a tool
// result must also be id-free and carry the full history (the model's
// function_call + the tool result ride along as input items).
func TestRunTurnResponsesDisabledPrevIDToolLoopSendsNoID(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := setupSessionWithResponseID(t, "gen-toolloop")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "one"}))

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			json.NewEncoder(w).Encode(makeResponsesAPIToolCallResponse("resp-1", "fake_tool", "{}"))
			return
		}
		json.NewEncoder(w).Encode(makeResponsesAPIResponse("resp-2", "done"))
	}))
	defer server.Close()

	cfg := AIConfig{
		Model:              "test-model",
		ResponsesAPI:       true,
		PreviousResponseID: false,
		Timeout:            10 * time.Second,
	}
	runner := makeResponsesRunner(t, server.URL, sid, cfg)
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	runner.runTurn(newTurnContext(sid, messages))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2, "expected tool round-trip (call + follow-up)")
	for i, raw := range bodies {
		prevID, input := parseRecordedRequestBody(t, raw)
		assert.Empty(t, prevID, "request %d: previous_response_id must NOT be sent when chaining is disabled", i+1)
		if i == 1 {
			// Full history resend: system + user + assistant function_call
			// + tool result.
			assert.GreaterOrEqual(t, len(input), 4, "follow-up must resend the full history, not just the tool result")
			var joined string
			for _, item := range input {
				joined += string(item)
			}
			assert.Contains(t, joined, "function_call", "follow-up input must carry the model's function_call")
			assert.Contains(t, joined, "function_call_output", "follow-up input must carry the tool result")
		}
	}
}

// TestRunTurnResponsesToolLoopChainsFreshID guards the chaining half of the
// invariant: with previous_response_id enabled but NO stored id (the first
// turn of a session), the opening request must be id-free full history, and
// the follow-up after a tool round-trip must chain the id saved from THIS
// turn's response, sending only the tool results as input. Gating the
// previous_response_id param must never break this path.
func TestRunTurnResponsesToolLoopChainsFreshID(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			setupTestDB(t)
			setupNoticesDefaults(t)

			sid := setupSessionWithResponseID(t, "")
			require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
			require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "one"}))

			var mu sync.Mutex
			var bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				n := len(bodies)
				bodies = append(bodies, string(raw))
				mu.Unlock()
				var payload map[string]any
				if n == 0 {
					payload = makeResponsesAPIToolCallResponse("resp-1", "fake_tool", "{}")
				} else {
					payload = makeResponsesAPIResponse("resp-2", "done")
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": payload})
					fmt.Fprintf(w, "data: %s\n\n", event)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(payload)
			}))
			defer server.Close()

			cfg := AIConfig{
				Model:              "test-model",
				ResponsesAPI:       true,
				PreviousResponseID: true,
				Streaming:          streaming,
				Timeout:            10 * time.Second,
				StreamTimeout:      10 * time.Second,
			}
			runner := makeResponsesRunner(t, server.URL, sid, cfg)
			messages, err := sessionMgr.GetMessages(sid)
			require.NoError(t, err)
			runner.runTurn(newTurnContext(sid, messages))

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, bodies, 2, "expected tool round-trip (call + follow-up)")

			prevID0, input0 := parseRecordedRequestBody(t, bodies[0])
			assert.Empty(t, prevID0, "opening request of an id-less session must not send previous_response_id")
			assert.Len(t, input0, 2, "opening request must send the full history")

			prevID1, input1 := parseRecordedRequestBody(t, bodies[1])
			assert.Equal(t, "resp-1", prevID1, "tool-loop follow-up must chain the id saved from this turn's response")
			assert.Len(t, input1, 1, "chained follow-up must send only the tool result")
			assert.Contains(t, string(input1[0]), "function_call_output", "follow-up input must be the tool result")
		})
	}
}

// TestRunTurnResponsesProxyRejectRetriesWithoutID covers the Layer 2 net
// for stateless Responses proxies (OpenRouter): when chaining IS enabled
// and the proxy rejects previous_response_id outright, the turn must retry
// once with the full history and no id instead of surfacing the 400 to the
// user. OpenRouter's rejection is 400 invalid_prompt with a
// "previous_response_id is not supported on this proxy" message — wording
// the 404/expired-id cases never see.
func TestRunTurnResponsesProxyRejectRetriesWithoutID(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := setupSessionWithResponseID(t, "gen-rejected")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "one"}))

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(raw))
		mu.Unlock()
		if n == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "invalid_prompt",
					"message": "previous_response_id is not supported on this proxy. Each response request is independent.",
				},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(makeResponsesAPIResponse("resp-good", "recovered answer"))
	}))
	defer server.Close()

	cfg := AIConfig{
		Model:              "test-model",
		ResponsesAPI:       true,
		PreviousResponseID: true,
		Timeout:            10 * time.Second,
	}
	outputCh := make(chan string, 100)
	runner := makeResponsesRunner(t, server.URL, sid, cfg)
	runner.outputCh = outputCh
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	runner.runTurn(newTurnContext(sid, messages))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2, "expected initial chained attempt + 1 retry without the id")
	prevID0, _ := parseRecordedRequestBody(t, bodies[0])
	assert.Equal(t, "gen-rejected", prevID0, "initial attempt chains the stored id")
	prevID1, input1 := parseRecordedRequestBody(t, bodies[1])
	assert.Empty(t, prevID1, "retry must drop previous_response_id")
	assert.Len(t, input1, 2, "retry must resend the full history")

	lines := drainOutput(t, outputCh, 2, time.Second)
	assert.Contains(t, strings.Join(lines, "\n"), "recovered answer")
}

func TestHandleResponseIDSave_SavesToRunnerSessionNotActive(t *testing.T) {
	setupTestDB(t)

	userID := ensureTestUser(t, "testnet", "shrew")
	sid1, err := sessionMgr.CreateSession("testnet", "#101", userID, "chat", "openai", "model-a")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid1, strPtrOrNil("resp-old"), "model-a"))

	sid2, err := sessionMgr.CreateSession("testnet", "#101", userID, "grk", "grok", "model-b")
	require.NoError(t, err)

	_, err = sessionMgr.SwitchActive("testnet", "#101", userID, sid2)
	require.NoError(t, err)

	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)

	transport := newDaveTransport(nil, nil)
	cr := &chatRunner{
		sessionID: sid1,
		network:   Network{Name: "testnet"},
		channel:   "#101",
		userID:    userID,
		logger:    logger,
		transport: transport,
	}

	result := cr.handleResponseIDSave("resp-new", "hello", nil, "resp-old")
	assert.Equal(t, "resp-new", result)

	s1, _ := sessionMgr.GetSession(sid1)
	require.NotNil(t, s1.ResponseID)
	assert.Equal(t, "resp-new", *s1.ResponseID, "response ID should be saved to runner's session (sid1)")

	s2, _ := sessionMgr.GetSession(sid2)
	assert.Nil(t, s2.ResponseID, "response ID should NOT leak to the active session (sid2)")
}

func TestHandleResponseIDSave_ClearsRunnerSessionNotActive(t *testing.T) {
	setupTestDB(t)

	userID := ensureTestUser(t, "testnet", "shrew")
	sid1, err := sessionMgr.CreateSession("testnet", "#101", userID, "chat", "openai", "model-a")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid1, strPtrOrNil("resp-old"), "model-a"))

	sid2, err := sessionMgr.CreateSession("testnet", "#101", userID, "grk", "grok", "model-b")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid2, strPtrOrNil("resp-grk"), "model-b"))

	_, err = sessionMgr.SwitchActive("testnet", "#101", userID, sid2)
	require.NoError(t, err)

	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)

	transport := newDaveTransport(nil, nil)
	cr := &chatRunner{
		sessionID: sid1,
		network:   Network{Name: "testnet"},
		channel:   "#101",
		userID:    userID,
		logger:    logger,
		transport: transport,
	}

	result := cr.handleResponseIDSave("resp-empty", "", nil, "resp-old")
	assert.Equal(t, "resp-old", result)

	s1, _ := sessionMgr.GetSession(sid1)
	assert.Nil(t, s1.ResponseID, "runner's session should have response_id cleared")

	s2, _ := sessionMgr.GetSession(sid2)
	require.NotNil(t, s2.ResponseID)
	assert.Equal(t, "resp-grk", *s2.ResponseID, "active session's response_id should be untouched")
}

func TestIsToolDisabled(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		disabled []string
		want     bool
	}{
		{name: "empty disabled list", tool: "ban_user", disabled: nil, want: false},
		{name: "tool in disabled list", tool: "ban_user", disabled: []string{"ban_user"}, want: true},
		{name: "tool not in disabled list", tool: "ban_user", disabled: []string{"check_ban_history"}, want: false},
		{name: "multiple disabled includes tool", tool: "register_background_job", disabled: []string{"ban_user", "register_background_job"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isToolDisabled(tt.tool, tt.disabled))
		})
	}
}

func TestIsToolHidden(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		hidden []string
		want   bool
	}{
		{name: "empty hidden list", tool: "ban_user", hidden: nil, want: false},
		{name: "tool in hidden list", tool: "ban_user", hidden: []string{"ban_user"}, want: true},
		{name: "tool not in hidden list", tool: "ban_user", hidden: []string{"register_background_job"}, want: false},
		{name: "multiple hidden includes tool", tool: "check_ban_history", hidden: []string{"ban_user", "check_ban_history"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isToolHidden(tt.tool, tt.hidden))
		})
	}
}

func TestGetBuiltinToolDefsFiltering(t *testing.T) {
	configMu.Lock()
	config.Bans.MaxDuration = "6h"
	config.Bans.DefaultDuration = "5m"
	configMu.Unlock()

	allTools := getBuiltinToolDefs(AIConfig{}, nil)
	assert.Len(t, allTools, 3, "all builtin tools should be returned with nil disabled")

	allToolsEmpty := getBuiltinToolDefs(AIConfig{}, []string{})
	assert.Len(t, allToolsEmpty, 3, "empty disabled list should return all tools")

	filteredBan := getBuiltinToolDefs(AIConfig{}, []string{"ban_user"})
	assert.Len(t, filteredBan, 2, "disabling ban_user should leave 2 tools")
	names := make(map[string]bool, len(filteredBan))
	for _, tool := range filteredBan {
		names[tool.Function.Name] = true
	}
	assert.True(t, names["register_background_job"], "register_background_job should remain")
	assert.True(t, names["check_ban_history"], "check_ban_history should remain")
	assert.False(t, names["ban_user"], "ban_user should be filtered out")

	filteredAll := getBuiltinToolDefs(AIConfig{}, []string{"register_background_job", "ban_user", "check_ban_history"})
	assert.Len(t, filteredAll, 0, "disabling all tools should return empty")
}

// TestGetBuiltinToolDefsBackgroundJobRoleDescription pins the per-config
// register_background_job description: the model is told the wire role its
// background results will arrive under (guidance Knob 1), so a
// user-role config never promises a system message.
func TestGetBuiltinToolDefsBackgroundJobRoleDescription(t *testing.T) {
	find := func(tools []Tool) *FunctionDefinition {
		for i := range tools {
			if tools[i].Function.Name == backgroundJobToolName {
				return tools[i].Function
			}
		}
		return nil
	}

	def := find(getBuiltinToolDefs(AIConfig{}, nil))
	require.NotNil(t, def)
	assert.Contains(t, def.Description, "in a system message", "default Knob 1 role wording")

	def = find(getBuiltinToolDefs(AIConfig{InjectionRole: RoleUser}, nil))
	require.NotNil(t, def)
	assert.Contains(t, def.Description, "in a user message")
	assert.NotContains(t, def.Description, "in a system message")

	def = find(getBuiltinToolDefs(AIConfig{InjectionRole: RoleDeveloper}, nil))
	require.NotNil(t, def)
	assert.Contains(t, def.Description, "in a developer message")

	// Tool-delivery mode: passive delivery fact, no role wording and no
	// meta-instruction inviting tool-call reasoning.
	def = find(getBuiltinToolDefs(AIConfig{AsyncResultDelivery: asyncDeliveryTool}, nil))
	require.NotNil(t, def)
	assert.Contains(t, def.Description, "delivered as a tool response")
	assert.NotContains(t, def.Description, "in a system message")
	assert.NotContains(t, def.Description, "as you would any tool result")
}

// TestToolDefsForConfig pins the cfg-level tool-assembly contract that
// /tokencount and the compaction trigger log reuse: a config with no
// MCP servers yields NO tools (builtins are appended only when MCP
// tools exist — a tool-less config has nothing to call), and a config
// pointing at a registered server yields that server's live tools plus
// the builtins. The fixture seeds the mcpServers map directly: getMCPTools
// reads only that in-memory map (no MCP I/O — the same guarantee that
// makes toolDefsForConfig safe to call from accounting paths), so no
// live server or transport is needed to exercise the real assembly
// logic. This mirrors the fixture pattern of TestGetMCPToolsHidden.
func TestToolDefsForConfig(t *testing.T) {
	fixtureServer := &MCPServer{
		Tools: []*mcp.Tool{
			{Name: "generate_image", Description: "Generate an image"},
			{Name: "get_transcript", Description: "Fetch a transcript"},
		},
	}
	origServers := mcpServers
	mcpServers = map[string]*MCPServer{"img-mcp": fixtureServer}
	t.Cleanup(func() { mcpServers = origServers })

	toolNames := func(tools []Tool) map[string]bool {
		names := make(map[string]bool, len(tools))
		for _, tool := range tools {
			require.NotNil(t, tool.Function, "every assembled tool must be a serializable function definition")
			names[tool.Function.Name] = true
		}
		return names
	}

	t.Run("no MCPs configured yields no tools, builtins withheld", func(t *testing.T) {
		assert.Empty(t, toolDefsForConfig(AIConfig{Name: "chat"}))
	})

	t.Run("unknown server name contributes nothing", func(t *testing.T) {
		assert.Empty(t, toolDefsForConfig(AIConfig{Name: "chat", MCPs: []string{"nonexistent"}}))
	})

	t.Run("MCP tools present pulls in the builtins", func(t *testing.T) {
		tools := toolDefsForConfig(AIConfig{Name: "chat", MCPs: []string{"img-mcp"}})
		names := toolNames(tools)
		assert.True(t, names["generate_image"], "live MCP tool included")
		assert.True(t, names["get_transcript"], "live MCP tool included")
		assert.True(t, names["register_background_job"], "builtins appended when MCP tools exist")
		assert.True(t, names["ban_user"], "builtins appended when MCP tools exist")
		assert.True(t, names["check_ban_history"], "builtins appended when MCP tools exist")
	})

	t.Run("hidden MCP tools are excluded", func(t *testing.T) {
		tools := toolDefsForConfig(AIConfig{Name: "chat", MCPs: []string{"img-mcp"}, HiddenMCPTools: []string{"get_transcript"}})
		names := toolNames(tools)
		assert.True(t, names["generate_image"])
		assert.False(t, names["get_transcript"], "hidden tool must not be offered (or counted)")
	})
}

func TestRegisterBackgroundJob_ServerNameAutoDetection(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	setupNoticesDefaults(t)

	mcpServersMu.Lock()
	origToolMap := mcpToolToServer
	origServers := mcpServers
	mcpToolToServer = map[string]string{
		"generate_image_async":       "img-mcp-async",
		"enhance_and_generate_async": "img-mcp-async",
	}
	mcpServers = map[string]*MCPServer{"img-mcp-async": {}}
	mcpServersMu.Unlock()
	defer func() {
		mcpServersMu.Lock()
		mcpToolToServer = origToolMap
		mcpServers = origServers
		mcpServersMu.Unlock()
	}()

	verbose := false
	uid := ensureTestUser(t, "testnet", "testuser")
	cr := &chatRunner{
		cfg:      AIConfig{ToolVerbose: &verbose},
		network:  Network{Name: "testnet"},
		channel:  "#test",
		nick:     "test",
		logger:   logxi.New("test"),
		ctx:      context.Background(),
		outputCh: make(chan string, 20),
		userID:   uid,
	}

	sid, err := sessionMgr.CreateSession("testnet", "#test", uid, "testcmd", "testservice", "testmodel")
	require.NoError(t, err)
	cr.sessionID = sid

	t.Run("auto-detect server_name from tool_name", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-auto",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j-auto","tool_name":"enhance_and_generate_async"}`},
		}
		turn := newTurnContext(sid, nil)
		cr.handleRegisterBackgroundJob(turn, tc)
		require.Len(t, turn.Messages(), 1)

		pj := PendingJob{}
		require.NoError(t, theDB.Where("job_id = ?", "j-auto").First(&pj).Error)
		assert.Equal(t, "img-mcp-async", pj.MCPServer, "server_name should be auto-detected")
	})

	t.Run("explicit server_name still works", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-explicit",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j-explicit","tool_name":"enhance_and_generate_async","server_name":"img-mcp-async"}`},
		}
		turn := newTurnContext(sid, nil)
		cr.handleRegisterBackgroundJob(turn, tc)
		require.Len(t, turn.Messages(), 1)

		pj := PendingJob{}
		require.NoError(t, theDB.Where("job_id = ?", "j-explicit").First(&pj).Error)
		assert.Equal(t, "img-mcp-async", pj.MCPServer, "server_name should match explicit value")
	})

	t.Run("unknown tool_name returns error", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-unknown",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j-unknown","tool_name":"nonexistent_tool"}`},
		}
		turn := newTurnContext(sid, nil)
		cr.handleRegisterBackgroundJob(turn, tc)
		require.Len(t, turn.Messages(), 1)
		assert.Contains(t, turn.Messages()[0].Content, "error: could not determine MCP server")
	})

	t.Run("missing job_id returns error", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-nojob",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"tool_name":"enhance_and_generate_async"}`},
		}
		turn := newTurnContext(sid, nil)
		cr.handleRegisterBackgroundJob(turn, tc)
		require.Len(t, turn.Messages(), 1)
		assert.Contains(t, turn.Messages()[0].Content, "error: job_id and tool_name are required")
	})

	t.Run("missing tool_name returns error", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-notool",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j1"}`},
		}
		turn := newTurnContext(sid, nil)
		cr.handleRegisterBackgroundJob(turn, tc)
		require.Len(t, turn.Messages(), 1)
		assert.Contains(t, turn.Messages()[0].Content, "error: job_id and tool_name are required")
	})

	t.Run("empty server_name triggers auto-detect", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-emptyserver",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j-emptyserver","tool_name":"enhance_and_generate_async","server_name":""}`},
		}
		turn := newTurnContext(sid, nil)
		cr.handleRegisterBackgroundJob(turn, tc)
		require.Len(t, turn.Messages(), 1)

		pj := PendingJob{}
		require.NoError(t, theDB.Where("job_id = ?", "j-emptyserver").First(&pj).Error)
		assert.Equal(t, "img-mcp-async", pj.MCPServer, "empty server_name should trigger auto-detect")
	})
}

func TestCheckEmptyRetry(t *testing.T) {
	setupNoticesDefaults(t)

	tests := []struct {
		name            string
		content         string
		reasoning       string
		emptyRetries    int
		maxEmptyRetries int
		wantRetry       bool
		wantContent     string
		wantOutput      []string
	}{
		{
			name:            "content present, no retry",
			content:         "hello",
			reasoning:       "",
			maxEmptyRetries: 3,
			wantRetry:       false,
			wantContent:     "hello",
		},
		{
			name:            "both empty, retries remaining",
			content:         "",
			reasoning:       "",
			emptyRetries:    0,
			maxEmptyRetries: 3,
			wantRetry:       true,
			wantContent:     "",
		},
		{
			name:            "both empty, max retries reached",
			content:         "",
			reasoning:       "",
			emptyRetries:    3,
			maxEmptyRetries: 3,
			wantRetry:       false,
			wantContent:     "...",
			// the user must be told the turn failed instead of
			// silently waiting for a reply that already died
			wantOutput: []string{"empty response from model after 4 attempt(s)"},
		},
		{
			name:            "reasoning only, retries remaining",
			content:         "",
			reasoning:       "let me think about this",
			emptyRetries:    0,
			maxEmptyRetries: 3,
			wantRetry:       true,
			wantContent:     "",
		},
		{
			name:            "reasoning only, max retries reached",
			content:         "",
			reasoning:       "let me think about this",
			emptyRetries:    3,
			maxEmptyRetries: 3,
			wantRetry:       false,
			wantContent:     "...",
			// explanation notice + the reasoning content itself
			wantOutput: []string{"reasoning channel", "let me think about this"},
		},
		{
			name:            "reasoning only, zero max retries",
			content:         "",
			reasoning:       "thinking...",
			emptyRetries:    0,
			maxEmptyRetries: 0,
			wantRetry:       false,
			wantContent:     "...",
			wantOutput:      []string{"reasoning channel", "thinking..."},
		},
		{
			name:            "both empty, zero max retries",
			content:         "",
			reasoning:       "",
			emptyRetries:    0,
			maxEmptyRetries: 0,
			wantRetry:       false,
			wantContent:     "...",
			wantOutput:      []string{"empty response from model after 1 attempt(s)"},
		},
		{
			name:            "content present with reasoning, no retry",
			content:         "here is my answer",
			reasoning:       "let me think",
			maxEmptyRetries: 3,
			wantRetry:       false,
			wantContent:     "here is my answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputCh := make(chan string, 8)
			cr := &chatRunner{
				logger:   logxi.New("test"),
				ctx:      context.Background(),
				outputCh: outputCh,
				network:  Network{Name: "testnet"},
				channel:  "#test",
			}
			cr.logger.SetLevel(logxi.LevelAll)

			retry, content := cr.checkEmptyRetry(tt.content, tt.reasoning, tt.emptyRetries, tt.maxEmptyRetries)
			assert.Equal(t, tt.wantRetry, retry)
			assert.Equal(t, tt.wantContent, content)

			if len(tt.wantOutput) == 0 {
				// Negative cases: a spurious notice on a healthy or
				// still-retrying response would be a regression.
				assert.Empty(t, drainOutput(t, outputCh, 1, 50*time.Millisecond),
					"no IRC output expected for this case")
				return
			}
			lines := drainOutput(t, outputCh, len(tt.wantOutput), 100*time.Millisecond)
			require.Len(t, lines, len(tt.wantOutput), "unexpected IRC output: %q", lines)
			for i, want := range tt.wantOutput {
				assert.Contains(t, lines[i], want)
			}
		})
	}
}

// newEmptyRetryTurnRunner builds a chatRunner against a stub chat-completions
// server that replies with the given bodies in order (repeating the last one
// when exhausted), recording every request body it received.
func newEmptyRetryTurnRunner(t *testing.T, bodies []string, requests *[]string, outputCh chan string) *chatRunner {
	t.Helper()
	var mu sync.Mutex
	seq := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		idx := seq
		if idx >= len(bodies) {
			idx = len(bodies) - 1
		}
		seq++
		*requests = append(*requests, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, bodies[idx])
	}))
	t.Cleanup(server.Close)

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	return &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      server.URL + "/v1",
		apiKey:       "test-key",
		cfg:          AIConfig{Model: "m", Timeout: 10 * time.Second, RetryOnEmpty: intPtr(1)},
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
	}
}

// TestRunTurnEmptyResponseRetryInjectsCorrection verifies the retry carries a
// self-correction instruction the model can act on: the first response is
// reasoning-only (the production failure mode), and the retry must both tell
// the model what went wrong and succeed normally — with the correction
// persisted to the session exactly once (design D7: DB parity with what the
// API saw + standing guidance for later turns).
func TestRunTurnEmptyResponseRetryInjectsCorrection(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	var requests []string
	outputCh := make(chan string, 8)
	cr := newEmptyRetryTurnRunner(t, []string{
		// attempt 1: empty content, answer stranded in the reasoning field
		`{"id":"cmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning":"call poison control"},"finish_reason":"stop"}]}`,
		// attempt 2: proper answer
		`{"id":"cmpl-2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"here is the real answer"},"finish_reason":"stop"}]}`,
	}, &requests, outputCh)
	cr.sessionID = sid

	cr.runTurn(newTurnContext(sid, messages))

	require.Len(t, requests, 2, "expected one retry, got %d requests", len(requests))
	assert.Contains(t, requests[1], "EMPTY response", "retry request must carry the correction, got: %s", requests[1])
	assert.Contains(t, requests[1], "reasoning channel", "correction must name the reasoning-channel failure mode")
	assert.NotContains(t, requests[0], "EMPTY response", "first request must not carry a correction")

	lines := drainOutput(t, outputCh, 4, time.Second)
	require.NotEmpty(t, lines, "user must see the successful answer")
	assert.Contains(t, strings.Join(lines, "\n"), "here is the real answer")
	assert.NotContains(t, strings.Join(lines, "\n"), "reasoning channel", "no failure notice expected on a successful retry")

	// D7: the correction persists — the stored session is exactly what
	// the API saw, and the standing nudge keeps steering later turns.
	final, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	corrections := 0
	for _, msg := range final {
		if strings.Contains(msg.Content, "Your previous response was rejected") {
			corrections++
			assert.Equal(t, RoleSystem, msg.Role, "default Knob 1 payload role")
		}
	}
	assert.Equal(t, 1, corrections, "exactly one correction row may persist, got %d: %+v", corrections, final)
	assert.Equal(t, "here is the real answer", final[len(final)-1].Content)
}

// TestRunTurnEmptyResponseExhaustedShowsReasoningToUser verifies the
// user-facing failure path: when every attempt strands the answer in the
// reasoning channel, the user gets an explanatory notice plus the reasoning
// content itself, and the reasoning is NOT accepted as the assistant reply.
func TestRunTurnEmptyResponseExhaustedShowsReasoningToUser(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	var requests []string
	outputCh := make(chan string, 8)
	cr := newEmptyRetryTurnRunner(t, []string{
		`{"id":"cmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning":"first attempt reasoning"},"finish_reason":"stop"}]}`,
		`{"id":"cmpl-2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning":"the answer is forty-two"},"finish_reason":"stop"}]}`,
	}, &requests, outputCh)
	cr.sessionID = sid

	cr.runTurn(newTurnContext(sid, messages))

	require.Len(t, requests, 2, "expected initial attempt + 1 retry, got %d", len(requests))
	assert.Contains(t, requests[1], "EMPTY response", "retry must carry the correction")

	lines := drainOutput(t, outputCh, 6, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "reasoning channel", "explanation notice expected, got %q", lines)
	// The LAST attempt's reasoning is shown — that is the model's most
	// recent word, exactly like the pre-existing reasoning log line.
	assert.Contains(t, joined, "the answer is forty-two")
	assert.NotContains(t, joined, "first attempt reasoning")

	// The stored reply stays the "..." sentinel — reasoning is not a reply.
	final, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	last := final[len(final)-1]
	assert.Equal(t, RoleAssistant, last.Role)
	assert.Equal(t, "...", last.Content)
}

// TestRunTurnEmptyResponseExhaustedNoReasoning verifies the plain-empty
// exhaustion path tells the user the turn failed (previously the user was
// left waiting with no feedback at all).
func TestRunTurnEmptyResponseExhaustedNoReasoning(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	var requests []string
	outputCh := make(chan string, 8)
	cr := newEmptyRetryTurnRunner(t, []string{
		`{"id":"cmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
	}, &requests, outputCh)
	cr.sessionID = sid

	cr.runTurn(newTurnContext(sid, messages))

	lines := drainOutput(t, outputCh, 4, time.Second)
	assert.Contains(t, strings.Join(lines, "\n"), "empty response from model after", "failure notice expected, got %q", lines)
	assert.Contains(t, strings.Join(lines, "\n"), "retries exhausted", "notice must state retries were exhausted")
}

// TestAddEmptyResponseCorrection pins the correction shape (guidance
// Knob 1 payload role — system by default, user/developer when configured;
// Knob 2 suffix appended for anthropic-class models), persistence (D7 —
// rows go through turn.Add and reach the session store), and the
// back-to-back dedupe: one nudge per failure shape per turn.
func TestAddEmptyResponseCorrection(t *testing.T) {
	setupTestDB(t)
	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	turn := newTurnContext(sid, nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "real"})

	newRunner := func(cfg AIConfig) *chatRunner {
		logger := logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
		return &chatRunner{cfg: cfg, logger: logger}
	}

	t.Run("default role system, no suffix for plain models", func(t *testing.T) {
		cr := newRunner(AIConfig{Model: "qwen3"})
		cr.addEmptyResponseCorrection(turn, "some reasoning")
		require.Len(t, turn.Messages(), 2) // real + correction
		c := turn.Messages()[1]
		assert.Equal(t, RoleSystem, c.Role)
		assert.Contains(t, c.Content, "reasoning")
		assert.Contains(t, c.Content, "automated notice", "correction must be marked as not-from-the-user")

		// identical repeat collapses (in-turn dedupe)
		cr.addEmptyResponseCorrection(turn, "more reasoning")
		require.Len(t, turn.Messages(), 2)

		// different failure shape earns its own nudge, and its repeat collapses
		cr.addEmptyResponseCorrection(turn, "")
		require.Len(t, turn.Messages(), 3)
		assert.Contains(t, turn.Messages()[2].Content, "completely empty")
		cr.addEmptyResponseCorrection(turn, "")
		require.Len(t, turn.Messages(), 3)

		// D7: correction rows persist to the session store
		stored, err := sessionMgr.GetMessages(sid)
		require.NoError(t, err)
		require.Len(t, stored, 3, "correction rows must persist (design D7)")
		assert.Equal(t, RoleSystem, stored[1].Role)
		assert.Contains(t, stored[1].Content, "Your previous response was rejected")
		assert.Equal(t, RoleSystem, stored[2].Role)
	})

	t.Run("anthropic model gets the knob 2 user suffix", func(t *testing.T) {
		sid2 := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "anthropic/claude-4")
		turn2 := newTurnContext(sid2, nil)
		turn2.Add(ChatMessage{Role: RoleUser, Content: "q"})
		cr := newRunner(AIConfig{Model: "anthropic/claude-4"})

		cr.addEmptyResponseCorrection(turn2, "")
		require.Len(t, turn2.Messages(), 3) // user + system payload + user suffix
		assert.Equal(t, RoleSystem, turn2.Messages()[1].Role)
		assert.Equal(t, RoleUser, turn2.Messages()[2].Role)
		assert.Equal(t, correctionUserSuffix, turn2.Messages()[2].Content)

		// dedupe covers payload+suffix tail
		cr.addEmptyResponseCorrection(turn2, "")
		require.Len(t, turn2.Messages(), 3)
	})

	t.Run("explicit false suppresses even anthropic suffix", func(t *testing.T) {
		turn3 := newTurnContext(0, nil)
		cr := newRunner(AIConfig{Model: "anthropic/claude-4", NeedsUserSuffix: boolPtr(false)})
		cr.addEmptyResponseCorrection(turn3, "")
		require.Len(t, turn3.Messages(), 1)
		assert.Equal(t, RoleSystem, turn3.Messages()[0].Role)
	})

	t.Run("user payload role gets no suffix", func(t *testing.T) {
		turn4 := newTurnContext(0, nil)
		cr := newRunner(AIConfig{Model: "anthropic/claude-4", InjectionRole: RoleUser})
		cr.addEmptyResponseCorrection(turn4, "")
		require.Len(t, turn4.Messages(), 1)
		assert.Equal(t, RoleUser, turn4.Messages()[0].Role)
	})
}

// TestRunTurnStreamEmptyResponseRetry verifies the streaming chat
// completions retry path: a reasoning-only stream (deltas carry
// reasoning_content but never content) triggers a retry whose request
// carries the self-correction, and a good second stream reaches the user.
func TestRunTurnStreamEmptyResponseRetry(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	chunk := func(delta string, finish any) string {
		return fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", delta, finish)
	}
	reasoningOnlyStream := chunk(`{"role":"assistant","reasoning_content":"answer stranded in reasoning"}`, `null`) +
		chunk(`{}`, `"stop"`) + "data: [DONE]\n\n"
	goodStream := chunk(`{"role":"assistant","content":"streamed answer"}`, `null`) +
		chunk(`{}`, `"stop"`) + "data: [DONE]\n\n"

	var requests []string
	var mu sync.Mutex
	seq := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		idx := seq
		if idx > 1 {
			idx = 1
		}
		seq++
		requests = append(requests, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			fmt.Fprint(w, reasoningOnlyStream)
			return
		}
		fmt.Fprint(w, goodStream)
	}))
	defer server.Close()

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	outputCh := make(chan string, 8)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	cr := &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      server.URL + "/v1",
		apiKey:       "test-key",
		cfg:          AIConfig{Model: "m", Timeout: 10 * time.Second, RetryOnEmpty: intPtr(1), Streaming: true, StreamTimeout: 5 * time.Second},
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
	}
	cr.sessionID = sid

	cr.runTurn(newTurnContext(sid, messages))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 2, "expected one retry, got %d requests", len(requests))
	assert.NotContains(t, requests[0], "EMPTY response")
	assert.Contains(t, requests[1], "EMPTY response", "streaming retry must carry the correction")
	assert.Contains(t, requests[1], "reasoning channel")

	lines := drainOutput(t, outputCh, 4, time.Second)
	assert.Contains(t, strings.Join(lines, "\n"), "streamed answer")
}

// TestParseChatCompletionResponseReasoningContent pins the ExtraFields
// extraction for reasoning_content: the openai-go SDK has no typed field for
// it, so it lands in ExtraFields with status=invalid — Valid() is false but
// Raw() carries the value. Producers that strand their whole answer in
// reasoning_content (DeepSeek-style, llama-server reasoning models) must be
// detected so the reasoning-only failure path can show the user what happened.
func TestParseChatCompletionResponseReasoningContent(t *testing.T) {
	body := `{"id":"cmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"the answer is in the reasoning"},"finish_reason":"stop"}]}`
	var resp openai.ChatCompletion
	require.NoError(t, json.Unmarshal([]byte(body), &resp))

	content, reasoning, toolCalls, usage := parseChatCompletionResponse(resp)
	assert.Empty(t, content)
	assert.Equal(t, "the answer is in the reasoning", reasoning)
	assert.Nil(t, toolCalls)
	require.NotNil(t, usage)
	assert.Equal(t, "stop", usage.FinishReason)
}

// TestRunTurnStreamEmptyResponseExhausted verifies the streaming exhaustion
// path end to end: every attempt strands the answer in reasoning_content, so
// the user gets the explanation notice plus the reasoning itself — and only
// that (the "..." stored on the session is never sent, and the post-retry
// stream flush must not double-send anything).
func TestRunTurnStreamEmptyResponseExhausted(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	chunk := func(delta string, finish any) string {
		return fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", delta, finish)
	}
	reasoningOnlyStream := chunk(`{"role":"assistant","reasoning_content":"streamed reasoning answer"}`, `null`) +
		chunk(`{}`, `"stop"`) + "data: [DONE]\n\n"

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, reasoningOnlyStream)
	}))
	defer server.Close()

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	outputCh := make(chan string, 8)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	cr := &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      server.URL + "/v1",
		apiKey:       "test-key",
		cfg:          AIConfig{Model: "m", Timeout: 10 * time.Second, RetryOnEmpty: intPtr(1), Streaming: true, StreamTimeout: 5 * time.Second},
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
	}
	cr.sessionID = sid

	cr.runTurn(newTurnContext(sid, messages))

	assert.Equal(t, int32(2), atomic.LoadInt32(&hits), "initial attempt + 1 retry expected")

	lines := drainOutput(t, outputCh, 6, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "reasoning channel", "explanation notice expected, got %q", lines)
	assert.Contains(t, joined, "streamed reasoning answer", "reasoning must be shown to the user")
	assert.Equal(t, 1, strings.Count(joined, "streamed reasoning answer"), "reasoning must be sent exactly once (no double-send from the stream flush)")

	final, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	last := final[len(final)-1]
	assert.Equal(t, RoleAssistant, last.Role)
	assert.Equal(t, "...", last.Content, "reasoning is not accepted as the reply; the sentinel stays")
}

// TestRunTurnStreamReadsUsageTrailer pins the Oct 2026 production fix:
// the OpenAI spec streams a FINAL usage chunk (empty choices) AFTER the
// finish_reason chunk — dave used to close the stream at the finish
// chunk, so usage was NEVER captured on any streaming command. The loop
// must keep reading until [DONE].
func TestRunTurnStreamReadsUsageTrailer(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	chunk := func(delta string, finish any) string {
		return fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", delta, finish)
	}
	stream := chunk(`{"role":"assistant","content":"hello"}`, `null`) +
		chunk(`{}`, `"stop"`) + // finish chunk — NOT the end of the wire
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":123,"completion_tokens":7,"total_tokens":130,"prompt_tokens_details":{"cached_tokens":100}}}` + "\n\n" +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer server.Close()

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	outputCh := make(chan string, 8)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	cr := &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      server.URL + "/v1",
		apiKey:       "test-key",
		cfg:          AIConfig{Model: "m", Timeout: 10 * time.Second, Streaming: true, StreamTimeout: 5 * time.Second},
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
	}
	cr.sessionID = sid

	cr.runTurn(newTurnContext(sid, messages))

	lines := drainOutput(t, outputCh, 4, time.Second)
	assert.Contains(t, strings.Join(lines, "\n"), "hello")

	// The trailer usage must land in turn_usage — this is what feeds the
	// auto-compaction trigger and /tokencount.
	tu, err := getLastTurnUsageForSession(sid)
	require.NoError(t, err)
	require.NotNil(t, tu, "turn_usage row must be written from the stream trailer")
	assert.Equal(t, 123, tu.PromptTokens)
	assert.Equal(t, 7, tu.CompletionTokens)
	assert.Equal(t, 100, tu.CachedTokens)
}

// TestRunTurnStreamToleratesMissingDoneAfterFinish pins two behaviors at
// once: the loop keeps reading AFTER the finish chunk (a usage trailer
// arriving inside the idle window is captured — pre-fix code closed at
// finish and never saw it), and the idle-timeout grace treats expiry
// after a finish reason as a normal end when the server never sends
// [DONE] (no error to the user, turn completes). Chunks are explicitly
// flushed so the client really idles between them.
func TestRunTurnStreamToleratesMissingDoneAfterFinish(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	chunk := func(delta string, finish any) string {
		return fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", delta, finish)
	}
	finishStream := chunk(`{"role":"assistant","content":"done talking"}`, `null`) + chunk(`{}`, `"stop"`)
	trailer := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":50,"completion_tokens":3,"total_tokens":53}}` + "\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		fmt.Fprint(w, finishStream)
		if err := rc.Flush(); err != nil {
			return
		}
		// Trailer arrives well inside the 1s idle window (300ms gap)…
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, trailer)
		if err := rc.Flush(); err != nil {
			return
		}
		// …then the server never sends [DONE]: hold the connection open
		// past StreamTimeout so the grace branch is what ends the loop.
		time.Sleep(3 * time.Second)
	}))
	defer server.Close()

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	outputCh := make(chan string, 8)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	cr := &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      server.URL + "/v1",
		apiKey:       "test-key",
		cfg:          AIConfig{Model: "m", Timeout: 10 * time.Second, Streaming: true, StreamTimeout: 1 * time.Second},
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
	}
	cr.sessionID = sid

	cr.runTurn(newTurnContext(sid, messages))

	lines := drainOutput(t, outputCh, 4, 3*time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "done talking")
	assert.NotContains(t, joined, "timed out", "a finished generation must not error on the missing [DONE]")

	// Pre-fix this row would not exist (stream closed at the finish
	// chunk); post-fix the late trailer inside the idle window is read.
	tu, err := getLastTurnUsageForSession(sid)
	require.NoError(t, err)
	require.NotNil(t, tu, "late usage trailer inside the idle window must be captured")
	assert.Equal(t, 50, tu.PromptTokens)

	final, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	assert.Equal(t, "done talking", final[len(final)-1].Content)
}

func TestCompletionMaxTokens(t *testing.T) {
	tests := []struct {
		name string
		cfg  AIConfig
		want int64
	}{
		{
			name: "command maxcompletiontokens beats service maxtokens",
			// the cascade in ApplyDefaults fills MaxTokens from the service,
			// so this is the post-cascade shape of a command declaring
			// maxcompletiontokens=200 against a service default of 500
			cfg:  AIConfig{MaxTokens: 500, MaxCompletionTokens: 200},
			want: 200,
		},
		{
			name: "service maxtokens when command sets nothing",
			cfg:  AIConfig{MaxTokens: 500},
			want: 500,
		},
		{
			name: "command maxtokens alone",
			cfg:  AIConfig{MaxTokens: 300},
			want: 300,
		},
		{
			name: "fully unset gets bounded default, never unbounded",
			cfg:  AIConfig{},
			want: defaultCompletionMaxTokens,
		},
		{
			name: "negative values fall through to the default",
			cfg:  AIConfig{MaxTokens: -1, MaxCompletionTokens: -5},
			want: defaultCompletionMaxTokens,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, completionMaxTokens(tt.cfg))
		})
	}
}

func TestEphemeralRunnerGuards(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	t.Run("ephemeral getTools excludes builtins", func(t *testing.T) {
		fixtureServer := &MCPServer{
			Tools: []*mcp.Tool{{Name: "generate_image", Description: "gen"}},
		}
		origServers := mcpServers
		mcpServers = map[string]*MCPServer{"img-mcp": fixtureServer}
		t.Cleanup(func() { mcpServers = origServers })

		cfg := AIConfig{Name: "tabloid", MCPs: []string{"img-mcp"}}
		reg := &chatRunner{cfg: cfg}
		ephe := &chatRunner{cfg: cfg, ephemeral: true}

		names := func(tools []Tool) map[string]bool {
			out := map[string]bool{}
			for _, tl := range tools {
				out[tl.Function.Name] = true
			}
			return out
		}
		assert.True(t, names(reg.getTools())["register_background_job"], "regular runner keeps builtins")
		assert.True(t, names(ephe.getTools())["generate_image"], "ephemeral keeps MCP tools")
		assert.False(t, names(ephe.getTools())["register_background_job"], "ephemeral drops builtins")
	})

	t.Run("ephemeral storeUsage writes session-0 row", func(t *testing.T) {
		cr := &chatRunner{
			cfg:       AIConfig{Name: "summary", Model: "qwen3", Service: "svc"},
			logger:    newTestLogger(),
			ephemeral: true,
			// sessionID stays 0
		}
		var before int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&before).Error)
		cr.storeUsage(&Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, FinishReason: "stop"}, "chat/completions", 100)
		var after int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&after).Error)
		assert.Equal(t, before+1, after, "ephemeral usage rows ARE written (session 0 attribution)")
		var row TurnUsage
		require.NoError(t, theDB.Order("id desc").First(&row).Error)
		assert.Equal(t, int64(0), row.SessionID)
		assert.Equal(t, "qwen3", row.Model)
		assert.Equal(t, "svc", row.Service)
	})

	t.Run("non-ephemeral session-0 runner still skips usage rows", func(t *testing.T) {
		cr := &chatRunner{
			cfg:    AIConfig{Name: "x"},
			logger: newTestLogger(),
			// sessionID 0, ephemeral false — legacy defensive skip
		}
		var before int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&before).Error)
		cr.storeUsage(&Usage{PromptTokens: 1}, "chat/completions", 1)
		var after int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&after).Error)
		assert.Equal(t, before, after)
	})

	t.Run("ephemeral handleResponseIDSave does not persist", func(t *testing.T) {
		sid := createTestSession(t, "testnet", "#st", "shrew", "cmd", "svc", "m")
		cr := &chatRunner{cfg: AIConfig{Name: "g"}, logger: newTestLogger(), sessionID: sid, ephemeral: true}
		got := cr.handleResponseIDSave("resp_1", "text", nil, "")
		assert.Equal(t, "resp_1", got, "return value contract preserved")
		var sess Session
		require.NoError(t, theDB.First(&sess, sid).Error)
		assert.Nil(t, sess.ResponseID, "no response id persisted for ephemeral runs")
	})
}
