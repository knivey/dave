package main

import (
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func messagesToResponseInputItems(messages []ChatMessage) []responses.ResponseInputItemUnionParam {
	input := make([]responses.ResponseInputItemUnionParam, 0, len(messages)*2)
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem:
			input = append(input, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleSystem,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: openai.String(msg.Content),
					},
				},
			})

		case RoleUser:
			if len(msg.MultiContent) > 0 {
				content := make(responses.ResponseInputMessageContentListParam, 0, len(msg.MultiContent))
				for _, part := range msg.MultiContent {
					switch part.Type {
					case PartTypeText:
						content = append(content, responses.ResponseInputContentParamOfInputText(part.Text))
					case PartTypeImageURL:
						imgParam := responses.ResponseInputImageParam{
							ImageURL: openai.String(part.ImageURL.URL),
						}
						if part.ImageURL.Detail != "" {
							imgParam.Detail = responses.ResponseInputImageDetail(part.ImageURL.Detail)
						}
						content = append(content, responses.ResponseInputContentUnionParam{
							OfInputImage: &imgParam,
						})
					}
				}
				input = append(input, responses.ResponseInputItemUnionParam{
					OfMessage: &responses.EasyInputMessageParam{
						Role:    responses.EasyInputMessageRoleUser,
						Content: responses.EasyInputMessageContentUnionParam{OfInputItemContentList: content},
					},
				})
			} else {
				input = append(input, responses.ResponseInputItemUnionParam{
					OfMessage: &responses.EasyInputMessageParam{
						Role:    responses.EasyInputMessageRoleUser,
						Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
					},
				})
			}

		case RoleAssistant:
			if len(msg.ToolCalls) > 0 {
				if msg.Content != "" {
					input = append(input, responses.ResponseInputItemUnionParam{
						OfMessage: &responses.EasyInputMessageParam{
							Role:    responses.EasyInputMessageRoleAssistant,
							Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
						},
					})
				}
				for _, tc := range msg.ToolCalls {
					input = append(input, responses.ResponseInputItemParamOfFunctionCall(
						tc.Function.Arguments, tc.ID, tc.Function.Name,
					))
				}
			} else {
				input = append(input, responses.ResponseInputItemUnionParam{
					OfMessage: &responses.EasyInputMessageParam{
						Role:    responses.EasyInputMessageRoleAssistant,
						Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
					},
				})
			}

		case RoleTool:
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(
				msg.ToolCallID, msg.Content,
			))
		}
	}
	return input
}

func toolResultMsgsToInputItems(messages []ChatMessage) []responses.ResponseInputItemUnionParam {
	input := make([]responses.ResponseInputItemUnionParam, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == RoleTool {
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(
				msg.ToolCallID, msg.Content,
			))
		}
	}
	return input
}

func toolsToResponseToolParams(tools []Tool) []responses.ToolUnionParam {
	result := make([]responses.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		if t.Function != nil {
			var params map[string]any
			if t.Function.Parameters != nil {
				if p, ok := t.Function.Parameters.(map[string]any); ok {
					params = p
				}
			}
			if params == nil {
				params = map[string]any{"type": "object"}
			}
			result = append(result, responses.ToolUnionParam{
				OfFunction: &responses.FunctionToolParam{
					Name:        t.Function.Name,
					Description: openai.String(t.Function.Description),
					Parameters:  params,
				},
			})
		}
	}
	return result
}

func parseSDKResponseOutput(resp responses.Response) (text string, reasoning string, toolCalls []ToolCall) {
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			if item.Role == "assistant" {
				for _, part := range item.Content {
					if part.Type == "output_text" {
						text += part.Text
					}
				}
			}
		case "reasoning":
			for _, s := range item.Summary {
				reasoning += s.Text
			}
		case "function_call":
			toolCalls = append(toolCalls, ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: FunctionCall{
					Name:      item.Name,
					Arguments: item.Arguments.OfString,
				},
			})
		}
	}
	return text, reasoning, toolCalls
}

func buildResponseParams(cfg AIConfig, input []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam, previousResponseID string) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: cfg.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: input,
		},
	}
	if cfg.MaxCompletionTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(cfg.MaxCompletionTokens))
	} else if cfg.MaxTokens > 0 {
		params.MaxOutputTokens = openai.Int(int64(cfg.MaxTokens))
	}
	if cfg.Temperature > 0 {
		params.Temperature = openai.Float(float64(cfg.Temperature))
	}
	if cfg.TopP > 0 {
		params.TopP = openai.Float(float64(cfg.TopP))
	}
	if cfg.ReasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(cfg.ReasoningEffort),
		}
	}
	if previousResponseID != "" {
		params.PreviousResponseID = openai.String(previousResponseID)
	}
	if cfg.ServiceTier != "" {
		params.ServiceTier = responses.ResponseNewParamsServiceTier(cfg.ServiceTier)
	}
	if cfg.Verbosity != "" {
		params.Text = responses.ResponseTextConfigParam{
			Verbosity: responses.ResponseTextConfigVerbosity(cfg.Verbosity),
		}
	}
	if len(tools) > 0 {
		params.Tools = tools
		params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: openai.Opt(responses.ToolChoiceOptionsAuto),
		}
		if cfg.ParallelToolCalls != nil {
			params.ParallelToolCalls = openai.Bool(*cfg.ParallelToolCalls)
		}
	}
	return params
}

func sdkResponseUsageToUsage(u responses.ResponseUsage) *Usage {
	usage := &Usage{
		PromptTokens:     int64(u.InputTokens),
		CompletionTokens: int64(u.OutputTokens),
		TotalTokens:      int64(u.TotalTokens),
	}
	if u.InputTokensDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens: int64(u.InputTokensDetails.CachedTokens),
		}
	}
	if u.OutputTokensDetails.ReasoningTokens > 0 {
		usage.CompletionTokensDetails = &CompletionTokensDetails{
			ReasoningTokens: int64(u.OutputTokensDetails.ReasoningTokens),
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
