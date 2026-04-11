package markdowntoirc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/knivey/dave/MarkdownToIRC/irc"
)

func testStreamingIncremental(t *testing.T, input string, contain, notContain []string) {
	t.Helper()
	r := NewStreamingRenderer()
	var output strings.Builder
	for i := 0; i < len(input); i += 5 {
		end := i + 5
		if end > len(input) {
			end = len(input)
		}
		chunk := input[i:end]
		for _, line := range r.Process(chunk) {
			output.WriteString(line)
			output.WriteString("\n")
		}
	}
	for _, line := range r.Process("") {
		output.WriteString(line)
		output.WriteString("\n")
	}
	got := output.String()
	for _, want := range contain {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\ngot:  %s\nwant: %s", humanize(want), humanize(got), humanize(want))
		}
	}
	for _, notwant := range notContain {
		if strings.Contains(got, notwant) {
			t.Errorf("output unexpectedly contains %q\ngot:  %s", humanize(notwant), humanize(got))
		}
	}
}

func TestStreamingMarkdown(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		contain    []string
		notContain []string
	}{
		{
			name:       "nested lists",
			input:      "- item1\n  - subitem1\n- item2",
			contain:    []string{"• item1", "  • subitem1", "• item2"},
			notContain: []string{},
		},
		{
			name:       "block quotes",
			input:      "> quote line 1\n> quote line 2",
			contain:    []string{"\x0309> quote line 1 quote line 2"},
			notContain: []string{},
		},
		{
			name:       "code blocks",
			input:      "```\ncode line 1\ncode line 2\n```",
			contain:    []string{" \x030,90code line 1", " \x030,90code line 2"},
			notContain: []string{},
		},
		{
			name:       "code blocks with blank lines",
			input:      "```\ncode line 1\n\ncode line 2\n```",
			contain:    []string{" \x030,90code line 1", " \x030,90                 ", " \x030,90code line 2"},
			notContain: []string{},
		},
		{
			name:       "mixed content",
			input:      "# Header\n\nParagraph.\n\n- list item",
			contain:    []string{"\x02Header\x02", "Paragraph.", "• list item"},
			notContain: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testStreamingIncremental(t, tt.input, tt.contain, tt.notContain)
		})
	}
}

