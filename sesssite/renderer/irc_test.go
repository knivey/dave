package renderer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIRC_Bold(t *testing.T) {
	input := "normal\x02bold\x02normal"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-bold`)
	assert.Contains(t, result, "bold")
}

func TestIRC_Italic(t *testing.T) {
	input := "normal\x1ditalic\x1dnormal"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-italic`)
}

func TestIRC_Underline(t *testing.T) {
	input := "normal\x1funderline\x1fnormal"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-underline`)
}

func TestIRC_Strikethrough(t *testing.T) {
	input := "normal\x1estrike\x1enormal"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-strikethrough`)
}

func TestIRC_Monospace(t *testing.T) {
	input := "normal\x11mono\x11normal"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-monospace`)
}

func TestIRC_ColorFg(t *testing.T) {
	input := "normal\x034red text"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-fg-4`)
}

func TestIRC_ColorFgBg(t *testing.T) {
	input := "\x034,12red on blue"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-fg-4`)
	assert.Contains(t, result, `irc-bg-12`)
}

func TestIRC_ColorReset(t *testing.T) {
	input := "\x034red\x03 plain"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-fg-4`)
}

func TestIRC_ColorTwoDigit(t *testing.T) {
	input := "\x0312light blue"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-fg-12`)
}

func TestIRC_Reverse(t *testing.T) {
	input := "\x034,12\x16reversed"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-fg-12`)
	assert.Contains(t, result, `irc-bg-4`)
}

func TestIRC_Reset(t *testing.T) {
	input := "\x02\x034bold red\x0f plain"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "irc-bold")
	assert.Contains(t, result, "irc-fg-4")
	body := result[strings.Index(result, "</span>")+len("</span>"):]
	assert.NotContains(t, body, "irc-bold")
}

