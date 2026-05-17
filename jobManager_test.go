package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockChatRunner struct {
	setChannelCalled bool
	setChannelCh     string
	setChannelNick   string
	runTurnCalled    int
	runTurnFn        func(messages []ChatMessage) ([]ChatMessage, bool)
}

func (m *mockChatRunner) setChannel(channel, nick string, userID int64) {
	m.setChannelCalled = true
	m.setChannelCh = channel
	m.setChannelNick = nick
}

func (m *mockChatRunner) setSessionInfo(sessionID int64, convID string) {
}

func (m *mockChatRunner) runTurn(messages []ChatMessage) ([]ChatMessage, bool) {
	m.runTurnCalled++
	if m.runTurnFn != nil {
		return m.runTurnFn(messages)
	}
	return messages, true
}

type mockBot struct {
	network  Network
	client   *girc.Client
	messages []string
	mu       sync.Mutex
}

func newMockBot(networkName, trigger string) *mockBot {
	mb := &mockBot{
		network: Network{
			Name:    networkName,
			Trigger: trigger,
		},
	}
	mb.client = girc.New(girc.Config{
		Server: "localhost",
		Port:   6667,
		Nick:   "testbot",
	})
	return mb
}

func (mb *mockBot) getMessages() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	result := make([]string, len(mb.messages))
	copy(result, mb.messages)
	return result
}

func setupMockDeps(t *testing.T) *mockBot {
	t.Helper()
	mb := newMockBot("testnet", "!")

	mockGetBot := func(network string) *Bot {
		if network == "testnet" {
			return &Bot{Client: mb.client, Network: mb.network}
		}
		return nil
	}

	mockBotReady := func(network, channel string) bool {
		return network == "testnet"
	}

	mockNewRunner := func(network Network, client *girc.Client, cfg AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				return messages, true
			},
		}
	}

	if queueMgr != nil {
		queueMgr.botReady = mockBotReady
		queueMgr.getBot = mockGetBot
	}
	if asyncJobMgr != nil {
		asyncJobMgr.getBot = mockGetBot
		asyncJobMgr.newChatRunner = mockNewRunner
	}

	configMu.Lock()
	config = Config{
		Commands: Commands{
			Chats: map[string]AIConfig{
				"testchat": makeTestAIConfig(),
			},
		},
	}
	configMu.Unlock()

	return mb
}

func TestDeliverAsyncResult_SameSession(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	mb := setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "you are helpful")
	insertTestMessage(t, sid, "user", "draw me a picture")

	require.NoError(t, createPendingJob(sid, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	require.NoError(t, completePendingJob("job-1", "image generated successfully"), "completePendingJob")

	entry := &jobEntry[asyncJobPayload]{
		jobID:     "job-1",
		payload:   asyncJobPayload{sessionID: sid},
		toolName:  "generate_image_async",
		mcpServer: "img-mcp",
		network:   "testnet",
		channel:   "#test",
		nick:      "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	msgs, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err)

	hasAsyncMsg := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			hasAsyncMsg = true
			assert.Contains(t, m.Content, "image generated successfully", "async result message missing result text")
		}
	}
	assert.True(t, hasAsyncMsg, "expected async result system message in context")
	_ = mb
}

func TestDeliverAsyncResult_DifferentSession(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	mb := setupMockDeps(t)

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionA, "system", "you are helpful")
	insertTestMessage(t, sessionA, "user", "draw me a picture")

	sessionB := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionB, "system", "you are helpful")
	insertTestMessage(t, sessionB, "user", "tell me a joke")

	require.NoError(t, createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	require.NoError(t, completePendingJob("job-1", "image url: http://example.com/img.png"), "completePendingJob")

	entry := &jobEntry[asyncJobPayload]{
		jobID:     "job-1",
		payload:   asyncJobPayload{sessionID: sessionA},
		toolName:  "generate_image_async",
		mcpServer: "img-mcp",
		network:   "testnet",
		channel:   "#test",
		nick:      "testuser",
		userID:    ensureTestUser(t, "testnet", "testuser"),
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, activeSession)
	assert.Equal(t, sessionA, activeSession.ID, "should have switched back to session A")

	sessB, err := getDBSessionByID(sessionB)
	require.NoError(t, err, "getDBSessionByID")
	assert.Equal(t, "completed", sessB.Status, "session B status")

	sessA, err := getDBSessionByID(sessionA)
	require.NoError(t, err, "getDBSessionByID")
	assert.Equal(t, "active", sessA.Status, "session A status")

	msgs, err := sessionMgr.GetMessages(sessionA, 20)
	require.NoError(t, err)
	hasAsyncMsg := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			hasAsyncMsg = true
		}
	}
	assert.True(t, hasAsyncMsg, "expected async result system message after switch")
	_ = mb
}

