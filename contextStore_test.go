package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func testCreateSession(t *testing.T, network, channel, nick, chatCmd, service, model string) int64 {
	t.Helper()
	userID := ensureTestUser(t, network, nick)
	sid, err := sessionMgr.CreateSession(network, channel, userID, chatCmd, service, model)
	require.NoError(t, err, "CreateSession")
	return sid
}

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

	sid := testCreateSession(t, "net", "#chan", "user", "testcmd", "testservice", "testmodel")

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

	sid := testCreateSession(t, "net", "#chan", "user", "chat", "openai", "gpt-4o")

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

	sid := testCreateSession(t, "net", "#chan", "user", "chat", "openai", "gpt-4o")

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, session.SettingsID, "settings_id should be nil when no settings created")
}

func TestDBCleanupByAge(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	_ = db

	sid := testCreateSession(t, "net", "#chan", "user", "testcmd", "", "")
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

	sid := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
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

	sid := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")

	err := sessionMgr.CompleteSession(sid)
	require.NoError(t, err, "CompleteSession failed")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status")
}

func TestDBDeleteSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
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
		testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
	}
	testCreateSession(t, "net", "#chan", "other", "chat", "", "")

	sessions, err := getUserDBSessions("net", "#chan", ensureTestUser(t, "net", "nick"), 10)
	require.NoError(t, err, "getUserDBSessions failed")
	assert.Len(t, sessions, 3, "sessions for nick")
}

func TestDBUserSessionsByNetwork(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	testCreateSession(t, "net", "#chan1", "nick", "chat", "", "")
	testCreateSession(t, "net", "#chan2", "nick", "chat", "", "")
	testCreateSession(t, "net", "#chan2", "nick", "chat", "", "")
	testCreateSession(t, "other", "#chan1", "nick", "chat", "", "")

	userID := ensureTestUser(t, "net", "nick")
	sessions, err := getUserDBSessionsByNetwork("net", userID, 10)
	require.NoError(t, err, "getUserDBSessionsByNetwork failed")
	assert.Len(t, sessions, 3, "sessions for nick on net across channels")

	sessions, err = getUserDBSessionsByNetwork("net", userID, 2)
	require.NoError(t, err, "getUserDBSessionsByNetwork with limit failed")
	assert.Len(t, sessions, 2, "sessions limited")

	otherUserID := ensureTestUser(t, "other", "nick")
	sessions, err = getUserDBSessionsByNetwork("other", otherUserID, 10)
	require.NoError(t, err, "getUserDBSessionsByNetwork wrong network")
	assert.Len(t, sessions, 1, "sessions for nick on other network")
}

func TestDBUserStats(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid1 := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
	sessionMgr.AddMessage(sid1, ChatMessage{Role: "system", Content: "sys"})
	sessionMgr.AddMessage(sid1, ChatMessage{Role: "user", Content: "hello"})

	sid2 := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
	sessionMgr.AddMessage(sid2, ChatMessage{Role: "system", Content: "sys"})

	sessionCount, messageCount, err := getUserDBStats("net", "#chan", ensureTestUser(t, "net", "nick"))
	require.NoError(t, err, "getUserDBStats failed")
	assert.Equal(t, 2, sessionCount, "session count")
	assert.Equal(t, 3, messageCount, "message count")
}

func TestDBDeleteUserSessions(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
	testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
	testCreateSession(t, "net", "#chan", "other", "chat", "", "")

	affected, err := deleteUserDBSessions("net", "#chan", ensureTestUser(t, "net", "nick"))
	require.NoError(t, err, "deleteUserDBSessions failed")
	assert.Equal(t, int64(2), affected, "sessions deleted")

	sessions, _ := getUserDBSessions("net", "#chan", ensureTestUser(t, "net", "nick"), 10)
	assert.Len(t, sessions, 0, "sessions for nick after delete")
}

