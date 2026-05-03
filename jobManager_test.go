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

func setupJMTestDB(t *testing.T) {
	t.Helper()
	db, err := initDB(DatabaseConfig{Path: t.TempDir() + "/test.db"})
	require.NoError(t, err, "initDB")
	theDB = db
	t.Cleanup(func() {
		closeDB(theDB)
		theDB = nil
	})
}

func setupTestJobManager(t *testing.T) {
	t.Helper()
	queueMgr = NewQueueManager([]string{"queued"}, "started", 5)
	queueMgr.UpdateServiceLimits(map[string]Service{"testsvc": {Parallel: 1}})
	queueMgr.Start()
	chatContextsMap = make(map[string]ChatContext)
	contextLastActive = make(map[string]int64)
	jobMgr.jobs = make(map[string]*asyncJob)
	jobMgr.ctx, jobMgr.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		if queueMgr != nil {
			queueMgr.Stop()
		}
		if jobMgr.cancel != nil {
			jobMgr.cancel()
		}
	})
}

func createTestSession(t *testing.T, ctxKey, network, channel, nick, chatCmd string) int64 {
	t.Helper()
	sid, err := createDBSession(ctxKey, network, channel, nick, chatCmd, "", "", "")
	require.NoError(t, err, "createDBSession")
	return sid
}

func insertTestMessage(t *testing.T, sessionID int64, role, content string) {
	t.Helper()
	err := insertDBMessage(sessionID, role, content, nil, nil, nil)
	require.NoError(t, err, "insertDBMessage")
}

func makeTestAIConfig() AIConfig {
	return AIConfig{
		Name:       "testchat",
		Service:    "testsvc",
		Model:      "test-model",
		MaxHistory: 20,
		Timeout:    30 * time.Second,
	}
}

type mockChatRunner struct {
	setChannelCalled bool
	setChannelCh     string
	setChannelNick   string
	runTurnCalled    int
	runTurnFn        func(messages []ChatMessage) ([]ChatMessage, bool)
}

func (m *mockChatRunner) setChannel(channel, nick string) {
	m.setChannelCalled = true
	m.setChannelCh = channel
	m.setChannelNick = nick
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

	origGetBot := getBotFn
	origBotReady := botReadyFn
	origNewRunner := newChatRunnerFn
	origConfig := config

	getBotFn = func(network string) *Bot {
		if network == "testnet" {
			return &Bot{Client: mb.client, Network: mb.network}
		}
		return nil
	}

	botReadyFn = func(network, channel string) bool {
		return network == "testnet"
	}

	newChatRunnerFn = func(network Network, client *girc.Client, cfg AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				return messages, true
			},
		}
	}

	config = Config{
		Commands: Commands{
			Chats: map[string]AIConfig{
				"testchat": makeTestAIConfig(),
			},
		},
	}

	t.Cleanup(func() {
		getBotFn = origGetBot
		botReadyFn = origBotReady
		newChatRunnerFn = origNewRunner
		config = origConfig
	})

	return mb
}

func TestDeliverAsyncResult_SameSession(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	mb := setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "you are helpful")
	insertTestMessage(t, sid, "user", "draw me a picture")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "draw"}},
		Config:    cfg,
		SessionID: sid,
	}

	require.NoError(t, createPendingJob(sid, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	require.NoError(t, completePendingJob("job-1", "image generated successfully"), "completePendingJob")

	job := &asyncJob{
		JobID:     "job-1",
		SessionID: sid,
		CtxKey:    ctxKey,
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

	ctx := chatContextsMap[ctxKey]
	assert.Equal(t, sid, ctx.SessionID, "SessionID")

	hasAsyncMsg := false
	for _, m := range ctx.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			hasAsyncMsg = true
			assert.Contains(t, m.Content, "image generated successfully", "async result message missing result text")
		}
	}
	assert.True(t, hasAsyncMsg, "expected async result system message in context")
	_ = mb
}

