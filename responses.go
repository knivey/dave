package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func messagesToResponseInputItems(messages []ChatMessage, replay, format string) []responses.ResponseInputItemUnionParam {
	input := make([]responses.ResponseInputItemUnionParam, 0, len(messages)*2)
	for i, msg := range messages {
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
			items := make([]responses.ResponseInputItemUnionParam, 0, 2+len(msg.ToolCalls))
			// Reasoning history replay (reasoningreplay.go): eligible rows
			// lead with reasoning item(s) so the provider (OpenRouter's
			// stateless proxy, llama.cpp's /v1/responses-adjacent paths)
			// can rebuild the model's thought context. On
			// previous_response_id chains this never fires — the chained
			// turns send delta-only tool-result items, and the server
			// replays its own reasoning. The form is the format knob's
			// choice — see the branches inside.
			if replayReasoningRow(msg, replay, format) {
				// Format "details": echo the row's stored RESPONSES-dialect
				// items VERBATIM — the provider's own ids, encrypted_content,
				// summary, content — per OpenRouter's preserving-reasoning
				// contract (the sequence must match the original output). A
				// chat-dialect envelope or no envelope falls back to the
				// synthetic text item below (exact replay only happens on the
				// path the payload was born on).
				echoed := false
				if format == ReasoningReplayFormatDetails {
					if src, rawItems, ok := parseReasoningDetailsEnvelope(msg.ReasoningDetails); ok && src == detailsSourceResponses {
						// All-or-nothing: OpenRouter requires the echoed
						// sequence to match the original output exactly, so ONE
						// unbuildable item voids the whole echo (a subset would
						// be a modified sequence) and the row falls through to
						// the text form below. NOTE the echo is struct-mediated,
						// not byte-for-byte: the SDK param models
						// id/summary/encrypted_content/content(/status) -
						// OpenRouter's documented echo shape - and any
						// provider-specific extra field in a stored item is
						// dropped rather than replayed (OpenAI-strict item
						// fidelity stays a non-goal; see reasoningreplay.go).
						echoItems := make([]responses.ResponseInputItemUnionParam, 0, len(rawItems))
						for _, raw := range rawItems {
							if item, itemOK := reasoningItemParamFromRaw(raw); itemOK {
								echoItems = append(echoItems, item)
							}
						}
						if len(echoItems) == len(rawItems) {
							items = append(items, echoItems...)
							echoed = true
						}
					}
				}
				// Text form (default, or the details fallback): the synthetic
				// item carries the stored REAL content only — content parts,
				// no summary key (a summary is not the thinking and is
				// display/log-only, never serialized).
				if !echoed && msg.ReasoningContent != "" {
					items = append(items, reasoningReplayInputItem(i, msg.ReasoningContent))
				}
			}
			if len(msg.ToolCalls) > 0 {
				if msg.Content != "" {
					items = append(items, responses.ResponseInputItemUnionParam{
						OfMessage: &responses.EasyInputMessageParam{
							Role:    responses.EasyInputMessageRoleAssistant,
							Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
						},
					})
				}
				for _, tc := range msg.ToolCalls {
					items = append(items, responses.ResponseInputItemParamOfFunctionCall(
						tc.Function.Arguments, tc.ID, tc.Function.Name,
					))
				}
			} else {
				items = append(items, responses.ResponseInputItemUnionParam{
					OfMessage: &responses.EasyInputMessageParam{
						Role:    responses.EasyInputMessageRoleAssistant,
						Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(msg.Content)},
					},
				})
			}
			input = append(input, items...)

		case RoleTool:
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(
				msg.ToolCallID, msg.Content,
			))
		}
	}
	return input
}

