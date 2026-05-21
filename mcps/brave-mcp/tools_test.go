package main

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
			name:     "short name normalized",
			input:    "web_search",
			expected: map[string]bool{"brave_web_search": true},
		},
		{
			name:     "multiple short names",
			input:    "web_search, news_search, image_search",
			expected: map[string]bool{"brave_web_search": true, "brave_news_search": true, "brave_image_search": true},
		},
		{
			name:     "whitespace trimmed",
			input:    " web_search , news_search ",
			expected: map[string]bool{"brave_web_search": true, "brave_news_search": true},
		},
		{
			name:     "full name passthrough",
			input:    "brave_web_search",
			expected: map[string]bool{"brave_web_search": true},
		},
		{
			name:     "mixed short and full",
			input:    "web_search, brave_news_search",
			expected: map[string]bool{"brave_web_search": true, "brave_news_search": true},
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
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	client := newBraveClient("test-key", "https://example.com", 0, "US", "en")
	handlers := NewToolHandlers(client)
	assert.NotPanics(t, func() { registerTools(server, handlers, nil) })
}

func TestRegisterToolsWhitelist(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	client := newBraveClient("test-key", "https://example.com", 0, "US", "en")
	handlers := NewToolHandlers(client)
	assert.NotPanics(t, func() {
		registerTools(server, handlers, map[string]bool{"brave_web_search": true, "brave_news_search": true})
	})
}

func TestRegisterToolsUnknownName(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	client := newBraveClient("test-key", "https://example.com", 0, "US", "en")
	handlers := NewToolHandlers(client)
	assert.NotPanics(t, func() {
		registerTools(server, handlers, map[string]bool{"brave_web_search": true, "nonexistent": true})
	})
}

func TestParseAndRegister(t *testing.T) {
	enabled := parseEnabledTools("web_search, news_search")
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	client := newBraveClient("test-key", "https://example.com", 0, "US", "en")
	handlers := NewToolHandlers(client)
	assert.NotPanics(t, func() { registerTools(server, handlers, enabled) })
}