func TestDeliverAsyncResult_DifferentSession(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	mb := setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionA, "system", "you are helpful")
	insertTestMessage(t, sessionA, "user", "draw me a picture")

	sessionB := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionB, "system", "you are helpful")
	insertTestMessage(t, sessionB, "user", "tell me a joke")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "joke"}},
		Config:    cfg,
		SessionID: sessionB,
	}

	require.NoError(t, createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	require.NoError(t, completePendingJob("job-1", "image url: http://example.com/img.png"), "completePendingJob")

	job := &asyncJob{
		JobID:     "job-1",
		SessionID: sessionA,
		CtxKey:    ctxKey,
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

	ctx := chatContextsMap[ctxKey]
	assert.Equal(t, sessionA, ctx.SessionID, "should have switched back to session A")

	sessB, err := getDBSessionByID(sessionB)
	require.NoError(t, err, "getDBSessionByID")
	assert.Equal(t, "completed", sessB.Status, "session B status")

	sessA, err := getDBSessionByID(sessionA)
	require.NoError(t, err, "getDBSessionByID")
	assert.Equal(t, "active", sessA.Status, "session A status")

	hasAsyncMsg := false
	for _, m := range ctx.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			hasAsyncMsg = true
		}
	}
	assert.True(t, hasAsyncMsg, "expected async result system message after switch")
	_ = mb
}

func TestOnAsyncJobCompleted_UserBusyWaitsThenDelivers(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	queueMgr.UpdateServiceLimits(map[string]Service{"testsvc": {Parallel: 1}, "": {Parallel: 1}})

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionA, "system", "you are helpful")
	insertTestMessage(t, sessionA, "user", "draw me a picture")

	sessionB := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionB, "system", "you are helpful")
	insertTestMessage(t, sessionB, "user", "tell me a joke")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "joke"}},
		Config:    cfg,
		SessionID: sessionB,
	}

	require.NoError(t, createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")

	blockDone := make(chan struct{})
	queueMgr.Enqueue("testnet", "#test", "testuser", "", "",
		func(ctx context.Context, output chan<- string) {
			<-blockDone
		})
	time.Sleep(100 * time.Millisecond)

	job := &asyncJob{
		JobID:     "job-1",
		SessionID: sessionA,
		CtxKey:    ctxKey,
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
	}

	onAsyncJobCompleted(job, "result text")
	time.Sleep(100 * time.Millisecond)

	assert.NotEqual(t, sessionA, chatContextsMap[ctxKey].SessionID, "session should NOT have switched while blocking job holds the slot")

	close(blockDone)
	time.Sleep(300 * time.Millisecond)

	assert.Equal(t, sessionA, chatContextsMap[ctxKey].SessionID, "session should have switched to session A after delivery completed")
}

func TestOnAsyncJobCompleted_MultipleJobsWhileBusy(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionA, "system", "you are helpful")
	insertTestMessage(t, sessionA, "user", "draw me a picture")

	sessionB := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionB, "system", "you are helpful")
	insertTestMessage(t, sessionB, "user", "tell me a joke")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "joke"}},
		Config:    cfg,
		SessionID: sessionB,
	}

	createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp")
	createPendingJob(sessionA, "job-2", "generate_image_async", "img-mcp")

	blockDone := make(chan struct{})
	queueMgr.Enqueue("testnet", "#test", "testuser", "testsvc", "",
		func(ctx context.Context, output chan<- string) {
			<-blockDone
		})
	time.Sleep(100 * time.Millisecond)

	job1 := &asyncJob{
		JobID: "job-1", SessionID: sessionA, CtxKey: ctxKey,
		ToolName: "generate_image_async", MCPServer: "img-mcp",
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}
	job2 := &asyncJob{
		JobID: "job-2", SessionID: sessionA, CtxKey: ctxKey,
		ToolName: "generate_image_async", MCPServer: "img-mcp",
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	onAsyncJobCompleted(job1, "image 1 ready")
	onAsyncJobCompleted(job2, "image 2 ready")

	time.Sleep(100 * time.Millisecond)

	close(blockDone)

	waitForSessionSwitch(t, ctxKey, sessionA, 5*time.Second)

	ctx := chatContextsMap[ctxKey]
	require.Equal(t, sessionA, ctx.SessionID, "SessionID")

	asyncCount := 0
	for _, m := range ctx.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			asyncCount++
		}
	}
	assert.GreaterOrEqual(t, asyncCount, 2, "expected at least 2 async result messages")
}