func TestOnAsyncJobCompleted_UserBusyWaitsThenDelivers(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	queueMgr.UpdateServiceLimits(map[string]Service{"testsvc": {Parallel: 1}, "": {Parallel: 1}})

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionA, "system", "you are helpful")
	insertTestMessage(t, sessionA, "user", "draw me a picture")

	sessionB := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionB, "system", "you are helpful")
	insertTestMessage(t, sessionB, "user", "tell me a joke")

	require.NoError(t, createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")

	blockDone := make(chan struct{})
	queueMgr.Enqueue("testnet", "#test", ensureTestUser(t, "testnet", "testuser"), "testuser", "", "",
		func(ctx context.Context, output chan<- string) {
			<-blockDone
		})
	time.Sleep(100 * time.Millisecond)

	entry := &jobEntry[asyncJobPayload]{
		jobID:     "job-1",
		payload:   asyncJobPayload{sessionID: sessionA},
		toolName:  "generate_image_async",
		mcpServer: "img-mcp",
		network:   "testnet",
		channel:   "#test",
		nick:      "testuser",
		userID:    ensureTestUser(t, "testnet", "testuser"),
	}

	onAsyncJobCompleted(entry, "result text")
	time.Sleep(100 * time.Millisecond)

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	if activeSession != nil {
		assert.NotEqual(t, sessionA, activeSession.ID, "session should NOT have switched while blocking job holds the slot")
	}

	close(blockDone)
	time.Sleep(300 * time.Millisecond)

	waitForActiveSession(t, "testnet", "#test", ensureTestUser(t, "testnet", "testuser"), sessionA, 5*time.Second)
}

func TestOnAsyncJobCompleted_MultipleJobsWhileBusy(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionA, "system", "you are helpful")
	insertTestMessage(t, sessionA, "user", "draw me a picture")

	sessionB := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionB, "system", "you are helpful")
	insertTestMessage(t, sessionB, "user", "tell me a joke")

	createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp")
	createPendingJob(sessionA, "job-2", "generate_image_async", "img-mcp")

	blockDone := make(chan struct{})
	queueMgr.Enqueue("testnet", "#test", ensureTestUser(t, "testnet", "testuser"), "testuser", "testsvc", "",
		func(ctx context.Context, output chan<- string) {
			<-blockDone
		})
	time.Sleep(100 * time.Millisecond)

	entry1 := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sessionA},
		toolName: "generate_image_async", mcpServer: "img-mcp",
		network: "testnet", channel: "#test", nick: "testuser",
	}
	entry2 := &jobEntry[asyncJobPayload]{
		jobID: "job-2", payload: asyncJobPayload{sessionID: sessionA},
		toolName: "generate_image_async", mcpServer: "img-mcp",
		network: "testnet", channel: "#test", nick: "testuser",
	}

	onAsyncJobCompleted(entry1, "image 1 ready")
	onAsyncJobCompleted(entry2, "image 2 ready")

	time.Sleep(100 * time.Millisecond)

	close(blockDone)

	waitForActiveSession(t, "testnet", "#test", ensureTestUser(t, "testnet", "testuser"), sessionA, 5*time.Second)

	msgs, err := sessionMgr.GetMessages(sessionA, 20)
	require.NoError(t, err)

	asyncCount := 0
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			asyncCount++
		}
	}
	assert.GreaterOrEqual(t, asyncCount, 2, "expected at least 2 async result messages")
}

func TestSwitchToSession_CompletesOldSession(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionA, "system", "sys prompt A")
	insertTestMessage(t, sessionA, "user", "hello A")

	sessionB := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionB, "system", "sys prompt B")
	insertTestMessage(t, sessionB, "user", "hello B")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sessionA},
		network: "testnet", channel: "#test", nick: "testuser",
		userID: ensureTestUser(t, "testnet", "testuser"),
	}

	switchToSession(entry)

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, activeSession)
	assert.Equal(t, sessionA, activeSession.ID, "active session should be A")

	sessB, _ := getDBSessionByID(sessionB)
	assert.Equal(t, "completed", sessB.Status, "session B status")

	sessA, _ := getDBSessionByID(sessionA)
	assert.Equal(t, "active", sessA.Status, "session A status")

	msgs, err := sessionMgr.GetMessages(sessionA, 20)
	require.NoError(t, err)
	foundUserMsg := false
	for _, m := range msgs {
		if m.Role == "user" && m.Content == "hello A" {
			foundUserMsg = true
		}
	}
	assert.True(t, foundUserMsg, "expected session A's messages to be loaded after switch")
}

func TestSwitchToSession_NoOldSession(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionA, "system", "sys prompt A")
	insertTestMessage(t, sessionA, "user", "hello A")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sessionA},
		network: "testnet", channel: "#test", nick: "testuser",
		userID: ensureTestUser(t, "testnet", "testuser"),
	}

	switchToSession(entry)

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, activeSession)
	assert.Equal(t, sessionA, activeSession.ID, "active session should be A")
}

