package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionHasIncompleteToolCalls_NoToolCalls(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "hello"}))

	result, err := sessionHasIncompleteToolCalls(sid)
	require.NoError(t, err)
	assert.False(t, result, "session with no tool calls should not be incomplete")
}

func TestSessionHasIncompleteToolCalls_CompletePairs(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:      RoleAssistant,
		Content:   "",
		ToolCalls: []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "test_tool", Arguments: "{}"}}},
	}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:       RoleTool,
		Content:    "result",
		ToolCallID: "call_1",
	}))

	result, err := sessionHasIncompleteToolCalls(sid)
	require.NoError(t, err)
	assert.False(t, result, "session with complete tool call pairs should not be incomplete")
}

func TestSessionHasIncompleteToolCalls_MissingResponse(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:      RoleAssistant,
		Content:   "",
		ToolCalls: []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "test_tool", Arguments: "{}"}}},
	}))

	result, err := sessionHasIncompleteToolCalls(sid)
	require.NoError(t, err)
	assert.True(t, result, "session with missing tool response should be incomplete")
}

func TestSessionHasIncompleteToolCalls_MultipleToolsOneMissing(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:    RoleAssistant,
		Content: "",
		ToolCalls: []ToolCall{
			{ID: "call_1", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}},
			{ID: "call_2", Function: FunctionCall{Name: "tool_b", Arguments: "{}"}},
		},
	}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:       RoleTool,
		Content:    "result_a",
		ToolCallID: "call_1",
	}))

	result, err := sessionHasIncompleteToolCalls(sid)
	require.NoError(t, err)
	assert.True(t, result, "session with one missing tool response out of two should be incomplete")
}

func TestSessionHasIncompleteToolCalls_MultipleRoundsAllComplete(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:      RoleAssistant,
		Content:   "",
		ToolCalls: []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "tool_a", Arguments: "{}"}}},
	}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:       RoleTool,
		Content:    "result_a",
		ToolCallID: "call_1",
	}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:      RoleAssistant,
		Content:   "",
		ToolCalls: []ToolCall{{ID: "call_2", Function: FunctionCall{Name: "tool_b", Arguments: "{}"}}},
	}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{
		Role:       RoleTool,
		Content:    "result_b",
		ToolCallID: "call_2",
	}))

	result, err := sessionHasIncompleteToolCalls(sid)
	require.NoError(t, err)
	assert.False(t, result, "session with multiple complete tool call rounds should not be incomplete")
}

func TestCloneDBSession_BasicClone(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "testchat", "svc", "model")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys prompt for srcnick"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hello from source"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleAssistant, Content: "hi back"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "question two"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleAssistant, Content: "answer two"}))

	tgtUserID := ensureTestUser(t, "net", "tgtnick")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID)
	require.NoError(t, err)
	require.NotEqual(t, srcSid, newSid, "cloned session should have different ID")

	newSession, err := sessionMgr.GetSession(newSid)
	require.NoError(t, err)
	assert.Equal(t, "net", newSession.Network)
	assert.Equal(t, "#c", newSession.Channel)
	assert.Equal(t, tgtUserID, *newSession.UserID)
	assert.Equal(t, "testchat", newSession.ChatCommand)
	assert.Equal(t, "active", newSession.Status)
	assert.Nil(t, newSession.ResponseID, "cloned session response_id should be nil")

	newMsgs, err := loadDBSessionMessages(newSid)
	require.NoError(t, err)
	assert.Len(t, newMsgs, 4, "cloned session should have user+assistant messages (system skipped)")

	assert.Equal(t, RoleUser, newMsgs[0].Role)
	assert.Equal(t, "hello from source", newMsgs[0].Content)
	assert.Equal(t, RoleAssistant, newMsgs[1].Role)
	assert.Equal(t, "hi back", newMsgs[1].Content)

	assert.Contains(t, newSession.FirstMessage, "hello from source")
}