func TestSwitchToSession_CompletesOldSession(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionA, "system", "sys prompt A")
	insertTestMessage(t, sessionA, "user", "hello A")

	sessionB := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionB, "system", "sys prompt B")
	insertTestMessage(t, sessionB, "user", "hello B")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys prompt B"}},
		Config:    cfg,
		SessionID: sessionB,
	}

	job := &asyncJob{
		JobID: "job-1", SessionID: sessionA, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	switchToSession(job)

	ctx := chatContextsMap[ctxKey]
	assert.Equal(t, sessionA, ctx.SessionID, "SessionID")

	sessB, _ := getDBSessionByID(sessionB)
	assert.Equal(t, "completed", sessB.Status, "session B status")

	sessA, _ := getDBSessionByID(sessionA)
	assert.Equal(t, "active", sessA.Status, "session A status")

	foundUserMsg := false
	for _, m := range ctx.Messages {
		if m.Role == "user" && m.Content == "hello A" {
			foundUserMsg = true
		}
	}
	assert.True(t, foundUserMsg, "expected session A's messages to be loaded after switch")
}

func TestSwitchToSession_NoOldSession(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionA, "system", "sys prompt A")
	insertTestMessage(t, sessionA, "user", "hello A")

	chatContextsMap[ctxKey] = ChatContext{}

	job := &asyncJob{
		JobID: "job-1", SessionID: sessionA, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	switchToSession(job)

	ctx := chatContextsMap[ctxKey]
	assert.Equal(t, sessionA, ctx.SessionID, "SessionID")
	assert.NotEmpty(t, ctx.Messages, "expected messages to be loaded")
}

func TestSwitchToSession_SameSessionIsNoop(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")
	insertTestMessage(t, sid, "user", "hello")

	originalCtx := ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "hello"}},
		Config:    cfg,
		SessionID: sid,
	}
	chatContextsMap[ctxKey] = originalCtx

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	switchToSession(job)

	ctx := chatContextsMap[ctxKey]
	assert.Equal(t, len(originalCtx.Messages), len(ctx.Messages), "messages count should not change")

	sess, _ := getDBSessionByID(sid)
	assert.Equal(t, "active", sess.Status, "session status")
}

func TestSwitchToSession_InvalidChatCommand(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "deletedcmd")
	insertTestMessage(t, sessionA, "system", "sys")

	sessionB := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    makeTestAIConfig(),
		SessionID: sessionB,
	}

	job := &asyncJob{
		JobID: "job-1", SessionID: sessionA, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	switchToSession(job)

	ctx := chatContextsMap[ctxKey]
	assert.Equal(t, sessionB, ctx.SessionID, "should remain session B when chat command not found")
}

func TestDeliverAsyncResult_NoContext(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionA, "system", "sys")
	require.NoError(t, createPendingJob(sessionA, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	completePendingJob("job-1", "result")

	delete(chatContextsMap, ctxKey)

	job := &asyncJob{
		JobID: "job-1", SessionID: sessionA, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()
	assert.Equal(t, sessionA, ctx.SessionID, "should have loaded from DB")

	hasAsyncMsg := false
	for _, m := range ctx.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "Background task completed") {
			hasAsyncMsg = true
		}
	}
	assert.True(t, hasAsyncMsg, "expected async result system message after loading context from DB")
}

func TestDeliverAsyncResult_UsesMockRunner(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "result data")

	var runner *mockChatRunner
	origNewRunner := newChatRunnerFn
	newChatRunnerFn = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		runner = &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				return messages, true
			},
		}
		return runner
	}
	defer func() { newChatRunnerFn = origNewRunner }()

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		ToolName: "generate_image_async", MCPServer: "img-mcp",
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

	require.NotNil(t, runner, "runner was never created")
	assert.True(t, runner.setChannelCalled, "setChannel was not called")
	assert.Equal(t, "#test", runner.setChannelCh, "setChannel channel")
	assert.Equal(t, "testuser", runner.setChannelNick, "setChannel nick")
	assert.NotZero(t, runner.runTurnCalled, "runTurn was never called")
}

