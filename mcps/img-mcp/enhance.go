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
}

type EnhanceResult struct {
	EnhancedPrompt string
	NegativePrompt string
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

func enhancePrompt(ctx context.Context, cfg Config, enhancementName, rawPrompt string) (*EnhanceResult, error) {
	enhCfg, ok := cfg.Enhancements[enhancementName]
	if !ok {
		return nil, fmt.Errorf("enhancement %q not found", enhancementName)
	}

	loggerTools.Debug("enhancing prompt",
		"enhancement", enhancementName,
		"model", enhCfg.Model,
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

	var enhanced string
	var err error
	if enhCfg.ResponsesAPI {
		enhanced, err = enhanceViaResponses(enhanceCtx, client, enhCfg, rawPrompt)
	} else {
		enhanced, err = enhanceViaChatCompletions(enhanceCtx, client, enhCfg, rawPrompt)
	}
	if err != nil {
		return nil, fmt.Errorf("enhancement API call: %w", err)
	}
	enhanced = strings.TrimSpace(enhanced)

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
	)

	return &EnhanceResult{
		EnhancedPrompt: strings.TrimSpace(result.EnhancedPrompt),
		NegativePrompt: strings.TrimSpace(result.NegativePrompt),
	}, nil
}

// enhanceViaChatCompletions calls the Chat Completions API and returns the raw
// assistant message content. Reasoning effort, when set, is sent as the
// top-level reasoning_effort field (the format xAI's Chat Completions API
// expects; same as dave's chatCompletion.go).
func enhanceViaChatCompletions(ctx context.Context, client openai.Client, enhCfg EnhancementConfig, rawPrompt string) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: enhCfg.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(enhCfg.SystemPrompt),
			openai.UserMessage(rawPrompt),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "prompt_enhancement",
					Schema: enhancementSchema,
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
// returns the concatenated output_text of the assistant message. Reasoning
// summaries are logged at INFO — the Responses API is the only way to get
// them back from reasoning models. Structured output goes in Text.Format,
// NOT a top-level ResponseFormat (different mechanism than Chat Completions).
func enhanceViaResponses(ctx context.Context, client openai.Client, enhCfg EnhancementConfig, rawPrompt string) (string, error) {
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
					Name:   "prompt_enhancement",
					Schema: enhancementSchema,
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
		return "", err
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
		return "", fmt.Errorf("responses API returned no message output")
	}
	return text, nil
}
