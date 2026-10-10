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

// TestReplayReasoningRow is the policy heart. "tool_calls" mirrors the gemma4
// template's preserve_thinking arm per row, position-independent: every
// assistant row that carries tool calls replays reasoning — the current
// turn's agentic loop AND past tool-call turns (the owner's cross-turn
// thought-context choice). Plain rows (no tool calls) only replay in "always"
// mode. The off mode never replays, and empty reasoning never qualifies in
// any mode.
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
		{"tool_calls mode replays past-turn tool row", ReasoningReplayToolCalls, 2, true},
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
			assert.Equal(t, tt.want, replayReasoningRow(msgs[tt.idx], tt.mode))
		})
	}

	// Unknown defensive mode behaves like tool_calls (the resolver maps
	// unknowns there, but the row predicate must agree on the raw value —
	// including the empty raw string, which the predicate arm matches
	// directly).
	t.Run("unknown raw mode falls back to tool_calls semantics", func(t *testing.T) {
		assert.True(t, replayReasoningRow(msgs[6], "bogus"))
		assert.False(t, replayReasoningRow(msgs[8], "bogus"))
	})
	t.Run("empty raw mode falls back to tool_calls semantics", func(t *testing.T) {
		assert.True(t, replayReasoningRow(msgs[6], ""))
		assert.False(t, replayReasoningRow(msgs[8], ""))
	})

	// Empty reasoning never qualifies in any mode, even eligible rows.
	t.Run("empty reasoning never qualifies", func(t *testing.T) {
		empty := []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t"}}},
		}
		for _, mode := range []string{ReasoningReplayOff, ReasoningReplayToolCalls, ReasoningReplayAlways} {
			assert.False(t, replayReasoningRow(empty[1], mode), mode)
		}
	})

	// A summary-only row — ReasoningSummary set, ReasoningContent empty —
	// is NEVER eligible in any mode: the summary is display/log-only and
	// never rides the wire. No exception for tool calls or "always".
	t.Run("summary-only row never qualifies in any mode", func(t *testing.T) {
		summaryOnly := []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, Content: "reply", ReasoningSummary: "summarized thinking"},
			{Role: RoleUser, Content: "u2"},
			{Role: RoleAssistant, ReasoningSummary: "summarized tool thinking", ToolCalls: []ToolCall{{ID: "t"}}},
		}
		for _, mode := range []string{ReasoningReplayOff, ReasoningReplayToolCalls, ReasoningReplayAlways} {
			for idx := range summaryOnly {
				assert.False(t, replayReasoningRow(summaryOnly[idx], mode), "mode %q row %d", mode, idx)
			}
		}
	})
}

// TestSummaryOnlyRowNeverReplays pins the summary-never-replays rule at the
// serializer levels too: a row carrying only a stored summary must gain no
// reasoning on the wire in ANY mode, on either API path — the summary is
// display/log-only (see reasoningParts in aiMessages.go). The fixture includes
// a summary-only TOOL row in a PAST position (a later user message follows
// it): under tool_calls mode tool calls are the only eligibility lever left
// besides real content, and a summary is never content — position is gone,
// tool calls must not rescue it.
func TestSummaryOnlyRowNeverReplays(t *testing.T) {
	summaryOnly := []ChatMessage{
		{Role: RoleSystem, Content: "system"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "reply", ReasoningSummary: "summarized past tool thinking", ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: RoleTool, Content: "result 1", ToolCallID: "t1"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, ReasoningSummary: "summarized current tool thinking", ToolCalls: []ToolCall{{ID: "t2"}}},
		{Role: RoleTool, Content: "result 2", ToolCallID: "t2"},
	}
	modes := []string{ReasoningReplayOff, ReasoningReplayToolCalls, ReasoningReplayAlways}

	t.Run("chat completions carries no reasoning_content", func(t *testing.T) {
		for _, mode := range modes {
			serialized := marshalChatParams(t, messagesToChatCompletionParams(summaryOnly, mode))
			for i, row := range serialized {
				_, ok := row["reasoning_content"]
				assert.False(t, ok, "mode %q row %d must not carry reasoning_content", mode, i)
			}
		}
	})

	t.Run("responses carries no reasoning items", func(t *testing.T) {
		for _, mode := range modes {
			rawItems := marshalInputItems(messagesToResponseInputItems(summaryOnly, mode))
			for _, raw := range rawItems {
				assert.NotContains(t, string(raw), `"type":"reasoning"`,
					"mode %q must not emit a reasoning item", mode)
				assert.NotContains(t, string(raw), "summarized",
					"mode %q must never serialize the summary", mode)
			}
		}
	})
}

// TestReplayReasoningFilter pins the copy-on-write filter used by the token
// accounting to pre-strip reasoning the wire would not carry: ineligible rows
// lose their ReasoningContent, eligible rows keep it, and the ORIGINAL slice
// must never be mutated (compaction callers share rows with live history).
func TestReplayReasoningFilter(t *testing.T) {
	// History shape: [user, assistant+tc(past reasoning), tool,
	// assistant plain(past reasoning), user, assistant+tc(current
	// reasoning), tool] — the past-turn tool row (idx 1) pins the
	// position independence of tool_calls mode.
	build := func() []ChatMessage {
		return []ChatMessage{
			{Role: RoleUser, Content: "u1"},
			{Role: RoleAssistant, ReasoningContent: "hist tool think", ToolCalls: []ToolCall{{ID: "t1"}}},
			{Role: RoleTool, Content: "result 1", ToolCallID: "t1"},
			{Role: RoleAssistant, Content: "hist reply", ReasoningContent: "hist plain think"},
			{Role: RoleUser, Content: "u2"},
			{Role: RoleAssistant, ReasoningContent: "cur think", ToolCalls: []ToolCall{{ID: "t2"}}},
			{Role: RoleTool, Content: "result 2", ToolCallID: "t2"},
		}
	}

	t.Run("tool_calls mode keeps tool-row reasoning, strips plain reasoning", func(t *testing.T) {
		msgs := build()
		filtered := replayReasoningFilter(msgs, ReasoningReplayToolCalls)
		// Tool rows keep their reasoning at ANY position — past turn
		// (idx 1) and current turn (idx 5); the plain rows are stripped.
		assert.Equal(t, "hist tool think", filtered[1].ReasoningContent)
		assert.Equal(t, "cur think", filtered[5].ReasoningContent)
		assert.Empty(t, filtered[3].ReasoningContent)
		assert.Empty(t, filtered[0].ReasoningContent)
		// Originals untouched.
		assert.Equal(t, "hist tool think", msgs[1].ReasoningContent)
		assert.Equal(t, "hist plain think", msgs[3].ReasoningContent)
		assert.Equal(t, "cur think", msgs[5].ReasoningContent)
	})

	t.Run("always mode keeps everything", func(t *testing.T) {
		msgs := build()
		filtered := replayReasoningFilter(msgs, ReasoningReplayAlways)
		assert.Equal(t, "hist tool think", filtered[1].ReasoningContent)
		assert.Equal(t, "hist plain think", filtered[3].ReasoningContent)
		assert.Equal(t, "cur think", filtered[5].ReasoningContent)
	})

	t.Run("off mode strips all reasoning", func(t *testing.T) {
		msgs := build()
		filtered := replayReasoningFilter(msgs, ReasoningReplayOff)
		assert.Empty(t, filtered[1].ReasoningContent)
		assert.Empty(t, filtered[3].ReasoningContent)
		assert.Empty(t, filtered[5].ReasoningContent)
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