func TestDeliverAsyncResult_RunnerSeesInjectedResult(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "image url: http://example.com/img.png")

	var receivedMessages []ChatMessage
	origNewRunner := newChatRunnerFn
	newChatRunnerFn = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				receivedMessages = messages
				return messages, true
			},
		}
	}
	defer func() { newChatRunnerFn = origNewRunner }()

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		ToolName: "generate_image_async", MCPServer: "img-mcp",
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

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
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "image 1")
	createPendingJob(sid, "job-2", "generate_image_async", "img-mcp")
	completePendingJob("job-2", "image 2")

	turnCount := 0
	origNewRunner := newChatRunnerFn
	newChatRunnerFn = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				turnCount++
				return messages, true
			},
		}
	}
	defer func() { newChatRunnerFn = origNewRunner }()

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		ToolName: "generate_image_async", MCPServer: "img-mcp",
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

	assert.Equal(t, 1, turnCount, "runTurn should be called once (both jobs delivered in one turn)")

	jobs, _ := getCompletedPendingJobs(sid)
	assert.Empty(t, jobs, "expected all jobs delivered")
}

func TestInjectAsyncResultFromDB(t *testing.T) {
	setupJMTestDB(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	result := "image url: http://example.com/test.png"
	pj := pendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Status:    "completed",
		Result:    &result,
	}

	ctx := chatContextsMap[ctxKey]
	injectAsyncResultFromDB(ctxKey, ctx, pj, "testnet", "#test", "testuser")

	ctx = chatContextsMap[ctxKey]
	require.Len(t, ctx.Messages, 2, "expected 2 messages")
	lastMsg := ctx.Messages[len(ctx.Messages)-1]
	assert.Equal(t, "system", lastMsg.Role, "injected message role")
	assert.Contains(t, lastMsg.Content, "Background task completed", "injected message missing expected text")
	assert.Contains(t, lastMsg.Content, "generate_image_async", "injected message missing tool name")
	assert.Contains(t, lastMsg.Content, "http://example.com/test.png", "injected message missing result")

	dbMsgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err, "loadDBSessionMessages")
	assert.Len(t, dbMsgs, 1, "expected 1 DB message (only the injected one)")
	assert.Equal(t, "system", dbMsgs[0].Role, "DB message role")
}

func TestInjectAsyncResultFromDB_AnthropicUserSuffix(t *testing.T) {
	setupJMTestDB(t)

	cfg := makeTestAIConfig()
	cfg.Model = "anthropic/claude-sonnet-4.6"
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	result := "image url: http://example.com/test.png"
	pj := pendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Status:    "completed",
		Result:    &result,
	}

	ctx := chatContextsMap[ctxKey]
	injectAsyncResultFromDB(ctxKey, ctx, pj, "testnet", "#test", "testuser")

	ctx = chatContextsMap[ctxKey]
	require.Len(t, ctx.Messages, 3, "expected 3 messages (sys + system result + user suffix)")
	assert.Equal(t, "system", ctx.Messages[1].Role)
	assert.Contains(t, ctx.Messages[1].Content, "Background task completed")
	assert.Equal(t, "user", ctx.Messages[2].Role, "last message should be user suffix")
	assert.Contains(t, ctx.Messages[2].Content, "Respond to the user based on the above background task result.")
}

func TestInjectAsyncResultFromDB_NeedsUserSuffixConfig(t *testing.T) {
	setupJMTestDB(t)

	cfg := makeTestAIConfig()
	cfg.NeedsUserSuffix = true
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	result := "image url: http://example.com/test.png"
	pj := pendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Status:    "completed",
		Result:    &result,
	}

	ctx := chatContextsMap[ctxKey]
	injectAsyncResultFromDB(ctxKey, ctx, pj, "testnet", "#test", "testuser")

	ctx = chatContextsMap[ctxKey]
	require.Len(t, ctx.Messages, 3, "expected 3 messages (sys + system result + user suffix)")
	assert.Equal(t, "user", ctx.Messages[2].Role, "last message should be user suffix")
}

