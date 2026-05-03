package main

import (
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/assert"
)

func TestFormatOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no IRC codes",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "converts color code",
			input: "\x03",
			want:  "\x1b[033m[C]\x1b[0m",
		},
		{
			name:  "converts bold code",
			input: "\x02",
			want:  "\x1b[034m[B]\x1b[0m",
		},
		{
			name:  "converts underline code",
			input: "\x1F",
			want:  "\x1b[035m[U]\x1b[0m",
		},
		{
			name:  "converts italic code",
			input: "\x1D",
			want:  "\x1b[036m[I]\x1b[0m",
		},
		{
			name:  "color with content",
			input: "\x03[0mred\x03",
			want:  "\x1b[033m[C]\x1b[0m[0mred\x1b[033m[C]\x1b[0m",
		},
		{
			name:  "bold with content",
			input: "\x02bold\x02",
			want:  "\x1b[034m[B]\x1b[0mbold\x1b[034m[B]\x1b[0m",
		},
		{
			name:  "underline with content",
			input: "\x1Fun underlined\x1F",
			want:  "\x1b[035m[U]\x1b[0mun underlined\x1b[035m[U]\x1b[0m",
		},
		{
			name:  "italic with content",
			input: "\x1Ditalicized\x1D",
			want:  "\x1b[036m[I]\x1b[0mitalicized\x1b[036m[I]\x1b[0m",
		},
		{
			name:  "mixed codes",
			input: "\x02bold\x02 and \x03color\x03",
			want:  "\x1b[034m[B]\x1b[0mbold\x1b[034m[B]\x1b[0m and \x1b[033m[C]\x1b[0mcolor\x1b[033m[C]\x1b[0m",
		},
		{
			name:  "consecutive codes",
			input: "\x02\x03mixed\x02text\x03",
			want:  "\x1b[034m[B]\x1b[0m\x1b[033m[C]\x1b[0mmixed\x1b[034m[B]\x1b[0mtext\x1b[033m[C]\x1b[0m",
		},
		{
			name:  "all codes",
			input: "\x02\x03\x1F\x1Dall\x1D\x1F\x03\x02",
			want:  "\x1b[034m[B]\x1b[0m\x1b[033m[C]\x1b[0m\x1b[035m[U]\x1b[0m\x1b[036m[I]\x1b[0mall\x1b[036m[I]\x1b[0m\x1b[035m[U]\x1b[0m\x1b[033m[C]\x1b[0m\x1b[034m[B]\x1b[0m",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatOutput(tt.input)
			assert.Equal(t, tt.want, got, "FormatOutput()")
		})
	}
}

func TestExtractFinalText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no marker",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "marker with text after",
			input: "intro </think>\ntest\nrest",
			want:  "test\nrest",
		},
		{
			name:  "marker at start",
			input: "</think>\nend",
			want:  "end",
		},
		{
			name:  "only marker with newline",
			input: "</think>\n",
			want:  "",
		},
		{
			name:  "marker with newline",
			input: "before\n</think>\nafter",
			want:  "after",
		},
		{
			name:  "marker without newline after unchanged",
			input: "text </think>end",
			want:  "text </think>end",
		},
		{
			name:  "multiple markers takes last",
			input: "first </think>\nsecond </think>\nfinal",
			want:  "final",
		},
		{
			name:  "marker not at end of line",
			input: "text </think> and more </think>\nend",
			want:  "end",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractFinalText(tt.input)
			assert.Equal(t, tt.want, got, "ExtractFinalText()")
		})
	}
}

