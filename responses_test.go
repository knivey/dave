package main

import (
	"encoding/json"
	"strings"
	"testing"

	gogpt "github.com/sashabaranov/go-openai"
)

func TestMessagesToResponsesInput(t *testing.T) {
	tests := []struct {
		name      string
		messages  []gogpt.ChatCompletionMessage
		wantParts []map[string]any
	}{
		{
			name: "plain text user message",
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
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
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleSystem, Content: "you are helpful"},
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
			messages: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeText, Text: "describe this"},
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL:    "data:image/png;base64,abc123",
							Detail: gogpt.ImageURLDetailAuto,
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
			messages: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
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
			messages: []gogpt.ChatCompletionMessage{
				{
					Role:    gogpt.ChatMessageRoleAssistant,
					Content: "let me check",
					ToolCalls: []gogpt.ToolCall{
						{
							ID:   "call_1",
							Type: "function",
							Function: gogpt.FunctionCall{
								Name:      "get_weather",
								Arguments: `{"city":"NYC"}`,
							},
						},
					},
				},
			},
			wantParts: []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "let me check"},
					},
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
			messages: []gogpt.ChatCompletionMessage{
				{
					Role:       gogpt.ChatMessageRoleTool,
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
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleAssistant, Content: "hello there"},
			},
			wantParts: []map[string]any{
				{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": "hello there"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := messagesToResponsesInput(tt.messages)
			if len(result) != len(tt.wantParts) {
				t.Fatalf("got %d input items, want %d", len(result), len(tt.wantParts))
			}
			for i, raw := range result {
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

func TestMessagesToResponsesInput_ImageURLIsString(t *testing.T) {
	msgs := []gogpt.ChatCompletionMessage{
		{
			Role: gogpt.ChatMessageRoleUser,
			MultiContent: []gogpt.ChatMessagePart{
				{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
					URL:    "data:image/png;base64,abc123",
					Detail: gogpt.ImageURLDetailAuto,
				}},
			},
		},
	}

	input := messagesToResponsesInput(msgs)
	if len(input) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(input))
	}

	var parsed map[string]any
	if err := json.Unmarshal(input[0], &parsed); err != nil {
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

	raw := string(input[0])
	if strings.Contains(raw, `"image_url":{"url":`) {
		t.Error("image_url was serialized as an object (chat completions format), expected a plain string (responses API format)")
	}
	if !strings.Contains(raw, `"input_image"`) {
		t.Error("expected type 'input_image' not found in output")
	}
	if !strings.Contains(raw, `"input_text"`) {
	}
}

func TestMessagesToResponsesInput_TextPartIsInputText(t *testing.T) {
	msgs := []gogpt.ChatCompletionMessage{
		{
			Role: gogpt.ChatMessageRoleUser,
			MultiContent: []gogpt.ChatMessagePart{
				{Type: gogpt.ChatMessagePartTypeText, Text: "hello"},
			},
		},
	}

	input := messagesToResponsesInput(msgs)
	var parsed map[string]any
	json.Unmarshal(input[0], &parsed)

	content, _ := parsed["content"].([]any)
	part, _ := content[0].(map[string]any)

	if part["type"] != "input_text" {
		t.Errorf("type = %v, want input_text", part["type"])
	}
	if part["text"] != "hello" {
		t.Errorf("text = %v, want hello", part["text"])
	}

	raw := string(input[0])
	if strings.Contains(raw, `"type":"text"`) {
		t.Error("found chat completions type 'text', expected responses API type 'input_text'")
	}
}