func TestInjectAsyncResultFromDB_NilResult(t *testing.T) {
	setupJMTestDB(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	pj := pendingJob{
		SessionID: &sid,
		JobID:     "job-1",
		ToolName:  "generate_image_async",
		Status:    "completed",
		Result:    nil,
	}

	ctx := chatContextsMap[ctxKey]
	injectAsyncResultFromDB(ctxKey, ctx, pj, "testnet", "#test", "testuser")

	ctx = chatContextsMap[ctxKey]
	lastMsg := ctx.Messages[len(ctx.Messages)-1]
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
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")

	jobMgr.jobs["job-1"] = &asyncJob{JobID: "job-1"}

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	onAsyncJobCompleted(job, "result")

	_, exists := jobMgr.jobs["job-1"]
	assert.False(t, exists, "job should be removed from in-memory map after completion")
}

func TestOnAsyncJobCompleted_MarksCompletedInDB(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	onAsyncJobCompleted(job, "the image result")

	var pj pendingJob
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, theDB.Get(&pj, "SELECT * FROM pending_jobs WHERE job_id = ?", "job-1"), "query job")
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
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "result")

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

	var pj pendingJob
	require.NoError(t, theDB.Get(&pj, "SELECT * FROM pending_jobs WHERE job_id = ?", "job-1"), "query job")
	assert.Equal(t, "delivered", pj.Status, "job status")
}

func waitForSessionSwitch(t *testing.T, ctxKey string, expectedSID int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		chatContextsMutex.Lock()
		ctx := chatContextsMap[ctxKey]
		chatContextsMutex.Unlock()
		if ctx.SessionID == expectedSID {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()
	require.Equal(t, expectedSID, ctx.SessionID, "timed out waiting for session switch")
}

func TestDeliverAsyncResult_NoContextLoaded_LoadsFromDB(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "you are helpful")
	insertTestMessage(t, sid, "user", "draw me a picture")

	require.NoError(t, createPendingJob(sid, "job-1", "generate_image_async", "img-mcp"), "createPendingJob")
	require.NoError(t, completePendingJob("job-1", "image url: http://example.com/img.png"), "completePendingJob")

	delete(chatContextsMap, ctxKey)

	job := &asyncJob{
		JobID:     "job-1",
		SessionID: sid,
		CtxKey:    ctxKey,
		ToolName:  "generate_image_async",
		MCPServer: "img-mcp",
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
	}

	output := make(chan string, 100)
	deliverAsyncResult(job, context.Background(), output)

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()

	assert.Equal(t, sid, ctx.SessionID, "should have loaded from DB")
	assert.NotEmpty(t, ctx.Messages, "expected messages to be loaded from DB")

	hasAsyncMsg := false
	for _, m := range ctx.Messages {
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
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "you are helpful")

	delete(chatContextsMap, ctxKey)

	job := &asyncJob{
		JobID:     "job-1",
		SessionID: sid,
		CtxKey:    ctxKey,
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
	}

	msg := switchToSession(job)
	assert.Equal(t, "", msg, "switchToSession should return empty string when no prior session")

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()

	assert.Equal(t, sid, ctx.SessionID, "should have loaded from DB")
	_ = cfg
}

func TestRecoverPendingJobs(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	ctxKey := "testnet#testuser"
	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	createPendingJob(sid, "recovery-job-1", "generate_image_async", "img-mcp")

	jobMgr.cancel()
	jobMgr.ctx, jobMgr.cancel = context.WithCancel(context.Background())

	recoverPendingJobs()

	jobMgr.mu.Lock()
	_, exists := jobMgr.jobs["recovery-job-1"]
	jobMgr.mu.Unlock()
	assert.True(t, exists, "expected job to be recovered in memory")

	jobMgr.cancel()
	time.Sleep(100 * time.Millisecond)
}