func TestBuildChatRequest(t *testing.T) {
	tests := []struct {
		name     string
		cfg      AIConfig
		messages []ChatMessage
		check    func(*testing.T, openai.ChatCompletionNewParams)
	}{
		{
			name: "basic fields",
			cfg: AIConfig{
				Model:               "gpt-4",
				MaxTokens:           100,
				MaxCompletionTokens: 200,
				Temperature:         0.7,
			},
			messages: []ChatMessage{
				{Role: RoleUser, Content: "hi"},
			},
			check: func(t *testing.T, req openai.ChatCompletionNewParams) {
				assert.Equal(t, "gpt-4", req.Model, "Model")
				if assert.True(t, req.MaxTokens.Valid(), "MaxTokens should be valid") {
					assert.Equal(t, int64(100), req.MaxTokens.Value, "MaxTokens")
				}
				if assert.True(t, req.MaxCompletionTokens.Valid(), "MaxCompletionTokens should be valid") {
					assert.Equal(t, int64(200), req.MaxCompletionTokens.Value, "MaxCompletionTokens")
				}
				if assert.True(t, req.Temperature.Valid(), "Temperature should be valid") {
					assert.InDelta(t, 0.7, req.Temperature.Value, 0.01, "Temperature")
				}
				assert.Len(t, req.Messages, 1, "Messages")
			},
		},
		{
			name: "streaming enabled",
			cfg: AIConfig{
				Model:     "gpt-4",
				Streaming: true,
			},
			messages: nil,
			check: func(t *testing.T, req openai.ChatCompletionNewParams) {
				if assert.True(t, req.StreamOptions.IncludeUsage.Valid(), "StreamOptions.IncludeUsage should be valid") {
					assert.True(t, req.StreamOptions.IncludeUsage.Value, "StreamOptions.IncludeUsage should be true for streaming")
				}
			},
		},
		{
			name: "zero values preserved",
			cfg: AIConfig{
				Model:               "test-model",
				MaxTokens:           0,
				MaxCompletionTokens: 0,
				Temperature:         0,
			},
			messages: []ChatMessage{},
			check: func(t *testing.T, req openai.ChatCompletionNewParams) {
				assert.False(t, req.MaxTokens.Valid(), "MaxTokens should be omitted, got %v", req.MaxTokens)
				assert.False(t, req.MaxCompletionTokens.Valid(), "MaxCompletionTokens should be omitted, got %v", req.MaxCompletionTokens)
				assert.False(t, req.Temperature.Valid(), "Temperature should be omitted, got %v", req.Temperature)
			},
		},
		{
			name: "multiple messages",
			cfg: AIConfig{
				Model: "gpt-4",
			},
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "you are helpful"},
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi there"},
				{Role: RoleUser, Content: "how are you"},
			},
			check: func(t *testing.T, req openai.ChatCompletionNewParams) {
				assert.Len(t, req.Messages, 4, "Messages")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := buildChatCompletionParams(tt.cfg, tt.messages, nil)
			tt.check(t, req)
		})
	}
}

func TestReasoningContent(t *testing.T) {
	tests := []struct {
		name             string
		content          string
		reasoningContent string
		wantContent      string
		wantReasoning    string
	}{
		{
			name:             "content only",
			content:          "Here is the answer",
			reasoningContent: "",
			wantContent:      "Here is the answer",
			wantReasoning:    "",
		},
		{
			name:             "content with reasoning",
			content:          "Here is the answer",
			reasoningContent: "Let me think about this...",
			wantContent:      "Here is the answer",
			wantReasoning:    "Let me think about this...",
		},
		{
			name:             "reasoning only",
			content:          "",
			reasoningContent: "Analyzing the problem...",
			wantContent:      "",
			wantReasoning:    "Analyzing the problem...",
		},
		{
			name:             "multi-line reasoning",
			content:          "Final answer",
			reasoningContent: "Step 1\nStep 2\nStep 3",
			wantContent:      "Final answer",
			wantReasoning:    "Step 1\nStep 2\nStep 3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := ChatMessage{
				Role:             RoleAssistant,
				Content:          tt.content,
				ReasoningContent: tt.reasoningContent,
			}

			assert.Equal(t, tt.wantContent, msg.Content, "Content")
			assert.Equal(t, tt.wantReasoning, msg.ReasoningContent, "ReasoningContent")
		})
	}
}
