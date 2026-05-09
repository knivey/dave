package main

import (
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
	os.Exit(m.Run())
}

func setupTestDB(t *testing.T) (*gorm.DB, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := initDB(DatabaseConfig{Path: dbPath, MaxAgeDays: 90}, logxi.New("test"))
	require.NoError(t, err, "failed to init test db")
	oldDB := theDB
	oldSM := sessionMgr
	theDB = db
	sessionMgr = NewSessionManager(db)
	return db, func() {
		sessionMgr = oldSM
		theDB = oldDB
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	}
}

func TestDBSessionRoundtrip(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, err := sessionMgr.CreateSession("net", "#chan", "user", "testcmd", "testservice", "testmodel")
	require.NoError(t, err, "failed to create session")

	msgs := []ChatMessage{
		{Role: "system", Content: "You are a helpful assistant"},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
	}
	for _, msg := range msgs {
		require.NoError(t, sessionMgr.AddMessage(sid, msg))
	}

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err, "failed to get session")
	assert.Equal(t, "active", session.Status)

	loaded, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err, "failed to load messages")
	assert.Len(t, loaded, 3, "messages count")
	assert.Equal(t, "system", loaded[0].Role, "first message role")
	assert.Equal(t, "You are a helpful assistant", loaded[0].Content, "system prompt")
}

func TestDBCreateSessionSettings(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	cfg := AIConfig{
		Name:             "chat",
		Service:          "openai",
		Model:            "gpt-4o",
		System:           "You are {{.Nick}}'s helper",
		DetectImages:     true,
		MaxImages:        5,
		MaxContextImages: 3,
		ReasoningEffort:  "high",
	}

	sid, err := sessionMgr.CreateSession("net", "#chan", "user", "chat", "openai", "gpt-4o")
	require.NoError(t, err)

	settingsID, err := sessionMgr.CreateSessionSettings(sid, cfg)
	require.NoError(t, err)
	require.NotZero(t, settingsID)

	settings, err := sessionMgr.GetSessionSettings(settingsID)
	require.NoError(t, err)
	require.NotNil(t, settings)

	assert.Equal(t, "You are {{.Nick}}'s helper", settings.System)
	assert.Equal(t, "gpt-4o", settings.Model)
	assert.True(t, settings.DetectImages)
	assert.Equal(t, 5, settings.MaxImages)
	assert.Equal(t, 3, settings.MaxContextImages)
	assert.Equal(t, "high", settings.ReasoningEffort)

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, session.SettingsID)
	assert.Equal(t, settingsID, *session.SettingsID)
}

func TestDBApplySettings(t *testing.T) {
	tests := []struct {
		name     string
		settings SessionSetting
		baseCfg  AIConfig
		expected AIConfig
	}{
		{
			name: "all fields override",
			settings: SessionSetting{
				System:           "stored system",
				Model:            "stored-model",
				DetectImages:     true,
				MaxImages:        3,
				MaxContextImages: 2,
				ReasoningEffort:  "low",
			},
			baseCfg: AIConfig{
				Name:             "chat",
				Service:          "openai",
				Model:            "base-model",
				System:           "base system",
				DetectImages:     false,
				MaxImages:        5,
				MaxContextImages: 5,
				ReasoningEffort:  "medium",
			},
			expected: AIConfig{
				Name:             "chat",
				Service:          "openai",
				Model:            "stored-model",
				System:           "stored system",
				DetectImages:     true,
				MaxImages:        3,
				MaxContextImages: 2,
				ReasoningEffort:  "low",
			},
		},
		{
			name: "zero values in settings override base",
			settings: SessionSetting{
				System:           "stored system",
				Model:            "",
				DetectImages:     false,
				MaxImages:        0,
				MaxContextImages: 0,
				ReasoningEffort:  "",
			},
			baseCfg: AIConfig{
				Name:             "chat",
				Model:            "base-model",
				System:           "base system",
				DetectImages:     true,
				MaxImages:        5,
				MaxContextImages: 10,
				ReasoningEffort:  "medium",
			},
			expected: AIConfig{
				Name:             "chat",
				Model:            "base-model",
				System:           "stored system",
				DetectImages:     false,
				MaxImages:        5,
				MaxContextImages: 10,
				ReasoningEffort:  "medium",
			},
		},
		{
			name: "detect_images false in settings overrides true in base",
			settings: SessionSetting{
				DetectImages: false,
				Model:        "other",
				System:       "sys",
			},
			baseCfg: AIConfig{
				Model:            "base-model",
				System:           "base system",
				DetectImages:     true,
				MaxImages:        5,
				MaxContextImages: 5,
			},
			expected: AIConfig{
				Model:            "other",
				System:           "sys",
				DetectImages:     false,
				MaxImages:        5,
				MaxContextImages: 5,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ApplySettings(&tt.settings, tt.baseCfg)
			assert.Equal(t, tt.expected.Model, result.Model)
			assert.Equal(t, tt.expected.System, result.System)
			assert.Equal(t, tt.expected.DetectImages, result.DetectImages)
			assert.Equal(t, tt.expected.MaxImages, result.MaxImages)
			assert.Equal(t, tt.expected.MaxContextImages, result.MaxContextImages)
			assert.Equal(t, tt.expected.ReasoningEffort, result.ReasoningEffort)
			assert.Equal(t, tt.expected.Name, result.Name, "Name should come from baseCfg")
			assert.Equal(t, tt.expected.Service, result.Service, "Service should come from baseCfg")
		})
	}
}

