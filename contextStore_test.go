package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	chatContextsMutex.Lock()
	chatContextsMap = make(map[string]ChatContext)
	chatContextsMutex.Unlock()
	contextLastActive = make(map[string]int64)

	os.Exit(m.Run())
}

func setupTestDB(t *testing.T) (*sqlx.DB, func()) {
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
		db.Close()
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

	theDB.Exec("UPDATE sessions SET last_active = datetime('now', '-100 days') WHERE id = ?", sid)

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

	assert.Equal(t, "data/dave.db", cfg.Path, "default path")
	assert.Equal(t, 90, cfg.MaxAgeDays, "default MaxAgeDays")
}

func TestDatabaseConfigNoOverwrite(t *testing.T) {
	cfg := DatabaseConfig{
		Path:       "custom/path.db",
		MaxAgeDays: 30,
	}
	cfg.SetDefaults()

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
