package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	os.Exit(m.Run())
}

func setupTestDB(t *testing.T) (*gorm.DB, func()) {
	t.Helper()
	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := initDB(DatabaseConfig{Path: dbPath, MaxAgeDays: 90})
	require.NoError(t, err, "failed to init test db")
	oldDB := theDB
	theDB = db
	return db, func() {
		theDB = oldDB
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	}
}

func TestDBSessionRoundtrip(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	chatContextsMutex.Lock()
	chatContextsMap["net#chanuser"] = ChatContext{
		Messages: []ChatMessage{
			{Role: "system", Content: "You are a helpful assistant"},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
		},
		Config:    AIConfig{Name: "testcmd", MaxHistory: 5, Temperature: 0.7},
		SessionID: 0,
	}
	chatContextsMutex.Unlock()

	ctxKey := "net#chanuser"
	sid, err := createDBSession(ctxKey, "net", "#chan", "user", "testcmd", "", "testservice", "testmodel")
	require.NoError(t, err, "failed to create session")

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	ctx.SessionID = sid
	chatContextsMap[ctxKey] = ctx
	chatContextsMutex.Unlock()

	for _, msg := range chatContextsMap[ctxKey].Messages {
		toolCallsJSON := func(m ChatMessage) *string {
			if len(m.ToolCalls) > 0 {
				data, _ := json.Marshal(m.ToolCalls)
				s := string(data)
				return &s
			}
			return nil
		}(msg)
		err := insertDBMessage(sid, msg.Role, msg.Content, toolCallsJSON, nil, nil)
		require.NoError(t, err, "failed to insert message")
	}

	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	oldChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"testcmd": {Name: "testcmd", MaxHistory: 5, Temperature: 0.7},
	}
	defer func() { config.Commands.Chats = oldChats }()

	LoadContextStore()

	chatContextsMutex.Lock()
	ctx2, ok := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()

	require.True(t, ok, "expected context to be loaded")

	assert.Len(t, ctx2.Messages, 3, "messages count")

	assert.Equal(t, "system", ctx2.Messages[0].Role, "first message role")

	assert.Equal(t, "You are a helpful assistant", ctx2.Messages[0].Content, "system prompt")
}

func TestDBCleanupByAge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, err := createDBSession("oldkey", "net", "#chan", "user", "testcmd", "", "", "")
	require.NoError(t, err, "failed to create session")
	insertDBMessage(sid, "user", "hello", nil, nil, nil)

	pastTime := time.Now().AddDate(0, 0, -100)
	theDB.Model(&Session{}).Where("id = ?", sid).Update("last_active", pastTime)

	affected, err := cleanupDBSessions(90)
	require.NoError(t, err, "cleanup failed")
	assert.Equal(t, int64(1), affected, "sessions cleaned up")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "failed to get session")
	assert.Equal(t, "completed", session.Status, "session status")
}

func TestDBSessionCreateAndMessage(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, err := createDBSession("testkey", "net", "#chan", "nick", "chat", "", "", "")
	require.NoError(t, err, "createDBSession failed")
	assert.NotZero(t, sid, "expected non-zero session id")

	err = insertDBMessage(sid, "system", "You are helpful", nil, nil, nil)
	require.NoError(t, err, "insertDBMessage failed")
	err = insertDBMessage(sid, "user", "Hello!", nil, nil, nil)
	require.NoError(t, err, "insertDBMessage failed")

	msgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err, "loadDBSessionMessages failed")
	assert.Len(t, msgs, 2, "messages count")
	assert.Equal(t, "system", msgs[0].Role, "first message role")
	assert.Equal(t, "Hello!", msgs[1].Content, "second message content")
}

func TestDBSessionComplete(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, _ := createDBSession("testkey", "net", "#chan", "nick", "chat", "", "", "")

	err := completeDBSession(sid)
	require.NoError(t, err, "completeDBSession failed")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status")
}

func TestDBDeleteSession(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, _ := createDBSession("testkey", "net", "#chan", "nick", "chat", "", "", "")
	insertDBMessage(sid, "user", "hello", nil, nil, nil)

	err := deleteDBSession(sid)
	require.NoError(t, err, "deleteDBSession failed")

	_, err = getDBSessionByID(sid)
	assert.Error(t, err, "expected error getting deleted session")
}

