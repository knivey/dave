package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncateHistory(t *testing.T) {
	tests := []struct {
		name        string
		messages    []ChatMessage
		maxHistory  int
		wantLen     int
		wantFirstIs []ChatMessage
	}{
		{
			name:       "empty messages",
			messages:   []ChatMessage{},
			maxHistory: 10,
			wantLen:    0,
		},
		{
			name: "under limit",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi"},
			},
			maxHistory: 10,
			wantLen:    3,
		},
		{
			name: "exactly at limit",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "1"},
				{Role: RoleUser, Content: "2"},
				{Role: RoleUser, Content: "3"},
			},
			maxHistory: 3,
			wantLen:    4,
		},
		{
			name: "over limit keeps system prompt and last messages",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "1"},
				{Role: RoleAssistant, Content: "a1"},
				{Role: RoleUser, Content: "2"},
				{Role: RoleAssistant, Content: "a2"},
				{Role: RoleUser, Content: "3"},
				{Role: RoleAssistant, Content: "a3"},
			},
			maxHistory: 3,
			wantLen:    4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateHistory(tt.messages, tt.maxHistory)
			assert.Len(t, got, tt.wantLen, "TruncateHistory() len")
			if len(got) > 0 {
				assert.Equal(t, RoleSystem, got[0].Role, "TruncateHistory()[0].Role")
			}
		})
	}
}
