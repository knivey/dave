package main

import (
	"encoding/json"
	gogpt "github.com/sashabaranov/go-openai"
)

// ExtendedMessage wraps ChatCompletionMessage to capture provider-specific fields
// like reasoning_details, thinking_content, and other non-standard fields.
type ExtendedMessage struct {
	gogpt.ChatCompletionMessage
	// Provider-specific fields that aren't in the standard library
	// These will be captured via custom unmarshaling
	ExtraFields map[string]json.RawMessage `json:"-"`
}

// UnmarshalJSON captures both standard fields and any extra fields
func (m *ExtendedMessage) UnmarshalJSON(data []byte) error {
	// Use a map to capture all fields first
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	// Unmarshal standard fields into embedded struct
	if err := json.Unmarshal(data, &m.ChatCompletionMessage); err != nil {
		return err
	}

	// Capture any extra fields (like reasoning_details)
	standardFields := map[string]bool{
		"role":              true,
		"content":           true,
		"refusal":           true,
		"multi_content":     true,
		"name":              true,
		"reasoning_content": true,
		"function_call":     true,
		"tool_calls":        true,
		"tool_call_id":      true,
	}

	m.ExtraFields = make(map[string]json.RawMessage)
	for key, value := range raw {
		if !standardFields[key] {
			m.ExtraFields[key] = value
		}
	}

	return nil
}

// ToChatCompletionMessage extracts the standard message for API calls
func (m *ExtendedMessage) ToChatCompletionMessage() gogpt.ChatCompletionMessage {
	return m.ChatCompletionMessage
}

// GetExtraField retrieves a specific extra field and unmarshals it into dest
func (m *ExtendedMessage) GetExtraField(key string, dest interface{}) error {
	if raw, ok := m.ExtraFields[key]; ok {
		return json.Unmarshal(raw, dest)
	}
	return nil
}

// HasExtraField checks if a specific extra field exists
func (m *ExtendedMessage) HasExtraField(key string) bool {
	_, ok := m.ExtraFields[key]
	return ok
}