// reasoningReplayInputItem builds the reasoning input item for assistant
// row rowIdx. The id is deterministic per row ("rs_dave_<row>") because a
// non-chained full-history request rebuilds these items every turn: a
// stable id keeps the request shape stable, and OpenAI proper never sees
// the item on validated paths (its chains are delta-only,
// previous_response_id; see the reasoningReplay doc block for the scope).
//
// The item carries the stored REAL content ONLY — content parts, never a
// summary: a summary is not the thinking (the split's whole point), Summary
// is omitzero in the SDK so the nil passed here omits the key entirely, and
// the wire shape is {"type":"reasoning","id":"rs_dave_<row>","content":
// [{"type":"reasoning_text","text":…}]} with no "summary" key.
func reasoningReplayInputItem(rowIdx int, text string) responses.ResponseInputItemUnionParam {
	item := responses.ResponseInputItemParamOfReasoning(
		fmt.Sprintf("rs_dave_%d", rowIdx),
		nil,
	)
	item.OfReasoning.Content = []responses.ResponseReasoningItemContentParam{{Text: text}}
	return item
}

// reasoningItemParamFromRaw builds a Responses input item from a stored
// verbatim reasoning item byte string (the exact-replay envelope). The
// unmarshal routes through the SDK's own param type — id, encrypted_content,
// summary, and content parts all survive, which is the provider's own echo
// shape (OpenRouter's preserving-reasoning example carries exactly these).
//
// The SDK's apijson unmarshal is maximally lenient — numbers, strings,
// arrays, and null all "succeed" with a zeroed item — so the buildability
// test is the ECHO SHAPE: a provider reasoning item always carries its id
// (rs_…), and an id-less result means the stored bytes were not a reasoning
// item. ok=false voids the item (the caller's all-or-nothing rule then
// falls the row back to the text form rather than echoing a partial or
// garbage sequence).
func reasoningItemParamFromRaw(raw json.RawMessage) (responses.ResponseInputItemUnionParam, bool) {
	var item responses.ResponseReasoningItemParam
	if err := json.Unmarshal(raw, &item); err != nil {
		return responses.ResponseInputItemUnionParam{}, false
	}
	if item.ID == "" {
		return responses.ResponseInputItemUnionParam{}, false
	}
	return responses.ResponseInputItemUnionParam{OfReasoning: &item}, true
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

func parseSDKResponseOutput(resp responses.Response) (text string, parts reasoningParts, toolCalls []ToolCall) {
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
			// The reasoning split (aiMessages.go): item.Content parts
			// (type reasoning_text) are the model's REAL thinking text —
			// the only replayable fidelity; item.Summary entries are the
			// provider's summarized form — display/log-only. Both are
			// captured, into separate slots.
			for _, part := range item.Content {
				if part.Type == "reasoning_text" {
					parts.Content += part.Text
				}
			}
			for _, s := range item.Summary {
				parts.Summary += s.Text
			}
			// Exact-replay capture (reasoningreplay.go): the item's
			// VERBATIM bytes (id, encrypted_content, summary, content —
			// via the SDK's RawJSON passthrough) under the "responses"
			// dialect. Capture is always on; replay is configured per
			// service/command. Multiple reasoning items (one before each
			// message/function_call) append in output order.
			if raw := item.AsReasoning().RawJSON(); raw != "" {
				parts.addDetails(detailsSourceResponses, []json.RawMessage{json.RawMessage(raw)})
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
	return text, parts, toolCalls
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
	// Exact-replay capture (reasoningreplay.go): in details format the
	// request asks for reasoning.encrypted_content — without the include
	// the response carries no encrypted material to capture and echo
	// (OpenRouter's preserving-reasoning example sets exactly this
	// include alongside echoed reasoning items). Gated on the replay MODE
	// too: the include exists solely to produce replay material, so
	// reasoningreplay = "off" (nothing ever replays) skips it instead of
	// paying the larger response/api-log on every request. Providers that
	// ignore the include simply return nothing encrypted and the replay
	// degrades to the text form per-row. The text format sends no include
	// so the request body stays byte-identical to the pre-knob behavior.
	if reasoningReplayFormat(cfg) == ReasoningReplayFormatDetails && reasoningReplay(cfg) != ReasoningReplayOff {
		params.Include = []responses.ResponseIncludable{
			responses.ResponseIncludableReasoningEncryptedContent,
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
