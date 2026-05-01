package main

import (
	"encoding/json"
	"strings"
)

type ExtendedMessage struct {
	ChatMessage
	ExtraFields map[string]json.RawMessage `json:"-"`
}

func (m *ExtendedMessage) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if err := json.Unmarshal(data, &m.ChatMessage); err != nil {
		return err
	}

	standardFields := map[string]bool{
		"role":              true,
		"content":           true,
		"reasoning_content": true,
		"multi_content":     true,
		"name":              true,
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

func (m *ExtendedMessage) ToChatCompletionMessage() ChatMessage {
	return m.ChatMessage
}

func (m *ExtendedMessage) GetExtraField(key string, dest interface{}) error {
	if raw, ok := m.ExtraFields[key]; ok {
		return json.Unmarshal(raw, dest)
	}
	return nil
}

func (m *ExtendedMessage) HasExtraField(key string) bool {
	_, ok := m.ExtraFields[key]
	return ok
}

type ReasoningDetail struct {
	Type      string `json:"type"`
	Summary   string `json:"summary,omitempty"`
	Text      string `json:"text,omitempty"`
	Data      string `json:"data,omitempty"`
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
	Format    string `json:"format,omitempty"`
	Index     int    `json:"index,omitempty"`
}

type extendedStreamDelta struct {
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`
}

type extendedStreamChoice struct {
	Delta extendedStreamDelta `json:"delta"`
}

type ExtendedStreamResponse struct {
	Choices []extendedStreamChoice `json:"choices,omitempty"`
}

type extendedMessageRaw struct {
	Reasoning        string            `json:"reasoning,omitempty"`
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`
}

type extendedChoiceRaw struct {
	Message extendedMessageRaw `json:"message"`
}

type ExtendedMessageResponse struct {
	Choices []extendedChoiceRaw `json:"choices,omitempty"`
}

func extractReasoningText(details []ReasoningDetail) string {
	var parts []string
	for _, d := range details {
		switch d.Type {
		case "reasoning.text":
			if d.Text != "" {
				parts = append(parts, d.Text)
			}
		case "reasoning.summary":
			if d.Summary != "" {
				parts = append(parts, d.Summary)
			}
		case "reasoning.encrypted":
			if d.Data != "" {
				parts = append(parts, "[encrypted reasoning, id="+d.ID+"]")
			}
		}
	}
	return strings.Join(parts, "\n")
}

func extractStreamReasoning(rawBytes []byte) string {
	var extResp ExtendedStreamResponse
	if err := json.Unmarshal(rawBytes, &extResp); err != nil {
		return ""
	}
	if len(extResp.Choices) == 0 {
		return ""
	}
	details := extResp.Choices[0].Delta.ReasoningDetails
	if len(details) == 0 {
		return ""
	}
	return extractReasoningText(details)
}

func extractReasoningFromField(rawBytes []byte) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rawBytes, &raw); err != nil {
		return ""
	}
	choicesRaw, ok := raw["choices"]
	if !ok {
		return ""
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil || len(choices) == 0 {
		return ""
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(choices[0], &delta); err != nil {
		return ""
	}
	msgRaw, ok := delta["delta"]
	if !ok {
		msgRaw = delta["message"]
	}
	if msgRaw == nil {
		return ""
	}
	var msgFields map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msgFields); err != nil {
		return ""
	}
	var reasoning string
	if r, ok := msgFields["reasoning"]; ok {
		json.Unmarshal(r, &reasoning)
	}
	return reasoning
}

func extractResponseReasoning(rawBytes []byte) (reasoning string, reasoningDetails []ReasoningDetail) {
	if len(rawBytes) == 0 {
		return "", nil
	}
	var extResp ExtendedMessageResponse
	if err := json.Unmarshal(rawBytes, &extResp); err != nil {
		return "", nil
	}
	if len(extResp.Choices) == 0 {
		return "", nil
	}
	return extResp.Choices[0].Message.Reasoning, extResp.Choices[0].Message.ReasoningDetails
}
