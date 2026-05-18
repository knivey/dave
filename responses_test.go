package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
					"content": []any{
						map[string]any{"type": "input_text", "text": "describe this"},
						map[string]any{"type": "input_image", "image_url": "data:image/png;base64,abc123", "detail": "auto"},
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
					"content": []any{
						map[string]any{"type": "input_image", "image_url": "https://example.com/img.png"},
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
			require.Len(t, result, len(tt.wantParts), "input items count")
			rawItems := marshalInputItems(result)
			for i, raw := range rawItems {
				var got map[string]any
				require.NoError(t, json.Unmarshal(raw, &got), "item %d: unmarshal", i)
				want := tt.wantParts[i]
				assert.Equal(t, want, got, "item %d mismatch", i)
			}
		})
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
	require.Len(t, input, 1, "input items")

	rawItems := marshalInputItems(input)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(rawItems[0], &parsed), "unmarshal")

	content, ok := parsed["content"].([]any)
	require.True(t, ok, "content is not a slice, got %T", parsed["content"])
	require.Len(t, content, 1, "content parts")

	part, ok := content[0].(map[string]any)
	require.True(t, ok, "content part is not a map, got %T", content[0])

	assert.Equal(t, "input_image", part["type"], "type")

	url, ok := part["image_url"].(string)
	assert.True(t, ok, "image_url is %T, want string", part["image_url"])
	assert.Equal(t, "data:image/png;base64,abc123", url, "image_url")

	assert.Equal(t, "auto", part["detail"], "detail")

	raw := string(rawItems[0])
	assert.False(t, strings.Contains(raw, `"image_url":{"url":`), "image_url was serialized as an object (chat completions format), expected a plain string (responses API format)")
	assert.True(t, strings.Contains(raw, `"input_image"`), "expected type 'input_image' not found in output")
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

	assert.Equal(t, "input_text", part["type"], "type")
	assert.Equal(t, "hello", part["text"], "text")

	raw := string(rawItems[0])
	assert.False(t, strings.Contains(raw, `"type":"text"`), "found chat completions type 'text', expected responses API type 'input_text'")
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
	require.Len(t, result, 1, "tools")
	require.NotNil(t, result[0].OfFunction, "OfFunction")
	assert.Equal(t, "get_weather", result[0].OfFunction.Name, "name")
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
	require.NoError(t, json.Unmarshal([]byte(responseJSON), &resp), "unmarshal")

	text, reasoning, encryptedReasoning, toolCalls := parseSDKResponseOutput(resp)
	assert.Equal(t, "Hello!", text, "text")
	assert.Equal(t, "I thought about it", reasoning, "reasoning")
	assert.Equal(t, "", encryptedReasoning, "encryptedReasoning should be empty when not present")
	require.Len(t, toolCalls, 1, "toolCalls")
	assert.Equal(t, "call_abc", toolCalls[0].ID, "toolCall.ID")
	assert.Equal(t, "get_weather", toolCalls[0].Function.Name, "toolCall.Name")
}

func TestParseSDKResponseOutput_EncryptedReasoning(t *testing.T) {
	responseJSON := `{
		"id": "resp_456",
		"output": [
			{
				"type": "reasoning",
				"summary": [{"type": "summary_text", "text": "thinking"}],
				"encrypted_content": "enc_blob_xyz"
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Answer"}]
			}
		]
	}`

	var resp responses.Response
	require.NoError(t, json.Unmarshal([]byte(responseJSON), &resp))

	text, reasoning, encryptedReasoning, toolCalls := parseSDKResponseOutput(resp)
	assert.Equal(t, "Answer", text)
	assert.Equal(t, "thinking", reasoning)
	assert.Equal(t, "enc_blob_xyz", encryptedReasoning)
	assert.Empty(t, toolCalls)
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
	require.NoError(t, json.Unmarshal([]byte(usageJSON), &sdkUsage), "unmarshal")

	usage := sdkResponseUsageToUsage(sdkUsage, "completed")
	assert.Equal(t, int64(100), usage.PromptTokens, "PromptTokens")
	assert.Equal(t, int64(50), usage.CompletionTokens, "CompletionTokens")
	assert.Equal(t, int64(20), usage.PromptTokensDetails.CachedTokens, "CachedTokens")
	assert.Equal(t, int64(10), usage.CompletionTokensDetails.ReasoningTokens, "ReasoningTokens")
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

	params := buildResponseParams(cfg, input, nil, "resp_prev", "testuser")
	assert.Equal(t, "gpt-4o", params.Model, "Model")
	assert.Equal(t, openai.String("resp_prev"), params.PreviousResponseID, "PreviousResponseID")
}

