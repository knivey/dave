package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
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

	text, reasoning, toolCalls := parseSDKResponseOutput(resp)
	assert.Equal(t, "Hello!", text, "text")
	assert.Equal(t, "I thought about it", reasoning, "reasoning")
	require.Len(t, toolCalls, 1, "toolCalls")
	assert.Equal(t, "call_abc", toolCalls[0].ID, "toolCall.ID")
	assert.Equal(t, "get_weather", toolCalls[0].Function.Name, "toolCall.Name")
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
		ReasoningSummary:    "auto",
		PreviousResponseID:  true,
	}
	input := []responses.ResponseInputItemUnionParam{
		responses.ResponseInputItemParamOfMessage("hello", responses.EasyInputMessageRoleUser),
	}

	params := buildResponseParams(cfg, input, nil, "resp_prev", apiIdentity{User: "testuser"})
	assert.Equal(t, "gpt-4o", params.Model, "Model")
	assert.Equal(t, openai.String("resp_prev"), params.PreviousResponseID, "PreviousResponseID")
	assert.Equal(t, shared.ReasoningEffort("medium"), params.Reasoning.Effort, "Reasoning.Effort")
	assert.Equal(t, shared.ReasoningSummary("auto"), params.Reasoning.Summary, "Reasoning.Summary")
}

func TestBuildResponseParams_ReasoningSummaryOnly(t *testing.T) {
	cfg := AIConfig{Model: "test-model", ReasoningSummary: "detailed"}
	params := buildResponseParams(cfg, nil, nil, "", apiIdentity{})
	assert.Equal(t, shared.ReasoningSummary("detailed"), params.Reasoning.Summary, "Reasoning.Summary")
	assert.Empty(t, params.Reasoning.Effort, "effort should stay unset when only summary is configured")
}

func TestBuildResponseParams_NoReasoningWhenUnset(t *testing.T) {
	cfg := AIConfig{Model: "test-model"}
	params := buildResponseParams(cfg, nil, nil, "", apiIdentity{})
	assert.Empty(t, params.Reasoning.Effort, "Reasoning.Effort")
	assert.Empty(t, params.Reasoning.Summary, "Reasoning.Summary")
}

func TestBuildResponseParams_ReasoningWireJSON(t *testing.T) {
	t.Run("summary only omits empty effort on the wire", func(t *testing.T) {
		cfg := AIConfig{Model: "test-model", ReasoningSummary: "auto"}
		params := buildResponseParams(cfg, nil, nil, "", apiIdentity{})
		raw, err := json.Marshal(params)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"summary":"auto"`, "wire body should request reasoning summaries")
		assert.NotContains(t, string(raw), `"effort"`, "empty effort must be omitted, not sent as \"\"")
	})

	t.Run("effort and summary both serialize", func(t *testing.T) {
		cfg := AIConfig{Model: "test-model", ReasoningEffort: "low", ReasoningSummary: "detailed"}
		params := buildResponseParams(cfg, nil, nil, "", apiIdentity{})
		raw, err := json.Marshal(params)
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"effort":"low"`)
		assert.Contains(t, string(raw), `"summary":"detailed"`)
	})

	t.Run("unset reasoning sends no reasoning object", func(t *testing.T) {
		cfg := AIConfig{Model: "test-model"}
		params := buildResponseParams(cfg, nil, nil, "", apiIdentity{})
		raw, err := json.Marshal(params)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), `"reasoning"`)
	})
}