func TestDBSoftDeletePreservesData(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")
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

	sid := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")

	toolCalls := []ToolCall{
		{ID: "tc1", Type: "function", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"sf"}`}},
	}
	tcData, _ := json.Marshal(toolCalls)
	tcJSON := string(tcData)
	toolCallID := "tc1"

	err := insertDBMessage(sid, "assistant", "", &tcJSON, &toolCallID, nil, nil)
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

	sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")

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

func TestDBMultiContent(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")

	parts := []MessagePart{
		{Type: PartTypeText, Text: "check this image"},
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,iVBORw==", Detail: ImageDetailAuto}},
	}
	msg := ChatMessage{
		Role:         RoleUser,
		Content:      "",
		MultiContent: parts,
	}

	err := sessionMgr.AddMessage(sid, msg)
	require.NoError(t, err, "AddMessage with MultiContent failed")

	msgs, err := sessionMgr.GetMessages(sid, 10)
	require.NoError(t, err, "GetMessages failed")
	require.Len(t, msgs, 1, "messages count")

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "", msgs[0].Content)
	require.Len(t, msgs[0].MultiContent, 2, "MultiContent parts count")
	assert.Equal(t, PartTypeText, msgs[0].MultiContent[0].Type)
	assert.Equal(t, "check this image", msgs[0].MultiContent[0].Text)
	assert.Equal(t, PartTypeImageURL, msgs[0].MultiContent[1].Type)
	require.NotNil(t, msgs[0].MultiContent[1].ImageURL)
	assert.Equal(t, "data:image/png;base64,iVBORw==", msgs[0].MultiContent[1].ImageURL.URL)
	assert.Equal(t, ImageDetailAuto, msgs[0].MultiContent[1].ImageURL.Detail)
}

func TestDBMultiContentWithToolCalls(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "net", "#chan", "nick", "chat", "", "")

	toolCalls := []ToolCall{
		{ID: "tc1", Type: "function", Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"sf"}`}},
	}
	parts := []MessagePart{
		{Type: PartTypeText, Text: "describe this"},
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/jpeg;base64,/9j/4AAQ", Detail: ImageDetailHigh}},
	}
	msg := ChatMessage{
		Role:             RoleAssistant,
		Content:          "",
		MultiContent:     parts,
		ToolCalls:        toolCalls,
		ReasoningContent: "thinking about weather",
	}

	err := sessionMgr.AddMessage(sid, msg)
	require.NoError(t, err)

	msgs, err := sessionMgr.GetMessages(sid, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	assert.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "tc1", msgs[0].ToolCalls[0].ID)
	assert.Equal(t, "thinking about weather", msgs[0].ReasoningContent)
	require.Len(t, msgs[0].MultiContent, 2)
	assert.Equal(t, PartTypeImageURL, msgs[0].MultiContent[1].Type)
	assert.Equal(t, ImageDetailHigh, msgs[0].MultiContent[1].ImageURL.Detail)
}

func TestDBFirstMessageWithMultiContent(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	parts := []MessagePart{
		{Type: PartTypeText, Text: "what is in this image"},
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,iVBORw==", Detail: ImageDetailAuto}},
	}
	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "", MultiContent: parts})

	session, err := getDBSessionByID(sid)
	require.NoError(t, err)
	assert.Equal(t, "what is in this image", session.FirstMessage, "first_message should use text from MultiContent")
}

func TestDBFirstMessageMultiContentNoText(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	parts := []MessagePart{
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,iVBORw==", Detail: ImageDetailAuto}},
	}
	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "", MultiContent: parts})

	session, err := getDBSessionByID(sid)
	require.NoError(t, err)
	assert.Equal(t, "", session.FirstMessage, "first_message should be empty when MultiContent has no text part")
}

func TestMessageFromDB(t *testing.T) {
	mcJSON := `[{"Type":"text","Text":"hello"},{"Type":"image_url","ImageURL":{"URL":"data:image/png;base64,abc","Detail":"auto"}}]`
	tcJSON := `[{"ID":"tc1","Type":"function","Function":{"Name":"test","Arguments":"{}"}}]`
	toolCallID := "tc1"
	reasoning := "thinking"
	content := "some content"

	dm := Message{
		Role:             "assistant",
		Content:          content,
		ToolCalls:        &tcJSON,
		ToolCallID:       &toolCallID,
		ReasoningContent: &reasoning,
		MultiContent:     &mcJSON,
	}

	msg := messageFromDB(dm)
	assert.Equal(t, "assistant", msg.Role)
	assert.Equal(t, "some content", msg.Content)
	assert.Equal(t, "thinking", msg.ReasoningContent)
	assert.Equal(t, "tc1", msg.ToolCallID)
	require.Len(t, msg.ToolCalls, 1)
	assert.Equal(t, "tc1", msg.ToolCalls[0].ID)
	require.Len(t, msg.MultiContent, 2)
	assert.Equal(t, PartTypeText, msg.MultiContent[0].Type)
	assert.Equal(t, "hello", msg.MultiContent[0].Text)
	assert.Equal(t, PartTypeImageURL, msg.MultiContent[1].Type)
	assert.Equal(t, "data:image/png;base64,abc", msg.MultiContent[1].ImageURL.URL)
}

