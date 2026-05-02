package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func marshalInputItems(items []responses.ResponseInputItemUnionParam) [][]byte {
	out := make([][]byte, len(items))
	for i, item := range items {
		b, _ := json.Marshal(item)
		out[i] = b
	}
	return out
}

func TestMessagesToResponseInputItems(t *testing.T) {
	tests := []struct {
		name      string
		messages  []ChatMessage
		wantParts []map[string]any
	}{
		{
			name: "plain text user message",
			messages: []ChatMessage{
				{Role: RoleUser, Content: "hello"},
			},
			wantParts: []map[string]any{
				{
					"role":    "user",
					"content": "hello",
				},
			},
		},
		{
			name: "system message",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "you are helpful"},
			},
			wantParts: []map[string]any{
				{
					"role":    "system",
					"content": "you are helpful",
				},
			},
		},
		{
			name: "user message with text and image MultiContent",
			messages: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeText, Text: "describe this"},
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL:    "data:image/png;base64,abc123",
							Detail: ImageDetailAuto,
						}},
					},
				},
			},
			wantParts: []map[string]any{
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "input_text", "text": "describe this"},
						{"type": "input_image", "image_url": "data:image/png;base64,abc123", "detail": "auto"},
					},
				},
			},
		},
		{
			name: "user message with image without detail",
			messages: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL: "https://example.com/img.png",
						}},
					},
				},
			},
			wantParts: []map[string]any{
				{
					"role": "user",
					"content": []map[string]any{
						{"type": "input_image", "image_url": "https://example.com/img.png"},
					},
				},
			},
		},
		{
			name: "assistant message with tool calls",
			messages: []ChatMessage{
				{
					Role:    RoleAssistant,
					Content: "let me check",
					ToolCalls: []ToolCall{
						{
							ID:   "call_1",
							Type: "function",
							Function: FunctionCall{
								Name:      "get_weather",
								Arguments: `{"city":"NYC"}`,
							},
						},
					},
				},
			},
			wantParts: []map[string]any{
				{
					"role":    "assistant",
					"content": "let me check",
				},
				{
					"type":      "function_call",
					"call_id":   "call_1",
					"name":      "get_weather",
					"arguments": `{"city":"NYC"}`,
				},
			},
		},
		{
			name: "tool result message",
			messages: []ChatMessage{
				{
					Role:       RoleTool,
					Content:    "sunny, 72F",
					ToolCallID: "call_1",
				},
			},
			wantParts: []map[string]any{
				{
					"type":    "function_call_output",
					"call_id": "call_1",
					"output":  "sunny, 72F",
				},
			},
		},
		{
			name: "assistant message without tool calls",
			messages: []ChatMessage{
				{Role: RoleAssistant, Content: "hello there"},
			},
			wantParts: []map[string]any{
				{
					"role":    "assistant",
					"content": "hello there",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := messagesToResponseInputItems(tt.messages)
			if len(result) != len(tt.wantParts) {
				t.Fatalf("got %d input items, want %d", len(result), len(tt.wantParts))
			}
			rawItems := marshalInputItems(result)
			for i, raw := range rawItems {
				var got map[string]any
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("item %d: unmarshal error: %v", i, err)
				}
				want := tt.wantParts[i]
				assertJSONEqual(t, i, got, want)
			}
		})
	}
}

