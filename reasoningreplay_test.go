package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestReasoningReplayResolver pins the defensive resolver: valid values pass
// through, hand-built configs that skipped ApplyDefaults/validation fall back
// to the default mode instead of leaking garbage values into the serializers.
func TestReasoningReplayResolver(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back to default", "", ReasoningReplayToolCalls},
		{"off passes through", ReasoningReplayOff, ReasoningReplayOff},
		{"tool_calls passes through", ReasoningReplayToolCalls, ReasoningReplayToolCalls},
		{"always passes through", ReasoningReplayAlways, ReasoningReplayAlways},
		{"unknown value falls back to default", "sometimes", ReasoningReplayToolCalls},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, reasoningReplay(AIConfig{ReasoningReplay: tt.in}))
		})
	}
}

func TestLastUserRoleIndex(t *testing.T) {
	tests := []struct {
		name string
		msgs []ChatMessage
		want int
	}{
		{"empty history", nil, -1},
		{"no user rows", []ChatMessage{
			{Role: RoleSystem, Content: "s"},
			{Role: RoleAssistant, Content: "a"},
		}, -1},
		{"single user row", []ChatMessage{
			{Role: RoleSystem, Content: "s"},
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, Content: "a"},
		}, 1},
		{"last user wins", []ChatMessage{
			{Role: RoleUser, Content: "u1"},
			{Role: RoleAssistant, Content: "a1"},
			{Role: RoleUser, Content: "u2"},
			{Role: RoleAssistant, Content: "a2"},
		}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lastUserRoleIndex(tt.msgs))
		})
	}
}

// TestReplayReasoningRow is the policy heart. It mirrors the gemma4 chat
// template's gate: assistant rows AFTER the last user row (current turn)
// always replay reasoning; earlier (historical) rows only in "always" mode;
// "tool_calls" mode additionally requires the row to carry tool calls. The
// off mode never replays, and empty reasoning never qualifies in any mode.
func TestReplayReasoningRow(t *testing.T) {
	// History shape: [system, user, assistant+tc(reasoning), tool,
	// assistant(reasoning), user, assistant+tc(reasoning), tool,
	// assistant(reasoning)]
	msgs := []ChatMessage{
		{Role: RoleSystem, Content: "system"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, ReasoningContent: "think tc hist", ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: RoleTool, Content: "tool result", ToolCallID: "t1"},
		{Role: RoleAssistant, ReasoningContent: "think hist", Content: "hist reply"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, ReasoningContent: "think tc cur", ToolCalls: []ToolCall{{ID: "t2"}}},
		{Role: RoleTool, Content: "tool result 2", ToolCallID: "t2"},
		{Role: RoleAssistant, ReasoningContent: "think cur", Content: "current reply"},
	}

	tests := []struct {
		name string
		mode string
		idx  int
		want bool
	}{
		// Current-turn tool-call row (idx 6).
		{"tool_calls mode replays current-turn tool row", ReasoningReplayToolCalls, 6, true},
		{"off mode never replays current-turn tool row", ReasoningReplayOff, 6, false},
		{"always replays current-turn tool row", ReasoningReplayAlways, 6, true},
		// Current-turn plain reply (idx 8).
		{"tool_calls mode skips current-turn plain row", ReasoningReplayToolCalls, 8, false},
		{"off mode skips current-turn plain row", ReasoningReplayOff, 8, false},
		{"always replays current-turn plain row", ReasoningReplayAlways, 8, true},
		// Historical tool-call row (idx 2).
		{"tool_calls mode skips historical tool row", ReasoningReplayToolCalls, 2, false},
		{"off mode skips historical tool row", ReasoningReplayOff, 2, false},
		{"always replays historical tool row", ReasoningReplayAlways, 2, true},
		// Historical plain reply (idx 4).
		{"always replays historical plain row", ReasoningReplayAlways, 4, true},
		{"tool_calls mode skips historical plain row", ReasoningReplayToolCalls, 4, false},
		// Non-assistant rows never qualify.
		{"tool role never qualifies", ReasoningReplayAlways, 7, false},
		{"user role never qualifies", ReasoningReplayAlways, 5, false},
		{"system role never qualifies", ReasoningReplayAlways, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, replayReasoningRow(msgs, tt.idx, tt.mode))
		})
	}

	// Unknown defensive mode behaves like tool_calls (the resolver maps
	// unknowns there, but the row predicate must agree on the raw value).
	t.Run("unknown raw mode falls back to tool_calls semantics", func(t *testing.T) {
		assert.True(t, replayReasoningRow(msgs, 6, "bogus"))
		assert.False(t, replayReasoningRow(msgs, 8, "bogus"))
	})

	// Empty reasoning never qualifies in any mode, even eligible positions.
	t.Run("empty reasoning never qualifies", func(t *testing.T) {
		empty := []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t"}}},
		}
		for _, mode := range []string{ReasoningReplayOff, ReasoningReplayToolCalls, ReasoningReplayAlways} {
			assert.False(t, replayReasoningRow(empty, 1, mode), mode)
		}
	})
}

// TestReplayReasoningFilter pins the copy-on-write filter used by the token
// accounting to pre-strip reasoning the wire would not carry: ineligible rows
// lose their ReasoningContent, eligible rows keep it, and the ORIGINAL slice
// must never be mutated (compaction callers share rows with live history).
func TestReplayReasoningFilter(t *testing.T) {
	build := func() []ChatMessage {
		return []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, Content: "hist reply", ReasoningContent: "hist think"},
			{Role: RoleUser, Content: "u2"},
			{Role: RoleAssistant, ReasoningContent: "cur think", ToolCalls: []ToolCall{{ID: "t"}}},
			{Role: RoleTool, Content: "result", ToolCallID: "t"},
		}
	}

	t.Run("tool_calls mode strips historical + plain reasoning", func(t *testing.T) {
		msgs := build()
		filtered := replayReasoningFilter(msgs, ReasoningReplayToolCalls)
		assert.Equal(t, "cur think", filtered[3].ReasoningContent)
		assert.Empty(t, filtered[1].ReasoningContent)
		assert.Empty(t, filtered[0].ReasoningContent)
		// Originals untouched.
		assert.Equal(t, "hist think", msgs[1].ReasoningContent)
		assert.Equal(t, "cur think", msgs[3].ReasoningContent)
	})

	t.Run("always mode keeps everything", func(t *testing.T) {
		msgs := build()
		filtered := replayReasoningFilter(msgs, ReasoningReplayAlways)
		assert.Equal(t, "hist think", filtered[1].ReasoningContent)
		assert.Equal(t, "cur think", filtered[3].ReasoningContent)
	})

	t.Run("off mode strips all reasoning", func(t *testing.T) {
		msgs := build()
		filtered := replayReasoningFilter(msgs, ReasoningReplayOff)
		assert.Empty(t, filtered[1].ReasoningContent)
		assert.Empty(t, filtered[3].ReasoningContent)
	})

	t.Run("clean slice returned unwrapped", func(t *testing.T) {
		msgs := []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, Content: "a"},
		}
		assert.Equal(t, msgs, replayReasoningFilter(msgs, ReasoningReplayToolCalls))
	})

	t.Run("nil slice safe", func(t *testing.T) {
		assert.Nil(t, replayReasoningFilter(nil, ReasoningReplayToolCalls))
	})
}