func TestCloneDBSession_CloneCompletesExistingActiveSession(t *testing.T) {
	setupTestDB(t)

	tgtUserID := ensureTestUser(t, "net", "tgtnick")
	existingSid, err := sessionMgr.CreateSession("net", "#c", tgtUserID, "oldchat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(existingSid, ChatMessage{Role: RoleSystem, Content: "old sys"}))

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "newchat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "src msg"}))

	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID)
	require.NoError(t, err)
	_ = newSid

	existing, err := sessionMgr.GetSession(existingSid)
	require.NoError(t, err)
	assert.Equal(t, "completed", existing.Status, "existing active session should be completed")
}

func TestCloneDBSession_OnlyLiveMessagesCloned(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "chat", "svc", "m")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "live msg"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleAssistant, Content: "live reply"}))

	var archivedMsg Message
	require.NoError(t, theDB.Where("session_id = ? AND role = ?", srcSid, RoleUser).First(&archivedMsg).Error)
	require.NoError(t, theDB.Model(&archivedMsg).Update("archived", true).Error)

	tgtUserID := ensureTestUser(t, "net", "tgt")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID)
	require.NoError(t, err)

	newMsgs, err := loadDBSessionMessages(newSid)
	require.NoError(t, err)

	for _, m := range newMsgs {
		assert.False(t, m.Archived, "cloned messages should not be archived")
	}
}

func TestCloneDBSession_SourceUntouched(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "chat", "svc", "m")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "msg"}))

	srcMsgsBefore, err := loadDBSessionMessages(srcSid)
	require.NoError(t, err)

	tgtUserID := ensureTestUser(t, "net", "tgt")
	_, err = cloneDBSession(srcSid, "net", "#c", tgtUserID)
	require.NoError(t, err)

	srcMsgsAfter, err := loadDBSessionMessages(srcSid)
	require.NoError(t, err)
	assert.Equal(t, len(srcMsgsBefore), len(srcMsgsAfter), "source session messages should be unchanged")
}

func TestCloneDBSession_SettingsCopied(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "chat", "svc", "m")
	require.NoError(t, err)

	settingsID, err := sessionMgr.CreateSessionSettings(srcSid, AIConfig{
		System:           "custom system",
		Model:            "custom-model",
		DetectImages:     true,
		MaxImages:        3,
		MaxContextImages: 1,
		ReasoningEffort:  "low",
	})
	require.NoError(t, err)
	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", srcSid).Update("settings_id", settingsID).Error)

	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hi"}))

	tgtUserID := ensureTestUser(t, "net", "tgt")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID)
	require.NoError(t, err)

	newSession, err := sessionMgr.GetSession(newSid)
	require.NoError(t, err)
	require.NotNil(t, newSession.SettingsID, "cloned session should have settings_id")
	assert.NotEqual(t, settingsID, *newSession.SettingsID, "settings should be a new copy, not shared")

	srcSettings, err := sessionMgr.GetSessionSettings(settingsID)
	require.NoError(t, err)
	newSettings, err := sessionMgr.GetSessionSettings(*newSession.SettingsID)
	require.NoError(t, err)

	assert.Equal(t, srcSettings.System, newSettings.System)
	assert.Equal(t, srcSettings.Model, newSettings.Model)
	assert.Equal(t, srcSettings.DetectImages, newSettings.DetectImages)
	assert.Equal(t, srcSettings.MaxImages, newSettings.MaxImages)
}