func TestIRC_Spoiler(t *testing.T) {
	input := "\x034,4hidden text"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-spoiler`)
}

// DIVERGED from the parsebin original (deliberate, Nov 2026): inline
// code spans STRIP IRC control codes instead of passing them through
// — the site runs BBCode translation before Render, and leftover
// \x03NN bytes inside code text would render the color digits as
// visible text. The span content still gets no irc-* styling.
func TestIRC_CodeSpanUntouched(t *testing.T) {
	input := "normal `\x02bold\x02 code` normal"
	result, err := Render(input)
	assert.NoError(t, err)
	codeStart := strings.Index(result, "<code>")
	codeEnd := strings.Index(result, "</code>")
	codeContent := result[codeStart+len("<code>") : codeEnd]
	assert.NotContains(t, codeContent, "irc-bold")
	assert.Contains(t, codeContent, "bold code", "text survives, codes stripped")
	for _, b := range []byte{0x02, 0x03, 0x04, 0x0f, 0x11, 0x16, 0x1d, 0x1e, 0x1f} {
		assert.NotContains(t, codeContent, string(rune(b)), "control byte %02x stripped", b)
	}
}

// TestIRC_CodeSpanFixDoesNotEatSiblings pins the collect-then-mutate
// discipline: rebuilding a code span DURING the walk broke goldmark's
// sibling traversal (RemoveChild nils the removed node's next-sibling),
// silently dropping IRC transformation for everything after the first
// rebuilt span in the paragraph — and leaving LATER spans unstripped.
func TestIRC_CodeSpanFixDoesNotEatSiblings(t *testing.T) {
	input := "a `\x02x\x02` mid \x02bold\x02 and `\x02y\x02` end"
	result, err := Render(input)
	assert.NoError(t, err)

	// the styled text BETWEEN the spans keeps its transformation
	assert.Contains(t, result, `class="irc-bold"`, "bold after the first span still styled")

	// BOTH code spans get stripped (the second one was silently missed
	// by the mid-walk mutation)
	chunks := splitCodeChunks(t, result)
	require.Len(t, chunks, 2)
	assert.Equal(t, "x", chunks[0])
	assert.Equal(t, "y", chunks[1])
	for _, chunk := range chunks {
		assert.NotContains(t, chunk, "\x02")
	}
}

// splitCodeChunks extracts the text between each <code>…</code> pair.
func splitCodeChunks(t *testing.T, html string) []string {
	t.Helper()
	var out []string
	for {
		i := strings.Index(html, "<code>")
		if i < 0 {
			return out
		}
		j := strings.Index(html[i:], "</code>")
		if j < 0 {
			t.Fatal("unclosed <code>")
		}
		out = append(out, html[i+6:i+j])
		html = html[i+j+len("</code>"):]
	}
}

// TestIRC_MultiLineCodeSpanNotRebuilt: \n inside an inline code span is
// not an IRC control code — the rebuild path must not trigger (the gate
// is hasIRCControl, not hasIRCCodes' any-byte-below-0x20).
func TestIRC_MultiLineCodeSpanNotRebuilt(t *testing.T) {
	input := "before `line1\nline2` after \x02bold\x02"
	result, err := Render(input)
	assert.NoError(t, err)
	// NOT rebuilt ⇒ goldmark's own code-span renderer handles the
	// newline (CommonMark: newline → space). A rebuilt span would keep
	// the literal \n — this assertion isolates the narrow gate.
	assert.Contains(t, result, "line1 line2")
	assert.Contains(t, result, `class="irc-bold"`, "the rest of the paragraph still transforms")
}

func TestIRC_FencedCodeUntouched(t *testing.T) {
	input := "```\n\x02bold\x02 in code\n```"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.NotContains(t, result, "irc-bold")
	assert.Contains(t, result, "\x02")
}

func TestIRC_MultipleFormats(t *testing.T) {
	input := "\x02\x034bold red\x0f plain"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "irc-bold")
	assert.Contains(t, result, "irc-fg-4")
}

func TestIRC_HexColorStripped(t *testing.T) {
	input := "\x04FF0000red text"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.NotContains(t, result, "FF0000")
	assert.Contains(t, result, "red text")
}

func TestIRC_PlainTextPassthrough(t *testing.T) {
	input := "just normal text"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.NotContains(t, result, "irc-")
}

func TestIRC_Color99(t *testing.T) {
	input := "\x034colored\x0399 default"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "irc-fg-4")
}

func TestIRC_ExtendedColor16(t *testing.T) {
	input := "\x0316extended"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "irc-fg-16")
}

func TestIRC_ExtendedColorBg(t *testing.T) {
	input := "\x034,16red on ext"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "irc-fg-4")
	assert.Contains(t, result, "irc-bg-16")
}

func TestParseIRCSegments_BoldToggle(t *testing.T) {
	data := []byte("a\x02b\x02c")
	segs := parseIRCSegments(data)
	assert.Equal(t, 3, len(segs))
	assert.Equal(t, "a", string(segs[0].text))
	assert.False(t, segs[0].state.bold)
	assert.Equal(t, "b", string(segs[1].text))
	assert.True(t, segs[1].state.bold)
	assert.Equal(t, "c", string(segs[2].text))
	assert.False(t, segs[2].state.bold)
}

func TestParseIRCSegments_ColorParsing(t *testing.T) {
	data := []byte("\x034,12hello")
	segs := parseIRCSegments(data)
	assert.Equal(t, 1, len(segs))
	assert.Equal(t, "hello", string(segs[0].text))
	assert.Equal(t, 4, segs[0].state.fg)
	assert.Equal(t, 12, segs[0].state.bg)
}

func TestIRC_ControlCharsStripped(t *testing.T) {
	input := "text\x01\x07\x1bmore"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.NotContains(t, result, "\x01")
	assert.NotContains(t, result, "\x07")
	assert.NotContains(t, result, "\x1b")
	assert.Contains(t, result, "text")
	assert.Contains(t, result, "more")
}

func TestIRC_ColorCarriesAcrossSplitNodes(t *testing.T) {
	input := "\x034red text here"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, `irc-fg-4`)
	assert.Contains(t, result, "red")
	assert.Contains(t, result, "text")
	assert.Contains(t, result, "here")

	lastSpanClose := strings.LastIndex(result, "</span>")
	afterSpan := result[lastSpanClose+len("</span>"):]
	afterSpan = strings.TrimSpace(afterSpan)
	assert.NotContains(t, afterSpan, "here", "last word should be inside the colored span, not after it")
}

func TestIRC_ColorCarriesAcrossLines(t *testing.T) {
	input := "\x034line one\nline two"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "line")
	assert.Contains(t, result, "two")

	count := strings.Count(result, "irc-fg-4")
	assert.GreaterOrEqual(t, count, 2, "both lines should have the color class")

	assert.NotContains(t, result, ">one</span><br />", "line break should come after full colored text, not mid-word")
}

func TestIRC_BoldCarriesAcrossSplitNodes(t *testing.T) {
	input := "\x02bold text here"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "irc-bold")
	assert.Contains(t, result, "bold")
	assert.Contains(t, result, "text")
	assert.Contains(t, result, "here")

	lastSpanClose := strings.LastIndex(result, "</span>")
	afterSpan := result[lastSpanClose+len("</span>"):]
	afterSpan = strings.TrimSpace(afterSpan)
	assert.NotContains(t, afterSpan, "here", "last word should be inside the bold span")
}

func TestIRC_ResetStopsCarryAcrossLines(t *testing.T) {
	input := "\x034red\x0f\nplain line"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "irc-fg-4")
	assert.Contains(t, result, "red")
	assert.Contains(t, result, "plain line")

	body := result[strings.Index(result, "red")+3:]
	plainIdx := strings.Index(body, "plain line")
	beforePlain := body[:plainIdx]
	assert.NotContains(t, beforePlain, "irc-fg-4", "text after reset should not carry color")
}

func TestIRC_SoftLineBreakPreserved(t *testing.T) {
	input := "\x034line one\nline two"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "<br />")
}

func TestIRC_ExactBugReproducer(t *testing.T) {
	input := "\x034 Tune in next week, phil! Heh, those mud people never stood a chance."
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "chance.")
	assert.Contains(t, result, "irc-fg-4")

	lastSpanClose := strings.LastIndex(result, "</span>")
	afterSpan := result[lastSpanClose+len("</span>"):]
	afterSpan = strings.TrimSpace(afterSpan)
	afterSpan = strings.TrimSuffix(afterSpan, "</p>")
	afterSpan = strings.TrimSpace(afterSpan)
	assert.Empty(t, afterSpan, "no text should appear after the last </span>: got %q", afterSpan)
}

func TestParseIRCSegmentsWithState_InheritedColor(t *testing.T) {
	initial := ircState{fg: 4}
	segs, finalState := parseIRCSegmentsWithState([]byte("plain text"), initial)
	assert.Equal(t, 1, len(segs))
	assert.Equal(t, "plain text", string(segs[0].text))
	assert.Equal(t, 4, segs[0].state.fg)
	assert.Equal(t, 4, finalState.fg)
}

func TestParseIRCSegmentsWithState_Reset(t *testing.T) {
	initial := ircState{fg: 4, bold: true}
	segs, finalState := parseIRCSegmentsWithState([]byte("\x0fplain"), initial)
	assert.Equal(t, 1, len(segs))
	assert.Equal(t, "plain", string(segs[0].text))
	assert.False(t, finalState.bold)
	assert.Equal(t, -1, finalState.fg)
}
