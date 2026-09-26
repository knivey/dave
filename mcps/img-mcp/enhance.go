package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

type EnhancementResponse struct {
	EnhancedPrompt string `json:"enhanced_prompt"`
	NegativePrompt string `json:"negative_prompt"`
	Refused        bool   `json:"refused"`
	Reason         string `json:"reason"`
	// NSFW is the safety first pass (safe-site split): the loose
	// enhancement prompts instruct the model to include "nsfw": true when
	// the request or the enhanced result contains sexual content. Absent
	// or false is only "no first-pass signal" — it NEVER asserts safety;
	// the strict second-pass vet still runs.
	NSFW bool `json:"nsfw"`
}

type EnhanceResult struct {
	EnhancedPrompt string
	NegativePrompt string
	// NSFW carries the first-pass sexual-content flag through to the
	// safety stage (true → safety unsafe, vet call skipped). False/absent
	// on the response means "no signal" — it never asserts safety.
	NSFW bool
	// Reasoning is the enhancement model's reasoning summary (Responses API
	// path only — Chat Completions does not return summaries). Carried into
	// the workflow's prompt note node so it is embedded in the image
	// metadata alongside the original prompt. Deliberately NOT trimmed
	// (unlike its siblings): it is verbatim provenance, concatenated
	// as-is from the summary items exactly like the log line emits it.
	Reasoning string
}

var enhancementSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"enhanced_prompt": map[string]any{"type": "string"},
		"negative_prompt": map[string]any{"type": "string"},
		"refused":         map[string]any{"type": "boolean"},
		"reason":          map[string]any{"type": "string"},
	},
	"required":             []string{"enhanced_prompt", "negative_prompt", "refused", "reason"},
	"additionalProperties": false,
}

// callEnhancementLLM runs one structured-output LLM call against the named
// enhancement config and returns the raw assistant JSON text plus the
// Responses-API reasoning summary (empty on Chat Completions). This is the
// shared machinery behind BOTH prompt enhancement and the safety vet
// (safety.go): config lookup, per-workflow instruction merging, client
// construction, timeouts, both API paths, and reasoning_effort live here so
// the vet inherits all of it — including SIGHUP hot-reload, which simply
// swaps the Config the caller passes. Callers own the response contract:
// schemaName/schema describe the JSON the model must produce
// ("prompt_enhancement" for enhancement, "safety_verdict" for the vet).
func callEnhancementLLM(ctx context.Context, cfg Config, enhancementName, rawPrompt, extraInstructions, schemaName string, schema map[string]any) (string, string, error) {
	enhCfg, ok := cfg.Enhancements[enhancementName]
	if !ok {
		return "", "", fmt.Errorf("enhancement %q not found", enhancementName)
	}

	// Per-workflow enhancement instructions (extracted from the workflow
	// file's dave_enhancement_instructions node) ride on a new line after
	// the profile's system prompt: the profile defines the enhancement
	// persona, the workflow instructions refine it for the target workflow.
	// Both API paths below read enhCfg.SystemPrompt, so merging here covers
	// Chat Completions and Responses in one place. enhCfg is a value copy
	// of the map entry — this assignment does not mutate shared config.
	// Trimmed so a whitespace-only node text appends nothing.
	if instructions := strings.TrimSpace(extraInstructions); instructions != "" {
		enhCfg.SystemPrompt += "\n" + instructions
	}
	loggerTools.Debug("enhancement llm call",
		"enhancement", enhancementName,
		"model", enhCfg.Model,
		"schema", schemaName,
		"raw_prompt", rawPrompt,
	)

	clientOpts := []option.RequestOption{
		option.WithAPIKey(enhCfg.Key),
	}
	if enhCfg.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(enhCfg.BaseURL))
	}
	client := openai.NewClient(clientOpts...)

	timeout := time.Duration(enhCfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	enhanceCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var content string
	var reasoning string
	var err error
	if enhCfg.ResponsesAPI {
		content, reasoning, err = enhanceViaResponses(enhanceCtx, client, enhCfg, rawPrompt, schemaName, schema)
	} else {
		content, err = enhanceViaChatCompletions(enhanceCtx, client, enhCfg, rawPrompt, schemaName, schema)
	}
	if err != nil {
		return "", "", fmt.Errorf("enhancement API call: %w", err)
	}
	return strings.TrimSpace(content), reasoning, nil
}

