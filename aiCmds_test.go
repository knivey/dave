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
		MaxHistory:         20,
		Timeout:            10 * time.Second,
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
		messages, _ := sessionMgr.GetMessages(session.ID, cfg.MaxHistory)
		runner := makeRunner()
		runner.runTurn(newTurnContext(runner.sessionID, messages))
	}()

	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		sessionMgr.AddMessage(session.ID, ChatMessage{Role: RoleSystem, Content: "bg job result"})
		messages, _ := sessionMgr.GetMessages(session.ID, cfg.MaxHistory)
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
		MaxHistory:         20,
		Timeout:            10 * time.Second,
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
		messages, _ := sessionMgr.GetMessages(sid1, cfg.MaxHistory)
		runner := makeRunner(sid1, "alice", ensureTestUser(t, "testnet", "alice"))
		runner.runTurn(newTurnContext(sid1, messages))
	}()

	go func() {
		defer wg.Done()
		sessionMgr.AddMessage(sid2, ChatMessage{Role: RoleUser, Content: "msg"})
		messages, _ := sessionMgr.GetMessages(sid2, cfg.MaxHistory)
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
			MaxHistory:         20,
			Timeout:            10 * time.Second,
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
		messages, err := sessionMgr.GetMessages(session.ID, cfg.MaxHistory)
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

	allTools := getBuiltinToolDefs(nil)
	assert.Len(t, allTools, 3, "all builtin tools should be returned with nil disabled")

	allToolsEmpty := getBuiltinToolDefs([]string{})
	assert.Len(t, allToolsEmpty, 3, "empty disabled list should return all tools")

	filteredBan := getBuiltinToolDefs([]string{"ban_user"})
	assert.Len(t, filteredBan, 2, "disabling ban_user should leave 2 tools")
	names := make(map[string]bool, len(filteredBan))
	for _, tool := range filteredBan {
		names[tool.Function.Name] = true
	}
	assert.True(t, names["register_background_job"], "register_background_job should remain")
	assert.True(t, names["check_ban_history"], "check_ban_history should remain")
	assert.False(t, names["ban_user"], "ban_user should be filtered out")

	filteredAll := getBuiltinToolDefs([]string{"register_background_job", "ban_user", "check_ban_history"})
	assert.Len(t, filteredAll, 0, "disabling all tools should return empty")
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
	cr := &chatRunner{
		logger: logxi.New("test"),
	}
	cr.logger.SetLevel(logxi.LevelAll)

	tests := []struct {
		name            string
		content         string
		reasoning       string
		emptyRetries    int
		maxEmptyRetries int
		wantRetry       bool
		wantContent     string
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
		},
		{
			name:            "reasoning only, zero max retries",
			content:         "",
			reasoning:       "thinking...",
			emptyRetries:    0,
			maxEmptyRetries: 0,
			wantRetry:       false,
			wantContent:     "...",
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
			retry, content := cr.checkEmptyRetry(tt.content, tt.reasoning, tt.emptyRetries, tt.maxEmptyRetries)
			assert.Equal(t, tt.wantRetry, retry)
			assert.Equal(t, tt.wantContent, content)
		})
	}
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