func TestCloneDBSession_WithToolCalls(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "chat", "svc", "m")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{
		Role:      RoleAssistant,
		Content:   "",
		ToolCalls: []ToolCall{{ID: "call_abc", Function: FunctionCall{Name: "my_tool", Arguments: `{"x":1}`}}},
	}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{
		Role:       RoleTool,
		Content:    "tool result",
		ToolCallID: "call_abc",
	}))

	tgtUserID := ensureTestUser(t, "net", "tgt")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID)
	require.NoError(t, err)

	newMsgs, err := loadDBSessionMessages(newSid)
	require.NoError(t, err)

	var foundAssistant, foundTool bool
	for _, m := range newMsgs {
		if m.Role == RoleAssistant && m.ToolCalls != nil {
			foundAssistant = true
			var calls []ToolCall
			require.NoError(t, json.Unmarshal([]byte(*m.ToolCalls), &calls))
			assert.Len(t, calls, 1)
			assert.Equal(t, "call_abc", calls[0].ID)
		}
		if m.Role == RoleTool && m.ToolCallID != nil {
			foundTool = true
			assert.Equal(t, "call_abc", *m.ToolCallID)
		}
	}
	assert.True(t, foundAssistant, "cloned messages should contain assistant tool_call msg")
	assert.True(t, foundTool, "cloned messages should contain tool response msg")
}

func TestGetChannelDBSessions(t *testing.T) {
	setupTestDB(t)

	u1 := ensureTestUser(t, "net", "alice")
	u2 := ensureTestUser(t, "net", "bob")

	s1, err := sessionMgr.CreateSession("net", "#c", u1, "chat1", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(s1, ChatMessage{Role: RoleUser, Content: "alice msg"}))

	s2, err := sessionMgr.CreateSession("net", "#c", u2, "chat2", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(s2, ChatMessage{Role: RoleUser, Content: "bob msg"}))

	results, err := getChannelDBSessions("net", "#c", 10)
	require.NoError(t, err)
	assert.Len(t, results, 2)

	nicks := map[string]string{}
	for _, r := range results {
		nicks[r.ChatCommand] = r.OwnerNick
	}
	assert.Equal(t, "alice", nicks["chat1"])
	assert.Equal(t, "bob", nicks["chat2"])
}

func TestHistoryClone_ByNick(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"

	srcUserID := ensureTestUser(t, network.Name, "srcuser")
	srcSid, err := sessionMgr.CreateSession(network.Name, channel, srcUserID, "testchat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hello"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleAssistant, Content: "hi"}))

	tgtUserID := ensureTestUser(t, network.Name, "tgtuser")

	tmpl := template.Must(template.New("system").Parse("you are {{.Nick}}'s helper"))
	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"testchat": {Name: "testchat", Service: "svc", Model: "m", System: "you are a helper", SystemTmpl: tmpl},
	}
	defer func() { config.Commands.Chats = prevChats }()

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "tgtuser")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, "srcuser")
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "Cloned session", "clone by nick should succeed")
	assert.Contains(t, joined, fmt.Sprintf("#%d", srcSid), "output should mention source session ID")

	newSess, err := sessionMgr.GetActiveSession(network.Name, channel, tgtUserID)
	require.NoError(t, err)
	require.NotNil(t, newSess, "cloned session should be active for target user")

	newMsgs, err := loadDBSessionMessages(newSess.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(newMsgs), 3, "cloned session should have system + copied messages")

	isSystem := false
	for _, m := range newMsgs {
		if m.Role == RoleSystem {
			isSystem = true
			assert.Contains(t, m.Content, "tgtuser", "system prompt should reference the cloning user's nick")
		}
	}
	assert.True(t, isSystem, "cloned session should have a re-rendered system prompt")
}

func TestHistoryClone_ByNick_TargetNotFound(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "caller")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, "nonexistent")
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "not found", "clone of nonexistent nick should report not found")
}

func TestHistoryClone_ByNick_NoActiveSession(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	ensureTestUser(t, network.Name, "inactiveuser")

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "caller")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, "inactiveuser")
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "no active session", "clone of user with no active session should report error")
}

func TestHistoryClone_ByID(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"

	srcUserID := ensureTestUser(t, network.Name, "srcuser")
	srcSid, err := sessionMgr.CreateSession(network.Name, channel, srcUserID, "testchat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hello"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleAssistant, Content: "hi"}))

	sessionMgr.CompleteSession(srcSid)

	ensureTestUser(t, network.Name, "tgtuser")

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"testchat": {Name: "testchat", Service: "svc", Model: "m", System: "sys"},
	}
	defer func() { config.Commands.Chats = prevChats }()

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "tgtuser")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, fmt.Sprintf("%d", srcSid))
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "Cloned session", "clone by ID should succeed")
}