func assertJSONEqual(t *testing.T, idx int, got, want map[string]any) {
	t.Helper()
	for k, wantV := range want {
		gotV, ok := got[k]
		if !ok {
			t.Errorf("item %d: missing key %q", idx, k)
			continue
		}
		switch wv := wantV.(type) {
		case []map[string]any:
			gotSlice, ok := gotV.([]any)
			if !ok {
				t.Errorf("item %d key %q: got %T, want slice", idx, k, gotV)
				continue
			}
			if len(gotSlice) != len(wv) {
				t.Errorf("item %d key %q: got %d items, want %d", idx, k, len(gotSlice), len(wv))
				continue
			}
			for j, wantItem := range wv {
				gotItem, ok := gotSlice[j].(map[string]any)
				if !ok {
					t.Errorf("item %d key %q[%d]: got %T, want map", idx, k, j, gotSlice[j])
					continue
				}
				for kk, wv2 := range wantItem {
					gv2, ok := gotItem[kk]
					if !ok {
						t.Errorf("item %d key %q[%d]: missing key %q", idx, k, j, kk)
						continue
					}
					if gv2 != wv2 {
						t.Errorf("item %d key %q[%d].%s: got %v, want %v", idx, k, j, kk, gv2, wv2)
					}
				}
				for kk := range gotItem {
					if _, ok := wantItem[kk]; !ok {
						t.Errorf("item %d key %q[%d]: unexpected key %q with value %v", idx, k, j, kk, gotItem[kk])
					}
				}
			}
		case string:
			gs, ok := gotV.(string)
			if !ok {
				t.Errorf("item %d key %q: got %T, want string", idx, k, gotV)
				continue
			}
			if gs != wv {
				t.Errorf("item %d key %q: got %q, want %q", idx, k, gs, wv)
			}
		default:
			if gotV != wantV {
				t.Errorf("item %d key %q: got %v, want %v", idx, k, gotV, wantV)
			}
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("item %d: unexpected key %q with value %v", idx, k, got[k])
		}
	}
}

func TestMessagesToResponseInputItems_ImageURLIsString(t *testing.T) {
	msgs := []ChatMessage{
		{
			Role: RoleUser,
			MultiContent: []MessagePart{
				{Type: PartTypeImageURL, ImageURL: &ImageURL{
					URL:    "data:image/png;base64,abc123",
					Detail: ImageDetailAuto,
				}},
			},
		},
	}

	input := messagesToResponseInputItems(msgs)
	if len(input) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(input))
	}

	rawItems := marshalInputItems(input)
	var parsed map[string]any
	if err := json.Unmarshal(rawItems[0], &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	content, ok := parsed["content"].([]any)
	if !ok {
		t.Fatalf("content is not a slice, got %T", parsed["content"])
	}
	if len(content) != 1 {
		t.Fatalf("expected 1 content part, got %d", len(content))
	}

	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content part is not a map, got %T", content[0])
	}

	if part["type"] != "input_image" {
		t.Errorf("type = %q, want %q", part["type"], "input_image")
	}

	url, ok := part["image_url"].(string)
	if !ok {
		t.Errorf("image_url is %T, want string", part["image_url"])
	} else if url != "data:image/png;base64,abc123" {
		t.Errorf("image_url = %q, want %q", url, "data:image/png;base64,abc123")
	}

	if part["detail"] != "auto" {
		t.Errorf("detail = %v, want %q", part["detail"], "auto")
	}

	raw := string(rawItems[0])
	if strings.Contains(raw, `"image_url":{"url":`) {
		t.Error("image_url was serialized as an object (chat completions format), expected a plain string (responses API format)")
	}
	if !strings.Contains(raw, `"input_image"`) {
		t.Error("expected type 'input_image' not found in output")
	}
}

func TestMessagesToResponseInputItems_TextPartIsInputText(t *testing.T) {
	msgs := []ChatMessage{
		{
			Role: RoleUser,
			MultiContent: []MessagePart{
				{Type: PartTypeText, Text: "hello"},
			},
		},
	}

	input := messagesToResponseInputItems(msgs)
	rawItems := marshalInputItems(input)
	var parsed map[string]any
	json.Unmarshal(rawItems[0], &parsed)

	content, _ := parsed["content"].([]any)
	part, _ := content[0].(map[string]any)

	if part["type"] != "input_text" {
		t.Errorf("type = %v, want input_text", part["type"])
	}
	if part["text"] != "hello" {
		t.Errorf("text = %v, want hello", part["text"])
	}

	raw := string(rawItems[0])
	if strings.Contains(raw, `"type":"text"`) {
		t.Error("found chat completions type 'text', expected responses API type 'input_text'")
	}
}