func TestSwitchToSession_SameSessionIsNoop(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")
	insertTestMessage(t, sid, "user", "hello")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		network: "testnet", channel: "#test", nick: "testuser",
		userID: ensureTestUser(t, "testnet", "testuser"),
	}

	switchToSession(entry)

	sess, _ := getDBSessionByID(sid)
	assert.Equal(t, "active", sess.Status, "session status")
}

func TestSwitchToSession_InvalidChatCommand(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "deletedcmd", "", "")
	insertTestMessage(t, sessionA, "system", "sys")

	sessionB := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sessionA},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	msg := switchToSession(entry)
	assert.Equal(t, "", msg, "should return empty string when chat command not found")

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, activeSession)
	assert.Equal(t, sessionB, activeSession.ID, "should remain session B when chat command not found")
	_ = sessionB
}

func TestDeliverAsyncResult_NoContext(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionA, "system", "sys")
	require.NoError(t, createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	completePendingJob("job-1", "result")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sessionA},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, activeSession)
	assert.Equal(t, sessionA, activeSession.ID, "should have loaded from DB")

	msgs, err := sessionMgr.GetMessages(sessionA, 20)
	require.NoError(t, err)
	hasAsyncMsg := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			hasAsyncMsg = true
		}
	}
	assert.True(t, hasAsyncMsg, "expected async result system message after loading context from DB")
}

func TestDeliverAsyncResult_UsesMockRunner(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "result data")

	var runner *mockChatRunner
	origNewRunner := asyncJobMgr.newChatRunner
	asyncJobMgr.newChatRunner = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		runner = &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				return messages, true
			},
		}
		return runner
	}
	defer func() { asyncJobMgr.newChatRunner = origNewRunner }()

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		toolName: "generate_image_async", mcpServer: "img-mcp",
		network: "testnet", channel: "#test", nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	require.NotNil(t, runner, "runner was never created")
	assert.True(t, runner.setChannelCalled, "setChannel was not called")
	assert.Equal(t, "#test", runner.setChannelCh, "setChannel channel")
	assert.Equal(t, "testuser", runner.setChannelNick, "setChannel nick")
	assert.NotZero(t, runner.runTurnCalled, "runTurn was never called")
}

func TestDeliverAsyncResult_RunnerSeesInjectedResult(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "image url: http://example.com/img.png")

	var receivedMessages []ChatMessage
	origNewRunner := asyncJobMgr.newChatRunner
	asyncJobMgr.newChatRunner = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				receivedMessages = messages
				return messages, true
			},
		}
	}
	defer func() { asyncJobMgr.newChatRunner = origNewRunner }()

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		toolName: "generate_image_async", mcpServer: "img-mcp",
		network: "testnet", channel: "#test", nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	foundInjected := false
	for _, m := range receivedMessages {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") && strings.Contains(m.Content, "http://example.com/img.png") {
			foundInjected = true
		}
	}
	if !foundInjected {
		t.Error("runTurn did not receive the injected async result message")
		t.Logf("messages received: %+v", receivedMessages)
	}
}

func TestDeliverAsyncResult_MultipleCompletedJobs(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "image 1")
	createPendingJob(sid, "job-2", "generate_image_async", "img-mcp")
	completePendingJob("job-2", "image 2")

	turnCount := 0
	origNewRunner := asyncJobMgr.newChatRunner
	asyncJobMgr.newChatRunner = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				turnCount++
				return messages, true
			},
		}
	}
	defer func() { asyncJobMgr.newChatRunner = origNewRunner }()

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		toolName: "generate_image_async", mcpServer: "img-mcp",
		network: "testnet", channel: "#test", nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	assert.Equal(t, 1, turnCount, "runTurn should be called once (both jobs delivered in one turn)")

	jobs, _ := getCompletedPendingJobs(sid)
	assert.Empty(t, jobs, "expected all jobs delivered")
}

func TestInjectAsyncResultFromDB(t *testing.T) {
	setupTestDB(t)

	cfg := makeTestAIConfig()

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	sessionMgr.AddMessage(sid, ChatMessage{Role: "system", Content: "sys"})

	result := "image url: http://example.com/test.png"
	pj := PendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Status:    "completed",
		Result:    &result,
	}

	injectAsyncResultFromDB(sid, cfg, pj, "testnet", "#test", "testuser")

	msgs, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "expected 2 messages")
	lastMsg := msgs[len(msgs)-1]
	assert.Equal(t, "system", lastMsg.Role, "injected message role")
	assert.Contains(t, lastMsg.Content, "Background task completed", "injected message missing expected text")
	assert.Contains(t, lastMsg.Content, "generate_image_async", "injected message missing tool name")
	assert.Contains(t, lastMsg.Content, "http://example.com/test.png", "injected message missing result")
}