func TestDBUserSessions(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	for i := 0; i < 3; i++ {
		createDBSession("key"+string(rune('a'+i)), "net", "#chan", "nick", "chat", "", "", "")
	}
	createDBSession("other", "net", "#chan", "other", "chat", "", "", "")

	sessions, err := getUserDBSessions("net", "#chan", "nick", 10)
	require.NoError(t, err, "getUserDBSessions failed")
	assert.Len(t, sessions, 3, "sessions for nick")
}

func TestDBUserStats(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid1, _ := createDBSession("key1", "net", "#chan", "nick", "chat", "", "", "")
	insertDBMessage(sid1, "system", "sys", nil, nil, nil)
	insertDBMessage(sid1, "user", "hello", nil, nil, nil)

	sid2, _ := createDBSession("key2", "net", "#chan", "nick", "chat", "", "", "")
	insertDBMessage(sid2, "system", "sys", nil, nil, nil)

	sessionCount, messageCount, err := getUserDBStats("net", "#chan", "nick")
	require.NoError(t, err, "getUserDBStats failed")
	assert.Equal(t, 2, sessionCount, "session count")
	assert.Equal(t, 3, messageCount, "message count")
}

func TestDBDeleteUserSessions(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	createDBSession("key1", "net", "#chan", "nick", "chat", "", "", "")
	createDBSession("key2", "net", "#chan", "nick", "chat", "", "", "")
	createDBSession("key3", "net", "#chan", "other", "chat", "", "", "")

	affected, err := deleteUserDBSessions("net", "#chan", "nick")
	require.NoError(t, err, "deleteUserDBSessions failed")
	assert.Equal(t, int64(2), affected, "sessions deleted")

	sessions, _ := getUserDBSessions("net", "#chan", "nick", 10)
	assert.Len(t, sessions, 0, "sessions for nick after delete")
}

func TestDatabaseConfigDefaults(t *testing.T) {
	cfg := DatabaseConfig{}
	cfg.SetDefaults()

	assert.Equal(t, "sqlite", cfg.Driver, "default driver")
	assert.Equal(t, "data/dave.db", cfg.Path, "default path")
	assert.Equal(t, 90, cfg.MaxAgeDays, "default MaxAgeDays")
}

func TestDatabaseConfigNoOverwrite(t *testing.T) {
	cfg := DatabaseConfig{
		Driver:     "postgres",
		Path:       "custom/path.db",
		MaxAgeDays: 30,
	}
	cfg.SetDefaults()

	assert.Equal(t, "postgres", cfg.Driver, "driver")
	assert.Equal(t, "custom/path.db", cfg.Path, "path")
	assert.Equal(t, 30, cfg.MaxAgeDays, "MaxAgeDays")
}