func TestDBSessionSettingsNilWhenNone(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, err := sessionMgr.CreateSession("net", "#chan", "user", "chat", "openai", "gpt-4o")
	require.NoError(t, err)

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, session.SettingsID, "settings_id should be nil when no settings created")
}

func TestDBCleanupByAge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid, err := sessionMgr.CreateSession("net", "#chan", "user", "testcmd", "", "")
	require.NoError(t, err, "failed to create session")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello"}))

	pastTime := time.Now().AddDate(0, 0, -100)
	db.Model(&Session{}).Where("id = ?", sid).Update("last_active", pastTime)

	affected, err := cleanupDBSessions(90)
	require.NoError(t, err, "cleanup failed")
	assert.Equal(t, int64(1), affected, "sessions cleaned up")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "failed to get session")
	assert.Equal(t, "completed", session.Status, "session status")
}

func TestDBSessionCreateAndMessage(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, err := sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	require.NoError(t, err, "createSession failed")
	assert.NotZero(t, sid, "expected non-zero session id")

	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: "system", Content: "You are helpful"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "Hello!"}))

	msgs, err := sessionMgr.GetMessages(sid, 20)
	require.NoError(t, err, "GetMessages failed")
	assert.Len(t, msgs, 2, "messages count")
	assert.Equal(t, "system", msgs[0].Role, "first message role")
	assert.Equal(t, "Hello!", msgs[1].Content, "second message content")
}

func TestDBSessionComplete(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, _ := sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")

	err := sessionMgr.CompleteSession(sid)
	require.NoError(t, err, "CompleteSession failed")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status")
}

func TestDBDeleteSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, _ := sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello"}))

	err := deleteDBSession(sid)
	require.NoError(t, err, "deleteDBSession failed")

	_, err = getDBSessionByID(sid)
	assert.Error(t, err, "expected error getting deleted session")
}

func TestDBUserSessions(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	}
	sessionMgr.CreateSession("net", "#chan", "other", "chat", "", "")

	sessions, err := getUserDBSessions("net", "#chan", "nick", 10)
	require.NoError(t, err, "getUserDBSessions failed")
	assert.Len(t, sessions, 3, "sessions for nick")
}

func TestDBUserStats(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid1, _ := sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	sessionMgr.AddMessage(sid1, ChatMessage{Role: "system", Content: "sys"})
	sessionMgr.AddMessage(sid1, ChatMessage{Role: "user", Content: "hello"})

	sid2, _ := sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	sessionMgr.AddMessage(sid2, ChatMessage{Role: "system", Content: "sys"})

	sessionCount, messageCount, err := getUserDBStats("net", "#chan", "nick")
	require.NoError(t, err, "getUserDBStats failed")
	assert.Equal(t, 2, sessionCount, "session count")
	assert.Equal(t, 3, messageCount, "message count")
}