func TestInjectAsyncResultFromDB_AnthropicUserSuffix(t *testing.T) {
	setupTestDB(t)

	cfg := makeTestAIConfig()
	cfg.Model = "anthropic/claude-sonnet-4.6"

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	sessionMgr.AddMessage(sid, ChatMessage{Role: "system", Content: "sys"})

	result := "image url: http://example.com/test.png"
	pj := PendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Status:    "completed",
		Result:    &result,
	}

	injectAsyncResultFromDB(sid, cfg, pj, "testnet", "#test", "testuser")

	msgs, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err)
	require.Len(t, msgs, 3, "expected 3 messages (sys + system result + user suffix)")
	assert.Equal(t, "system", msgs[1].Role)
	assert.Contains(t, msgs[1].Content, "Background task completed")
	assert.Equal(t, "user", msgs[2].Role, "last message should be user suffix")
	assert.Contains(t, msgs[2].Content, "Respond to the user based on the above background task result.")
}

func TestInjectAsyncResultFromDB_NeedsUserSuffixConfig(t *testing.T) {
	setupTestDB(t)

	cfg := makeTestAIConfig()
	cfg.NeedsUserSuffix = true

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	sessionMgr.AddMessage(sid, ChatMessage{Role: "system", Content: "sys"})

	result := "image url: http://example.com/test.png"
	pj := PendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Status:    "completed",
		Result:    &result,
	}

	injectAsyncResultFromDB(sid, cfg, pj, "testnet", "#test", "testuser")

	msgs, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err)
	require.Len(t, msgs, 3, "expected 3 messages (sys + system result + user suffix)")
	assert.Equal(t, "user", msgs[2].Role, "last message should be user suffix")
}

func TestInjectAsyncResultFromDB_NilResult(t *testing.T) {
	setupTestDB(t)

	cfg := makeTestAIConfig()

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	sessionMgr.AddMessage(sid, ChatMessage{Role: "system", Content: "sys"})

	pj := PendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		Status:    "completed",
		Result:    nil,
	}

	injectAsyncResultFromDB(sid, cfg, pj, "testnet", "#test", "testuser")

	msgs, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err)
	lastMsg := msgs[len(msgs)-1]
	assert.Contains(t, lastMsg.Content, "Background task completed", "injected message missing expected text even with nil result")
}

func TestModelNeedsUserSuffix(t *testing.T) {
	tests := []struct {
		model    string
		expected bool
	}{
		{"anthropic/claude-sonnet-4.6", true},
		{"anthropic/claude-opus-4.6", true},
		{"anthropic/claude-sonnet-4.5", true},
		{"anthropic/claude-3.5-sonnet", true},
		{"Anthropic/Claude-Sonnet-4.6", true},
		{"openai/gpt-4o", false},
		{"test-model", false},
		{"google/gemini-pro", false},
		{"", false},
		{"anthropic", false},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			assert.Equal(t, tc.expected, modelNeedsUserSuffix(tc.model))
		})
	}
}

func TestOnAsyncJobCompleted_RemovesJobFromMap(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")

	asyncJobMgr.jobs["job-1"] = &jobEntry[asyncJobPayload]{jobID: "job-1"}

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	onAsyncJobCompleted(entry, "result")

	_, exists := asyncJobMgr.jobs["job-1"]
	assert.False(t, exists, "job should be removed from in-memory map after completion")
}

func TestOnAsyncJobCompleted_MarksCompletedInDB(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	onAsyncJobCompleted(entry, "the image result")

	var pj PendingJob
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, theDB.Where("job_id = ?", "job-1").First(&pj).Error, "query job")
		if pj.Status == "delivered" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Equal(t, "delivered", pj.Status, "job status (completed then delivered by deliverAsyncResult)")
	require.NotNil(t, pj.Result, "job result")
	assert.Equal(t, "the image result", *pj.Result, "job result")
}

func TestDeliverAsyncResult_MarksJobsDelivered(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "result")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	var pj PendingJob
	require.NoError(t, theDB.Where("job_id = ?", "job-1").First(&pj).Error, "query job")
	assert.Equal(t, "delivered", pj.Status, "job status")
}

