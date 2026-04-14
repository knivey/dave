package main

import (
	"testing"

	gogpt "github.com/sashabaranov/go-openai"
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
			if got != tt.want {
				t.Errorf("FormatOutput() = %q, want %q", got, tt.want)
			}
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
			if got != tt.want {
				t.Errorf("ExtractFinalText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildChatRequest(t *testing.T) {
	tests := []struct {
		name     string
		cfg      AIConfig
		messages []gogpt.ChatCompletionMessage
		check    func(*testing.T, gogpt.ChatCompletionRequest)
	}{
		{
			name: "basic fields",
			cfg: AIConfig{
				Model:               "gpt-4",
				MaxTokens:           100,
				MaxCompletionTokens: 200,
				Temperature:         0.7,
			},
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleUser, Content: "hi"},
			},
			check: func(t *testing.T, req gogpt.ChatCompletionRequest) {
				if req.Model != "gpt-4" {
					t.Errorf("Model = %q, want %q", req.Model, "gpt-4")
				}
				if req.MaxTokens != 100 {
					t.Errorf("MaxTokens = %d, want %d", req.MaxTokens, 100)
				}
				if req.MaxCompletionTokens != 200 {
					t.Errorf("MaxCompletionTokens = %d, want %d", req.MaxCompletionTokens, 200)
				}
				if req.Temperature != 0.7 {
					t.Errorf("Temperature = %f, want %f", req.Temperature, 0.7)
				}
				if len(req.Messages) != 1 {
					t.Errorf("Messages len = %d, want %d", len(req.Messages), 1)
				}
				if req.Stream {
					t.Error("Stream = true, want false")
				}
				if req.StreamOptions != nil {
					t.Error("StreamOptions should be nil for non-streaming")
				}
			},
		},
		{
			name: "streaming enabled",
			cfg: AIConfig{
				Model:     "gpt-4",
				Streaming: true,
			},
			messages: nil,
			check: func(t *testing.T, req gogpt.ChatCompletionRequest) {
				if !req.Stream {
					t.Error("Stream = false, want true")
				}
				if req.StreamOptions == nil {
					t.Error("StreamOptions = nil, want non-nil")
				}
				if !req.StreamOptions.IncludeUsage {
					t.Error("StreamOptions.IncludeUsage = false, want true")
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
			messages: []gogpt.ChatCompletionMessage{},
			check: func(t *testing.T, req gogpt.ChatCompletionRequest) {
				if req.MaxTokens != 0 {
					t.Errorf("MaxTokens = %d, want 0", req.MaxTokens)
				}
				if req.MaxCompletionTokens != 0 {
					t.Errorf("MaxCompletionTokens = %d, want 0", req.MaxCompletionTokens)
				}
				if req.Temperature != 0 {
					t.Errorf("Temperature = %f, want 0", req.Temperature)
				}
			},
		},
		{
			name: "multiple messages",
			cfg: AIConfig{
				Model: "gpt-4",
			},
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleSystem, Content: "you are helpful"},
				{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
				{Role: gogpt.ChatMessageRoleAssistant, Content: "hi there"},
				{Role: gogpt.ChatMessageRoleUser, Content: "how are you"},
			},
			check: func(t *testing.T, req gogpt.ChatCompletionRequest) {
				if len(req.Messages) != 4 {
					t.Errorf("Messages len = %d, want 4", len(req.Messages))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := BuildChatRequest(tt.cfg, tt.messages)
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
			msg := gogpt.ChatCompletionMessage{
				Role:             gogpt.ChatMessageRoleAssistant,
				Content:          tt.content,
				ReasoningContent: tt.reasoningContent,
			}

			if msg.Content != tt.wantContent {
				t.Errorf("Content = %q, want %q", msg.Content, tt.wantContent)
			}
			if msg.ReasoningContent != tt.wantReasoning {
				t.Errorf("ReasoningContent = %q, want %q", msg.ReasoningContent, tt.wantReasoning)
			}
		})
	}
}
