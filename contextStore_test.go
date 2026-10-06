package main

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func TestDBSessionRoundtrip(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "user", "testcmd", "testservice", "testmodel")

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
	setupTestDB(t)

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

	sid := createTestSession(t, "net", "#chan", "user", "chat", "openai", "gpt-4o")

	settingsID, err := sessionMgr.CreateSessionSettings(sid, cfg)
	require.NoError(t, err)
	require.NotZero(t, settingsID)

	// Read the row directly — GetSessionSettings was removed with the
	// overlay; stored settings are provenance only now.
	var settings SessionSetting
	require.NoError(t, theDB.Where("id = ?", settingsID).First(&settings).Error)

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

func TestDBSessionSettingsNilWhenNone(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "user", "chat", "openai", "gpt-4o")

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, session.SettingsID, "settings_id should be nil when no settings created")
}

func TestDBCleanupByAge(t *testing.T) {
	db := setupTestDB(t)
	_ = db

	sid := createTestSession(t, "net", "#chan", "user", "testcmd", "", "")
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
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "nick", "chat", "", "")
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
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "nick", "chat", "", "")

	err := sessionMgr.CompleteSession(sid)
	require.NoError(t, err, "CompleteSession failed")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status")
}

func TestDBDeleteSession(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "nick", "chat", "", "")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello"}))

	err := deleteDBSession(sid)
	require.NoError(t, err, "deleteDBSession failed")

	_, err = getDBSessionByID(sid)
	assert.Error(t, err, "expected error getting deleted session")
}

func TestDBUserSessions(t *testing.T) {
	setupTestDB(t)

	for i := 0; i < 3; i++ {
		createTestSession(t, "net", "#chan", "nick", "chat", "", "")
	}
	createTestSession(t, "net", "#chan", "other", "chat", "", "")

	sessions, err := getUserDBSessions("net", "#chan", ensureTestUser(t, "net", "nick"), 10)
	require.NoError(t, err, "getUserDBSessions failed")
	assert.Len(t, sessions, 3, "sessions for nick")
}

func TestDBUserSessionsByNetwork(t *testing.T) {
	setupTestDB(t)

	createTestSession(t, "net", "#chan1", "nick", "chat", "", "")
	createTestSession(t, "net", "#chan2", "nick", "chat", "", "")
	createTestSession(t, "net", "#chan2", "nick", "chat", "", "")
	createTestSession(t, "other", "#chan1", "nick", "chat", "", "")

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
	setupTestDB(t)

	sid1 := createTestSession(t, "net", "#chan", "nick", "chat", "", "")
	sessionMgr.AddMessage(sid1, ChatMessage{Role: "system", Content: "sys"})
	sessionMgr.AddMessage(sid1, ChatMessage{Role: "user", Content: "hello"})

	sid2 := createTestSession(t, "net", "#chan", "nick", "chat", "", "")
	sessionMgr.AddMessage(sid2, ChatMessage{Role: "system", Content: "sys"})

	sessionCount, messageCount, err := getUserDBStats(ensureTestUser(t, "net", "nick"), "net", "#chan")
	require.NoError(t, err, "getUserDBStats failed")
	assert.Equal(t, 2, sessionCount, "session count")
	assert.Equal(t, 3, messageCount, "message count")
}

func TestDBDeleteUserSessions(t *testing.T) {
	setupTestDB(t)

	createTestSession(t, "net", "#chan", "nick", "chat", "", "")
	createTestSession(t, "net", "#chan", "nick", "chat", "", "")
	createTestSession(t, "net", "#chan", "other", "chat", "", "")

	affected, err := deleteUserDBSessions("net", "#chan", ensureTestUser(t, "net", "nick"))
	require.NoError(t, err, "deleteUserDBSessions failed")
	assert.Equal(t, int64(2), affected, "sessions deleted")

	sessions, _ := getUserDBSessions("net", "#chan", ensureTestUser(t, "net", "nick"), 10)
	assert.Len(t, sessions, 0, "sessions for nick after delete")
}

func TestDBSoftDeletePreservesData(t *testing.T) {
	db := setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "nick", "chat", "", "")
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
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "nick", "chat", "", "")

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
	setupTestDB(t)

	sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")

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
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "nick", "chat", "", "")

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
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "nick", "chat", "", "")

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
	setupTestDB(t)

	sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")

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
	setupTestDB(t)

	sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")

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
	setupTestDB(t)

	sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	sessionMgr.AddMessage(sid, ChatMessage{Role: "user", Content: "hello"})

	ClearContext("testnet", "#chan", ensureTestUser(t, "testnet", "user"))

	session, err := getDBSessionByID(sid)
	require.NoError(t, err, "getDBSessionByID failed")
	assert.Equal(t, "completed", session.Status, "session status after ClearContext")

	assert.False(t, ContextExists("testnet", "#chan", ensureTestUser(t, "testnet", "user")), "expected context to be cleared")
}