func waitForActiveSession(t *testing.T, network, channel string, userID int64, expectedSID int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		session, _ := sessionMgr.GetActiveSession(network, channel, userID)
		if session != nil && session.ID == expectedSID {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	session, _ := sessionMgr.GetActiveSession(network, channel, userID)
	sid := int64(0)
	if session != nil {
		sid = session.ID
	}
	require.Equal(t, expectedSID, sid, "timed out waiting for session switch")
}

func TestDeliverAsyncResult_NoContextLoaded_LoadsFromDB(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "you are helpful")
	insertTestMessage(t, sid, "user", "draw me a picture")

	require.NoError(t, createPendingJob(sid, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	require.NoError(t, completePendingJob("job-1", "image url: http://example.com/img.png"), "completePendingJob")

	entry := &jobEntry[asyncJobPayload]{
		jobID:     "job-1",
		payload:   asyncJobPayload{sessionID: sid},
		toolName:  "generate_image_async",
		mcpServer: "img-mcp",
		network:   "testnet",
		channel:   "#test",
		nick:      "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(entry, context.Background(), output)

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, activeSession)
	assert.Equal(t, sid, activeSession.ID, "should have loaded from DB")

	msgs, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err)
	assert.NotEmpty(t, msgs, "expected messages to be loaded from DB")

	hasAsyncMsg := false
	for _, m := range msgs {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			hasAsyncMsg = true
		}
	}
	assert.True(t, hasAsyncMsg, "expected async result system message after loading context from DB")

	select {
	case msg := <-output:
		t.Errorf("unexpected switch message (no prior session): %q", msg)
	default:
	}
}

func TestSwitchToSession_NoCurrentSession_NoSwitchMessage(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "you are helpful")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	msg := switchToSession(entry)
	assert.Equal(t, "", msg, "switchToSession should return empty string when no prior session")

	activeSession, _ := sessionMgr.GetActiveSession("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, activeSession)
	assert.Equal(t, sid, activeSession.ID, "should have loaded from DB")
}

func TestSwitchToSession_RestoresConvIDAndResponseID(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sidA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sidA, "system", "sys")
	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sidA).Update("conv_id", "grok-conv-123").Error, "update conv_id")
	respID := "resp-abc-456"
	require.NoError(t, updateDBSessionResponseID(sidA, &respID), "updateDBSessionResponseID")

	sidB := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sidB, "system", "sys2")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sidA},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	switchToSession(entry)

	session, err := sessionMgr.GetSession(sidA)
	require.NoError(t, err)
	assert.Equal(t, "active", session.Status)
	require.NotNil(t, session.ConvID)
	assert.Equal(t, "grok-conv-123", *session.ConvID, "should restore ConvID from DB session")
	require.NotNil(t, session.ResponseID)
	assert.Equal(t, "resp-abc-456", *session.ResponseID, "should restore ResponseID from DB session")
}

func TestRecoverPendingJobs(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "recovery-job-1", "generate_image_async", "img-mcp")

	asyncJobMgr.cancel()
	asyncJobMgr.ctx, asyncJobMgr.cancel = context.WithCancel(context.Background())

	recoverPendingJobs()

	asyncJobMgr.mu.Lock()
	_, exists := asyncJobMgr.jobs["recovery-job-1"]
	asyncJobMgr.mu.Unlock()
	assert.True(t, exists, "expected job to be recovered in memory")

	asyncJobMgr.cancel()
	time.Sleep(100 * time.Millisecond)
}

func TestRecoverPendingJobs_NoDB(t *testing.T) {
	theDB = nil
	sessionMgr = nil
	recoverPendingJobs()
}

func TestRegisterAsyncJob_Duplicate(t *testing.T) {
	setupTestJobManager(t)
	asyncJobMgr.ctx, asyncJobMgr.cancel = context.WithCancel(context.Background())

	registerAsyncJob("dup-job", 1, "tool", "server", "net", "#chan", "user", 0)
	registerAsyncJob("dup-job", 1, "tool", "server", "net", "#chan", "user", 0)

	asyncJobMgr.mu.Lock()
	count := len(asyncJobMgr.jobs)
	asyncJobMgr.mu.Unlock()
	assert.Equal(t, 1, count, "expected 1 job (duplicate should be ignored)")

	asyncJobMgr.cancel()
	asyncJobMgr.wg.Wait()
}

func TestSwitchToSession_DBMessagesWithToolCalls(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	toolCallsJSON, _ := json.Marshal([]ToolCall{
		{ID: "tc-1", Type: "function", Function: FunctionCall{Name: "test_tool", Arguments: `{"arg":"val"}`}},
	})
	toolCallsStr := string(toolCallsJSON)
	insertDBMessage(sessionA, "system", "sys", nil, nil, nil, nil)
	insertDBMessage(sessionA, "assistant", "using tool", &toolCallsStr, nil, nil, nil)
	toolCallID := "tc-1"
	insertDBMessage(sessionA, "tool", "tool result", nil, &toolCallID, nil, nil)

	_ = createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sessionA},
		network: "testnet", channel: "#test", nick: "testuser",
	}
	switchToSession(entry)

	msgs, err := sessionMgr.GetMessages(sessionA, 20)
	require.NoError(t, err)

	foundToolCall := false
	foundToolCallID := false
	for _, m := range msgs {
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "tc-1" {
			foundToolCall = true
		}
		if m.ToolCallID == "tc-1" {
			foundToolCallID = true
		}
	}
	assert.True(t, foundToolCall, "tool_calls not restored from DB")
	assert.True(t, foundToolCallID, "tool_call_id not restored from DB")
}