func enhancePrompt(ctx context.Context, cfg Config, enhancementName, rawPrompt, extraInstructions string) (*EnhanceResult, error) {
	enhanced, reasoning, err := callEnhancementLLM(ctx, cfg, enhancementName, rawPrompt, extraInstructions, "prompt_enhancement", enhancementSchema)
	if err != nil {
		return nil, err
	}

	var result EnhancementResponse
	if err := json.Unmarshal([]byte(enhanced), &result); err != nil {
		return nil, fmt.Errorf("parsing enhancement response: %w", err)
	}

	if result.Refused {
		loggerTools.Warn("enhancement refused", "reason", result.Reason, "raw_prompt", rawPrompt)
		return nil, fmt.Errorf("enhancement refused: %s", result.Reason)
	}

	if result.EnhancedPrompt == "" {
		return nil, fmt.Errorf("enhancement returned empty prompt")
	}

	loggerTools.Info("enhancement complete",
		"enhanced_prompt", strings.TrimSpace(result.EnhancedPrompt),
		"negative_prompt", strings.TrimSpace(result.NegativePrompt),
		"nsfw", result.NSFW,
	)

	return &EnhanceResult{
		EnhancedPrompt: strings.TrimSpace(result.EnhancedPrompt),
		NegativePrompt: strings.TrimSpace(result.NegativePrompt),
		Reasoning:      reasoning,
		NSFW:           result.NSFW,
	}, nil
}

// enhanceViaChatCompletions calls the Chat Completions API and returns the raw
// assistant message content. Reasoning effort, when set, is sent as the
// top-level reasoning_effort field (the format xAI's Chat Completions API
// expects; same as dave's chatCompletion.go). schemaName/schema select the
// structured-output contract (enhancement or safety verdict).
func enhanceViaChatCompletions(ctx context.Context, client openai.Client, enhCfg EnhancementConfig, rawPrompt, schemaName string, schema map[string]any) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: enhCfg.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(enhCfg.SystemPrompt),
			openai.UserMessage(rawPrompt),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   schemaName,
					Schema: schema,
					Strict: openai.Bool(true),
				},
			},
		},
	}
	if enhCfg.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(enhCfg.ReasoningEffort)
	}

	resp, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", err
	}

	loggerTools.Info("enhancement LLM response",
		"path", "chat_completions",
		"model", resp.Model,
		"finish_reason", resp.Choices[0].FinishReason,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
	)
	return resp.Choices[0].Message.Content, nil
}

// enhanceViaResponses calls the OpenAI Responses API (POST /v1/responses) and
// returns the concatenated output_text of the assistant message plus the
// concatenated reasoning summary (empty when the model emitted none).
// Reasoning summaries are logged at INFO — the Responses API is the only way
// to get them back from reasoning models. Structured output goes in
// Text.Format, NOT a top-level ResponseFormat (different mechanism than Chat
// Completions). schemaName/schema select the structured-output contract
// (enhancement or safety verdict).
func enhanceViaResponses(ctx context.Context, client openai.Client, enhCfg EnhancementConfig, rawPrompt, schemaName string, schema map[string]any) (string, string, error) {
	params := responses.ResponseNewParams{
		Model: enhCfg.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: []responses.ResponseInputItemUnionParam{
				{OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleSystem,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: openai.String(enhCfg.SystemPrompt),
					},
				}},
				{OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleUser,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: openai.String(rawPrompt),
					},
				}},
			},
		},
		Text: responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:   schemaName,
					Schema: schema,
					Strict: openai.Bool(true),
				},
			},
		},
	}
	if enhCfg.ReasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(enhCfg.ReasoningEffort),
		}
	}

	resp, err := client.Responses.New(ctx, params)
	if err != nil {
		return "", "", err
	}

	// Same output-walk as dave's parseSDKResponseOutput (responses.go): message
	// items yield output_text, reasoning items yield summary text. Summary is
	// an array of {Text, Type} entries — concatenate all of them. We log
	// Summary only (never Content, the raw reasoning) for consistency with dave.
	var text, reasoning string
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text += part.Text
				}
			}
		case "reasoning":
			for _, s := range item.Summary {
				reasoning += s.Text
			}
		}
	}
	if reasoning != "" {
		loggerTools.Info("enhancement reasoning",
			"path", "responses",
			"model", resp.Model,
			"reasoning", reasoning,
		)
	}
	loggerTools.Info("enhancement LLM response",
		"path", "responses",
		"model", resp.Model,
		"status", resp.Status,
		"prompt_tokens", resp.Usage.InputTokens,
		"completion_tokens", resp.Usage.OutputTokens,
		"total_tokens", resp.Usage.TotalTokens,
	)
	if text == "" {
		return "", "", fmt.Errorf("responses API returned no message output")
	}
	return text, reasoning, nil
}