func TestBuildResponseParams_IncludeEncryptedReasoning(t *testing.T) {
	cfg := AIConfig{Model: "test-model", EncryptedReasoning: true}
	params := buildResponseParams(cfg, nil, nil, "", "")
	assert.Contains(t, params.Include, responses.ResponseIncludableReasoningEncryptedContent,
		"buildResponseParams should include reasoning.encrypted_content when enabled")
}

func TestBuildResponseParams_NoIncludeWhenDisabled(t *testing.T) {
	cfg := AIConfig{Model: "test-model"}
	params := buildResponseParams(cfg, nil, nil, "", "")
	assert.Empty(t, params.Include,
		"buildResponseParams should not include reasoning.encrypted_content when disabled")
}

func TestMessagesToResponseInputItems_EncryptedReasoning(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi", EncryptedReasoning: "enc_blob_123"},
	}
	input := messagesToResponseInputItems(messages)

	found := false
	for _, item := range input {
		if item.OfReasoning != nil {
			found = true
			assert.Equal(t, param.NewOpt("enc_blob_123"), item.OfReasoning.EncryptedContent)
		}
	}
	assert.True(t, found, "expected a reasoning input item for assistant message with encrypted reasoning")
}

func TestMessagesToResponseInputItems_NoReasoningWhenEmpty(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	}
	input := messagesToResponseInputItems(messages)

	for _, item := range input {
		assert.Nil(t, item.OfReasoning, "should not emit reasoning item when EncryptedReasoning is empty")
	}
}

func newAPIError(statusCode int, code string, message string) *openai.Error {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: statusCode, Status: http.StatusText(statusCode)}
	return &openai.Error{
		StatusCode: statusCode,
		Code:       code,
		Message:    message,
		Request:    req,
		Response:   resp,
	}
}

func TestIsResponseIDError(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		match bool
	}{
		{"nil", nil, false},
		{"generic error", fmt.Errorf("rate limit exceeded"), false},
		{"openai.Error 404", newAPIError(http.StatusNotFound, "", "Response with id=abc not found"), true},
		{"openai.Error 404 with code", newAPIError(http.StatusNotFound, "response_not_found", "not found"), true},
		{"openai.Error code response_not_found", newAPIError(http.StatusBadRequest, "response_not_found", "response not found"), true},
		{"openai.Error code invalid_previous_response_id", newAPIError(http.StatusBadRequest, "invalid_previous_response_id", "bad id"), true},
		{"openai.Error 400 empty content", newAPIError(http.StatusBadRequest, "", "Each message must have at least one content element."), true},
		{"openai.Error 400 other", newAPIError(http.StatusBadRequest, "invalid_request", "something else"), false},
		{"openai.Error 401", newAPIError(http.StatusUnauthorized, "invalid_api_key", "bad key"), false},
		{"openai.Error 429", newAPIError(http.StatusTooManyRequests, "rate_limit_exceeded", "slow down"), false},
		{"wrapped openai.Error 404", fmt.Errorf("wrapped: %w", newAPIError(http.StatusNotFound, "", "Response with id=abc not found")), true},
		{"string fallback response_not_found", fmt.Errorf(`"code":"response_not_found"`), true},
		{"string fallback invalid_previous_response_id", fmt.Errorf(`"code":"invalid_previous_response_id"`), true},
		{"string fallback previous_response_id not found", fmt.Errorf("previous_response_id abc not found"), true},
		{"string fallback empty content", fmt.Errorf("Invalid request content: Each message must have at least one content element."), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				assert.False(t, isResponseIDError(nil))
				return
			}
			result := isResponseIDError(tt.err)
			assert.Equal(t, tt.match, result)
			if tt.match {
				assert.True(t, errors.Is(tt.err, tt.err), "error should support errors.As chain")
			}
		})
	}
}
