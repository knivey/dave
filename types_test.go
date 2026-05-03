package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummarizeMessages(t *testing.T) {
	longContent := strings.Repeat("a", 100)

	tests := []struct {
		name            string
		messages        []ChatMessage
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "empty messages",
			messages:     []ChatMessage{},
			wantContains: []string{"[0 messages, 0 turns]"},
		},
		{
			name: "few turns shows all",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "you are helpful"},
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi there"},
			},
			wantContains: []string{
				"[3 messages, 2 turns]",
				"Turn 0: #0 system: you are helpful",
				"Turn 1: #1 user: hello + #2 assistant: hi there",
			},
		},
		{
			name: "long content truncated",
			messages: []ChatMessage{
				{Role: RoleUser, Content: longContent},
			},
			wantContains:    []string{"[1 messages, 1 turns]", "#0 user: " + strings.Repeat("a", 50) + "..."},
			wantNotContains: []string{longContent},
		},
		{
			name: "multibyte utf8 truncated at rune boundary",
			messages: []ChatMessage{
				{Role: RoleUser, Content: strings.Repeat("🎉", 60)},
			},
			wantContains:    []string{strings.Repeat("🎉", 50) + "..."},
			wantNotContains: []string{strings.Repeat("🎉", 51)},
		},
		{
			name: "many turns collapses middle",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "1"},
				{Role: RoleAssistant, Content: "a1"},
				{Role: RoleUser, Content: "2"},
				{Role: RoleAssistant, Content: "a2"},
				{Role: RoleUser, Content: "3"},
				{Role: RoleAssistant, Content: "a3"},
				{Role: RoleUser, Content: "4"},
				{Role: RoleAssistant, Content: "a4"},
				{Role: RoleUser, Content: "5"},
				{Role: RoleAssistant, Content: "a5"},
				{Role: RoleUser, Content: "6"},
			},
			wantContains: []string{
				"[12 messages, 7 turns]",
				"Turn 0: #0 system: sys",
				"Turn 1: #1 user: 1 + #2 assistant: a1",
				"Turn 2: #3 user: 2 + #4 assistant: a2",
				"1 turns (#3-#3) omitted",
				"Turn 4: #7 user: 4 + #8 assistant: a4",
				"Turn 5: #9 user: 5 + #10 assistant: a5",
				"Turn 6: #11 user: 6",
			},
			wantNotContains: []string{"Turn 3"},
		},
		{
			name: "tool calls noted",
			messages: []ChatMessage{
				{Role: RoleAssistant, Content: "let me check", ToolCalls: []ToolCall{{ID: "tc1"}, {ID: "tc2"}}},
			},
			wantContains: []string{"[tool_calls: 2]"},
		},
		{
			name: "image part shown",
			messages: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,abc123"}},
						{Type: PartTypeText, Text: "describe this"},
					},
				},
			},
			wantContains: []string{"[image]", "describe this"},
		},
		{
			name: "reasoning content noted",
			messages: []ChatMessage{
				{Role: RoleAssistant, Content: "answer", ReasoningContent: "thinking..."},
			},
			wantContains: []string{"[reasoning]"},
		},
		{
			name: "newlines replaced with spaces",
			messages: []ChatMessage{
				{Role: RoleUser, Content: "hello\nworld\nfoo"},
			},
			wantContains:    []string{"hello world foo"},
			wantNotContains: []string{"hello\n"},
		},
		{
			name: "background task messages grouped before user",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "You are a bot"},
				{Role: RoleAssistant, Content: "here is a pic"},
				{Role: RoleSystem, Content: "[System: Background task completed"},
				{Role: RoleAssistant, Content: "another pic"},
				{Role: RoleSystem, Content: "[System: Background task completed"},
				{Role: RoleAssistant, Content: "last pic"},
				{Role: RoleUser, Content: "lol"},
				{Role: RoleAssistant, Content: "heh yeah"},
				{Role: RoleUser, Content: "ye"},
			},
			wantContains: []string{
				"[9 messages, 3 turns]",
				"Turn 0: #0 system: You are a bot + #1 assistant: here is a pic + #2 system: [System: Background task",
				"Turn 1: #6 user: lol + #7 assistant: heh yeah",
				"Turn 2: #8 user: ye",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeMessages(tt.messages)
			for _, want := range tt.wantContains {
				assert.Contains(t, got, want, "summarizeMessages()")
			}
			for _, notWant := range tt.wantNotContains {
				assert.NotContains(t, got, notWant, "summarizeMessages() should NOT contain")
			}
		})
	}
}

func TestBuildTurns(t *testing.T) {
	tests := []struct {
		name     string
		messages []ChatMessage
		want     []messageTurn
	}{
		{
			name:     "empty",
			messages: []ChatMessage{},
			want:     nil,
		},
		{
			name: "single system",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
			},
			want: []messageTurn{{start: 0, end: 1}},
		},
		{
			name: "system then user assistant",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "hi"},
				{Role: RoleAssistant, Content: "hello"},
			},
			want: []messageTurn{
				{start: 0, end: 1},
				{start: 1, end: 3},
			},
		},
		{
			name: "assistant system grouped before user",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleAssistant, Content: "pic"},
				{Role: RoleSystem, Content: "bg done"},
				{Role: RoleUser, Content: "lol"},
			},
			want: []messageTurn{
				{start: 0, end: 3},
				{start: 3, end: 4},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildTurns(tt.messages)
			require.Len(t, got, len(tt.want), "buildTurns() = %v, want %v", got, tt.want)
			for i := range got {
				assert.Equal(t, tt.want[i], got[i], "buildTurns()[%d]", i)
			}
		})
	}
}
