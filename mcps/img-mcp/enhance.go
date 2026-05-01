package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
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

	resp, err := client.Chat.Completions.New(enhanceCtx, openai.ChatCompletionNewParams{
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
	})
	if err != nil {
		return nil, fmt.Errorf("enhancement API call: %w", err)
	}

	enhanced := strings.TrimSpace(resp.Choices[0].Message.Content)
	var result EnhancementResponse
	if err := json.Unmarshal([]byte(enhanced), &result); err != nil {
		return nil, fmt.Errorf("parsing enhancement response: %w", err)
	}

	if result.Refused {
		return nil, fmt.Errorf("enhancement refused: %s", result.Reason)
	}

	if result.EnhancedPrompt == "" {
		return nil, fmt.Errorf("enhancement returned empty prompt")
	}

	return &EnhanceResult{
		EnhancedPrompt: strings.TrimSpace(result.EnhancedPrompt),
		NegativePrompt: strings.TrimSpace(result.NegativePrompt),
	}, nil
}
