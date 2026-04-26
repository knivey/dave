package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	gogpt "github.com/sashabaranov/go-openai"
)

type ResponsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input"`
	Instructions       string              `json:"instructions,omitempty"`
	Tools              []ResponseTool      `json:"tools,omitempty"`
	ToolChoice         any                 `json:"tool_choice,omitempty"`
	Store              *bool               `json:"store,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	Temperature        float32             `json:"temperature,omitempty"`
	TopP               float32             `json:"top_p,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
	Include            []string            `json:"include,omitempty"`
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	ServiceTier        string              `json:"service_tier,omitempty"`
	Verbosity          string              `json:"verbosity,omitempty"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls,omitempty"`
}

type ResponsesReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type ResponseTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name,omitempty"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Strict      *bool          `json:"strict,omitempty"`
}

type ResponsesResponse struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	CreatedAt         int64           `json:"created_at"`
	Model             string          `json:"model"`
	Status            string          `json:"status"`
	Output            json.RawMessage `json:"output"`
	OutputText        string          `json:"output_text,omitempty"`
	Usage             *ResponsesUsage `json:"usage,omitempty"`
	IncompleteDetails json.RawMessage `json:"incomplete_details,omitempty"`
}

type ResponsesUsage struct {
	InputTokens         int                    `json:"input_tokens"`
	OutputTokens        int                    `json:"output_tokens"`
	TotalTokens         int                    `json:"total_tokens"`
	InputTokensDetails  *ResponsesTokenDetails `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesTokenDetails `json:"output_tokens_details,omitempty"`
}

type ResponsesTokenDetails struct {
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

type ResponseOutputItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id,omitempty"`
	Status           string          `json:"status,omitempty"`
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        string          `json:"arguments,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
}

type ResponseContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type ResponseStreamEvent struct {
	Type         string          `json:"type"`
	Delta        string          `json:"delta,omitempty"`
	OutputIndex  int             `json:"output_index,omitempty"`
	ContentIndex int             `json:"content_index,omitempty"`
	Item         json.RawMessage `json:"item,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
	Arguments    string          `json:"arguments,omitempty"`
	Text         string          `json:"text,omitempty"`
}

func messagesToResponsesInput(messages []gogpt.ChatCompletionMessage) []json.RawMessage {
	input := make([]json.RawMessage, 0, len(messages)*2)
	for _, msg := range messages {
		switch msg.Role {
		case gogpt.ChatMessageRoleSystem:
			b, _ := json.Marshal(map[string]any{
				"role":    "system",
				"content": msg.Content,
			})
			input = append(input, b)

		case gogpt.ChatMessageRoleUser:
			if len(msg.MultiContent) > 0 {
				content := make([]map[string]any, 0, len(msg.MultiContent))
				for _, part := range msg.MultiContent {
					switch part.Type {
					case gogpt.ChatMessagePartTypeText:
						content = append(content, map[string]any{
							"type": "input_text",
							"text": part.Text,
						})
					case gogpt.ChatMessagePartTypeImageURL:
						entry := map[string]any{
							"type":      "input_image",
							"image_url": part.ImageURL.URL,
						}
						if part.ImageURL.Detail != "" {
							entry["detail"] = string(part.ImageURL.Detail)
						}
						content = append(content, entry)
					}
				}
				b, _ := json.Marshal(map[string]any{
					"role":    "user",
					"content": content,
				})
				input = append(input, b)
			} else {
				b, _ := json.Marshal(map[string]any{
					"role":    "user",
					"content": msg.Content,
				})
				input = append(input, b)
			}

		case gogpt.ChatMessageRoleAssistant:
			if len(msg.ToolCalls) > 0 {
				if msg.Content != "" {
					b, _ := json.Marshal(map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []map[string]any{
							{"type": "output_text", "text": msg.Content},
						},
					})
					input = append(input, b)
				}
				for _, tc := range msg.ToolCalls {
					b, _ := json.Marshal(map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
					input = append(input, b)
				}
			} else {
				b, _ := json.Marshal(map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": msg.Content},
					},
				})
				input = append(input, b)
			}

		case gogpt.ChatMessageRoleTool:
			b, _ := json.Marshal(map[string]any{
				"type":    "function_call_output",
				"call_id": msg.ToolCallID,
				"output":  msg.Content,
			})
			input = append(input, b)
		}
	}
	return input
}

