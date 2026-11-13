package markdowntoirc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func bb(s string) string { return MarkdownToIRCOpts(BBRenderOptions{BBCode: true}, s) }

func TestBBRenderSemantics(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"basic", "[color=red]x[/color]", "\x0304x\x03"},
		{"nested stack", "[color=red]a[color=blue]b[/color]c[/color]d", "\x0304a\x0302b\x0304c\x03d"},
		{"placeholder pairs", "a[color=bogus]b[/color]c", "abc"},
		{"paragraph re-emit", "[color=red]a\n\nb[/color]", "\x0304a\n\x0304b\x03"},
		// P-1 correction: a trailing source newline never reaches the
		// render (KindText trims it — probed: "[color=red]a\n" renders
		// "[color=red]a"). The suppression the brief pins — an open
		// color with a written newline and NOTHING visible after — is
		// produced by an empty trailing fence, whose leading "\n" is
		// the last write. autoClose must then stay silent.
		{"auto-close suppressed after newline", "[color=red]a\n\n```", "\x0304a\n"},
		{"auto-close when open", "[color=red]a", "\x0304a\x03"},
		{"stray close stripped", "a[/color]b", "ab"},
		{"toggles verbatim", "[b]x[/b]", "[b]x[/b]"},
		{"tags literal in fence", "```\n[color=red]x[/color]\n```", " \x030,90[color=red]x[/color]\x03 "},
		{"tags literal in inline code", "use `[color=red]`", "use \x030,90[color=red]\x03"},
		{"unknown token verbatim", "see [table] and [1]", "see [table] and [1]"},
		{"link with unknown label still link", "[b](http://x.example)", "b: http://x.example"},
		{"colors beat links", "[color=red](http://x.example)", "\x0304(http://x.example)\x03"},
		// P-1 correction: termtex renders "$[color=red]x$" successfully
		// as "[color = red]x" (probed) — the raw-delimiter fallback
		// does not fire. Intent kept: the math parser consumes the tag,
		// so no IRC color code ever comes out of it.
		{"tags literal in math", "$[color=red]x$", "[color = red]x"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, bb(tc.in), tc.name)
	}
}

// The session-3178 fixture: bold AND color, every line (today: literal **).
func TestBBRenderFlanking(t *testing.T) {
	in := "[color=cyan]**A**\n**B**\n**C**[/color]"
	out := bb(in)
	assert.Contains(t, out, "\x02A\x02")
	assert.Contains(t, out, "\x02C\x02")
	assert.NotContains(t, out, "**")
	assert.Contains(t, out, "\x0310", "colored")
}

func TestBBRenderGuards(t *testing.T) {
	assert.Equal(t, "\x0304\x02\x025x\x03", bb("[color=red]5x[/color]"), "guard_after_open")
	// P-1 correction: "a\n5" is a SOFT break (renders as "a 5"); the
	// re-emit-then-digit guard needs a real output newline, so the digit
	// opens its own paragraph.
	assert.Equal(t, "\x0304a\n\x0304\x02\x025\x03", bb("[color=red]a\n\n5[/color]"), "guard_after_reemit")
	// P-1 correction: the brief's literal omitted the autoClose tail, but
	// a real color (red, re-opened) is still open at end of render, so the
	// one-shot entry point appends it.
	assert.Equal(t, "\x0304a\x0304\x02\x025\x03", bb("[color=red]a[color=red]5"), "guard_after_close")
}

func TestBBRenderSpanCrossingCodeBlock(t *testing.T) {
	out := bb("[color=red]before\n```\n[color=blue]x[/color]\n```\nafter[/color]")
	assert.Contains(t, out, "[color=blue]x[/color]", "tags inside fence literal")
	assert.Regexp(t, `\x0304after`, out, "color resumes after the block")
}

func TestBBRenderStrayAndPlaceholder(t *testing.T) {
	var warns []string
	warn := func(m string) { warns = append(warns, m) }
	out := MarkdownToIRCOpts(BBRenderOptions{BBCode: true, Warn: warn}, "a[/color]b[color=bogus]c[/color]d")
	assert.Equal(t, "abcd", out)
	assert.Contains(t, strings.Join(warns, "\n"), "stray close")
	assert.Contains(t, strings.Join(warns, "\n"), "unknown color")
}

// Pinned divergences (spec §2): soft-wrap drops the no-op re-emit; table
// rows inside a span now re-emit on every rendered row line.
func TestBBRenderPinnedDivergences(t *testing.T) {
	assert.Equal(t, "\x0304a b\x03", bb("[color=red]a\nb[/color]"), "soft wrap joins, one color")
	// Table rows: every row line carries the color (today only coded rows
	// did). Blank-line separated so the close tag is not itself absorbed
	// as a table row (P-1: the brief's single-\n input makes "[/color]"
	// the table's last row — still true of the intent, but this shape
	// isolates it).
	out := bb("[color=red]\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n[/color]")
	rows := strings.Split(strings.TrimSpace(out), "\n")
	n := strings.Count(out, "\x0304")
	assert.GreaterOrEqual(t, n, len(rows)-1, "each rendered row line re-emits")
}

// Idempotence at the pipeline level: re-rendering output adds no codes.
func TestBBRenderIdempotent(t *testing.T) {
	once := bb("[color=red]x[/color] and [table]")
	assert.Equal(t, once, bb(once))
}

// Color tags inside table cells: the digit guard must apply INSIDE cells
// (today's byte-translated cells carried exactly these bytes), and a tag
// pair split across the cells of one row pairs correctly through the
// shared state.
func TestBBRenderTableCellCodes(t *testing.T) {
	out := bb("| [color=red]5 | x[/color] |\n|---|---|\n| 6 | 7 |")
	assert.Contains(t, out, "\x0304\x02\x025", "open + digit guard inside cell")
	assert.Contains(t, out, "x\x03", "close inside cell emits the bare reset")
}

// bbcode off: tags never render as anything (existing signature unchanged).
func TestBBRenderOff(t *testing.T) {
	assert.Equal(t, "[color=red]x[/color]", MarkdownToIRC("[color=red]x[/color]"))
}