func TestGogptToolsToResponseToolParams(t *testing.T) {
	tools := []Tool{
		{
			Type: "function",
			Function: &FunctionDefinition{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	result := toolsToResponseToolParams(tools)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	if result[0].OfFunction == nil {
		t.Fatal("expected OfFunction to be set")
	}
	if result[0].OfFunction.Name != "get_weather" {
		t.Errorf("name = %q, want %q", result[0].OfFunction.Name, "get_weather")
	}
}

func TestParseSDKResponseOutput(t *testing.T) {
	responseJSON := `{
		"id": "resp_123",
		"output": [
			{
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "output_text", "text": "Hello!"}
				]
			},
			{
				"type": "reasoning",
				"summary": [
					{"type": "summary_text", "text": "I thought about it"}
				]
			},
			{
				"type": "function_call",
				"call_id": "call_abc",
				"name": "get_weather",
				"arguments": "{\"city\":\"NYC\"}"
			}
		],
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"total_tokens": 150,
			"input_tokens_details": {"cached_tokens": 20},
			"output_tokens_details": {"reasoning_tokens": 10}
		}
	}`

	var resp responses.Response
	if err := json.Unmarshal([]byte(responseJSON), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	text, reasoning, toolCalls := parseSDKResponseOutput(resp)
	if text != "Hello!" {
		t.Errorf("text = %q, want %q", text, "Hello!")
	}
	if reasoning != "I thought about it" {
		t.Errorf("reasoning = %q, want %q", reasoning, "I thought about it")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("toolCalls = %d, want 1", len(toolCalls))
	}
	if toolCalls[0].ID != "call_abc" {
		t.Errorf("toolCall.ID = %q, want %q", toolCalls[0].ID, "call_abc")
	}
	if toolCalls[0].Function.Name != "get_weather" {
		t.Errorf("toolCall.Name = %q, want %q", toolCalls[0].Function.Name, "get_weather")
	}
}

func TestSDKResponseUsageToGogpt(t *testing.T) {
	usageJSON := `{
		"input_tokens": 100,
		"output_tokens": 50,
		"total_tokens": 150,
		"input_tokens_details": {"cached_tokens": 20},
		"output_tokens_details": {"reasoning_tokens": 10}
	}`

	var sdkUsage responses.ResponseUsage
	if err := json.Unmarshal([]byte(usageJSON), &sdkUsage); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	usage := sdkResponseUsageToUsage(sdkUsage, "completed")
	if usage.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100", usage.PromptTokens)
	}
	if usage.CompletionTokens != 50 {
		t.Errorf("CompletionTokens = %d, want 50", usage.CompletionTokens)
	}
	if usage.PromptTokensDetails.CachedTokens != 20 {
		t.Errorf("CachedTokens = %d, want 20", usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails.ReasoningTokens != 10 {
		t.Errorf("ReasoningTokens = %d, want 10", usage.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestBuildResponseParams(t *testing.T) {
	cfg := AIConfig{
		Model:               "gpt-4o",
		MaxCompletionTokens: 1024,
		Temperature:         0.7,
		TopP:                0.9,
		ReasoningEffort:     "medium",
		PreviousResponseID:  true,
	}
	input := []responses.ResponseInputItemUnionParam{
		responses.ResponseInputItemParamOfMessage("hello", responses.EasyInputMessageRoleUser),
	}

	params := buildResponseParams(cfg, input, nil, "resp_prev")
	if params.Model != "gpt-4o" {
		t.Errorf("Model = %q, want %q", params.Model, "gpt-4o")
	}
	if params.PreviousResponseID != openai.String("resp_prev") {
		t.Error("PreviousResponseID not set correctly")
	}
}

func TestIsResponseIDError(t *testing.T) {
	if !isResponseIDError(fmt.Errorf(`"code":"response_not_found"`)) {
		t.Error("should match response_not_found")
	}
	if !isResponseIDError(fmt.Errorf(`"code":"invalid_previous_response_id"`)) {
		t.Error("should match invalid_previous_response_id")
	}
	if isResponseIDError(fmt.Errorf("rate limit exceeded")) {
		t.Error("should not match generic error")
	}
	if isResponseIDError(nil) {
		t.Error("nil should not match")
	}
}
