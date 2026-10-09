package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dbMsgsToChatMsgs(t *testing.T, dbMsgs []Message) []ChatMessage {
	t.Helper()
	msgs := make([]ChatMessage, len(dbMsgs))
	for i, dm := range dbMsgs {
		msgs[i] = messageFromDB(dm)
	}
	return msgs
}

func TestTurnContext_Add_AppendsAndPersists(t *testing.T) {
	setupTestDB(t)

	sid, err := sessionMgr.CreateSession("net", "#c", 1, "cmd", "svc", "m")
	require.NoError(t, err)

	tc := newTurnContext(sid, nil)
	tc.Add(ChatMessage{Role: RoleUser, Content: "hello"})
	tc.Add(ChatMessage{Role: RoleAssistant, Content: "world"})

	assert.Len(t, tc.Messages(), 2)
	assert.Equal(t, "hello", tc.Messages()[0].Content)
	assert.Equal(t, "world", tc.Messages()[1].Content)

	dbMsgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	assert.Len(t, dbMsgs, 2)
	assert.Equal(t, "hello", dbMsgs[0].Content)
	assert.Equal(t, "world", dbMsgs[1].Content)
}

func TestTurnContext_Add_WithInitialMessages(t *testing.T) {
	setupTestDB(t)

	sid, err := sessionMgr.CreateSession("net", "#c", 1, "cmd", "svc", "m")
	require.NoError(t, err)
	sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"})

	dbMsgs, _ := loadDBSessionMessages(sid)
	tc := newTurnContext(sid, dbMsgsToChatMsgs(t, dbMsgs))

	tc.Add(ChatMessage{Role: RoleUser, Content: "hi"})

	assert.Len(t, tc.Messages(), 2)
	assert.Equal(t, "sys", tc.Messages()[0].Content)
	assert.Equal(t, "hi", tc.Messages()[1].Content)

	allMsgs, _ := loadDBSessionMessages(sid)
	assert.Len(t, allMsgs, 2, "initial messages should not be re-persisted")
}

func TestTurnContext_Messages_ReturnsSlice(t *testing.T) {
	tc := newTurnContext(1, []ChatMessage{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "hi"},
	})
	assert.Len(t, tc.Messages(), 2)
}

func TestTurnContext_LastN(t *testing.T) {
	tc := newTurnContext(1, []ChatMessage{
		{Role: RoleSystem, Content: "s"},
		{Role: RoleUser, Content: "u"},
		{Role: RoleAssistant, Content: "a"},
	})

	last2 := tc.LastN(2)
	assert.Len(t, last2, 2)
	assert.Equal(t, "u", last2[0].Content)
	assert.Equal(t, "a", last2[1].Content)

	last5 := tc.LastN(5)
	assert.Len(t, last5, 3, "LastN should return all if n > len")
}

func TestTurnContext_Add_WithToolCalls(t *testing.T) {
	setupTestDB(t)

	sid, err := sessionMgr.CreateSession("net", "#c", 1, "cmd", "svc", "m")
	require.NoError(t, err)

	tc := newTurnContext(sid, nil)
	tc.Add(ChatMessage{
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "test_tool", Arguments: "{}"}}},
	})
	tc.Add(ChatMessage{Role: RoleTool, Content: "result", ToolCallID: "call_1"})

	assert.Len(t, tc.Messages(), 2)

	incomplete, err := sessionHasIncompleteToolCalls(sid)
	require.NoError(t, err)
	assert.False(t, incomplete, "tool calls persisted via Add should be complete")
}

func TestEphemeralTurnAddSkipsPersistence(t *testing.T) {
	setupTestDB(t)

	var before int64
	require.NoError(t, theDB.Model(&Message{}).Count(&before).Error)

	turn := newEphemeralTurnContext([]ChatMessage{{Role: RoleSystem, Content: "sys"}})
	turn.Add(ChatMessage{Role: RoleUser, Content: "hi"})
	turn.Add(ChatMessage{Role: RoleAssistant, Content: "yo"})

	assert.Len(t, turn.Messages(), 3, "in-memory accumulation works")
	assert.True(t, turn.ephemeral)

	var after int64
	require.NoError(t, theDB.Model(&Message{}).Count(&after).Error)
	assert.Equal(t, before, after, "ephemeral Add must not persist rows")
}

func TestRegularTurnAddPersists(t *testing.T) {
	setupTestDB(t)
	sid := createTestSession(t, "testnet", "#st", "shrew", "cmd", "svc", "m")

	var before int64
	require.NoError(t, theDB.Model(&Message{}).Count(&before).Error)

	turn := newTurnContext(sid, nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "hi"})

	var after int64
	require.NoError(t, theDB.Model(&Message{}).Count(&after).Error)
	assert.Equal(t, before+1, after, "regular turns still persist")
}
