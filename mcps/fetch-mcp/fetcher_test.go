package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPaginate(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		startIndex int
		maxLength  int
		expected   string
		truncated  bool
	}{
		{
			name:       "full content fits",
			content:    "hello world",
			startIndex: 0,
			maxLength:  100,
			expected:   "hello world",
			truncated:  false,
		},
		{
			name:       "truncated",
			content:    "hello world",
			startIndex: 0,
			maxLength:  5,
			expected:   "hello",
			truncated:  true,
		},
		{
			name:       "start index past content",
			content:    "hello",
			startIndex: 10,
			maxLength:  100,
			expected:   "",
			truncated:  false,
		},
		{
			name:       "start in middle",
			content:    "hello world",
			startIndex: 6,
			maxLength:  100,
			expected:   "world",
			truncated:  false,
		},
		{
			name:       "start in middle truncated",
			content:    "hello world",
			startIndex: 6,
			maxLength:  3,
			expected:   "wor",
			truncated:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := paginate(tt.content, tt.startIndex, tt.maxLength)
			assert.Equal(t, tt.expected, got)
			assert.Equal(t, tt.truncated, truncated)
		})
	}
}

func TestNextIndex(t *testing.T) {
	assert.Equal(t, 10, nextIndex(0, 10, 100))
	assert.Equal(t, 20, nextIndex(10, 10, 100))
	assert.Equal(t, 0, nextIndex(90, 10, 100))
	assert.Equal(t, 0, nextIndex(95, 10, 100))
}

func TestIsHTML(t *testing.T) {
	assert.True(t, isHTML("text/html; charset=utf-8", nil))
	assert.True(t, isHTML("application/xhtml+xml", nil))
	assert.True(t, isHTML("text/plain", []byte("<html><body>hello</body></html>")))
	assert.False(t, isHTML("application/json", []byte(`{"key":"value"}`)))
	assert.False(t, isHTML("text/plain", []byte(`just some text`)))
}

func TestExtractTitle(t *testing.T) {
	assert.Equal(t, "My Page", extractTitle([]byte("<html><head><title>My Page</title></head></html>")))
	assert.Equal(t, "My Page", extractTitle([]byte("<html><head><TITLE>My Page</TITLE></head></html>")))
	assert.Equal(t, "", extractTitle([]byte("<html><body>no title</body></html>")))
	assert.Equal(t, "", extractTitle([]byte("")))
}
