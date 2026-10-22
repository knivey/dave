package main

import (
	"bytes"
	"encoding/json"
	"testing"

	logxi "github.com/mgutz/logxi/v1"
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
		// Details are reasoning too: an encrypted-only capture (no real
		// text, no summary) must count as captured reasoning so the
		// "..." sentinel / empty-response paths treat the row as a
		// reasoning-only response, not a fully empty one. Details are
		// never displayable — display stays "" (encrypted blobs and
		// structured items are not renderable text).
		{"details only", reasoningParts{DetailsSource: "chat", DetailsItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning.encrypted","data":"eyJk"}`)}}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantAny, tt.parts.any())
			assert.Equal(t, tt.wantDisplay, tt.parts.display())
		})
	}
}

// TestLogReasoningDetailsPresence pins the log line for exact-replay
// capture: PRESENCE ONLY (dialect + byte size) — the verbatim blobs are
// kilobytes of base64 that would drown the log, and their content is in
// the api-logs anyway.
func TestLogReasoningDetailsPresence(t *testing.T) {
	var buf bytes.Buffer
	logger := logxi.NewLogger3(logxi.NewConcurrentWriter(&buf), "test", logxi.NewJSONFormatter("test"))
	logger.SetLevel(logxi.LevelAll)

	parts := reasoningParts{Content: "real think"}
	parts.addDetails(detailsSourceChat, []json.RawMessage{json.RawMessage(`{"type":"reasoning.encrypted","data":"eyJkYXRhIjoiIn0="}`)})
	logReasoning(logger, parts)

	out := buf.String()
	assert.Contains(t, out, `"_m":"reasoning"`, "content line still logs")
	assert.Contains(t, out, `"source":"chat"`, "details line carries the dialect")
	assert.Contains(t, out, `"bytes":`, "details line carries the size, not the blob")
	assert.NotContains(t, out, "eyJkYXRhIjoiIn0=", "the blob itself never logs")

	buf.Reset()
	logReasoning(logger, reasoningParts{Content: "no details"})
	assert.NotContains(t, buf.String(), `"source"`, "no details line without capture")
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

// TestReasoningDetailsEnvelopeRoundTrip pins the exact-replay envelope: the
// stored string carries the dialect the payload was born in ("chat" = an
// OpenRouter message.reasoning_details array, "responses" = Responses API
// reasoning output items) plus the item bytes VERBATIM — OpenRouter's
// preserving-reasoning contract says the sequence "must match the outputs
// generated by the model during the original request", so no field may be
// dropped, re-ordered, or re-encoded on the way into or out of storage.
func TestReasoningDetailsEnvelopeRoundTrip(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning.text","text":"think","signature":"sha256:abc","id":"rs_1","format":"anthropic-claude-v1","index":0}`),
		json.RawMessage(`{"type":"reasoning.encrypted","data":"eyJlbmNyeXB0ZWQiOnRydWV9","id":"rc_2"}`),
	}
	env := marshalReasoningDetailsEnvelope("chat", items)
	assert.Equal(t,
		`{"source":"chat","items":[{"type":"reasoning.text","text":"think","signature":"sha256:abc","id":"rs_1","format":"anthropic-claude-v1","index":0},{"type":"reasoning.encrypted","data":"eyJlbmNyeXB0ZWQiOnRydWV9","id":"rc_2"}]}`,
		env)

	src, got, ok := parseReasoningDetailsEnvelope(env)
	require.True(t, ok, "round trip parses")
	assert.Equal(t, "chat", src)
	assert.Equal(t, items, got, "item bytes verbatim")

	t.Run("rejections", func(t *testing.T) {
		for _, bad := range []string{
			"",         // nothing captured
			"not json", // corrupt
			`{"no":"source"}`,
			`{"source":"chat"}`, // no items key
			`{"source":"chat","items":"not-an-array"}`,
			`{"source":"bogus","items":[]}`, // unknown dialect
		} {
			_, _, ok := parseReasoningDetailsEnvelope(bad)
			assert.False(t, ok, "input %q should not parse", bad)
		}
	})
}

