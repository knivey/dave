package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	gogpt "github.com/sashabaranov/go-openai"
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
	if err != nil {
		t.Fatalf("failed to init test db: %v", err)
	}
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
		Messages: []gogpt.ChatCompletionMessage{
			{Role: "system", Content: "You are a helpful assistant"},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
		},
		Config:    AIConfig{Name: "testcmd", MaxHistory: 5, Temperature: 0.7},
		SessionID: 0,
	}
	chatContextsMutex.Unlock()

	ctxKey := "net#chanuser"
	sid, err := createDBSession(ctxKey, "net", "#chan", "user", "testcmd", "")
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	ctx.SessionID = sid
	chatContextsMap[ctxKey] = ctx
	chatContextsMutex.Unlock()

	for _, msg := range chatContextsMap[ctxKey].Messages {
		toolCallsJSON := func(m gogpt.ChatCompletionMessage) *string {
			if len(m.ToolCalls) > 0 {
				data, _ := json.Marshal(m.ToolCalls)
				s := string(data)
				return &s
			}
			return nil
		}(msg)
		if err := insertDBMessage(sid, msg.Role, msg.Content, toolCallsJSON, nil, nil); err != nil {
			t.Fatalf("failed to insert message: %v", err)
		}
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

	if !ok {
		t.Fatal("expected context to be loaded")
	}

	if len(ctx2.Messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(ctx2.Messages))
	}

	if ctx2.Messages[0].Role != "system" {
		t.Errorf("first message should be system, got %s", ctx2.Messages[0].Role)
	}

	if ctx2.Messages[0].Content != "You are a helpful assistant" {
		t.Errorf("system prompt mismatch: %s", ctx2.Messages[0].Content)
	}
}

func TestDBCleanupByAge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, err := createDBSession("oldkey", "net", "#chan", "user", "testcmd", "")
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	insertDBMessage(sid, "user", "hello", nil, nil, nil)

	theDB.Exec("UPDATE sessions SET last_active = datetime('now', '-100 days') WHERE id = ?", sid)

	affected, err := cleanupDBSessions(90)
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if affected != 1 {
		t.Errorf("expected 1 session cleaned up, got %d", affected)
	}

	session, err := getDBSessionByID(sid)
	if err != nil {
		t.Fatalf("failed to get session: %v", err)
	}
	if session.Status != "completed" {
		t.Errorf("expected status 'completed', got %s", session.Status)
	}
}

func TestDBSessionCreateAndMessage(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, err := createDBSession("testkey", "net", "#chan", "nick", "chat", "")
	if err != nil {
		t.Fatalf("createDBSession failed: %v", err)
	}
	if sid == 0 {
		t.Error("expected non-zero session id")
	}

	err = insertDBMessage(sid, "system", "You are helpful", nil, nil, nil)
	if err != nil {
		t.Fatalf("insertDBMessage failed: %v", err)
	}
	err = insertDBMessage(sid, "user", "Hello!", nil, nil, nil)
	if err != nil {
		t.Fatalf("insertDBMessage failed: %v", err)
	}

	msgs, err := loadDBSessionMessages(sid)
	if err != nil {
		t.Fatalf("loadDBSessionMessages failed: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("expected first message role 'system', got %s", msgs[0].Role)
	}
	if msgs[1].Content != "Hello!" {
		t.Errorf("expected second message content 'Hello!', got %s", msgs[1].Content)
	}
}

func TestDBSessionComplete(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, _ := createDBSession("testkey", "net", "#chan", "nick", "chat", "")

	err := completeDBSession(sid)
	if err != nil {
		t.Fatalf("completeDBSession failed: %v", err)
	}

	session, err := getDBSessionByID(sid)
	if err != nil {
		t.Fatalf("getDBSessionByID failed: %v", err)
	}
	if session.Status != "completed" {
		t.Errorf("expected status 'completed', got %s", session.Status)
	}
}

func TestDBDeleteSession(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, _ := createDBSession("testkey", "net", "#chan", "nick", "chat", "")
	insertDBMessage(sid, "user", "hello", nil, nil, nil)

	err := deleteDBSession(sid)
	if err != nil {
		t.Fatalf("deleteDBSession failed: %v", err)
	}

	_, err = getDBSessionByID(sid)
	if err == nil {
		t.Error("expected error getting deleted session")
	}
}

func TestDBUserSessions(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	for i := 0; i < 3; i++ {
		createDBSession("key"+string(rune('a'+i)), "net", "#chan", "nick", "chat", "")
	}
	createDBSession("other", "net", "#chan", "other", "chat", "")

	sessions, err := getUserDBSessions("net", "#chan", "nick", 10)
	if err != nil {
		t.Fatalf("getUserDBSessions failed: %v", err)
	}
	if len(sessions) != 3 {
		t.Errorf("expected 3 sessions for nick, got %d", len(sessions))
	}
}

func TestDBUserStats(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid1, _ := createDBSession("key1", "net", "#chan", "nick", "chat", "")
	insertDBMessage(sid1, "system", "sys", nil, nil, nil)
	insertDBMessage(sid1, "user", "hello", nil, nil, nil)

	sid2, _ := createDBSession("key2", "net", "#chan", "nick", "chat", "")
	insertDBMessage(sid2, "system", "sys", nil, nil, nil)

	sessionCount, messageCount, err := getUserDBStats("net", "#chan", "nick")
	if err != nil {
		t.Fatalf("getUserDBStats failed: %v", err)
	}
	if sessionCount != 2 {
		t.Errorf("expected 2 sessions, got %d", sessionCount)
	}
	if messageCount != 3 {
		t.Errorf("expected 3 messages, got %d", messageCount)
	}
}

func TestDBDeleteUserSessions(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	createDBSession("key1", "net", "#chan", "nick", "chat", "")
	createDBSession("key2", "net", "#chan", "nick", "chat", "")
	createDBSession("key3", "net", "#chan", "other", "chat", "")

	affected, err := deleteUserDBSessions("net", "#chan", "nick")
	if err != nil {
		t.Fatalf("deleteUserDBSessions failed: %v", err)
	}
	if affected != 2 {
		t.Errorf("expected 2 sessions deleted, got %d", affected)
	}

	sessions, _ := getUserDBSessions("net", "#chan", "nick", 10)
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions for nick after delete, got %d", len(sessions))
	}
}

