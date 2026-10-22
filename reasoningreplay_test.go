package main

import (
	"encoding/json"
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
			assert.Equal(t, tt.want, replayReasoningRow(msgs[tt.idx], tt.mode, ReasoningReplayFormatText))
		})
	}

	// Unknown defensive mode behaves like tool_calls (the resolver maps
	// unknowns there, but the row predicate must agree on the raw value —
	// including the empty raw string, which the predicate arm matches
	// directly).
	t.Run("unknown raw mode falls back to tool_calls semantics", func(t *testing.T) {
		assert.True(t, replayReasoningRow(msgs[6], "bogus", ReasoningReplayFormatText))
		assert.False(t, replayReasoningRow(msgs[8], "bogus", ReasoningReplayFormatText))
	})
	t.Run("empty raw mode falls back to tool_calls semantics", func(t *testing.T) {
		assert.True(t, replayReasoningRow(msgs[6], "", ReasoningReplayFormatText))
		assert.False(t, replayReasoningRow(msgs[8], "", ReasoningReplayFormatText))
	})

	// Empty reasoning never qualifies in any mode, even eligible rows.
	t.Run("empty reasoning never qualifies", func(t *testing.T) {
		empty := []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "t"}}},
		}
		for _, mode := range []string{ReasoningReplayOff, ReasoningReplayToolCalls, ReasoningReplayAlways} {
			assert.False(t, replayReasoningRow(empty[1], mode, ReasoningReplayFormatText), mode)
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
				assert.False(t, replayReasoningRow(summaryOnly[idx], mode, ReasoningReplayFormatText), "mode %q row %d", mode, idx)
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
			serialized := marshalChatParams(t, messagesToChatCompletionParams(summaryOnly, mode, ReasoningReplayFormatText))
			for i, row := range serialized {
				_, ok := row["reasoning_content"]
				assert.False(t, ok, "mode %q row %d must not carry reasoning_content", mode, i)
			}
		}
	})

	t.Run("responses carries no reasoning items", func(t *testing.T) {
		for _, mode := range modes {
			rawItems := marshalInputItems(messagesToResponseInputItems(summaryOnly, mode, ReasoningReplayFormatText))
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
		filtered := replayReasoningFilter(msgs, ReasoningReplayToolCalls, ReasoningReplayFormatText)
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

	t.Run("always mode keeps content but strips details in text format", func(t *testing.T) {
		// Regression pin (code review finding 1): the old always+text
		// fast path returned the messages unstripped, leaving envelopes
		// in the counted payload although text format NEVER puts details
		// on the wire — inflating every "always"-mode session's neutral
		// token count once capture-always-on started filling the column.
		env := marshalReasoningDetailsEnvelope(detailsSourceChat, []json.RawMessage{
			json.RawMessage(`{"type":"reasoning.encrypted","data":"eyJk"}`),
		})
		msgs := append(build(), ChatMessage{Role: RoleAssistant, Content: "with env", ReasoningContent: "think", ReasoningDetails: env})
		filtered := replayReasoningFilter(msgs, ReasoningReplayAlways, ReasoningReplayFormatText)
		assert.Equal(t, "hist tool think", filtered[1].ReasoningContent)
		assert.Equal(t, "hist plain think", filtered[3].ReasoningContent)
		assert.Equal(t, "cur think", filtered[5].ReasoningContent)
		last := filtered[len(filtered)-1]
		assert.Equal(t, "think", last.ReasoningContent, "always keeps eligible content")
		assert.Empty(t, last.ReasoningDetails, "text format never rides details — the envelope must not be counted")
	})

	t.Run("off mode strips all reasoning", func(t *testing.T) {
		msgs := build()
		filtered := replayReasoningFilter(msgs, ReasoningReplayOff, ReasoningReplayFormatText)
		assert.Empty(t, filtered[1].ReasoningContent)
		assert.Empty(t, filtered[3].ReasoningContent)
		assert.Empty(t, filtered[5].ReasoningContent)
	})

	t.Run("clean slice returned unwrapped", func(t *testing.T) {
		msgs := []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, Content: "a"},
		}
		assert.Equal(t, msgs, replayReasoningFilter(msgs, ReasoningReplayToolCalls, ReasoningReplayFormatText))
	})

	t.Run("nil slice safe", func(t *testing.T) {
		assert.Nil(t, replayReasoningFilter(nil, ReasoningReplayToolCalls, ReasoningReplayFormatText))
	})
}