func toolResultMsgsToInput(messages []gogpt.ChatCompletionMessage) []json.RawMessage {
	input := make([]json.RawMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == gogpt.ChatMessageRoleTool {
			b, _ := json.Marshal(map[string]any{
				"type":    "function_call_output",
				"call_id": msg.ToolCallID,
				"output":  msg.Content,
			})
			input = append(input, b)
		}
	}
	return input
}

func gogptToolsToResponseTools(tools []gogpt.Tool) []ResponseTool {
	result := make([]ResponseTool, 0, len(tools))
	for _, t := range tools {
		if t.Function != nil {
			var params map[string]any
			if t.Function.Parameters != nil {
				if p, ok := t.Function.Parameters.(map[string]any); ok {
					params = p
				}
			}
			result = append(result, ResponseTool{
				Type:        "function",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  params,
			})
		}
	}
	return result
}

func parseResponseOutput(raw json.RawMessage) (text string, reasoning string, toolCalls []gogpt.ToolCall) {
	if len(raw) == 0 {
		return "", "", nil
	}
	var items []ResponseOutputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", "", nil
	}
	for _, item := range items {
		switch item.Type {
		case "message":
			if item.Role == "assistant" && len(item.Content) > 0 {
				var parts []ResponseContentPart
				json.Unmarshal(item.Content, &parts)
				for _, p := range parts {
					if p.Type == "output_text" {
						text += p.Text
					}
				}
			}
		case "reasoning":
			if len(item.Summary) > 0 {
				var summaries []map[string]string
				json.Unmarshal(item.Summary, &summaries)
				for _, s := range summaries {
					if t, ok := s["text"]; ok {
						reasoning += t
					}
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, gogpt.ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: gogpt.FunctionCall{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		}
	}
	return text, reasoning, toolCalls
}

func buildResponsesRequest(cfg AIConfig, input []json.RawMessage, tools []ResponseTool, previousResponseID string) ResponsesRequest {
	inputJSON, _ := json.Marshal(input)
	req := ResponsesRequest{
		Model:              cfg.Model,
		Input:              inputJSON,
		MaxOutputTokens:    cfg.MaxCompletionTokens,
		Temperature:        cfg.Temperature,
		TopP:               cfg.TopP,
		Tools:              tools,
		ServiceTier:        cfg.ServiceTier,
		Verbosity:          cfg.Verbosity,
		PreviousResponseID: previousResponseID,
	}
	if req.MaxOutputTokens == 0 && cfg.MaxTokens > 0 {
		req.MaxOutputTokens = cfg.MaxTokens
	}
	if cfg.ReasoningEffort != "" {
		req.Reasoning = &ResponsesReasoning{Effort: cfg.ReasoningEffort}
	}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
		if cfg.ParallelToolCalls != nil {
			req.ParallelToolCalls = cfg.ParallelToolCalls
		}
	}
	return req
}

func callResponsesAPI(ctx context.Context, client *http.Client, baseURL, apiKey string, req ResponsesRequest) (*ResponsesResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling responses request: %w", err)
	}
	apiURL := strings.TrimRight(baseURL, "/") + "/responses"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating responses request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("responses API call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("responses API error %d: %s", resp.StatusCode, string(respBody))
	}
	var result ResponsesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding responses response: %w", err)
	}
	return &result, nil
}

type responsesStreamReader struct {
	scanner *bufio.Scanner
}

func newResponsesStreamReader(body io.Reader) *responsesStreamReader {
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &responsesStreamReader{scanner: s}
}

func (r *responsesStreamReader) recv() (ResponseStreamEvent, error) {
	var eventType string
	var data string
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				return ResponseStreamEvent{}, io.EOF
			}
			var event ResponseStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				data = ""
				eventType = ""
				continue
			}
			if eventType != "" && event.Type == "" {
				event.Type = eventType
			}
			return event, nil
		}
		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	return ResponseStreamEvent{}, io.EOF
}

func responsesUsageToGogpt(u *ResponsesUsage) *gogpt.Usage {
	if u == nil {
		return nil
	}
	usage := &gogpt.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &gogpt.PromptTokensDetails{
			CachedTokens: u.InputTokensDetails.CachedTokens,
		}
	}
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens > 0 {
		usage.CompletionTokensDetails = &gogpt.CompletionTokensDetails{
			ReasoningTokens: u.OutputTokensDetails.ReasoningTokens,
		}
	}
	return usage
}

func isResponseIDError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, `"code":"response_not_found"`) ||
		strings.Contains(s, `"code":"invalid_previous_response_id"`) ||
		strings.Contains(s, "previous_response_id") && strings.Contains(s, "not found")
}