func TestDBToolCalls(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, _ := createDBSession("testkey", "net", "#chan", "nick", "chat", "", "", "")

	toolCalls := []ToolCall{
		{ID: "tc1", Type: "function", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"sf"}`}},
	}
	tcData, _ := json.Marshal(toolCalls)
	tcJSON := string(tcData)
	toolCallID := "tc1"

	err := insertDBMessage(sid, "assistant", "", &tcJSON, &toolCallID, nil)
	require.NoError(t, err, "insertDBMessage with tool calls failed")

	msgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err, "loadDBSessionMessages failed")
	require.Len(t, msgs, 1, "messages count")

	if msgs[0].ToolCalls == nil || *msgs[0].ToolCalls != tcJSON {
		t.Error("tool_calls mismatch")
	}
	if msgs[0].ToolCallID == nil || *msgs[0].ToolCallID != "tc1" {
		t.Error("tool_call_id mismatch")
	}
}

func TestDBFirstMessage(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "net#chanuser"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 5}

	AddContext(cfg, ctxKey,
		ChatMessage{Role: "system", Content: "you are helpful"},
		"net", "#chan", "user")

	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sid, "expected session to be created")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "", session.FirstMessage, "first_message after system prompt")

	AddContext(cfg, ctxKey,
		ChatMessage{Role: "user", Content: "hello world this is my first message"},
		"net", "#chan", "user")

	session, err = getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "hello world this is my first message", session.FirstMessage, "first_message")

	AddContext(cfg, ctxKey,
		ChatMessage{Role: "user", Content: "this should not overwrite"},
		"net", "#chan", "user")

	session, err = getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "hello world this is my first message", session.FirstMessage, "first_message unchanged")
}

func TestClearContextCompletesSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	oldChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"testcmd": {Name: "testcmd", MaxHistory: 5},
	}
	defer func() { config.Commands.Chats = oldChats }()

	ctxKey := "net#chanuser"
	AddContext(AIConfig{Name: "testcmd", MaxHistory: 5}, ctxKey,
		ChatMessage{Role: "user", Content: "hello"},
		"net", "#chan", "user")

	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sid, "expected session to be created")

	ClearContext(ctxKey)

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status after ClearContext")

	assert.False(t, ContextExists(ctxKey), "expected context to be cleared")
}

func TestContextLastActive(t *testing.T) {
	contextLastActive = make(map[string]int64)
	key := "testkey"

	before := time.Now().Unix()
	SetContextLastActive(key)
	after := time.Now().Unix()

	active := GetContextLastActive(key)
	assert.GreaterOrEqual(t, active, before, "GetContextLastActive >= before")
	assert.LessOrEqual(t, active, after, "GetContextLastActive <= after")

	DeleteContextLastActive(key)
	_, ok := contextLastActive[key]
	assert.False(t, ok, "expected key to be deleted")
}

func makeTestChatRunner(t *testing.T, cfg AIConfig) *chatRunner {
	t.Helper()
	return &chatRunner{
		cfg:     cfg,
		network: Network{Name: "testnet", Nick: "dave"},
		channel: "#chan",
		nick:    "testuser",
		ctxKey:  "testnet#chantestuser",
		ctx:     context.Background(),
		logger:  logxi.New("test"),
	}
}

func TestAddContext_OwnedSessionWritesToMap(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "testnet#chanuser"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 10}

	AddContext(cfg, ctxKey, ChatMessage{Role: "system", Content: "sys"}, "testnet", "#chan", "user")

	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sid, "session should be created")

	runner := makeTestChatRunner(t, cfg)
	runner.ctxKey = ctxKey
	runner.sessionID = sid

	msg := ChatMessage{Role: "assistant", Content: "hello"}
	runner.addContext(msg)

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()

	assert.False(t, runner.detached, "should not be detached")
	assert.Equal(t, sid, ctx.SessionID, "session ID should match")
	assert.Len(t, ctx.Messages, 2, "should have system + assistant")

	var dbMsgs []Message
	require.NoError(t, theDB.Where("session_id = ?", sid).Find(&dbMsgs).Error)
	assert.Len(t, dbMsgs, 2, "should have 2 DB messages")
}

func TestAddContext_DetachedWhenSessionReplaced(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "testnet#chanuser"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 10}

	AddContext(cfg, ctxKey, ChatMessage{Role: "system", Content: "sys"}, "testnet", "#chan", "user")

	chatContextsMutex.Lock()
	sidA := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sidA, "session A should be created")

	runner := makeTestChatRunner(t, cfg)
	runner.ctxKey = ctxKey
	runner.sessionID = sidA

	ClearContext(ctxKey)

	chatContextsMutex.Lock()
	clearedCtx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()
	assert.Equal(t, int64(0), clearedCtx.SessionID, "session ID should be 0 after ClearContext")
	assert.False(t, ContextExists(ctxKey), "context should not exist after ClearContext")

	msg := ChatMessage{Role: "assistant", Content: "detached msg"}
	runner.addContext(msg)

	assert.True(t, runner.detached, "should be detached after ClearContext")

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()
	assert.Equal(t, int64(0), ctx.SessionID, "shared map should NOT have the detached message")
	assert.Empty(t, ctx.Messages, "shared map messages should be empty")

	var dbMsgs []Message
	require.NoError(t, theDB.Where("session_id = ?", sidA).Order("id").Find(&dbMsgs).Error)
	assert.Len(t, dbMsgs, 2, "DB should have system + detached assistant message")
	if len(dbMsgs) >= 2 {
		assert.Equal(t, "assistant", dbMsgs[1].Role)
		assert.Equal(t, "detached msg", dbMsgs[1].Content)
	}
}

func TestAddContext_DetachedDoesNotPolluteNewSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "testnet#chanuser"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 10}

	AddContext(cfg, ctxKey, ChatMessage{Role: "system", Content: "sys A"}, "testnet", "#chan", "user")
	chatContextsMutex.Lock()
	sidA := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sidA)

	runnerA := makeTestChatRunner(t, cfg)
	runnerA.ctxKey = ctxKey
	runnerA.sessionID = sidA

	ClearContext(ctxKey)

	AddContext(cfg, ctxKey, ChatMessage{Role: "system", Content: "sys B"}, "testnet", "#chan", "user")
	chatContextsMutex.Lock()
	sidB := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sidB)
	require.NotEqual(t, sidA, sidB, "should be different sessions")

	detachedMsg := ChatMessage{Role: "assistant", Content: "from runner A"}
	runnerA.addContext(detachedMsg)

	assert.True(t, runnerA.detached)

	chatContextsMutex.Lock()
	ctxB := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()
	assert.Equal(t, sidB, ctxB.SessionID, "active session should be B")
	assert.Len(t, ctxB.Messages, 1, "session B should only have its own system message")

	var msgsA []Message
	require.NoError(t, theDB.Where("session_id = ?", sidA).Order("id").Find(&msgsA).Error)
	assert.Len(t, msgsA, 2, "session A DB should have system + detached msg")

	var msgsB []Message
	require.NoError(t, theDB.Where("session_id = ?", sidB).Order("id").Find(&msgsB).Error)
	assert.Len(t, msgsB, 1, "session B DB should have only its own system msg")
}

func TestAddContext_SessionIDZeroFallsThrough(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "testnet#chanuser_new"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 10}

	runner := makeTestChatRunner(t, cfg)
	runner.ctxKey = ctxKey
	assert.Equal(t, int64(0), runner.sessionID, "sessionID should start at 0")

	msg := ChatMessage{Role: "system", Content: "sys"}
	runner.addContext(msg)

	assert.False(t, runner.detached, "should not be detached when sessionID is 0")
	assert.True(t, ContextExists(ctxKey), "context should exist after add")
}

func TestGetSessionID(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "testnet#chanuser"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 10}

	runner := makeTestChatRunner(t, cfg)
	runner.ctxKey = ctxKey

	assert.Equal(t, int64(0), runner.getSessionID(), "should return 0 when no session")

	AddContext(cfg, ctxKey, ChatMessage{Role: "system", Content: "sys"}, "testnet", "#chan", "user")
	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()

	runner.sessionID = sid
	assert.Equal(t, sid, runner.getSessionID(), "should return captured sessionID")

	ClearContext(ctxKey)
	assert.Equal(t, sid, runner.getSessionID(), "should still return captured sessionID after clear")
}

func TestWriteToDBOnly(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "testnet#chanuser"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 10}

	AddContext(cfg, ctxKey, ChatMessage{Role: "system", Content: "sys"}, "testnet", "#chan", "user")
	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sid)

	runner := makeTestChatRunner(t, cfg)
	runner.ctxKey = ctxKey
	runner.sessionID = sid

	runner.writeToDBOnly(ChatMessage{Role: "assistant", Content: "db only msg"})

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()
	assert.Len(t, ctx.Messages, 1, "shared map should NOT have the DB-only message")

	var msgs []Message
	require.NoError(t, theDB.Where("session_id = ?", sid).Order("id").Find(&msgs).Error)
	assert.Len(t, msgs, 2, "DB should have system + db-only msg")
	assert.Equal(t, "db only msg", msgs[1].Content)
}

func TestWriteToDBOnly_WithToolCalls(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	ctxKey := "testnet#chanuser"
	cfg := AIConfig{Name: "testcmd", MaxHistory: 10}

	AddContext(cfg, ctxKey, ChatMessage{Role: "system", Content: "sys"}, "testnet", "#chan", "user")
	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	require.NotZero(t, sid)

	runner := makeTestChatRunner(t, cfg)
	runner.ctxKey = ctxKey
	runner.sessionID = sid

	runner.writeToDBOnly(ChatMessage{
		Role:    "tool",
		Content: "result text",
		ToolCalls: []ToolCall{
			{ID: "call_123", Function: FunctionCall{Name: "test_tool", Arguments: `{"k":"v"}`}},
		},
		ToolCallID:       "call_123",
		ReasoningContent: "thinking...",
	})

	var msgs []Message
	require.NoError(t, theDB.Where("session_id = ? AND role = ?", sid, "tool").Find(&msgs).Error)
	require.Len(t, msgs, 1)
	assert.Equal(t, "result text", msgs[0].Content)
	assert.NotNil(t, msgs[0].ToolCallID)
	assert.Equal(t, "call_123", *msgs[0].ToolCallID)
	assert.NotNil(t, msgs[0].ToolCalls)
	assert.NotNil(t, msgs[0].ReasoningContent)
	assert.Equal(t, "thinking...", *msgs[0].ReasoningContent)
}