func TestDBDeleteUserSessions(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	sessionMgr.CreateSession("net", "#chan", "other", "chat", "", "")

	affected, err := deleteUserDBSessions("net", "#chan", "nick")
	require.NoError(t, err, "deleteUserDBSessions failed")
	assert.Equal(t, int64(2), affected, "sessions deleted")

	sessions, _ := getUserDBSessions("net", "#chan", "nick", 10)
	assert.Len(t, sessions, 0, "sessions for nick after delete")
}

func TestDBSoftDeletePreservesData(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	sid, _ := sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")
	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello"})

	err := deleteDBSession(sid)
	require.NoError(t, err, "deleteDBSession failed")

	_, err = getDBSessionByID(sid)
	assert.Error(t, err, "normal query should not find soft-deleted session")

	var session Session
	err = db.Unscoped().Where("id = ?", sid).First(&session).Error
	require.NoError(t, err, "unscoped query should find soft-deleted session")
	assert.NotNil(t, session.DeletedAt, "deleted_at should be set")
	assert.Equal(t, "chat", session.ChatCommand, "data should be preserved")
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
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, _ := sessionMgr.CreateSession("net", "#chan", "nick", "chat", "", "")

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

	sid, err := sessionMgr.CreateSession("testnet", "#chan", "user", "testcmd", "", "")
	require.NoError(t, err)

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "", session.FirstMessage, "first_message after creation")

	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello world this is my first message"})

	session, err = getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "hello world this is my first message", session.FirstMessage, "first_message")

	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "this should not overwrite"})

	session, err = getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "hello world this is my first message", session.FirstMessage, "first_message unchanged")
}

func TestClearContextCompletesSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, err := sessionMgr.CreateSession("testnet", "#chan", "user", "testcmd", "", "")
	require.NoError(t, err)

	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello"})

	ClearContext("testnet", "#chan", "user")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status after ClearContext")

	assert.False(t, ContextExists("testnet", "#chan", "user"), "expected context to be cleared")
}

func TestSessionManagerGetActiveSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	session, err := sessionMgr.GetActiveSession("testnet", "#chan", "user")
	require.NoError(t, err)
	assert.Nil(t, session, "expected nil when no active session")

	sid, err := sessionMgr.CreateSession("testnet", "#chan", "user", "testcmd", "", "")
	require.NoError(t, err)

	session, err = sessionMgr.GetActiveSession("testnet", "#chan", "user")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, sid, session.ID)
	assert.Equal(t, "active", session.Status)
}

func TestSessionManagerSwitchActive(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sidA, _ := sessionMgr.CreateSession("testnet", "#chan", "user", "testcmd", "", "")
	sessionMgr.AddMessage(sidA, ChatMessage{Role: "user", Content: "msg A"})

	sidB, _ := sessionMgr.CreateSession("testnet", "#chan", "user", "testcmd", "", "")
	sessionMgr.AddMessage(sidB, ChatMessage{Role: "user", Content: "msg B"})

	session, _ := sessionMgr.GetActiveSession("testnet", "#chan", "user")
	require.NotNil(t, session)
	assert.Equal(t, sidB, session.ID, "latest created session should be active")

	sessionMgr.SwitchActive("testnet", "#chan", "user", sidA)

	session, _ = sessionMgr.GetActiveSession("testnet", "#chan", "user")
	require.NotNil(t, session)
	assert.Equal(t, sidA, session.ID, "should have switched to session A")

	sessB, err := getDBSessionByID(sidB)
	require.NoError(t, err)
	assert.Equal(t, "completed", sessB.Status, "session B should be completed")
}

func TestSessionManagerUpdateResponseID(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid, _ := sessionMgr.CreateSession("testnet", "#chan", "user", "testcmd", "", "")

	respID := "resp-123"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, strPtrOrNil(respID)))

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, session.ResponseID)
	assert.Equal(t, respID, *session.ResponseID)
}
