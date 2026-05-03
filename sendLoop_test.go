package main

import (
	"strings"
	"testing"

	"github.com/knivey/dave/MarkdownToIRC/irc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapForIRC(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, got []string)
	}{
		{
			name:  "Short text no wrap",
			input: "hello world",
			check: func(t *testing.T, got []string) {
				require.Len(t, got, 1, "expected 1 line")
				assert.Equal(t, "hello world", got[0])
			},
		},
		{
			name:  "Multi-line input preserved",
			input: "line1\nline2\nline3",
			check: func(t *testing.T, got []string) {
				require.Len(t, got, 3, "expected 3 lines")
				assert.Equal(t, "line1", got[0], "line 0")
				assert.Equal(t, "line2", got[1], "line 1")
				assert.Equal(t, "line3", got[2], "line 2")
			},
		},
		{
			name:  "Empty string",
			input: "",
			check: func(t *testing.T, got []string) {
				require.Len(t, got, 1, "expected 1 line")
				assert.Equal(t, "", got[0], "expected empty string")
			},
		},
		{
			name:  "Long line wraps by bytes",
			input: strings.Repeat("x", 400),
			check: func(t *testing.T, got []string) {
				require.GreaterOrEqual(t, len(got), 2, "expected >= 2 lines")
				for i, line := range got {
					assert.LessOrEqual(t, len([]byte(line)), maxLineLen+10, "line %d exceeds byte budget", i)
				}
				stripped := irc.StripCodes(strings.Join(got, ""))
				assert.Equal(t, strings.Repeat("x", 400), stripped, "content mismatch after stripping codes")
			},
		},
		{
			name:  "Multi-line with one long line",
			input: "short\n" + strings.Repeat("y", 400) + "\nalso short",
			check: func(t *testing.T, got []string) {
				require.GreaterOrEqual(t, len(got), 3, "expected >= 3 lines")
				assert.Equal(t, "short", got[0], "line 0")
				last := got[len(got)-1]
				stripped := irc.StripCodes(strings.Join(got, ""))
				expected := "short" + strings.Repeat("y", 400) + "also short"
				assert.Equal(t, expected, stripped, "content mismatch")
				_ = last
			},
		},
		{
			name:  "Bold formatting preserved across wraps",
			input: "\x02" + strings.Repeat("hello ", 60) + "\x02",
			check: func(t *testing.T, got []string) {
				require.GreaterOrEqual(t, len(got), 2, "expected >= 2 lines")
				for i, line := range got {
					assert.LessOrEqual(t, len([]byte(line)), maxLineLen+10, "line %d exceeds byte budget", i)
				}
				assert.Equal(t, byte('\x02'), got[0][0], "first line should start with bold")
				for i := 1; i < len(got); i++ {
					if len(got[i]) > 0 {
						assert.Equal(t, byte('\x02'), got[i][0], "line %d should start with bold open code", i)
					}
				}
				stripped := irc.StripCodes(strings.Join(got, ""))
				assert.GreaterOrEqual(t, len(stripped), 358, "expected >= 358 visible chars after wrapping")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapForIRC(tt.input)
			tt.check(t, got)
		})
	}
}
