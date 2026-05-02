package main

import (
	"encoding/json"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

func messagesToChatCompletionParams(messages []ChatMessage) []openai.ChatCompletionMessageParamUnion {
	params := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			params = append(params, openai.SystemMessage(msg.Content))

		case RoleUser:
			if len(msg.MultiContent) > 0 {
				parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(msg.MultiContent))
				for _, part := range msg.MultiContent {
					switch part.Type {
					case PartTypeText:
						parts = append(parts, openai.TextContentPart(part.Text))
					case PartTypeImageURL:
						imgParam := openai.ChatCompletionContentPartImageImageURLParam{
							URL: part.ImageURL.URL,
						}
						if part.ImageURL.Detail != "" {
							imgParam.Detail = part.ImageURL.Detail
						}
						parts = append(parts, openai.ImageContentPart(imgParam))
					}
				}
				params = append(params, openai.UserMessage(parts))
			} else {
				params = append(params, openai.UserMessage(msg.Content))
			}

		case RoleAssistant:
			if len(msg.ToolCalls) > 0 {
				tcParams := make([]openai.ChatCompletionMessageToolCallUnionParam, 0, len(msg.ToolCalls))
				for _, tc := range msg.ToolCalls {
					tcParams = append(tcParams, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID: tc.ID,
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Arguments: tc.Function.Arguments,
								Name:      tc.Function.Name,
							},
						},
					})
				}
				param := openai.ChatCompletionMessageParamOfAssistant(msg.Content)
				param.OfAssistant.ToolCalls = tcParams
				params = append(params, param)
			} else {
				params = append(params, openai.AssistantMessage(msg.Content))
			}

		case RoleTool:
			params = append(params, openai.ToolMessage(msg.Content, msg.ToolCallID))
		}
	}
	return params
}

func toolsToChatCompletionToolParams(tools []Tool) []openai.ChatCompletionToolUnionParam {
	result := make([]openai.ChatCompletionToolUnionParam, 0, len(tools))
	for _, t := range tools {
		if t.Function != nil {
			var fnParams shared.FunctionParameters
			if p, ok := t.Function.Parameters.(map[string]any); ok {
				fnParams = p
			}
			if fnParams == nil {
				fnParams = shared.FunctionParameters{"type": "object"}
			}
			result = append(result, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        t.Function.Name,
				Description: openai.String(t.Function.Description),
				Parameters:  fnParams,
			}))
		}
	}
	return result
}

func buildChatCompletionParams(cfg AIConfig, messages []ChatMessage, tools []Tool) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model:    cfg.Model,
		Messages: messagesToChatCompletionParams(messages),
	}
	if cfg.MaxCompletionTokens > 0 {
		params.MaxCompletionTokens = openai.Int(int64(cfg.MaxCompletionTokens))
	}
	if cfg.MaxTokens > 0 {
		params.MaxTokens = openai.Int(int64(cfg.MaxTokens))
	}
	if cfg.Temperature > 0 {
		params.Temperature = openai.Float(float64(cfg.Temperature))
	}
	if cfg.TopP > 0 {
		params.TopP = openai.Float(float64(cfg.TopP))
	}
	if len(cfg.Stop) > 0 {
		params.Stop = openai.ChatCompletionNewParamsStopUnion{
			OfStringArray: cfg.Stop,
		}
	}
	if cfg.PresencePenalty > 0 {
		params.PresencePenalty = openai.Float(float64(cfg.PresencePenalty))
	}
	if cfg.FrequencyPenalty > 0 {
		params.FrequencyPenalty = openai.Float(float64(cfg.FrequencyPenalty))
	}
	if cfg.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(cfg.ReasoningEffort)
	}
	if cfg.ServiceTier != "" {
		params.ServiceTier = openai.ChatCompletionNewParamsServiceTier(cfg.ServiceTier)
	}
	if cfg.Streaming {
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		}
	}
	if len(tools) > 0 {
		params.Tools = toolsToChatCompletionToolParams(tools)
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.Opt("auto"),
		}
		if cfg.ParallelToolCalls != nil {
			params.ParallelToolCalls = openai.Bool(*cfg.ParallelToolCalls)
		}
	}
	return params
}

func parseChatCompletionResponse(resp openai.ChatCompletion) (string, string, []ToolCall, *Usage) {
	if len(resp.Choices) == 0 {
		return "", "", nil, nil
	}
	choice := resp.Choices[0]
	msg := choice.Message

	var toolCalls []ToolCall
	for _, tc := range msg.ToolCalls {
		toolCalls = append(toolCalls, ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: FunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}

	var usage *Usage
	usage = sdkChatUsageToUsage(resp.Usage)
	if usage != nil {
		usage.FinishReason = string(choice.FinishReason)
	}

	reasoning := ""
	if raw, ok := msg.JSON.ExtraFields["reasoning_content"]; ok && raw.Valid() {
		json.Unmarshal([]byte(raw.Raw()), &reasoning)
	}

	return msg.Content, reasoning, toolCalls, usage
}

func sdkChatUsageToUsage(u openai.CompletionUsage) *Usage {
	usage := &Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.PromptTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens: u.PromptTokensDetails.CachedTokens,
		}
	}
	if u.CompletionTokensDetails.ReasoningTokens > 0 {
		usage.CompletionTokensDetails = &CompletionTokensDetails{
			ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return usage
}