func TestHistoryClone_ByID_WrongChannel(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}

	srcUserID := ensureTestUser(t, network.Name, "srcuser")
	srcSid, err := sessionMgr.CreateSession(network.Name, "#other", srcUserID, "testchat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hi"}))

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "caller")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, fmt.Sprintf("%d", srcSid))
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "not in this channel", "clone of session from different channel should fail")
}

func TestHistoryClone_ByID_NotFound(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "caller")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, "99999")
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "not found", "clone of nonexistent session ID should report not found")
}

func TestHistoryClone_IncompleteToolCalls(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"

	srcUserID := ensureTestUser(t, network.Name, "srcuser")
	srcSid, err := sessionMgr.CreateSession(network.Name, channel, srcUserID, "testchat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{
		Role:      RoleAssistant,
		Content:   "",
		ToolCalls: []ToolCall{{ID: "call_x", Function: FunctionCall{Name: "tool", Arguments: "{}"}}},
	}))

	ensureTestUser(t, network.Name, "tgtuser")

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "tgtuser")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, "srcuser")
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "incomplete tool calls", "clone of session with incomplete tool calls should be rejected")
}

func TestHistoryClone_SelfClone(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"

	userID := ensureTestUser(t, network.Name, "myuser")
	srcSid, err := sessionMgr.CreateSession(network.Name, channel, userID, "testchat", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hello"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleAssistant, Content: "hi"}))

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"testchat": {Name: "testchat", Service: "svc", Model: "m", System: "sys"},
	}
	defer func() { config.Commands.Chats = prevChats }()

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "myuser")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, "myuser")
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "Cloned session", "self-clone should succeed")

	srcSession, err := sessionMgr.GetSession(srcSid)
	require.NoError(t, err)
	assert.Equal(t, "completed", srcSession.Status, "source session should be completed after self-clone")
}

func TestHistoryClone_CommandGone(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"

	srcUserID := ensureTestUser(t, network.Name, "srcuser")
	srcSid, err := sessionMgr.CreateSession(network.Name, channel, srcUserID, "deletedcmd", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hi"}))

	ensureTestUser(t, network.Name, "tgtuser")

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{}
	defer func() { config.Commands.Chats = prevChats }()

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "tgtuser")

	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyClone(network, client, e, ctx, output, "srcuser")
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "no longer exists", "clone with deleted command should report command_gone")
}

func TestIsAllDigits(t *testing.T) {
	assert.True(t, isAllDigits("123"))
	assert.True(t, isAllDigits("0"))
	assert.False(t, isAllDigits(""))
	assert.False(t, isAllDigits("abc"))
	assert.False(t, isAllDigits("12a"))
	assert.False(t, isAllDigits("nick"))
}

func TestCloneDBSession_EncryptedReasoning(t *testing.T) {
	setupTestDB(t)

	srcUserID := ensureTestUser(t, "net", "srcnick")
	srcSid, err := sessionMgr.CreateSession("net", "#c", srcUserID, "testchat", "svc", "m")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(srcSid, ChatMessage{
		Role:               RoleAssistant,
		Content:            "hello",
		ReasoningContent:   "thinking...",
		EncryptedReasoning: "enc_blob_abc",
	}))

	tgtUserID := ensureTestUser(t, "net", "tgt")
	newSid, err := cloneDBSession(srcSid, "net", "#c", tgtUserID)
	require.NoError(t, err)

	newMsgs, err := loadDBSessionMessages(newSid)
	require.NoError(t, err)

	found := false
	for _, m := range newMsgs {
		if m.Role == RoleAssistant && m.EncryptedReasoning != nil && *m.EncryptedReasoning == "enc_blob_abc" {
			found = true
		}
	}
	assert.True(t, found, "cloned session should preserve encrypted reasoning")
}