func TestBuildResponseParamsIdentityFields(t *testing.T) {
	cfg := AIConfig{Model: "gpt-4o"}

	params := buildResponseParams(cfg, nil, nil, "", apiIdentity{User: "legacy-user"})
	assert.Equal(t, openai.String("legacy-user"), params.User, "User")
	assert.False(t, params.SafetyIdentifier.Valid(), "SafetyIdentifier should be omitted")
	assert.False(t, params.PromptCacheKey.Valid(), "PromptCacheKey should be omitted")

	params = buildResponseParams(cfg, nil, nil, "", apiIdentity{SafetyID: "safety-id", CacheKey: "cache-key"})
	assert.Equal(t, openai.String("safety-id"), params.SafetyIdentifier, "SafetyIdentifier")
	assert.Equal(t, openai.String("cache-key"), params.PromptCacheKey, "PromptCacheKey")
	assert.False(t, params.User.Valid(), "User should be omitted")

	params = buildResponseParams(cfg, nil, nil, "", apiIdentity{})
	assert.False(t, params.User.Valid(), "empty identity: User should be omitted")
	assert.False(t, params.SafetyIdentifier.Valid(), "empty identity: SafetyIdentifier should be omitted")
	assert.False(t, params.PromptCacheKey.Valid(), "empty identity: PromptCacheKey should be omitted")
}

// TestBuildResponseParamsIdentityWireJSON pins the serialized request body
// against SDK upgrades: identity fields must appear under their wire names
// and unset fields must be absent entirely (omitzero), not sent empty.
func TestBuildResponseParamsIdentityWireJSON(t *testing.T) {
	cfg := AIConfig{Model: "gpt-4o"}

	marshal := func(t *testing.T, ident apiIdentity) map[string]any {
		t.Helper()
		body, err := json.Marshal(buildResponseParams(cfg, nil, nil, "", ident))
		require.NoError(t, err)
		var wire map[string]any
		require.NoError(t, json.Unmarshal(body, &wire))
		return wire
	}

	wire := marshal(t, apiIdentity{SafetyID: "safety-id", CacheKey: "cache-key"})
	assert.Equal(t, "safety-id", wire["safety_identifier"], "safety_identifier wire name")
	assert.Equal(t, "cache-key", wire["prompt_cache_key"], "prompt_cache_key wire name")
	assert.NotContains(t, wire, "user", "legacy user field must stay absent")

	wire = marshal(t, apiIdentity{User: "legacy-user"})
	assert.Equal(t, "legacy-user", wire["user"], "user wire name")
	assert.NotContains(t, wire, "safety_identifier", "safety_identifier must stay absent")
	assert.NotContains(t, wire, "prompt_cache_key", "prompt_cache_key must stay absent")
}

func TestBuildResponseParams_NoIncludeWhenDisabled(t *testing.T) {
	cfg := AIConfig{Model: "test-model"}
	params := buildResponseParams(cfg, nil, nil, "", apiIdentity{})
	assert.Empty(t, params.Include,
		"buildResponseParams should not populate Include")
}

