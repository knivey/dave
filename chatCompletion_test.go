package main

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildReplayHistory is the canonical history for replay-shape tests:
// [system, user, assistant+tc(hist reasoning), tool, assistant(hist
// reasoning), user, assistant+tc(current reasoning), tool,
// assistant(current reasoning)]. The serialized params array maps 1:1 onto
// these indices (every ChatMessage becomes exactly one chat param).
func buildReplayHistory() []ChatMessage {
	return []ChatMessage{
		{Role: RoleSystem, Content: "system"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "hist reply", ReasoningContent: "hist think tc", ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}},
		{Role: RoleTool, Content: "tool result", ToolCallID: "t1"},
		{Role: RoleAssistant, Content: "hist final", ReasoningContent: "hist think plain"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, ReasoningContent: "cur think tc", ToolCalls: []ToolCall{{ID: "t2", Type: "function", Function: FunctionCall{Name: "g", Arguments: "{}"}}}},
		{Role: RoleTool, Content: "tool result 2", ToolCallID: "t2"},
		{Role: RoleAssistant, Content: "current final", ReasoningContent: "cur think plain"},
	}
}

// TestMessagesToChatCompletionParamsReplayDetails pins the exact wire shape
// of details-format replay on the Chat Completions path: eligible rows ride
// message.reasoning_details with the envelope's VERBATIM items (and never
// reasoning_content alongside); rows without a usable chat-dialect envelope
// fall back to the text form. Exactly one of the two fields per row.
func TestMessagesToChatCompletionParamsReplayDetails(t *testing.T) {
	chatEnv := marshalReasoningDetailsEnvelope(detailsSourceChat, []json.RawMessage{
		json.RawMessage(`{"type":"reasoning.text","text":"real think","signature":"sha256:abc"}`),
		json.RawMessage(`{"type":"reasoning.encrypted","data":"eyJkYXRhIjoiIn0=","id":"rc_1"}`),
	})
	respEnv := marshalReasoningDetailsEnvelope(detailsSourceResponses, []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_p1","encrypted_content":"ENC"}`),
	})
	msgs := []ChatMessage{
		{Role: RoleUser, Content: "u"},
		// Row 1: both fidelities + tool calls.
		{Role: RoleAssistant, Content: "a1", ReasoningContent: "text form", ReasoningDetails: chatEnv, ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}},
		// Row 2: no envelope — text fallback target.
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, Content: "a2", ReasoningContent: "just text", ToolCalls: []ToolCall{{ID: "t2", Type: "function", Function: FunctionCall{Name: "g", Arguments: "{}"}}}},
		// Row 4: cross-dialect envelope — NOT usable on the chat path.
		{Role: RoleUser, Content: "u3"},
		{Role: RoleAssistant, Content: "a3", ReasoningContent: "born responses", ReasoningDetails: respEnv, ToolCalls: []ToolCall{{ID: "t3", Type: "function", Function: FunctionCall{Name: "h", Arguments: "{}"}}}},
	}

	t.Run("details format rides the verbatim array instead of the text", func(t *testing.T) {
		serialized := marshalChatParams(t, messagesToChatCompletionParams(msgs, ReasoningReplayToolCalls, ReasoningReplayFormatDetails))
		// Row 1: chat-dialect envelope → verbatim items, no text field.
		details, ok := serialized[1]["reasoning_details"]
		require.True(t, ok, "row 1 carries reasoning_details")
		raw, err := json.Marshal(details)
		require.NoError(t, err)
		assert.JSONEq(t, `[{"type":"reasoning.text","text":"real think","signature":"sha256:abc"},{"type":"reasoning.encrypted","data":"eyJkYXRhIjoiIn0=","id":"rc_1"}]`, string(raw))
		_, hasContent := serialized[1]["reasoning_content"]
		assert.False(t, hasContent, "details ride INSTEAD of the text — never both")
		// Row 2: no envelope → text fallback.
		assert.Equal(t, "just text", serialized[3]["reasoning_content"])
		_, hasDetails := serialized[3]["reasoning_details"]
		assert.False(t, hasDetails)
		// Row 4: responses-dialect envelope is not usable on the chat
		// path → text fallback.
		assert.Equal(t, "born responses", serialized[5]["reasoning_content"])
		_, hasDetails = serialized[5]["reasoning_details"]
		assert.False(t, hasDetails, "cross-dialect envelope never rides the chat path")
	})

	t.Run("text format is unchanged by the envelope's presence", func(t *testing.T) {
		serialized := marshalChatParams(t, messagesToChatCompletionParams(msgs, ReasoningReplayToolCalls, ReasoningReplayFormatText))
		assert.Equal(t, "text form", serialized[1]["reasoning_content"])
		_, hasDetails := serialized[1]["reasoning_details"]
		assert.False(t, hasDetails, "text format never rides the array")
	})

	t.Run("details-only row replays the array with no text fallback", func(t *testing.T) {
		detailsOnly := []ChatMessage{
			{Role: RoleUser, Content: "u"},
			{Role: RoleAssistant, Content: "final", ReasoningDetails: chatEnv},
		}
		serialized := marshalChatParams(t, messagesToChatCompletionParams(detailsOnly, ReasoningReplayAlways, ReasoningReplayFormatDetails))
		details, ok := serialized[1]["reasoning_details"]
		require.True(t, ok, "eligibility widening: envelope-only row replays in details mode")
		raw, err := json.Marshal(details)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"signature":"sha256:abc"`)
		_, hasContent := serialized[1]["reasoning_content"]
		assert.False(t, hasContent, "no content captured — nothing to fall back to")
	})
}

func marshalChatParams(t *testing.T, params []openai.ChatCompletionMessageParamUnion) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// TestMessagesToChatCompletionParamsReplayReasoning pins the exact wire
// shape of reasoning replay on the Chat Completions path: `reasoning_content`
// rides on assistant rows the resolved mode makes eligible, and nowhere else.
func TestMessagesToChatCompletionParamsReplayReasoning(t *testing.T) {
	msgs := buildReplayHistory()

	tests := []struct {
		name string
		mode string
		want map[int]string // param index -> expected reasoning_content ("" = key must be absent)
	}{
		{
			name: "off mode never attaches reasoning",
			mode: ReasoningReplayOff,
			want: map[int]string{},
		},
		{
			name: "tool_calls mode attaches every tool row, past and current turns",
			mode: ReasoningReplayToolCalls,
			want: map[int]string{2: "hist think tc", 6: "cur think tc"},
		},
		{
			name: "always mode attaches every assistant reasoning row",
			mode: ReasoningReplayAlways,
			want: map[int]string{
				2: "hist think tc",
				4: "hist think plain",
				6: "cur think tc",
				8: "cur think plain",
			},
		},
		{
			name: "unknown raw mode falls back to tool_calls semantics",
			mode: "bogus",
			want: map[int]string{2: "hist think tc", 6: "cur think tc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serialized := marshalChatParams(t, messagesToChatCompletionParams(msgs, tt.mode, ReasoningReplayFormatText))
			require.Len(t, serialized, len(msgs))
			for i := range msgs {
				got, ok := serialized[i]["reasoning_content"]
				var want any
				if wantStr, has := tt.want[i]; has {
					want = wantStr
				}
				if want == nil {
					assert.False(t, ok, "row %d must not carry reasoning_content", i)
				} else {
					require.True(t, ok, "row %d must carry reasoning_content", i)
					assert.Equal(t, want, got)
				}
			}
		})
	}

	t.Run("tool calls and content ride alongside reasoning", func(t *testing.T) {
		serialized := marshalChatParams(t, messagesToChatCompletionParams(msgs, ReasoningReplayToolCalls, ReasoningReplayFormatText))
		assert.Equal(t, "cur think tc", serialized[6]["reasoning_content"])
		assert.NotEmpty(t, serialized[6]["tool_calls"], "tool calls preserved with the reasoning")
	})

	// Production incident pin (session 1891): a tool-call assistant row
	// from a CLOSED past turn — a later user message sits after it —
	// still gets its stored reasoning on the wire in tool_calls mode.
	// Per-row tool calls decide eligibility; position no longer does.
	t.Run("tool_calls mode attaches reasoning on a past-turn tool row before a later user message", func(t *testing.T) {
		past := []ChatMessage{
			{Role: RoleUser, Content: "u1"},
			{Role: RoleAssistant, ReasoningContent: "past tool think", ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}}}},
			{Role: RoleTool, Content: "result", ToolCallID: "t1"},
			{Role: RoleUser, Content: "u2"},
			{Role: RoleAssistant, Content: "final", ReasoningContent: "past plain think"},
		}
		serialized := marshalChatParams(t, messagesToChatCompletionParams(past, ReasoningReplayToolCalls, ReasoningReplayFormatText))
		require.Len(t, serialized, len(past))
		require.Equal(t, "past tool think", serialized[1]["reasoning_content"],
			"the past-turn tool row (before the later user message) must carry reasoning_content")
		_, ok := serialized[4]["reasoning_content"]
		assert.False(t, ok, "the past-turn plain final answer stays stripped — no tool calls on the row")
	})
}

// TestMessagesToChatCompletionParamsNoReasoning ensures the baseline shape
// is unchanged for the off mode: no assistant row gains a reasoning_content
// key, whether or not it carries stored reasoning or tool calls.
func TestMessagesToChatCompletionParamsNoReasoning(t *testing.T) {
	serialized := marshalChatParams(t, messagesToChatCompletionParams(buildReplayHistory(), ReasoningReplayOff, ReasoningReplayFormatText))
	for i, row := range serialized {
		_, ok := row["reasoning_content"]
		assert.False(t, ok, "row %d", i)
	}
}