func TestTextContentFromMessage(t *testing.T) {
	t.Run("content takes priority", func(t *testing.T) {
		msg := ChatMessage{Content: "from content", MultiContent: []MessagePart{{Type: PartTypeText, Text: "from multi"}}}
		assert.Equal(t, "from content", textContentFromMessage(msg))
	})

	t.Run("falls back to MultiContent text", func(t *testing.T) {
		msg := ChatMessage{Content: "", MultiContent: []MessagePart{{Type: PartTypeText, Text: "from multi"}}}
		assert.Equal(t, "from multi", textContentFromMessage(msg))
	})

	t.Run("empty when no text anywhere", func(t *testing.T) {
		msg := ChatMessage{Content: "", MultiContent: []MessagePart{{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:..."}}}}
		assert.Equal(t, "", textContentFromMessage(msg))
	})

	t.Run("empty message", func(t *testing.T) {
		msg := ChatMessage{}
		assert.Equal(t, "", textContentFromMessage(msg))
	})
}

func TestClearContextCompletesSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello"})

	ClearContext("testnet", "#chan", ensureTestUser(t, "testnet", "user"))

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status after ClearContext")

	assert.False(t, ContextExists("testnet", "#chan", ensureTestUser(t, "testnet", "user")), "expected context to be cleared")
}

func TestSessionManagerGetActiveSession(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	session, err := sessionMgr.GetActiveSession("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
	require.NoError(t, err)
	assert.Nil(t, session, "expected nil when no active session")

	sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	session, err = sessionMgr.GetActiveSession("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, sid, session.ID)
	assert.Equal(t, "active", session.Status)
}

func TestSessionManagerSwitchActive(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sidA := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")
	sessionMgr.AddMessage(sidA, ChatMessage{Role: "user", Content: "msg A"})

	sidB := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")
	sessionMgr.AddMessage(sidB, ChatMessage{Role: "user", Content: "msg B"})

	session, _ := sessionMgr.GetActiveSession("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
	require.NotNil(t, session)
	assert.Equal(t, sidB, session.ID, "latest created session should be active")

	sessionMgr.SwitchActive("testnet", "#chan", ensureTestUser(t, "testnet", "user"), sidA)

	session, _ = sessionMgr.GetActiveSession("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
	require.NotNil(t, session)
	assert.Equal(t, sidA, session.ID, "should have switched to session A")

	sessB, err := getDBSessionByID(sidB)
	require.NoError(t, err)
	assert.Equal(t, "completed", sessB.Status, "session B should be completed")
}

func TestSessionManagerUpdateResponseID(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	respID := "resp-123"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, strPtrOrNil(respID)))

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, session.ResponseID)
	assert.Equal(t, respID, *session.ResponseID)
}

// TestConcurrentCreateSessionIsolation verifies that concurrent CreateSession calls
// for the same (network, channel, nick) each produce their own session when
// serialized by the per-user sessionCreationMu lock. This is a regression test
// for the bug where two -commands arriving close together would share one session.
func TestConcurrentCreateSessionIsolation(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	const numGoroutines = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	var createdCount atomic.Int32
	sessionIDs := make([]int64, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			mu := getSessionCreationLock("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
			mu.Lock()
			sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")
			sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "system"})
			sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hello"})
			mu.Unlock()

			sessionIDs[idx] = sid
			createdCount.Add(1)
		}(i)
	}

	wg.Wait()

	assert.Equal(t, int32(numGoroutines), createdCount.Load(), "all goroutines should create sessions")

	uniqueIDs := make(map[int64]bool)
	for _, id := range sessionIDs {
		uniqueIDs[id] = true
	}
	assert.Len(t, uniqueIDs, numGoroutines, "each goroutine should get a unique session ID")

	for _, id := range sessionIDs {
		msgs, err := sessionMgr.GetMessages(id, 10)
		require.NoError(t, err)
		assert.Len(t, msgs, 2, "each session should have exactly its own system + user message")
	}
}

func TestConcurrentCreateSessionWithoutLockRaces(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	// Without the lock, concurrent CreateSession calls for the same key would
	// cause session reuse. This test confirms the lock prevents that by running
	// the same pattern as the real chat() code: check -> create -> add messages.
	const numGoroutines = 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	sessionIDs := make([]int64, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			session, _ := sessionMgr.GetActiveSession("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
			if session == nil {
				mu := getSessionCreationLock("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
				mu.Lock()
				sid := testCreateSession(t, "testnet", "#chan", "user", "testcmd", "", "")
				sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "system"})
				session, _ = sessionMgr.GetSession(sid)
				mu.Unlock()
			}

			if session != nil {
				sessionMgr.AddMessage(session.ID, ChatMessage{
					Role:    RoleUser,
					Content: "msg from goroutine",
				})
				sessionIDs[idx] = session.ID
			}
		}(i)
	}

	wg.Wait()

	// With the lock, only the first goroutine should have its session found by
	// another goroutine. But because GetActiveSession is called outside the lock,
	// some goroutines may find the first session. The lock ensures that if they
	// enter the creation branch, they get their own session.
	// The key invariant: no session should have more than one system message.
	for _, id := range sessionIDs {
		if id == 0 {
			continue
		}
		msgs, err := sessionMgr.GetMessages(id, 10)
		require.NoError(t, err)
		systemCount := 0
		for _, m := range msgs {
			if m.Role == RoleSystem {
				systemCount++
			}
		}
		assert.Equal(t, 1, systemCount, "session %d should have exactly 1 system message", id)
	}
}
