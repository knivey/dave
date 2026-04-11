package markdowntoirc

import (
	"strings"
	"testing"

	"github.com/knivey/dave/MarkdownToIRC/irc"
)

func humanize(s string) string {
	s = strings.ReplaceAll(s, "\x02", "[BOLD]")
	s = strings.ReplaceAll(s, "\x1D", "[ITALIC]")
	s = strings.ReplaceAll(s, "\x1F", "[UNDERLINE]")
	s = strings.ReplaceAll(s, "\x03", "[COLOR]")
	s = strings.ReplaceAll(s, "\u2022", "[BULLET]")
	return s
}

func dehumanize(s string) string {
	s = strings.ReplaceAll(s, "[BOLD]", "\x02")
	s = strings.ReplaceAll(s, "[ITALIC]", "\x1D")
	s = strings.ReplaceAll(s, "[UNDERLINE]", "\x1F")
	s = strings.ReplaceAll(s, "[COLOR]", "\x03")
	s = strings.ReplaceAll(s, "[BULLET]", "\u2022")
	return s
}

type mdTest struct {
	name       string
	input      string
	contain    []string
	notContain []string
}

func runTests(t *testing.T, tests []mdTest) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToIRC(tt.input)
			for _, want := range tt.contain {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\ngot:  %s\nwant: %s", humanize(want), humanize(got), humanize(want))
				}
			}
			for _, notwant := range tt.notContain {
				if strings.Contains(got, notwant) {
					t.Errorf("output unexpectedly contains %q\ngot:  %s", humanize(notwant), humanize(got))
				}
			}
		})
	}
}

func runTestsStripIRC(t *testing.T, tests []struct {
	name         string
	input        string
	lines        []string
	checkBgColor bool
}) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToIRC(tt.input)
			if tt.checkBgColor && !strings.Contains(got, "\x030,90") {
				t.Errorf("missing background color code \\x030,90\ngot: %s", humanize(got))
			}
			stripped := irc.StripCodes(got)
			gotLines := strings.Split(stripped, "\n")

			if len(gotLines) > 0 && gotLines[0] == "" {
				gotLines = gotLines[1:]
			}

			if len(gotLines) != len(tt.lines) {
				t.Errorf("line count mismatch: got %d, want %d", len(gotLines), len(tt.lines))
				return
			}
			for i, want := range tt.lines {
				if gotLines[i] != want {
					t.Errorf("line %d mismatch\ngot:  %q\nwant: %q", i, gotLines[i], want)
				}
			}
		})
	}
}