func TestSessionManagerGetActiveSession(t *testing.T) {
	setupTestDB(t)

	session, err := sessionMgr.GetActiveSession("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
	require.NoError(t, err)
	assert.Nil(t, session, "expected nil when no active session")

	sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	session, err = sessionMgr.GetActiveSession("testnet", "#chan", ensureTestUser(t, "testnet", "user"))
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, sid, session.ID)
	assert.Equal(t, "active", session.Status)
}

func TestSessionManagerSwitchActive(t *testing.T) {
	setupTestDB(t)

	sidA := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")
	sessionMgr.AddMessage(sidA, ChatMessage{Role: "user", Content: "msg A"})

	sidB := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")
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

func TestUpdateResponseIDWritesModel(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")

	// Save: both columns written together.
	respID := "resp-123"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, strPtrOrNil(respID), "grok-4-1-fast"))

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, session.ResponseID)
	assert.Equal(t, respID, *session.ResponseID)
	require.NotNil(t, session.ResponseModel, "response_model must be written alongside response_id")
	assert.Equal(t, "grok-4-1-fast", *session.ResponseModel)

	// Overwrite: both columns replaced together.
	require.NoError(t, sessionMgr.UpdateResponseID(sid, strPtrOrNil("resp-456"), "gpt-5"))
	session, err = sessionMgr.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, session.ResponseID)
	assert.Equal(t, "resp-456", *session.ResponseID)
	require.NotNil(t, session.ResponseModel)
	assert.Equal(t, "gpt-5", *session.ResponseModel)

	// Clear: both columns nulled together — the pair can never diverge.
	require.NoError(t, sessionMgr.UpdateResponseID(sid, nil, ""))
	session, err = sessionMgr.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, session.ResponseID)
	assert.Nil(t, session.ResponseModel)
}

// TestInsertTurnUsageAttribution verifies the per-turn attribution columns:
// every turn_usage row records the model/service/reasoning_effort of the
// config that actually produced it, so cost-per-model and effort-vs-
// reasoning-token stats are a plain GROUP BY (spec:
// docs/superpowers/specs/2026-10-06-live-config-and-usage-attribution-design.md).
func TestInsertTurnUsageAttribution(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "xai", "grok-4-1-fast")

	cfg := AIConfig{
		Name:            "chat",
		Service:         "xai",
		Model:           "grok-4-1-fast",
		ReasoningEffort: "low",
	}
	usage := &Usage{
		PromptTokens:            100,
		CompletionTokens:        50,
		TotalTokens:             150,
		FinishReason:            "stop",
		PromptTokensDetails:     &PromptTokensDetails{CachedTokens: 40},
		CompletionTokensDetails: &CompletionTokensDetails{ReasoningTokens: 20},
	}

	require.NoError(t, insertDBTurnUsage(sid, cfg, usage, "stop", "responses", 1234))

	var stored TurnUsage
	require.NoError(t, theDB.Where("session_id = ?", sid).First(&stored).Error)
	assert.Equal(t, "grok-4-1-fast", stored.Model)
	assert.Equal(t, "xai", stored.Service)
	assert.Equal(t, "low", stored.ReasoningEffort)
	assert.Equal(t, 100, stored.PromptTokens)
	assert.Equal(t, 50, stored.CompletionTokens)
	assert.Equal(t, 40, stored.CachedTokens)
	assert.Equal(t, 20, stored.ReasoningTokens)
	assert.Equal(t, "stop", stored.FinishReason)
	assert.Equal(t, "responses", stored.APIPath)
	assert.Equal(t, 1234, stored.DurationMs)

	// Empty effort must persist as '' — the "not sent" config shape — so it
	// stays distinguishable from an explicit effort in stats.
	cfgNoEffort := cfg
	cfgNoEffort.ReasoningEffort = ""
	require.NoError(t, insertDBTurnUsage(sid, cfgNoEffort, usage, "stop", "chat_completions", 10))
	var stored2 TurnUsage
	require.NoError(t, theDB.Where("api_path = ?", "chat_completions").First(&stored2).Error)
	assert.Empty(t, stored2.ReasoningEffort)
}

// TestConcurrentCreateSessionIsolation verifies that concurrent CreateSession calls
// for the same (network, channel, nick) each produce their own session when
// serialized by the per-user sessionCreationMu lock. This is a regression test
// for the bug where two -commands arriving close together would share one session.
func TestConcurrentCreateSessionIsolation(t *testing.T) {
	setupTestDB(t)

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
			sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")
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
	setupTestDB(t)

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
				sid := createTestSession(t, "testnet", "#chan", "user", "testcmd", "", "")
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
