package main

import (
	"strings"
	"testing"

	"github.com/knivey/dave/MarkdownToIRC/irc"
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
				if len(got) != 1 {
					t.Fatalf("expected 1 line, got %d", len(got))
				}
				if got[0] != "hello world" {
					t.Errorf("expected %q, got %q", "hello world", got[0])
				}
			},
		},
		{
			name:  "Multi-line input preserved",
			input: "line1\nline2\nline3",
			check: func(t *testing.T, got []string) {
				if len(got) != 3 {
					t.Fatalf("expected 3 lines, got %d", len(got))
				}
				if got[0] != "line1" {
					t.Errorf("line 0: expected %q, got %q", "line1", got[0])
				}
				if got[1] != "line2" {
					t.Errorf("line 1: expected %q, got %q", "line2", got[1])
				}
				if got[2] != "line3" {
					t.Errorf("line 2: expected %q, got %q", "line3", got[2])
				}
			},
		},
		{
			name:  "Empty string",
			input: "",
			check: func(t *testing.T, got []string) {
				if len(got) != 1 {
					t.Fatalf("expected 1 line, got %d", len(got))
				}
				if got[0] != "" {
					t.Errorf("expected empty string, got %q", got[0])
				}
			},
		},
		{
			name:  "Long line wraps by bytes",
			input: strings.Repeat("x", 400),
			check: func(t *testing.T, got []string) {
				if len(got) < 2 {
					t.Fatalf("expected >= 2 lines, got %d", len(got))
				}
				for i, line := range got {
					if len([]byte(line)) > maxLineLen+10 {
						t.Errorf("line %d exceeds byte budget: %d bytes", i, len([]byte(line)))
					}
				}
				stripped := irc.StripCodes(strings.Join(got, ""))
				if stripped != strings.Repeat("x", 400) {
					t.Errorf("content mismatch after stripping codes")
				}
			},
		},
		{
			name:  "Multi-line with one long line",
			input: "short\n" + strings.Repeat("y", 400) + "\nalso short",
			check: func(t *testing.T, got []string) {
				if len(got) < 3 {
					t.Fatalf("expected >= 3 lines, got %d", len(got))
				}
				if got[0] != "short" {
					t.Errorf("line 0: expected %q, got %q", "short", got[0])
				}
				last := got[len(got)-1]
				stripped := irc.StripCodes(strings.Join(got, ""))
				expected := "short" + strings.Repeat("y", 400) + "also short"
				if stripped != expected {
					t.Errorf("content mismatch")
				}
				_ = last
			},
		},
		{
			name:  "Bold formatting preserved across wraps",
			input: "\x02" + strings.Repeat("hello ", 60) + "\x02",
			check: func(t *testing.T, got []string) {
				if len(got) < 2 {
					t.Fatalf("expected >= 2 lines, got %d", len(got))
				}
				for i, line := range got {
					if len([]byte(line)) > maxLineLen+10 {
						t.Errorf("line %d exceeds byte budget: %d bytes", i, len([]byte(line)))
					}
				}
				if got[0][0] != '\x02' {
					t.Errorf("first line should start with bold, got %q", got[0][:min(10, len(got[0]))])
				}
				for i := 1; i < len(got); i++ {
					if len(got[i]) > 0 && got[i][0] != '\x02' {
						t.Errorf("line %d should start with bold open code", i)
					}
				}
				stripped := irc.StripCodes(strings.Join(got, ""))
				if len(stripped) < 358 {
					t.Errorf("expected >= 358 visible chars after wrapping, got %d", len(stripped))
				}
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
