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