// TestReplayReasoningRowDetailsEligibility pins the eligibility widening
// under the details format: a row whose ONLY replayable material is the
// stored envelope (encrypted-only capture — e.g. Claude via OpenRouter,
// no real text) is eligible in details mode and stays NOT eligible in text
// mode (nothing displayable/replayable there). The mode gate still applies
// on top: "off" never, "tool_calls" still requires tool calls on the row.
func TestReplayReasoningRowDetailsEligibility(t *testing.T) {
	env := marshalReasoningDetailsEnvelope(detailsSourceChat, []json.RawMessage{
		json.RawMessage(`{"type":"reasoning.encrypted","data":"eyJk","id":"rc_1"}`),
	})
	detailsOnly := ChatMessage{Role: RoleAssistant, ReasoningDetails: env}

	assert.True(t, replayReasoningRow(detailsOnly, ReasoningReplayAlways, ReasoningReplayFormatDetails),
		"details mode: encrypted-only row is replayable material")
	assert.False(t, replayReasoningRow(detailsOnly, ReasoningReplayToolCalls, ReasoningReplayFormatText),
		"text mode: encrypted-only row has no replayable material")
	assert.False(t, replayReasoningRow(detailsOnly, ReasoningReplayOff, ReasoningReplayFormatDetails),
		"off never replays regardless of format")
	assert.False(t, replayReasoningRow(detailsOnly, ReasoningReplayToolCalls, ReasoningReplayFormatDetails),
		"tool_calls mode still requires tool calls on the row")

	detailsTC := detailsOnly
	detailsTC.ToolCalls = []ToolCall{{ID: "t1"}}
	assert.True(t, replayReasoningRow(detailsTC, ReasoningReplayToolCalls, ReasoningReplayFormatDetails),
		"details + tool calls row replays under tool_calls mode")

	// A row with BOTH fidelities stays eligible in both formats.
	both := ChatMessage{Role: RoleAssistant, ReasoningContent: "real think", ReasoningDetails: env}
	assert.True(t, replayReasoningRow(both, ReasoningReplayAlways, ReasoningReplayFormatText))
	assert.True(t, replayReasoningRow(both, ReasoningReplayAlways, ReasoningReplayFormatDetails))

	// Corrupt envelope does not fake eligibility.
	corrupt := ChatMessage{Role: RoleAssistant, ReasoningDetails: "not json"}
	assert.False(t, replayReasoningRow(corrupt, ReasoningReplayAlways, ReasoningReplayFormatDetails),
		"unparseable envelope is not replayable material")
}

// TestReplayReasoningFilterDetails pins the wire-true filter under the
// details format: whatever will NOT ride the wire is stripped so the token
// accounting counts exactly the payload the serializers emit. Details-mode
// rows with a parseable envelope lose their Content (details ride INSTEAD
// of the text — never both); rows without an envelope keep Content (text
// fallback) and lose the empty/absent envelope; text mode always strips
// Details. Not-eligible rows lose both.
func TestReplayReasoningFilterDetails(t *testing.T) {
	env := marshalReasoningDetailsEnvelope(detailsSourceChat, []json.RawMessage{
		json.RawMessage(`{"type":"reasoning.encrypted","data":"eyJk"}`),
	})
	msgs := []ChatMessage{
		{Role: RoleAssistant, Content: "r1", ReasoningContent: "think 1", ReasoningDetails: env, ToolCalls: []ToolCall{{ID: "t1"}}},
		{Role: RoleAssistant, Content: "r2", ReasoningContent: "think 2"}, // not eligible under tool_calls
	}

	t.Run("details format keeps envelope and strips the text it replaces", func(t *testing.T) {
		got := replayReasoningFilter(msgs, ReasoningReplayToolCalls, ReasoningReplayFormatDetails)
		assert.Equal(t, env, got[0].ReasoningDetails, "eligible row keeps its envelope")
		assert.Equal(t, "", got[0].ReasoningContent, "details ride INSTEAD of the text")
		assert.Equal(t, "", got[1].ReasoningDetails, "not-eligible row loses Details")
		assert.Equal(t, "", got[1].ReasoningContent, "not-eligible row loses Content")
		// Copy-on-write: originals untouched.
		assert.Equal(t, "think 1", msgs[0].ReasoningContent)
		assert.Equal(t, "think 2", msgs[1].ReasoningContent)
	})

	t.Run("details format without envelope falls back to text", func(t *testing.T) {
		noEnv := []ChatMessage{{Role: RoleAssistant, Content: "r", ReasoningContent: "think", ToolCalls: []ToolCall{{ID: "t"}}}}
		got := replayReasoningFilter(noEnv, ReasoningReplayToolCalls, ReasoningReplayFormatDetails)
		assert.Equal(t, "think", got[0].ReasoningContent, "no envelope: text rides")
		assert.Equal(t, "", got[0].ReasoningDetails)
	})

	t.Run("text format strips Details everywhere", func(t *testing.T) {
		got := replayReasoningFilter(msgs, ReasoningReplayToolCalls, ReasoningReplayFormatText)
		assert.Equal(t, "think 1", got[0].ReasoningContent, "text rides")
		assert.Equal(t, "", got[0].ReasoningDetails, "details never ride in text format")
		assert.Equal(t, "", got[1].ReasoningContent)
	})

	t.Run("always mode with details-only row keeps the envelope", func(t *testing.T) {
		detailsOnly := []ChatMessage{{Role: RoleAssistant, ReasoningDetails: env}}
		got := replayReasoningFilter(detailsOnly, ReasoningReplayAlways, ReasoningReplayFormatDetails)
		assert.Equal(t, env, got[0].ReasoningDetails)
	})
}
