package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtendedMessageUnmarshal(t *testing.T) {
	tests := []struct {
		name                    string
		jsonData                string
		wantRole                string
		wantContent             string
		wantHasReasoningDetails bool
	}{
		{
			name:                    "Standard message only",
			jsonData:                `{"role":"assistant","content":"Hello"}`,
			wantRole:                "assistant",
			wantContent:             "Hello",
			wantHasReasoningDetails: false,
		},
		{
			name:                    "Message with reasoning_details",
			jsonData:                `{"role":"assistant","content":"Answer","reasoning_details":[{"step":1,"content":"Thinking..."}]}`,
			wantRole:                "assistant",
			wantContent:             "Answer",
			wantHasReasoningDetails: true,
		},
		{
			name:                    "Message with reasoning_content",
			jsonData:                `{"role":"assistant","content":"Answer","reasoning_content":"I think...","reasoning_details":[{"step":1,"content":"Step 1"}]}`,
			wantRole:                "assistant",
			wantContent:             "Answer",
			wantHasReasoningDetails: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg ExtendedMessage
			require.NoError(t, json.Unmarshal([]byte(tt.jsonData), &msg), "Unmarshal failed")

			assert.Equal(t, tt.wantRole, msg.Role, "Role")
			assert.Equal(t, tt.wantContent, msg.Content, "Content")
			assert.Equal(t, tt.wantHasReasoningDetails, msg.HasExtraField("reasoning_details"), "HasExtraField(reasoning_details)")

			stdMsg := msg.ToChatCompletionMessage()
			assert.Equal(t, tt.wantRole, stdMsg.Role, "ToChatCompletionMessage().Role")
			assert.Equal(t, tt.wantContent, stdMsg.Content, "ToChatCompletionMessage().Content")
		})
	}
}

func TestExtendedMessageGetExtraField(t *testing.T) {
	jsonData := `{"role":"assistant","content":"Answer","reasoning_details":[{"step":1,"content":"Thinking..."}],"custom_field":"value"}`

	var msg ExtendedMessage
	require.NoError(t, json.Unmarshal([]byte(jsonData), &msg), "Unmarshal failed")

	var reasoningDetails []map[string]any
	assert.NoError(t, msg.GetExtraField("reasoning_details", &reasoningDetails), "GetExtraField(reasoning_details)")

	assert.Len(t, reasoningDetails, 1, "reasoning details")
	assert.Equal(t, float64(1), reasoningDetails[0]["step"], "reasoning_details[0].step")

	var customField string
	assert.NoError(t, msg.GetExtraField("custom_field", &customField), "GetExtraField(custom_field)")
	assert.Equal(t, "value", customField, "custom_field")

	var nonExistent string
	assert.NoError(t, msg.GetExtraField("non_existent", &nonExistent), "GetExtraField(non_existent)")
}

func TestExtendedMessageBackwardCompatible(t *testing.T) {
	stdMsg := ChatMessage{
		Role:    "user",
		Content: "Hello",
	}

	var extMsg ExtendedMessage
	extMsg.ChatMessage = stdMsg

	result := extMsg.ToChatCompletionMessage()
	assert.Equal(t, stdMsg.Role, result.Role, "Role mismatch")
	assert.Equal(t, stdMsg.Content, result.Content, "Content mismatch")
}

// TestReasoningPartsAnyDisplay pins the split-capture helpers: any() reports
// either fidelity, display() prefers the REAL content (the stranded answer
// is usually there) and falls back to the summary.
func TestReasoningPartsAnyDisplay(t *testing.T) {
	tests := []struct {
		name        string
		parts       reasoningParts
		wantAny     bool
		wantDisplay string
	}{
		{"empty", reasoningParts{}, false, ""},
		{"content only", reasoningParts{Content: "real thinking"}, true, "real thinking"},
		{"summary only", reasoningParts{Summary: "summarized"}, true, "summarized"},
		{"both — content preferred", reasoningParts{Content: "real thinking", Summary: "summarized"}, true, "real thinking"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantAny, tt.parts.any())
			assert.Equal(t, tt.wantDisplay, tt.parts.display())
		})
	}
}

// TestSplitReasoningDetails pins the OpenRouter reasoning_details split:
// `reasoning.text` entries land in Content (the REAL thinking — the only
// replayable fidelity); `reasoning.summary` entries land in Summary
// (display/log-only); `reasoning.encrypted` entries keep the existing
// non-replayable marker text and land in Summary. Same-fidelity entries
// keep the old extractReasoningText join's newline separation; unknown
// entry types and empty texts are ignored.
func TestSplitReasoningDetails(t *testing.T) {
	tests := []struct {
		name    string
		details []ReasoningDetail
		want    reasoningParts
	}{
		{
			name:    "no details",
			details: nil,
			want:    reasoningParts{},
		},
		{
			name: "text detail lands in content only",
			details: []ReasoningDetail{
				{Type: "reasoning.text", Text: "the real thinking"},
			},
			want: reasoningParts{Content: "the real thinking"},
		},
		{
			name: "summary detail lands in summary only",
			details: []ReasoningDetail{
				{Type: "reasoning.summary", Summary: "the summarized form"},
			},
			want: reasoningParts{Summary: "the summarized form"},
		},
		{
			name: "encrypted detail keeps the marker in summary",
			details: []ReasoningDetail{
				{Type: "reasoning.encrypted", Data: "encblob", ID: "rc_1"},
			},
			want: reasoningParts{Summary: "[encrypted reasoning, id=rc_1]"},
		},
		{
			name: "mixed details split by fidelity with newline joins",
			details: []ReasoningDetail{
				{Type: "reasoning.text", Text: "think one"},
				{Type: "reasoning.summary", Summary: "sum one"},
				{Type: "reasoning.text", Text: "think two"},
				{Type: "reasoning.summary", Summary: "sum two"},
				{Type: "reasoning.encrypted", Data: "enc", ID: "rc_9"},
			},
			want: reasoningParts{
				Content: "think one\nthink two",
				Summary: "sum one\nsum two\n[encrypted reasoning, id=rc_9]",
			},
		},
		{
			name: "empty texts and unknown types are ignored",
			details: []ReasoningDetail{
				{Type: "reasoning.text", Text: ""},
				{Type: "reasoning.summary", Summary: ""},
				{Type: "reasoning.encrypted", Data: "", ID: "rc_0"},
				{Type: "reasoning.other", Text: "mystery"},
			},
			want: reasoningParts{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, splitReasoningDetails(tt.details))
		})
	}
}

// TestExtractStreamReasoning pins the stream-chunk reasoning_details walk:
// it returns the split parts (Content from reasoning.text deltas, Summary
// from reasoning.summary deltas) and never merges them.
func TestExtractStreamReasoning(t *testing.T) {
	t.Run("reasoning_details delta splits into parts", func(t *testing.T) {
		raw := []byte(`{"choices":[{"index":0,"delta":{"reasoning_details":[` +
			`{"type":"reasoning.text","text":"real chunk"},` +
			`{"type":"reasoning.summary","summary":"summary chunk"}]}}]}`)
		got := extractStreamReasoning(raw)
		assert.Equal(t, reasoningParts{Content: "real chunk", Summary: "summary chunk"}, got)
	})

	t.Run("no details yield empty parts", func(t *testing.T) {
		got := extractStreamReasoning([]byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`))
		assert.Equal(t, reasoningParts{}, got)
	})

	t.Run("unparseable bytes yield empty parts", func(t *testing.T) {
		assert.Equal(t, reasoningParts{}, extractStreamReasoning([]byte(`not json`)))
	})
}