func TestSwitchToSession_TruncatesHistory(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	cfg.MaxHistory = 3

	sessionA := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sessionA, "system", "sys")
	for i := 0; i < 10; i++ {
		insertTestMessage(t, sessionA, "user", fmt.Sprintf("msg %d", i))
	}

	_ = createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	config.Commands.Chats["testchat"] = cfg

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sessionA},
		network: "testnet", channel: "#test", nick: "testuser",
	}
	switchToSession(entry)

	msgs, err := sessionMgr.GetMessages(sessionA, cfg.MaxHistory)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(msgs), cfg.MaxHistory+1, "messages not truncated")
	assert.Equal(t, "system", msgs[0].Role, "first message should be system prompt after truncation")
}

func TestDeliverAsyncResult_RunningDuringTurn(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "result")

	runningDuringTurn := false
	var wg sync.WaitGroup
	wg.Add(1)

	origNewRunner := asyncJobMgr.newChatRunner
	asyncJobMgr.newChatRunner = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				runningDuringTurn = queueMgr.IsRunning("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
				wg.Done()
				return messages, true
			},
		}
	}
	defer func() { asyncJobMgr.newChatRunner = origNewRunner }()

	entry := &jobEntry[asyncJobPayload]{
		jobID: "job-1", payload: asyncJobPayload{sessionID: sid},
		network: "testnet", channel: "#test", nick: "testuser",
	}

	queueMgr.Enqueue("testnet", "#test", ensureTestUser(t, "testnet", "testuser"), "testuser", "testsvc", "", func(ctx context.Context, output chan<- string) {
		deliverAsyncResult(entry, ctx, output)
	})

	wg.Wait()

	assert.True(t, runningDuringTurn, "queueMgr.IsRunning() returned false during runTurn — item should be active")
}

func TestDeliverAsyncResult_RunningDuringTurn_BusyPath(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	ready := make(chan struct{})
	unblockFirst := make(chan struct{})

	queueMgr.Enqueue("testnet", "#test", ensureTestUser(t, "testnet", "testuser"), "testuser", "testsvc", "", func(ctx context.Context, output chan<- string) {
		ready <- struct{}{}
		<-unblockFirst
	})

	<-ready

	require.True(t, queueMgr.IsRunning("testnet", "#test", ensureTestUser(t, "testnet", "testuser")), "expected first job to be running")

	runningDuringTurn := false
	var turnWg sync.WaitGroup
	turnWg.Add(1)

	queueMgr.Enqueue("testnet", "#test", ensureTestUser(t, "testnet", "testuser"), "testuser", "testsvc", "", func(ctx context.Context, output chan<- string) {
		runningDuringTurn = queueMgr.IsRunning("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
		turnWg.Done()
	})

	current, pending := queueMgr.QueueStatus("testnet", "#test", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, current, "expected first job still running")
	require.Len(t, pending, 1, "expected 1 pending job")

	close(unblockFirst)

	turnWg.Wait()

	assert.True(t, runningDuringTurn, "queueMgr.IsRunning() returned false during queued job execution — item should be active")
}

func setupCancelTestMCP(t *testing.T) {
	t.Helper()

	type CancelJobInput struct {
		JobID string `json:"job_id" jsonschema:"the job ID to cancel"`
	}
	type CancelJobOutput struct {
		Cancelled bool `json:"cancelled"`
	}

	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-mcp", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "cancel_job", Description: "cancel a job"}, func(ctx context.Context, req *mcp.CallToolRequest, input CancelJobInput) (*mcp.CallToolResult, CancelJobOutput, error) {
		return nil, CancelJobOutput{Cancelled: true}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "wait_for_job", Description: "wait for a job"}, func(ctx context.Context, req *mcp.CallToolRequest, input struct {
		JobID   string `json:"job_id"`
		Timeout int    `json:"timeout,omitempty"`
	}) (*mcp.CallToolResult, struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}, error) {
		return nil, struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}{JobID: input.JobID, Status: "completed"}, nil
	})

	t1, t2 := mcp.NewInMemoryTransports()

	_, err := server.Connect(ctx, t1, nil)
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })

	origServers := mcpServers
	origToolMap := mcpToolToServer

	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Config:  MCPConfig{Timeout: 10 * time.Second},
		Client:  client,
		Session: session,
	}
	for tool, err := range session.Tools(ctx, nil) {
		require.NoError(t, err)
		srv.Tools = append(srv.Tools, tool)
	}

	mcpServers["img-mcp"] = srv
	mcpToolToServer["cancel_job"] = "img-mcp"
	mcpToolToServer["wait_for_job"] = "img-mcp"

	t.Cleanup(func() {
		mcpServers = origServers
		mcpToolToServer = origToolMap
	})
}

