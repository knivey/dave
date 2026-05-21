package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseEnabledTools(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]bool
	}{
		{
			name:     "empty string means all enabled",
			input:    "",
			expected: nil,
		},
		{
			name:     "single tool",
			input:    "web_search",
			expected: map[string]bool{"web_search": true},
		},
		{
			name:     "multiple tools",
			input:    "web_search, news_search, image_search",
			expected: map[string]bool{"web_search": true, "news_search": true, "image_search": true},
		},
		{
			name:     "whitespace trimmed",
			input:    " web_search , news_search ",
			expected: map[string]bool{"web_search": true, "news_search": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseEnabledTools(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRegisterToolsAll(t *testing.T) {
	count := countRegisteredTools(nil)
	assert.Equal(t, 8, count)
}

func TestRegisterToolsWhitelist(t *testing.T) {
	count := countRegisteredTools(map[string]bool{"brave_web_search": true, "brave_news_search": true})
	assert.Equal(t, 2, count)
}

func TestRegisterToolsUnknownName(t *testing.T) {
	count := countRegisteredTools(map[string]bool{"brave_web_search": true, "nonexistent": true})
	assert.Equal(t, 1, count)
}

func countRegisteredTools(enabled map[string]bool) int {
	count := 0
	for _, name := range allToolNames {
		if enabled != nil && !enabled[name] {
			continue
		}
		count++
	}
	return count
}
