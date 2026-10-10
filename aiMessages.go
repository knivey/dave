package main

import (
	"encoding/json"
	"strings"

	logxi "github.com/mgutz/logxi/v1"
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

// reasoningParts is captured model reasoning split by fidelity. The split
// exists for replay fidelity: Content is the model's REAL thinking text and
// the only thing that ever rides the wire back to a provider (Chat
// Completions `reasoning_content` on assistant rows; Responses API
// `reasoning` input items with content parts only); Summary is the
// provider's summarized/annotated form (Responses `item.Summary[]`,
// OpenRouter `reasoning_details` `reasoning.summary` entries, the
// `[encrypted reasoning, id=…]` marker) — a summary is NOT the thinking, so
// it is display/log-only and is NEVER serialized, replayed, or
// token-counted. The two are deliberately never concatenated (except
// display(), which prefers Content and falls back to Summary, and the
// img-mcp enhancer's display-only note string).
type reasoningParts struct {
	Content string
	Summary string
}

// any reports whether either fidelity was captured.
func (p reasoningParts) any() bool { return p.Content != "" || p.Summary != "" }

// display returns the human-facing form: the REAL content when captured
// (the stranded answer is usually there), else the summary.
func (p reasoningParts) display() string {
	if p.Content != "" {
		return p.Content
	}
	return p.Summary
}

// logReasoning shows BOTH captured reasoning forms in the logs — the real
// content and the provider's summary — each only when non-empty, so an
// operator can see exactly what was captured in which fidelity slot.
func logReasoning(logger logxi.Logger, parts reasoningParts) {
	if parts.Content != "" {
		logger.Info("reasoning", "content", parts.Content)
	}
	if parts.Summary != "" {
		logger.Info("reasoning summary", "content", parts.Summary)
	}
}

// splitReasoningDetails splits OpenRouter reasoning_details entries by
// fidelity: `reasoning.text` → Content (the real thinking, replayable);
// `reasoning.summary` → Summary (display/log-only); `reasoning.encrypted` →
// Summary with the existing `[encrypted reasoning, id=…]` marker text (a
// non-replayable annotation — encrypted content is deliberately never
// captured, stored, or replayed). Entries of the same fidelity keep the old
// extractReasoningText join's newline separation.
func splitReasoningDetails(details []ReasoningDetail) reasoningParts {
	var contentParts, summaryParts []string
	for _, d := range details {
		switch d.Type {
		case "reasoning.text":
			if d.Text != "" {
				contentParts = append(contentParts, d.Text)
			}
		case "reasoning.summary":
			if d.Summary != "" {
				summaryParts = append(summaryParts, d.Summary)
			}
		case "reasoning.encrypted":
			if d.Data != "" {
				summaryParts = append(summaryParts, "[encrypted reasoning, id="+d.ID+"]")
			}
		}
	}
	return reasoningParts{
		Content: strings.Join(contentParts, "\n"),
		Summary: strings.Join(summaryParts, "\n"),
	}
}

func extractStreamReasoning(rawBytes []byte) reasoningParts {
	var extResp ExtendedStreamResponse
	if err := json.Unmarshal(rawBytes, &extResp); err != nil {
		return reasoningParts{}
	}
	if len(extResp.Choices) == 0 {
		return reasoningParts{}
	}
	details := extResp.Choices[0].Delta.ReasoningDetails
	if len(details) == 0 {
		return reasoningParts{}
	}
	return splitReasoningDetails(details)
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
