package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	gogpt "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
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

func enhancePrompt(ctx context.Context, cfg Config, enhancementName, rawPrompt string) (*EnhanceResult, error) {
	enhCfg, ok := cfg.Enhancements[enhancementName]
	if !ok {
		return nil, fmt.Errorf("enhancement %q not found", enhancementName)
	}

	schema, err := jsonschema.GenerateSchemaForType(EnhancementResponse{})
	if err != nil {
		return nil, fmt.Errorf("generating schema: %w", err)
	}

	aiConfig := gogpt.DefaultConfig(enhCfg.Key)
	aiConfig.BaseURL = enhCfg.BaseURL
	aiClient := gogpt.NewClientWithConfig(aiConfig)

	timeout := time.Duration(enhCfg.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	enhanceCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := aiClient.CreateChatCompletion(enhanceCtx, gogpt.ChatCompletionRequest{
		Model: enhCfg.Model,
		Messages: []gogpt.ChatCompletionMessage{
			{Role: gogpt.ChatMessageRoleSystem, Content: enhCfg.SystemPrompt},
			{Role: gogpt.ChatMessageRoleUser, Content: rawPrompt},
		},
		ResponseFormat: &gogpt.ChatCompletionResponseFormat{
			Type: gogpt.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &gogpt.ChatCompletionResponseFormatJSONSchema{
				Name:   "prompt_enhancement",
				Schema: schema,
				Strict: true,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("enhancement API call: %w", err)
	}

	enhanced := strings.TrimSpace(resp.Choices[0].Message.Content)
	var result EnhancementResponse
	if err := schema.Unmarshal(enhanced, &result); err != nil {
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