// TestReasoningPartsDetailsAccumulation pins streaming capture: chunk
// reasoning_details arrays append IN ORDER (OpenRouter: "the complete
// reasoning sequence is built by concatenating all chunks in order"), the
// first capture fixes the source dialect, and nothing was captured until
// addDetails fires.
func TestReasoningPartsDetailsAccumulation(t *testing.T) {
	p := reasoningParts{}
	assert.Equal(t, "", p.detailsEnvelope(), "nothing captured yet")

	p.addDetails(detailsSourceChat, []json.RawMessage{json.RawMessage(`{"type":"reasoning.text","text":"a"}`)})
	p.addDetails(detailsSourceChat, []json.RawMessage{
		json.RawMessage(`{"type":"reasoning.encrypted","data":"e"}`),
		json.RawMessage(`{"type":"reasoning.summary","summary":"s"}`),
	})

	src, items, ok := parseReasoningDetailsEnvelope(p.detailsEnvelope())
	require.True(t, ok)
	assert.Equal(t, detailsSourceChat, src)
	assert.Equal(t, []json.RawMessage{
		json.RawMessage(`{"type":"reasoning.text","text":"a"}`),
		json.RawMessage(`{"type":"reasoning.encrypted","data":"e"}`),
		json.RawMessage(`{"type":"reasoning.summary","summary":"s"}`),
	}, items, "chunks concatenated in arrival order")
}

// TestExtractRawReasoningDetails pins the exact-capture helper for
// non-streaming chat completions: message.reasoning_details items come out
// as VERBATIM bytes (every field — signature, id, format, encrypted data —
// must survive into storage, unlike the typed split which keeps only the
// fidelity slots), and absent/malformed shapes yield nil.
func TestExtractRawReasoningDetails(t *testing.T) {
	body := []byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"a",` +
		`"reasoning_details":[{"type":"reasoning.text","text":"think","signature":"sha256:abc","id":"rs_1","format":"anthropic-claude-v1","index":0},` +
		`{"type":"reasoning.encrypted","data":"eyJkYXRhIjoiIn0=","id":"rc_2"}]}}]}`)
	items := extractRawReasoningDetails(body)
	require.Len(t, items, 2)
	assert.Equal(t, `{"type":"reasoning.text","text":"think","signature":"sha256:abc","id":"rs_1","format":"anthropic-claude-v1","index":0}`, string(items[0]))
	assert.Equal(t, `{"type":"reasoning.encrypted","data":"eyJkYXRhIjoiIn0=","id":"rc_2"}`, string(items[1]))

	assert.Nil(t, extractRawReasoningDetails([]byte(`{"choices":[{"message":{"role":"assistant"}}]}`)), "no reasoning_details key")
	assert.Nil(t, extractRawReasoningDetails([]byte(`{"choices":[{"message":{"reasoning_details":"not-an-array"}}]}`)), "non-array shape")
	assert.Nil(t, extractRawReasoningDetails([]byte(`not json`)))
	assert.Nil(t, extractRawReasoningDetails(nil))
}

// TestExtractStreamReasoningDetailsRaw pins the streaming exact-capture
// helper: delta.reasoning_details items come out VERBATIM; absent or
// malformed shapes yield nil.
func TestExtractStreamReasoningDetailsRaw(t *testing.T) {
	raw := []byte(`{"choices":[{"index":0,"delta":{"reasoning_details":[` +
		`{"type":"reasoning.text","text":"think","signature":"sha256:abc"},` +
		`{"type":"reasoning.encrypted","data":"eyJk","id":"rc_1"}]}}]}`)
	items := extractStreamReasoningDetailsRaw(raw)
	require.Len(t, items, 2)
	assert.Equal(t, `{"type":"reasoning.text","text":"think","signature":"sha256:abc"}`, string(items[0]))
	assert.Equal(t, `{"type":"reasoning.encrypted","data":"eyJk","id":"rc_1"}`, string(items[1]))

	assert.Nil(t, extractStreamReasoningDetailsRaw([]byte(`{"choices":[{"delta":{"content":"x"}}]}`)))
	assert.Nil(t, extractStreamReasoningDetailsRaw([]byte(`{"choices":[]}`)))
	assert.Nil(t, extractStreamReasoningDetailsRaw([]byte(`not json`)))
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
