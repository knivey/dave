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

func setupSessionWithResponseID(t *testing.T, responseID string) int64 {
	t.Helper()
	db := setupTestDB(t)
	_ = db

	sid, err := sessionMgr.CreateSession("testnet", "#101", ensureTestUser(t, "testnet", "shrew"), "testcmd", "testservice", "testmodel")
	require.NoError(t, err)
	if responseID != "" {
		require.NoError(t, sessionMgr.UpdateResponseID(sid, &responseID))
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
		runner.runTurn(messages)
	}()

	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		sessionMgr.AddMessage(session.ID, ChatMessage{Role: RoleSystem, Content: "bg job result"})
		messages, _ := sessionMgr.GetMessages(session.ID, cfg.MaxHistory)
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
	require.NoError(t, sessionMgr.UpdateResponseID(sid1, strPtrOrNil("resp-alice")))

	sid2, err := sessionMgr.CreateSession("testnet", "#101", ensureTestUser(t, "testnet", "bob"), "testcmd", "svc", "model")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid2, strPtrOrNil("resp-bob")))

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
		runner.runTurn(messages)
	}()

	go func() {
		defer wg.Done()
		sessionMgr.AddMessage(sid2, ChatMessage{Role: RoleUser, Content: "msg"})
		messages, _ := sessionMgr.GetMessages(sid2, cfg.MaxHistory)
		runner := makeRunner(sid2, "bob", ensureTestUser(t, "testnet", "bob"))
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

func TestHandleResponseIDSave_SavesToRunnerSessionNotActive(t *testing.T) {
	setupTestDB(t)

	userID := ensureTestUser(t, "testnet", "shrew")
	sid1, err := sessionMgr.CreateSession("testnet", "#101", userID, "chat", "openai", "model-a")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid1, strPtrOrNil("resp-old")))

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
	require.NoError(t, sessionMgr.UpdateResponseID(sid1, strPtrOrNil("resp-old")))

	sid2, err := sessionMgr.CreateSession("testnet", "#101", userID, "grk", "grok", "model-b")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.UpdateResponseID(sid2, strPtrOrNil("resp-grk")))

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
		msgs := cr.handleRegisterBackgroundJob(nil, tc)
		require.Len(t, msgs, 1)

		pj := PendingJob{}
		require.NoError(t, theDB.Where("job_id = ?", "j-auto").First(&pj).Error)
		assert.Equal(t, "img-mcp-async", pj.MCPServer, "server_name should be auto-detected")
	})

	t.Run("explicit server_name still works", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-explicit",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j-explicit","tool_name":"enhance_and_generate_async","server_name":"img-mcp-async"}`},
		}
		msgs := cr.handleRegisterBackgroundJob(nil, tc)
		require.Len(t, msgs, 1)

		pj := PendingJob{}
		require.NoError(t, theDB.Where("job_id = ?", "j-explicit").First(&pj).Error)
		assert.Equal(t, "img-mcp-async", pj.MCPServer, "server_name should match explicit value")
	})

	t.Run("unknown tool_name returns error", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-unknown",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j-unknown","tool_name":"nonexistent_tool"}`},
		}
		msgs := cr.handleRegisterBackgroundJob(nil, tc)
		require.Len(t, msgs, 1)
		assert.Contains(t, msgs[0].Content, "error: could not determine MCP server")
	})

	t.Run("missing job_id returns error", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-nojob",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"tool_name":"enhance_and_generate_async"}`},
		}
		msgs := cr.handleRegisterBackgroundJob(nil, tc)
		require.Len(t, msgs, 1)
		assert.Contains(t, msgs[0].Content, "error: job_id and tool_name are required")
	})

	t.Run("missing tool_name returns error", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-notool",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j1"}`},
		}
		msgs := cr.handleRegisterBackgroundJob(nil, tc)
		require.Len(t, msgs, 1)
		assert.Contains(t, msgs[0].Content, "error: job_id and tool_name are required")
	})

	t.Run("empty server_name triggers auto-detect", func(t *testing.T) {
		tc := ToolCall{
			ID:       "tc-emptyserver",
			Function: FunctionCall{Name: "register_background_job", Arguments: `{"job_id":"j-emptyserver","tool_name":"enhance_and_generate_async","server_name":""}`},
		}
		msgs := cr.handleRegisterBackgroundJob(nil, tc)
		require.Len(t, msgs, 1)

		pj := PendingJob{}
		require.NoError(t, theDB.Where("job_id = ?", "j-emptyserver").First(&pj).Error)
		assert.Equal(t, "img-mcp-async", pj.MCPServer, "empty server_name should trigger auto-detect")
	})
}
