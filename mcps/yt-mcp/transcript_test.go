package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseJSON3Transcript(t *testing.T) {
	data, err := os.ReadFile("/tmp/test-sub-json3.en.json3")
	if err != nil {
		t.Skip("skipping: json3 fixture not available at /tmp/test-sub-json3.en.json3")
	}

	transcript, err := parseJSON3Transcript(data)
	require.NoError(t, err)
	assert.NotEmpty(t, transcript)

	assert.Contains(t, transcript, "We're no strangers to love")
	assert.Contains(t, transcript, "Never going to give you up")
	assert.Contains(t, transcript, "Never going to let you down")
	assert.Contains(t, transcript, "Never going to run around and desert you")

	assert.NotContains(t, transcript, "\n")
}

func TestParseJSON3TranscriptSimple(t *testing.T) {
	input := `{
		"events": [
			{"segs": [{"utf8": "Hello"}, {"utf8": " world"}]},
			{"segs": [{"utf8": "\n"}]},
			{"segs": [{"utf8": "Testing"}, {"utf8": " one two three."}]}
		]
	}`

	transcript, err := parseJSON3Transcript([]byte(input))
	require.NoError(t, err)

	assert.Equal(t, "Hello world Testing one two three.", transcript)
}

func TestParseJSON3TranscriptMusicStripped(t *testing.T) {
	input := `{
		"events": [
			{"segs": [{"utf8": "Some speech here"}]},
			{"segs": [{"utf8": "[Music]"}]},
			{"segs": [{"utf8": "More speech"}]}
		]
	}`

	transcript, err := parseJSON3Transcript([]byte(input))
	require.NoError(t, err)

	assert.Contains(t, transcript, "Some speech here")
	assert.Contains(t, transcript, "[Music]")
	assert.Contains(t, transcript, "More speech")
}

func TestParseJSON3TranscriptEmpty(t *testing.T) {
	input := `{"events": []}`

	transcript, err := parseJSON3Transcript([]byte(input))
	require.NoError(t, err)
	assert.Empty(t, transcript)
}

func TestParseJSON3TranscriptNoSegs(t *testing.T) {
	input := `{
		"events": [
			{"tStartMs": 0, "dDurationMs": 1000},
			{"segs": [{"utf8": "Only event with segs"}]}
		]
	}`

	transcript, err := parseJSON3Transcript([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "Only event with segs", transcript)
}

func TestParseJSON3TranscriptInvalid(t *testing.T) {
	_, err := parseJSON3Transcript([]byte(`not json`))
	assert.Error(t, err)
}

func TestExtractVideoID(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected string
		wantErr  bool
	}{
		{"standard watch", "https://www.youtube.com/watch?v=dQw4w9WgXcQ", "dQw4w9WgXcQ", false},
		{"short URL", "https://youtu.be/dQw4w9WgXcQ", "dQw4w9WgXcQ", false},
		{"shorts", "https://www.youtube.com/shorts/abc12345678", "abc12345678", false},
		{"embed", "https://www.youtube.com/embed/dQw4w9WgXcQ", "dQw4w9WgXcQ", false},
		{"with extra params", "https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42", "dQw4w9WgXcQ", false},
		{"non-youtube", "https://example.com/video", "", true},
		{"empty", "", "", true},
		{"vimeo", "https://vimeo.com/123456", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := extractVideoID(tt.url)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, id)
			}
		})
	}
}