func TestMessagesToResponseInputItems_NoReasoningItems(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleAssistant, Content: "hi"},
	}
	input := messagesToResponseInputItems(messages)

	for _, item := range input {
		assert.Nil(t, item.OfReasoning, "should not emit reasoning items")
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
		{"openai.Error 400 reasoning items mismatch", newAPIError(http.StatusBadRequest, "", "Reasoning input items can only be provided to a reasoning or computer use model. Remove reasoning items from your input and try again."), true},
		{"openai.Error 400 openrouter proxy reject", newAPIError(http.StatusBadRequest, "invalid_prompt", "previous_response_id is not supported on this proxy. Each response request is independent."), true},
		{"openai.Error 400 invalid_prompt unrelated", newAPIError(http.StatusBadRequest, "invalid_prompt", "input messages are malformed"), false},
		{"openai.Error 400 unrelated reasoning mention", newAPIError(http.StatusBadRequest, "invalid_request", "reasoning is not enabled for this model"), false},
		{"openai.Error 400 other", newAPIError(http.StatusBadRequest, "invalid_request", "something else"), false},
		{"openai.Error 401", newAPIError(http.StatusUnauthorized, "invalid_api_key", "bad key"), false},
		{"openai.Error 429", newAPIError(http.StatusTooManyRequests, "rate_limit_exceeded", "slow down"), false},
		{"wrapped openai.Error 404", fmt.Errorf("wrapped: %w", newAPIError(http.StatusNotFound, "", "Response with id=abc not found")), true},
		{"string fallback response_not_found", fmt.Errorf(`"code":"response_not_found"`), true},
		{"string fallback invalid_previous_response_id", fmt.Errorf(`"code":"invalid_previous_response_id"`), true},
		{"string fallback previous_response_id not found", fmt.Errorf("previous_response_id abc not found"), true},
		{"string fallback openrouter proxy reject", fmt.Errorf(`POST "https://openrouter.ai/api/v1/responses": 400 Bad Request {"code":"invalid_prompt","message":"previous_response_id is not supported on this proxy. Each response request is independent."}`), true},
		{"string fallback previous_response_id unrelated", fmt.Errorf("the previous_response_id field will be supported soon"), false},
		{"string fallback empty content", fmt.Errorf("Invalid request content: Each message must have at least one content element."), true},
		{"string fallback reasoning items mismatch", fmt.Errorf("Error code: 400 - {'error': {'message': 'Reasoning input items can only be provided to a reasoning or computer use model. Remove reasoning items from your input and try again.', 'type': 'invalid_request_error'}}"), true},
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

// TestIsResponseIDErrorSDKErrorShapes drives the REAL openai-go error
// machinery (requestconfig builds *openai.Error from the wire body) against
// both provider body shapes for the OpenRouter stateless-proxy rejection,
// pining the Layer 2 net to what the SDK actually surfaces:
//
//   - WRAPPED body ({"error":{code,message}} — production OpenRouter, per
//     the 2026-10-09 incident's captured response body): the SDK parses
//     Code/Message, the structured invalid_prompt case matches → retry.
//   - UNWRAPPED body (top-level {code,message}): the SDK extracts
//     gjson(body,"error"), gets "", and UnmarshalJSON("") leaves Code,
//     Message AND the embedded raw body EMPTY — the error string carries
//     no wording at all, so the shape is unmatchable at this layer. This
//     test pins that limitation honestly instead of pretending a string
//     fallback covers it; no known provider sends the unwrapped shape.
func TestIsResponseIDErrorSDKErrorShapes(t *testing.T) {
	const openRouterReject = "previous_response_id is not supported on this proxy. Each response request is independent."

	makeServerError := func(t *testing.T, body string) error {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(body))
		}))
		defer server.Close()

		client := openai.NewClient(
			option.WithAPIKey("test-key"),
			option.WithBaseURL(server.URL),
		)
		_, err := client.Models.List(context.Background())
		require.Error(t, err)
		var apiErr *openai.Error
		require.ErrorAs(t, err, &apiErr, "SDK must surface non-2xx as *openai.Error")
		return err
	}

	t.Run("wrapped body matches", func(t *testing.T) {
		err := makeServerError(t, fmt.Sprintf(`{"error":{"code":"invalid_prompt","message":%q}}`, openRouterReject))
		var apiErr *openai.Error
		require.True(t, errors.As(err, &apiErr))
		assert.Equal(t, "invalid_prompt", apiErr.Code, "wrapped body parses into Code")
		assert.Contains(t, apiErr.Message, "previous_response_id")
		assert.True(t, isResponseIDError(err), "production OpenRouter shape must trigger the retry-without-id net")
	})

	t.Run("unwrapped body is unmatchable", func(t *testing.T) {
		err := makeServerError(t, fmt.Sprintf(`{"code":"invalid_prompt","message":%q}`, openRouterReject))
		assert.False(t, isResponseIDError(err), "SDK drops unwrapped bodies; documented limitation")
		var apiErr *openai.Error
		errors.As(err, &apiErr)
		assert.Empty(t, apiErr.Message, "unwrapped body leaves Message empty")
		assert.NotContains(t, err.Error(), "previous_response_id", "unwrapped body never reaches the error string")
	})
}