func TestUserProvidedExample(t *testing.T) {
	input := "# This is a header\n\nThis is a paragraph.\n\n```\ncode block line 1\n\ncode block line 2\n```\n\n- list item 1\n- list item 2"
	expected := "\x02This is a header\x02\nThis is a paragraph.\n \x030,90code block line 1\x03 \n \x030,90                 \x03 \n \x030,90code block line 2\x03 \n • list item 1\n • list item 2"
	got := MarkdownToIRC(input)
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestStreamingCodeBlockWidthCapping(t *testing.T) {
	longLine := strings.Repeat("a", 120)
	shortLine := "short"

	stripAll := func(s string) string {
		return irc.StripCodes(s)
	}

	t.Run("PlainCodeBlockAllLinesUnder80", func(t *testing.T) {
		input := "```\nfoo\nbarbaz\nqux\n```"
		got := streamingRender(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) < 2 {
			t.Fatalf("expected multiple code lines, got %d", len(codeLines))
		}
		firstLen := utf8.RuneCountInString(codeLines[0])
		secondLen := utf8.RuneCountInString(codeLines[1])
		if firstLen != secondLen {
			t.Errorf("expected all lines padded to same length, got %d vs %d", firstLen, secondLen)
		}
	})

	t.Run("PlainCodeBlockMixedLengths", func(t *testing.T) {
		input := "```\n" + shortLine + "\n" + longLine + "\n```"
		got := streamingRender(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, l)
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		shortClean := stripAll(codeLines[0])
		longClean := stripAll(codeLines[1])
		shortLen := utf8.RuneCountInString(shortClean)
		longLen := utf8.RuneCountInString(longClean)
		expectedPad := maxCodeBlockPadWidth + 2
		if shortLen != expectedPad {
			t.Errorf("short line padded to %d chars, want %d (80 + borders), got: %q", shortLen, expectedPad, shortClean)
		}
		if longLen <= shortLen {
			t.Errorf("long line should exceed padded width, got length %d vs short %d", longLen, shortLen)
		}
	})

	t.Run("PlainCodeBlockAllLinesOver80", func(t *testing.T) {
		line1 := strings.Repeat("x", 90)
		line2 := strings.Repeat("y", 100)
		input := "```\n" + line1 + "\n" + line2 + "\n```"
		got := streamingRender(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		len1 := utf8.RuneCountInString(codeLines[0])
		len2 := utf8.RuneCountInString(codeLines[1])
		if len1 == len2 {
			t.Errorf("expected different lengths when all lines exceed 80, got both %d", len1)
		}
	})
}

func TestStreamingHighlightedCodeBlockWidthCapping(t *testing.T) {
	longLine := strings.Repeat("x", 120)
	shortLine := "x"

	stripAll := func(s string) string {
		return irc.StripCodes(s)
	}

	t.Run("MixedLengthsWithLanguage", func(t *testing.T) {
		input := "```go\n" + shortLine + "\n" + longLine + "\n```"
		got := streamingRender(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, l)
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		shortClean := stripAll(codeLines[0])
		longClean := stripAll(codeLines[1])
		shortLen := utf8.RuneCountInString(shortClean)
		longLen := utf8.RuneCountInString(longClean)
		expectedPad := maxCodeBlockPadWidth + 2
		if shortLen != expectedPad {
			t.Errorf("short line padded to %d chars, want %d (80 + borders), got: %q", shortLen, expectedPad, shortClean)
		}
		if longLen <= shortLen {
			t.Errorf("long line should exceed padded width, got length %d vs short %d", longLen, shortLen)
		}
	})

	t.Run("AllLinesOver80WithLanguage", func(t *testing.T) {
		line1 := strings.Repeat("a", 90)
		line2 := strings.Repeat("b", 100)
		input := "```python\n" + line1 + "\n" + line2 + "\n```"
		got := streamingRender(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		len1 := utf8.RuneCountInString(codeLines[0])
		len2 := utf8.RuneCountInString(codeLines[1])
		if len1 == len2 {
			t.Errorf("expected different lengths when all lines exceed 80, got both %d", len1)
		}
	})

	t.Run("AllLinesUnder80WithLanguage", func(t *testing.T) {
		input := "```go\nfoo\nbar\n```"
		got := streamingRender(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) < 2 {
			t.Fatalf("expected multiple code lines, got %d", len(codeLines))
		}
		firstLen := utf8.RuneCountInString(codeLines[0])
		secondLen := utf8.RuneCountInString(codeLines[1])
		if firstLen != secondLen {
			t.Errorf("expected all lines padded to same length, got %d vs %d", firstLen, secondLen)
		}
	})
}

func streamingRender(input string) string {
	r := NewStreamingRenderer()
	var output strings.Builder
	for _, line := range r.Process(input) {
		output.WriteString(line)
		output.WriteString("\n")
	}
	for _, line := range r.Process("") {
		output.WriteString(line)
		output.WriteString("\n")
	}
	return output.String()
}

func TestStreamingFromTestData(t *testing.T) {
	// Test data in testdata/streaming/ should contain:
	// - .md file with markdown input
	// - .irc file with expected output (use [COLOR], [BOLD], etc., will be dehumanized)
	//
	// Note: .irc files include leading/trailing newlines as produced by streamingRender().
	// Use [COLOR] for \x03, [BOLD] for \x02, etc. (see dehumanize in helpers_test.go)

	testDataDir := "testdata/streaming"
	files, err := os.ReadDir(testDataDir)
	if err != nil {
		t.Fatalf("failed to read testdata directory: %v", err)
	}

	mdFiles := make(map[string]string)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".md") {
			mdFiles[f.Name()] = filepath.Join(testDataDir, f.Name())
		}
	}

	for mdFile := range mdFiles {
		name := strings.TrimSuffix(mdFile, ".md")
		mdPath := filepath.Join(testDataDir, mdFile)
		ircPath := filepath.Join(testDataDir, name+".irc")

		ircData, err := os.ReadFile(ircPath)
		if err != nil {
			t.Errorf("failed to read %s: %v", ircPath, err)
			continue
		}
		expected := dehumanize(string(ircData))

		mdData, err := os.ReadFile(mdPath)
		if err != nil {
			t.Errorf("failed to read %s: %v", mdPath, err)
			continue
		}
		input := string(mdData)

		got := streamingRender(input)

		if got != expected {
			t.Errorf("output mismatch for %s\ngot:\n%s\nwant:\n%s", name, humanize(got), humanize(expected))
		}
	}
}
