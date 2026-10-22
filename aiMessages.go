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
// the only thing that rides the wire back to a provider in the default
// "text" format (Chat Completions `reasoning_content` on assistant rows;
// Responses API `reasoning` input items with content parts only); Summary
// is the provider's summarized/annotated form (Responses `item.Summary[]`,
// OpenRouter `reasoning_details` `reasoning.summary` entries, the
// `[encrypted reasoning, id=…]` marker) — a summary is NOT the thinking, so
// it is display/log-only and is NEVER serialized, replayed, or
// token-counted. DetailsItems/DetailsSource are the exact-replay fidelity
// for `reasoningreplayformat = "details"` (see reasoningreplay.go). The
// forms are deliberately never concatenated (except display(), which
// prefers Content and falls back to Summary, and the img-mcp enhancer's
// display-only note string).
type reasoningParts struct {
	Content string
	Summary string
	// DetailsItems/DetailsSource are the EXACT-replay fidelity: verbatim
	// reasoning detail items in arrival order (OpenRouter
	// message.reasoning_details arrays — streaming chunks concatenate in
	// order per OpenRouter's docs — or Responses API reasoning output
	// items). Captured always (storage is local and cheap), replayed only
	// when reasoningreplayformat = "details" (see reasoningreplay.go).
	// Never displayable: encrypted blobs and structured items are not
	// renderable text.
	DetailsSource string
	DetailsItems  []json.RawMessage
}

// any reports whether either fidelity was captured.
func (p reasoningParts) any() bool {
	return p.Content != "" || p.Summary != "" || len(p.DetailsItems) > 0
}

// addDetails appends verbatim detail items captured from the given
// dialect. The first capture fixes the source; a later capture from a
// different dialect is ignored (a single assistant row is one API
// round-trip on one path — a second dialect would mean corrupted capture).
func (p *reasoningParts) addDetails(source string, items []json.RawMessage) {
	if len(items) == 0 {
		return
	}
	if p.DetailsSource == "" {
		p.DetailsSource = source
	}
	if p.DetailsSource != source {
		return
	}
	p.DetailsItems = append(p.DetailsItems, items...)
}

// detailsEnvelope renders the storage form of the captured details ("" when
// nothing was captured).
func (p reasoningParts) detailsEnvelope() string {
	return marshalReasoningDetailsEnvelope(p.DetailsSource, p.DetailsItems)
}

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
// operator can see exactly what was captured in which fidelity slot. The
// exact-replay envelope logs PRESENCE ONLY (dialect + byte size): the
// verbatim blobs are frequently kilobytes of base64 that would drown the
// log line, and their content is fully visible in the api-logs anyway.
func logReasoning(logger logxi.Logger, parts reasoningParts) {
	if parts.Content != "" {
		logger.Info("reasoning", "content", parts.Content)
	}
	if parts.Summary != "" {
		logger.Info("reasoning summary", "content", parts.Summary)
	}
	if len(parts.DetailsItems) > 0 {
		if env := parts.detailsEnvelope(); env != "" {
			logger.Info("reasoning details", "source", parts.DetailsSource, "bytes", len(env))
		}
	}
}

// Detail-source dialects for the exact-replay envelope. The payload's wire
// shape differs per API: "chat" is an OpenRouter message.reasoning_details
// array (replayed as an extra field on the assistant row), "responses" is
// Responses API reasoning output items (replayed as standalone reasoning
// input items). Exact replay only ever happens on the dialect the payload
// was born on — cross-dialect conversion would modify the sequence, which
// OpenRouter's preserving-reasoning contract forbids.
const (
	detailsSourceChat      = "chat"
	detailsSourceResponses = "responses"
)

type reasoningDetailsEnvelope struct {
	Source string            `json:"source"`
	Items  []json.RawMessage `json:"items"`
}

// marshalReasoningDetailsEnvelope renders the exact-replay envelope. Items
// marshal VERBATIM (json.RawMessage) — OpenRouter requires the sequence to
// match the original output, so nothing may be dropped or re-encoded.
func marshalReasoningDetailsEnvelope(source string, items []json.RawMessage) string {
	if len(items) == 0 {
		return ""
	}
	data, err := json.Marshal(reasoningDetailsEnvelope{Source: source, Items: items})
	if err != nil {
		// RawMessage items already parsed as JSON; marshal cannot fail.
		return ""
	}
	return string(data)
}

// parseReasoningDetailsEnvelope is the storage-side inverse. ok=false for
// empty/corrupt/unknown-dialect payloads — callers fall back to the text
// form (reasoning_content) exactly as if nothing had been captured.
func parseReasoningDetailsEnvelope(s string) (source string, items []json.RawMessage, ok bool) {
	if s == "" {
		return "", nil, false
	}
	var env reasoningDetailsEnvelope
	if err := json.Unmarshal([]byte(s), &env); err != nil {
		return "", nil, false
	}
	switch env.Source {
	case detailsSourceChat, detailsSourceResponses:
	default:
		return "", nil, false
	}
	if len(env.Items) == 0 {
		return "", nil, false
	}
	return env.Source, env.Items, true
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

// extractStreamReasoningDetailsRaw pulls delta.reasoning_details from a
// streaming chat chunk as VERBATIM item bytes — the exact-replay capture
// counterpart of extractStreamReasoning (which keeps only the fidelity
// split). nil when absent or malformed.
func extractStreamReasoningDetailsRaw(chunkRaw []byte) []json.RawMessage {
	var raw struct {
		Choices []struct {
			Delta json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(chunkRaw, &raw); err != nil || len(raw.Choices) == 0 {
		return nil
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(raw.Choices[0].Delta, &delta); err != nil {
		return nil
	}
	arr, ok := delta["reasoning_details"]
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil {
		return nil
	}
	return items
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

// extractRawReasoningDetails pulls message.reasoning_details from a
// non-streaming chat completion body as VERBATIM item bytes. Unlike
// splitReasoningDetails (which keeps only the replayable-text and summary
// slots), this is the exact-replay capture: every field — signature, id,
// format, encrypted data — must survive into storage because OpenRouter's
// preserving-reasoning contract requires the echoed sequence to match the
// original output. nil when absent or malformed.
func extractRawReasoningDetails(body []byte) []json.RawMessage {
	var raw struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || len(raw.Choices) == 0 {
		return nil
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(raw.Choices[0].Message, &msg); err != nil {
		return nil
	}
	arr, ok := msg["reasoning_details"]
	if !ok {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(arr, &items); err != nil {
		return nil
	}
	return items
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
