package main

import (
	"errors"
	"net/http"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func messagesToResponseInputItems(messages []ChatMessage) []responses.ResponseInputItemUnionParam {
	input := make([]responses.ResponseInputItemUnionParam, 0, len(messages)*2)
	for _, msg := range messages {
		switch msg.Role {
		case RoleSystem, RoleDeveloper:
			// Developer is guidance Knob 1's payload role; llama.cpp maps
			// it to system server-side (see guidance.go).
			input = append(input, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRole(msg.Role),
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

// chainActive reports whether a Responses API request may rely on
// server-side stored context: chaining is enabled AND a chain head exists.
// Every site that rebuilds Responses API input must re-derive this (turn
// entry, both tool loops) so the input shape (full history vs delta-only)
// and the previous_response_id param can never disagree — the param is sent
// iff chainActive says the input is delta-only.
func chainActive(cfg AIConfig, responseID string) bool {
	return cfg.PreviousResponseID && responseID != ""
}

func buildResponseParams(cfg AIConfig, input []responses.ResponseInputItemUnionParam, tools []responses.ToolUnionParam, previousResponseID string, ident apiIdentity) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: cfg.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: input,
		},
	}
	if ident.User != "" {
		params.User = openai.String(ident.User)
	}
	if ident.SafetyID != "" {
		params.SafetyIdentifier = openai.String(ident.SafetyID)
	}
	if ident.CacheKey != "" {
		params.PromptCacheKey = openai.String(ident.CacheKey)
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
	// Reasoning summaries are only returned when the request asks for them via
	// reasoning.summary ("auto"/"concise"/"detailed"); the retrieve endpoint's
	// include parameter cannot recover them after the fact. Effort and summary
	// are independent: either may be set without the other.
	if cfg.ReasoningEffort != "" || cfg.ReasoningSummary != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort:  shared.ReasoningEffort(cfg.ReasoningEffort),
			Summary: shared.ReasoningSummary(cfg.ReasoningSummary),
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

func sdkResponseUsageToUsage(u responses.ResponseUsage, status string) *Usage {
	usage := &Usage{
		PromptTokens:     int64(u.InputTokens),
		CompletionTokens: int64(u.OutputTokens),
		TotalTokens:      int64(u.TotalTokens),
		FinishReason:     status,
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

// isResponseIDError checks if an API error is caused by an invalid or unusable
// previous_response_id chain, so the caller can retry without it.
//
// Primary detection uses the structured openai.Error type (StatusCode + Code fields),
// which is provider-agnostic. A 404 on POST /v1/responses when previous_response_id
// was sent always means the response ID is gone/expired, regardless of how the
// provider words the error message.
//
// String matching on err.Error() is kept as a fallback for errors that don't
// type-assert to *openai.Error (e.g. mid-stream StreamError from the SDK).
//
// DESIGN NOTE — "Each message must have at least one content element":
// This is technically a request validation error, not an invalid response ID.
// However, it occurs when chaining via previous_response_id to a response that
// had empty output (output:[]). The API reconstructs the conversation server-side
// and finds an assistant message with no content. Retrying without
// previous_response_id (sending full history from our side) is the correct fix.
// Layer 1 prevention: we avoid saving empty-output response IDs (in aiCmds.go).
// This check is the Layer 2 safety net in case something slips through.
// Do not remove this condition without understanding the full two-layer design.
//
// DESIGN NOTE — "Reasoning input items can only be provided to a reasoning or
// computer use model": the observed cross-model chain failure (reasoning →
// non-reasoning). Retrying without previous_response_id sends our full
// history, which never contains reasoning input items (encrypted reasoning
// content is intentionally dropped), so the retry succeeds. Layer 1
// prevention: runTurnResponses refuses to chain when the stored
// response_model differs from the live config's model (sessions.response_model,
// written atomically alongside response_id). This check is the Layer 2 net
// for NULL-model legacy rows and provider wording variance (e.g. xAI).
//
// DESIGN NOTE — "previous_response_id is not supported on this proxy"
// (code invalid_prompt): OpenRouter's Responses API proxy is STATELESS — it
// rejects any previous_response_id outright, not because the id expired but
// because chaining does not exist there. The retry-without-id recovery is
// still exactly right: our full history is always sufficient. Matched on
// code invalid_prompt + the field named in the message so unrelated
// invalid_prompt validation errors stay out.
func isResponseIDError(err error) bool {
	if err == nil {
		return false
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusNotFound:
			return true
		case apiErr.Code == "response_not_found":
			return true
		case apiErr.Code == "invalid_previous_response_id":
			return true
		case apiErr.StatusCode == http.StatusBadRequest &&
			strings.Contains(apiErr.Message, "Each message must have at least one content element"):
			return true
		case apiErr.StatusCode == http.StatusBadRequest &&
			strings.Contains(apiErr.Message, "Reasoning input items can only be provided to a reasoning or computer use model"):
			return true
		case apiErr.StatusCode == http.StatusBadRequest &&
			apiErr.Code == "invalid_prompt" &&
			strings.Contains(apiErr.Message, "previous_response_id"):
			return true
		}
		// No structured match: fall through to the string fallback below
		// instead of returning false. For WRAPPED error bodies the SDK
		// populates Code/Message from gjson(body,"error") but Error()
		// also embeds the raw inner object, so a provider whose code is
		// not one of the enumerated cases above can still match on
		// message wording (e.g. a "invalid_request"-coded expired-id
		// error). NOTE: UNWRAPPED bodies (no "error" object) are dropped
		// by the SDK entirely — UnmarshalJSON("") leaves Code, Message
		// AND the embedded raw body empty — so no string matching can
		// ever see them; that shape is unmatchable at this layer
		// (verified against openai-go v3.33.0; production OpenRouter
		// wraps, per the 2026-10-09 incident's captured response body).
	}

	s := err.Error()
	return strings.Contains(s, `"code":"response_not_found"`) ||
		strings.Contains(s, `"code":"invalid_previous_response_id"`) ||
		strings.Contains(s, "previous_response_id") && strings.Contains(s, "not found") ||
		strings.Contains(s, "previous_response_id") && strings.Contains(s, "not supported") ||
		strings.Contains(s, "Each message must have at least one content element") ||
		strings.Contains(s, "Reasoning input items can only be provided to a reasoning or computer use model")
}