func TestRecoverPendingJobs_NoDB(t *testing.T) {
	theDB = nil
	recoverPendingJobs()
}

func TestRegisterAsyncJob_Duplicate(t *testing.T) {
	setupTestJobManager(t)
	jobMgr.ctx, jobMgr.cancel = context.WithCancel(context.Background())
	defer jobMgr.cancel()

	registerAsyncJob("dup-job", 1, "key", "tool", "server", "net", "#chan", "user")
	registerAsyncJob("dup-job", 1, "key", "tool", "server", "net", "#chan", "user")

	jobMgr.mu.Lock()
	count := len(jobMgr.jobs)
	jobMgr.mu.Unlock()
	assert.Equal(t, 1, count, "expected 1 job (duplicate should be ignored)")
}

func TestSwitchToSession_DBMessagesWithToolCalls(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	toolCallsJSON, _ := json.Marshal([]ToolCall{
		{ID: "tc-1", Type: "function", Function: FunctionCall{Name: "test_tool", Arguments: `{"arg":"val"}`}},
	})
	toolCallsStr := string(toolCallsJSON)
	insertDBMessage(sessionA, "system", "sys", nil, nil, nil)
	insertDBMessage(sessionA, "assistant", "using tool", &toolCallsStr, nil, nil)
	toolCallID := "tc-1"
	insertDBMessage(sessionA, "tool", "tool result", nil, &toolCallID, nil)

	sessionB := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys B"}},
		Config:    cfg,
		SessionID: sessionB,
	}

	job := &asyncJob{
		JobID: "job-1", SessionID: sessionA, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}
	switchToSession(job)

	ctx := chatContextsMap[ctxKey]
	require.Equal(t, sessionA, ctx.SessionID, "SessionID")

	foundToolCall := false
	foundToolCallID := false
	for _, m := range ctx.Messages {
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
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	cfg.MaxHistory = 3
	ctxKey := "testnet#testuser"

	sessionA := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sessionA, "system", "sys")
	for i := 0; i < 10; i++ {
		insertTestMessage(t, sessionA, "user", fmt.Sprintf("msg %d", i))
	}

	sessionB := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	chatContextsMap[ctxKey] = ChatContext{
		Config:    cfg,
		SessionID: sessionB,
	}

	config.Commands.Chats["testchat"] = cfg

	job := &asyncJob{
		JobID: "job-1", SessionID: sessionA, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}
	switchToSession(job)

	ctx := chatContextsMap[ctxKey]
	assert.LessOrEqual(t, len(ctx.Messages), cfg.MaxHistory+1, "messages not truncated")
	assert.Equal(t, "system", ctx.Messages[0].Role, "first message should be system prompt after truncation")
}

func TestDeliverAsyncResult_RunningDuringTurn(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	cfg := makeTestAIConfig()
	ctxKey := "testnet#testuser"

	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")
	insertTestMessage(t, sid, "system", "sys")

	chatContextsMap[ctxKey] = ChatContext{
		Messages:  []ChatMessage{{Role: "system", Content: "sys"}},
		Config:    cfg,
		SessionID: sid,
	}

	createPendingJob(sid, "job-1", "generate_image_async", "img-mcp")
	completePendingJob("job-1", "result")

	runningDuringTurn := false
	var wg sync.WaitGroup
	wg.Add(1)

	origNewRunner := newChatRunnerFn
	newChatRunnerFn = func(network Network, client *girc.Client, c AIConfig, _ context.Context, _ chan<- string) chatRunnerInterface {
		return &mockChatRunner{
			runTurnFn: func(messages []ChatMessage) ([]ChatMessage, bool) {
				runningDuringTurn = queueMgr.IsRunning("testnet", "#test", "testuser")
				wg.Done()
				return messages, true
			},
		}
	}
	defer func() { newChatRunnerFn = origNewRunner }()

	job := &asyncJob{
		JobID: "job-1", SessionID: sid, CtxKey: ctxKey,
		Network: "testnet", Channel: "#test", Nick: "testuser",
	}

	queueMgr.Enqueue("testnet", "#test", "testuser", "testsvc", "", func(ctx context.Context, output chan<- string) {
		deliverAsyncResult(job, ctx, output)
	})

	wg.Wait()

	assert.True(t, runningDuringTurn, "queueMgr.IsRunning() returned false during runTurn — item should be active")
}

