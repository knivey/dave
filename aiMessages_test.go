package main

import (
	"encoding/json"
	"testing"

	gogpt "github.com/sashabaranov/go-openai"
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
			if err := json.Unmarshal([]byte(tt.jsonData), &msg); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}

			if msg.Role != tt.wantRole {
				t.Errorf("Role = %q, want %q", msg.Role, tt.wantRole)
			}

			if msg.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", msg.Content, tt.wantContent)
			}

			if msg.HasExtraField("reasoning_details") != tt.wantHasReasoningDetails {
				t.Errorf("HasExtraField(reasoning_details) = %v, want %v", msg.HasExtraField("reasoning_details"), tt.wantHasReasoningDetails)
			}

			// Test ToChatCompletionMessage
			stdMsg := msg.ToChatCompletionMessage()
			if stdMsg.Role != tt.wantRole {
				t.Errorf("ToChatCompletionMessage().Role = %q, want %q", stdMsg.Role, tt.wantRole)
			}
			if stdMsg.Content != tt.wantContent {
				t.Errorf("ToChatCompletionMessage().Content = %q, want %q", stdMsg.Content, tt.wantContent)
			}
		})
	}
}

func TestExtendedMessageGetExtraField(t *testing.T) {
	jsonData := `{"role":"assistant","content":"Answer","reasoning_details":[{"step":1,"content":"Thinking..."}],"custom_field":"value"}`

	var msg ExtendedMessage
	if err := json.Unmarshal([]byte(jsonData), &msg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Test getting reasoning_details
	var reasoningDetails []map[string]any
	if err := msg.GetExtraField("reasoning_details", &reasoningDetails); err != nil {
		t.Errorf("GetExtraField(reasoning_details) failed: %v", err)
	}

	if len(reasoningDetails) != 1 {
		t.Errorf("Got %d reasoning details, want 1", len(reasoningDetails))
	}

	if reasoningDetails[0]["step"].(float64) != 1 {
		t.Errorf("reasoning_details[0].step = %v, want 1", reasoningDetails[0]["step"])
	}

	// Test getting custom_field
	var customField string
	if err := msg.GetExtraField("custom_field", &customField); err != nil {
		t.Errorf("GetExtraField(custom_field) failed: %v", err)
	}

	if customField != "value" {
		t.Errorf("custom_field = %q, want %q", customField, "value")
	}

	// Test non-existent field (should not error)
	var nonExistent string
	if err := msg.GetExtraField("non_existent", &nonExistent); err != nil {
		t.Errorf("GetExtraField(non_existent) should not error: %v", err)
	}
}

func TestExtendedMessageBackwardCompatible(t *testing.T) {
	// Test that we can still use standard ChatCompletionMessage where needed
	stdMsg := gogpt.ChatCompletionMessage{
		Role:    "user",
		Content: "Hello",
	}

	// Convert to ExtendedMessage
	var extMsg ExtendedMessage
	extMsg.ChatCompletionMessage = stdMsg

	// Should be able to convert back
	result := extMsg.ToChatCompletionMessage()
	if result.Role != stdMsg.Role {
		t.Errorf("Role mismatch: got %q, want %q", result.Role, stdMsg.Role)
	}
	if result.Content != stdMsg.Content {
		t.Errorf("Content mismatch: got %q, want %q", result.Content, stdMsg.Content)
	}
}