func TestDatabaseConfigDefaults(t *testing.T) {
	cfg := DatabaseConfig{}
	cfg.SetDefaults()

	if cfg.Path != "data/dave.db" {
		t.Errorf("expected default path 'data/dave.db', got %s", cfg.Path)
	}
	if cfg.MaxAgeDays != 90 {
		t.Errorf("expected default MaxAgeDays 90, got %d", cfg.MaxAgeDays)
	}
}

func TestDatabaseConfigNoOverwrite(t *testing.T) {
	cfg := DatabaseConfig{
		Path:       "custom/path.db",
		MaxAgeDays: 30,
	}
	cfg.SetDefaults()

	if cfg.Path != "custom/path.db" {
		t.Errorf("expected path 'custom/path.db', got %s", cfg.Path)
	}
	if cfg.MaxAgeDays != 30 {
		t.Errorf("expected MaxAgeDays 30, got %d", cfg.MaxAgeDays)
	}
}

func TestDBToolCalls(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, _ := createDBSession("testkey", "net", "#chan", "nick", "chat", "")

	toolCalls := []gogpt.ToolCall{
		{ID: "tc1", Type: "function", Function: gogpt.FunctionCall{Name: "get_weather", Arguments: `{"city":"sf"}`}},
	}
	tcData, _ := json.Marshal(toolCalls)
	tcJSON := string(tcData)
	toolCallID := "tc1"

	err := insertDBMessage(sid, "assistant", "", &tcJSON, &toolCallID, nil)
	if err != nil {
		t.Fatalf("insertDBMessage with tool calls failed: %v", err)
	}

	msgs, err := loadDBSessionMessages(sid)
	if err != nil {
		t.Fatalf("loadDBSessionMessages failed: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
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
		gogpt.ChatCompletionMessage{Role: "system", Content: "you are helpful"},
		"net", "#chan", "user")

	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	if sid == 0 {
		t.Fatal("expected session to be created")
	}

	session, err := getDBSessionByID(sid)
	if err != nil {
		t.Fatalf("getDBSessionByID failed: %v", err)
	}
	if session.FirstMessage != "" {
		t.Errorf("expected empty first_message after system prompt, got %q", session.FirstMessage)
	}

	AddContext(cfg, ctxKey,
		gogpt.ChatCompletionMessage{Role: "user", Content: "hello world this is my first message"},
		"net", "#chan", "user")

	session, err = getDBSessionByID(sid)
	if err != nil {
		t.Fatalf("getDBSessionByID failed: %v", err)
	}
	if session.FirstMessage != "hello world this is my first message" {
		t.Errorf("expected first_message to be saved, got %q", session.FirstMessage)
	}

	AddContext(cfg, ctxKey,
		gogpt.ChatCompletionMessage{Role: "user", Content: "this should not overwrite"},
		"net", "#chan", "user")

	session, err = getDBSessionByID(sid)
	if err != nil {
		t.Fatalf("getDBSessionByID failed: %v", err)
	}
	if session.FirstMessage != "hello world this is my first message" {
		t.Errorf("expected first_message to remain unchanged, got %q", session.FirstMessage)
	}
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
		gogpt.ChatCompletionMessage{Role: "user", Content: "hello"},
		"net", "#chan", "user")

	chatContextsMutex.Lock()
	sid := chatContextsMap[ctxKey].SessionID
	chatContextsMutex.Unlock()
	if sid == 0 {
		t.Fatal("expected session to be created")
	}

	ClearContext(ctxKey)

	session, err := getDBSessionByID(sid)
	if err != nil {
		t.Fatalf("getDBSessionByID failed: %v", err)
	}
	if session.Status != "completed" {
		t.Errorf("expected session status 'completed' after ClearContext, got %s", session.Status)
	}

	if ContextExists(ctxKey) {
		t.Error("expected context to be cleared")
	}
}

func TestContextLastActive(t *testing.T) {
	contextLastActive = make(map[string]int64)
	key := "testkey"

	before := time.Now().Unix()
	SetContextLastActive(key)
	after := time.Now().Unix()

	active := GetContextLastActive(key)
	if active < before || active > after {
		t.Errorf("GetContextLastActive = %d, want between %d and %d", active, before, after)
	}

	DeleteContextLastActive(key)
	if _, ok := contextLastActive[key]; ok {
		t.Error("expected key to be deleted")
	}
}