func TestDeliverAsyncResult_RunningDuringTurn_BusyPath(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	_ = setupMockDeps(t)

	ready := make(chan struct{})
	unblockFirst := make(chan struct{})

	queueMgr.Enqueue("testnet", "#test", "testuser", "testsvc", "", func(ctx context.Context, output chan<- string) {
		ready <- struct{}{}
		<-unblockFirst
	})

	<-ready

	require.True(t, queueMgr.IsRunning("testnet", "#test", "testuser"), "expected first job to be running")

	runningDuringTurn := false
	var turnWg sync.WaitGroup
	turnWg.Add(1)

	queueMgr.Enqueue("testnet", "#test", "testuser", "testsvc", "", func(ctx context.Context, output chan<- string) {
		runningDuringTurn = queueMgr.IsRunning("testnet", "#test", "testuser")
		turnWg.Done()
	})

	current, pending := queueMgr.QueueStatus("testnet", "#test", "testuser")
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
	setupJMTestDB(t)
	setupTestJobManager(t)
	setupCancelTestMCP(t)

	ctxKey := "testnet#testuser"
	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	jobMgr.jobs["job-a"] = &asyncJob{
		JobID:     "job-a",
		SessionID: sid,
		CtxKey:    ctxKey,
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
		cancel:    func() {},
	}
	jobMgr.jobs["job-b"] = &asyncJob{
		JobID:     "job-b",
		SessionID: sid,
		CtxKey:    ctxKey,
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
		cancel:    func() {},
	}

	otherSID := createTestSession(t, "testnet#otheruser", "testnet", "#test", "otheruser", "testchat")
	jobMgr.jobs["job-c"] = &asyncJob{
		JobID:     "job-c",
		SessionID: otherSID,
		CtxKey:    "testnet#otheruser",
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "otheruser",
		cancel:    func() {},
	}

	cancelAsyncJobsForSession(sid)

	_, exists := jobMgr.jobs["job-a"]
	assert.False(t, exists, "job-a should be removed from jobMgr.jobs")
	_, exists = jobMgr.jobs["job-b"]
	assert.False(t, exists, "job-b should be removed from jobMgr.jobs")
	_, exists = jobMgr.jobs["job-c"]
	assert.True(t, exists, "job-c should still exist (different session)")
}

func TestCancelAsyncJobsForSession_NoJobs(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)

	cancelAsyncJobsForSession(99999)
}

func TestCancelAsyncJobsForSession_DeletesFromMap(t *testing.T) {
	setupJMTestDB(t)
	setupTestJobManager(t)
	setupCancelTestMCP(t)

	ctxKey := "testnet#testuser"
	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	jobMgr.jobs["job-x"] = &asyncJob{
		JobID:     "job-x",
		SessionID: sid,
		CtxKey:    ctxKey,
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
		cancel:    func() {},
	}

	cancelAsyncJobsForSession(sid)

	assert.Empty(t, jobMgr.jobs, "expected 0 jobs in map")
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
	setupJMTestDB(t)
	setupTestJobManager(t)
	setupBlockingWaitMCP(t)

	ctx, cancel := context.WithCancel(context.Background())

	job := &asyncJob{
		JobID:     "job-cancel-test",
		SessionID: 1,
		CtxKey:    "testnet#testuser",
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
		cancel:    cancel,
	}
	jobMgr.jobs["job-cancel-test"] = job

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	waitForAsyncJob(ctx, job)

	time.Sleep(100 * time.Millisecond)

	jobMgr.mu.Lock()
	_, exists := jobMgr.jobs["job-cancel-test"]
	jobMgr.mu.Unlock()
	assert.False(t, exists, "cancelled job should be removed from jobMgr.jobs")
}