func TestCancelAsyncJobsForSession_CancelsMatchingJobs(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	setupCancelTestMCP(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	asyncJobMgr.jobs["job-a"] = &jobEntry[asyncJobPayload]{
		jobID:   "job-a",
		payload: asyncJobPayload{sessionID: sid},
		network: "testnet",
		channel: "#test",
		nick:    "testuser",
		cancel:  func() {},
	}
	asyncJobMgr.jobs["job-b"] = &jobEntry[asyncJobPayload]{
		jobID:   "job-b",
		payload: asyncJobPayload{sessionID: sid},
		network: "testnet",
		channel: "#test",
		nick:    "testuser",
		cancel:  func() {},
	}

	otherSID := createTestSession(t, "testnet", "#test", "otheruser", "testchat", "", "")
	asyncJobMgr.jobs["job-c"] = &jobEntry[asyncJobPayload]{
		jobID:   "job-c",
		payload: asyncJobPayload{sessionID: otherSID},
		network: "testnet",
		channel: "#test",
		nick:    "otheruser",
		cancel:  func() {},
	}

	cancelAsyncJobsForSession(sid)

	_, exists := asyncJobMgr.jobs["job-a"]
	assert.False(t, exists, "job-a should be removed from asyncJobMgr.jobs")
	_, exists = asyncJobMgr.jobs["job-b"]
	assert.False(t, exists, "job-b should be removed from asyncJobMgr.jobs")
	_, exists = asyncJobMgr.jobs["job-c"]
	assert.True(t, exists, "job-c should still exist (different session)")
}

func TestCancelAsyncJobsForSession_NoJobs(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)

	cancelAsyncJobsForSession(99999)
}

func TestCancelAsyncJobsForSession_DeletesFromMap(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	setupCancelTestMCP(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	asyncJobMgr.jobs["job-x"] = &jobEntry[asyncJobPayload]{
		jobID:   "job-x",
		payload: asyncJobPayload{sessionID: sid},
		network: "testnet",
		channel: "#test",
		nick:    "testuser",
		cancel:  func() {},
	}

	cancelAsyncJobsForSession(sid)

	assert.Empty(t, asyncJobMgr.jobs, "expected 0 jobs in map")
}

func setupBlockingWaitMCP(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-mcp-blocking", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "wait_for_job", Description: "wait for a job"}, func(ctx context.Context, req *mcp.CallToolRequest, input struct {
		JobID   string `json:"job_id"`
		Timeout int    `json:"timeout,omitempty"`
	}) (*mcp.CallToolResult, struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}, error) {
		<-ctx.Done()
		return nil, struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}{}, ctx.Err()
	})

	t1, t2 := mcp.NewInMemoryTransports()

	_, err := server.Connect(ctx, t1, nil)
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })

	origServers := mcpServers
	origToolMap := mcpToolToServer

	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Config:  MCPConfig{Timeout: 10 * time.Second},
		Client:  client,
		Session: session,
	}
	for tool, err := range session.Tools(ctx, nil) {
		require.NoError(t, err)
		srv.Tools = append(srv.Tools, tool)
	}

	mcpServers["img-mcp"] = srv
	mcpToolToServer["wait_for_job"] = "img-mcp"

	t.Cleanup(func() {
		mcpServers = origServers
		mcpToolToServer = origToolMap
	})
}

func TestWaitForAsyncJob_CleanupOnCancel(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	setupBlockingWaitMCP(t)

	ctx, cancel := context.WithCancel(context.Background())

	entry := &jobEntry[asyncJobPayload]{
		jobID:   "job-cancel-test",
		payload: asyncJobPayload{sessionID: 1},
		network: "testnet",
		channel: "#test",
		nick:    "testuser",
		cancel:  cancel,
	}
	asyncJobMgr.jobs["job-cancel-test"] = entry

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	waitForAsyncJob(ctx, entry)

	time.Sleep(100 * time.Millisecond)

	asyncJobMgr.mu.Lock()
	_, exists := asyncJobMgr.jobs["job-cancel-test"]
	asyncJobMgr.mu.Unlock()
	assert.False(t, exists, "cancelled job should be removed from asyncJobMgr.jobs")
}

func setupImmediateResultMCP(t *testing.T, resultText string) {
	t.Helper()

	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-mcp-immediate", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "wait_for_job", Description: "wait for a job"}, func(ctx context.Context, req *mcp.CallToolRequest, input struct {
		JobID string `json:"job_id"`
	}) (*mcp.CallToolResult, struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
	}, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: resultText}}}, struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		}{JobID: input.JobID, Status: "completed"}, nil
	})

	t1, t2 := mcp.NewInMemoryTransports()

	_, err := server.Connect(ctx, t1, nil)
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })

	origServers := mcpServers
	origToolMap := mcpToolToServer

	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Config:  MCPConfig{Timeout: 10 * time.Second},
		Client:  client,
		Session: session,
	}
	for tool, err := range session.Tools(ctx, nil) {
		require.NoError(t, err)
		srv.Tools = append(srv.Tools, tool)
	}

	mcpServers["img-mcp"] = srv
	mcpToolToServer["wait_for_job"] = "img-mcp"

	t.Cleanup(func() {
		mcpServers = origServers
		mcpToolToServer = origToolMap
	})
}

func TestWaitForAsyncJob_InlineDelivery(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	setupImmediateResultMCP(t, `{"job_id":"inline-1","status":"completed","result":{"images":[{"url":"http://example.com/img.png"}]}}`)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	require.NoError(t, createPendingJob(sid, "inline-1", "generate_image_async", "img-mcp"), "createPendingJob")

	job := registerAsyncJob("inline-1", sid, "generate_image_async", "img-mcp", "testnet", "#test", "testuser", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, job, "registerAsyncJob should return job")

	var inlineResult string
	received := make(chan struct{})
	go func() {
		select {
		case inlineResult = <-job.payload.inlineResultCh:
			close(received)
		case <-time.After(3 * time.Second):
		}
	}()

	select {
	case <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for inline result")
	}

	assert.Contains(t, inlineResult, "inline-1", "inline result should contain job_id")
	assert.Contains(t, inlineResult, "completed", "inline result should contain status")

	asyncJobMgr.mu.Lock()
	_, exists := asyncJobMgr.jobs["inline-1"]
	asyncJobMgr.mu.Unlock()
	assert.False(t, exists, "job should be removed from asyncJobMgr.jobs after inline delivery")

	var pj PendingJob
	err := theDB.Where("job_id = ?", "inline-1").First(&pj).Error
	require.NoError(t, err, "find pending job")
	assert.Equal(t, "delivered", pj.Status, "job should be delivered directly (skipping completed state)")
}

func TestWaitForAsyncJob_AsyncDeliveryWhenNotWaiting(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)
	setupImmediateResultMCP(t, `{"job_id":"async-1","status":"completed"}`)
	_ = setupMockDeps(t)

	queueMgr.UpdateServiceLimits(map[string]Service{"testsvc": {Parallel: 1}, "": {Parallel: 1}})

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "sys")

	require.NoError(t, createPendingJob(sid, "async-1", "generate_image_async", "img-mcp"), "createPendingJob")

	job := registerAsyncJob("async-1", sid, "generate_image_async", "img-mcp", "testnet", "#test", "testuser", ensureTestUser(t, "testnet", "testuser"))
	require.NotNil(t, job, "registerAsyncJob should return job")

	time.Sleep(500 * time.Millisecond)

	select {
	case <-job.payload.inlineResultCh:
		t.Fatal("inlineResultCh should not receive when no one is waiting")
	default:
	}

	var pj PendingJob
	err := theDB.Where("job_id = ?", "async-1").First(&pj).Error
	require.NoError(t, err)
	assert.Equal(t, "delivered", pj.Status, "job should be fully delivered via onAsyncJobCompleted + queue path")
}

func TestDeliverInlinePendingJob_SkipsCompletedState(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	require.NoError(t, createPendingJob(sid, "inline-db-1", "generate_image_async", "img-mcp"), "createPendingJob")

	var pj PendingJob
	err := theDB.Where("job_id = ?", "inline-db-1").First(&pj).Error
	require.NoError(t, err)
	assert.Equal(t, "pending", pj.Status, "initial status should be pending")

	require.NoError(t, deliverInlinePendingJob("inline-db-1", "result text"), "deliverInlinePendingJob")

	err = theDB.Where("job_id = ?", "inline-db-1").First(&pj).Error
	require.NoError(t, err)
	assert.Equal(t, "delivered", pj.Status, "status should go directly to delivered")
	require.NotNil(t, pj.Result, "result should be set")
	assert.Equal(t, "result text", *pj.Result, "result text should match")
	require.NotNil(t, pj.CompletedAt, "completed_at should be set")
}

func TestDeliverInlinePendingJob_IdempotentOnWrongStatus(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	require.NoError(t, createPendingJob(sid, "inline-db-2", "generate_image_async", "img-mcp"), "createPendingJob")
	require.NoError(t, completePendingJob("inline-db-2", "already done"), "completePendingJob")

	err := deliverInlinePendingJob("inline-db-2", "result text")
	assert.NoError(t, err, "should not error when status is not pending")

	var pj PendingJob
	err = theDB.Where("job_id = ?", "inline-db-2").First(&pj).Error
	require.NoError(t, err)
	assert.Equal(t, "completed", pj.Status, "status should remain completed (unchanged)")
}
