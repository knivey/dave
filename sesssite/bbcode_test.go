package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bbWarnCapture() (func(string), *[]string) {
	var msgs []string
	return func(m string) { msgs = append(msgs, m) }, &msgs
}

// Ported VERBATIM from dave's bbcode_test.go — the pure-translator
// tests (config-gating tests stay dave-side). Pins parity between the
// emission-layer translator and the site's read-side copy.

func TestTranslateBBCodeBasicTags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text verbatim", "hello world", "hello world"},
		{"bold", "[b]x[/b]", "\x02x\x02"},
		{"italic", "[i]x[/i]", "\x1Dx\x1D"},
		{"underline", "[u]x[/u]", "\x1Fx\x1F"},
		{"strike", "[s]x[/s]", "\x1Ex\x1E"},
		{"uppercase tags", "[B]x[/B]", "\x02x\x02"},
		{"mixed case color tag", "[CoLoR=red]x[/COLOR]", "\x0304x\x03"},
		{"flat toggles nest freely", "[b]a[i]b[/i]c[/b]", "\x02a\x1Db\x1Dc\x02"},
		{"double open is IRC-faithful toggle", "[b][b]x", "\x02\x02x\x02"},
		{"markdown untouched", "**bold** and `code`", "**bold** and `code`"},
		{"markdown link untouched", "see [label](http://e.example) now", "see [label](http://e.example) now"},
		{"unknown tag verbatim", "see [table] here", "see [table] here"},
		{"citation verbatim", "source [1] says", "source [1] says"},
		{"empty brackets verbatim", "a[]b[/]c", "a[]b[/]c"},
		{"not tag shaped verbatim", "[col!or]x", "[col!or]x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, nil, true))
		})
	}
}

func TestTranslateBBCodeColors(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"white", "white", "00"},
		{"black", "black", "01"},
		{"blue", "blue", "02"},
		{"green", "green", "03"},
		{"red", "red", "04"},
		{"brown", "brown", "05"},
		{"maroon alias", "maroon", "05"},
		{"magenta", "magenta", "06"},
		{"purple alias", "purple", "06"},
		{"orange", "orange", "07"},
		{"yellow", "yellow", "08"},
		{"lightgreen", "lightgreen", "09"},
		{"light green spaced", "light green", "09"},
		{"lime alias", "lime", "09"},
		{"cyan", "cyan", "10"},
		{"teal alias", "teal", "10"},
		{"lightcyan", "lightcyan", "11"},
		{"light cyan spaced", "light cyan", "11"},
		{"lightblue", "lightblue", "12"},
		{"light blue spaced", "light blue", "12"},
		{"royal alias", "royal", "12"},
		{"pink", "pink", "13"},
		{"grey", "grey", "14"},
		{"gray alias", "gray", "14"},
		{"lightgrey", "lightgrey", "15"},
		{"light gray spaced", "light gray", "15"},
		{"silver alias", "silver", "15"},
		{"uppercase value", "RED", "04"},
		{"padded value", " red ", "04"},
		{"numeric one digit", "4", "04"},
		{"numeric two digit", "04", "04"},
		{"numeric zero", "0", "00"},
		{"numeric fifteen", "15", "15"},
		{"numeric padded zero", "00", "00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TranslateBBCode("[color="+tt.value+"]x[/color]", nil, true)
			assert.Equal(t, "\x03"+tt.want+"x\x03", got)
		})
	}
}

func TestTranslateBBCodeInvalidTags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare color", "a[color]b", "ab"},
		{"empty value", "a[color=]b", "ab"},
		{"bogus value", "a[color=bogus]b", "ab"},
		{"value too numeric", "a[color=16]b", "ab"},
		{"three digit numeric", "a[color=044]b", "ab"},
		{"value on valueless tag", "a[b=x]b", "ab"},
		{"stray bold close", "a[/b]b", "ab"},
		{"stray color close", "a[/color]b", "ab"},
		{"double close", "[b]x[/b][/b]", "\x02x\x02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, msgs := bbWarnCapture()
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, warn, true))
			assert.NotEmpty(t, *msgs, "invalid tags must not be silent")
		})
	}
	t.Run("close with value is a clean close", func(t *testing.T) {
		// [/color=red] pairs with its open just like [/color] —
		// dropping it would strand the open color until auto-close.
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "a\x0304x\x03b", TranslateBBCode("a[color=red]x[/color=red]b", warn, true))
		assert.Empty(t, *msgs)
	})
	t.Run("unknown tags do not warn", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "see [table] here", TranslateBBCode("see [table] here", warn, true))
		assert.Empty(t, *msgs, "unknown names are prose/markdown, not BBCode failures")
	})
}

func TestTranslateBBCodeGuards(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"digit after open", "[color=red]1. item", "\x0304\x02\x021. item\x03"},
		{"digit after bare close", "[color=red]x[/color]1 y", "\x0304x\x03\x02\x021 y"},
		{"comma digit after open", "[color=red],13 here", "\x0304\x02\x02,13 here\x03"},
		{"comma after bare close", "[color=red]x[/color],13", "\x0304x\x03\x02\x02,13"},
		{"comma digit after re-emit enclosing", "[color=blue][color=red]x[/color]1", "\x0302\x0304x\x0302\x02\x021\x03"},
		{"letter needs no guard", "[color=red]abc", "\x0304abc\x03"},
		{"newline clears adjacency", "[color=red]a\nb", "\x0304a\n\x0304b\x03"},
		{"tag between color and digit", "[color=red][b]1x", "\x0304\x021x\x02\x03"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, nil, true))
		})
	}
}

func TestTranslateBBCodeColorStack(t *testing.T) {
	t.Run("close re-emits enclosing", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x0302b\x0304c\x03d",
			TranslateBBCode("[color=red]a[color=blue]b[/color]c[/color]d", nil, true))
	})
	t.Run("auto close all colors single reset", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x0302b\x03",
			TranslateBBCode("[color=red]a[color=blue]b", nil, true))
	})
	t.Run("unclosed inner with closed outer text", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x0302b\x0304c\x03",
			TranslateBBCode("[color=red]a[color=blue]b[/color]c", nil, true))
	})
	t.Run("reset then new color", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x03\x0302b\x03",
			TranslateBBCode("[color=red]a[/color][color=blue]b", nil, true))
	})
}

func TestTranslateBBCodeLinePersistence(t *testing.T) {
	t.Run("color spans lines", func(t *testing.T) {
		assert.Equal(t, "\x0304line1\n\x0304line2still\x03",
			TranslateBBCode("[color=red]line1\nline2still[/color]", nil, true))
	})
	t.Run("all open styles re-emit", func(t *testing.T) {
		assert.Equal(t, "\x02\x0304line1\n\x0304\x02line2\x03\n\x02b plain\x02",
			TranslateBBCode("[b][color=red]line1\nline2[/color]\nb plain", nil, true))
	})
	t.Run("no styles no re-emit", func(t *testing.T) {
		assert.Equal(t, "a\nb", TranslateBBCode("a\nb", nil, true))
	})
	t.Run("blank line keeps waiting for visible text", func(t *testing.T) {
		// the empty middle line must not produce a codes-only message
		assert.Equal(t, "\x0304a\n\n\x0304b\x03",
			TranslateBBCode("[color=red]a\n\nb[/color]", nil, true))
	})
	t.Run("re-emit guard on digit line start", func(t *testing.T) {
		assert.Equal(t, "\x0304a\n\x0304\x02\x021. b\x03",
			TranslateBBCode("[color=red]a\n1. b[/color]", nil, true))
	})
	t.Run("close on next line after newline", func(t *testing.T) {
		// close tag right after the newline: no re-emit sandwich AND no
		// close code — the close is deferred (state-only) and 'b' needs
		// nothing (color closed, nothing open). Review finding B trace.
		assert.Equal(t, "\x0304a\nb",
			TranslateBBCode("[color=red]a\n[/color]b", nil, true))
	})
}

func TestTranslateBBCodeInvalidColorTracking(t *testing.T) {
	// An invalid color open is TRACKED as a transparent placeholder so its
	// eventual [/color] pairs with it — an untracked open would desync
	// every later close onto the wrong span (design review, Oct 2026).
	t.Run("invalid open pairs with its close", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "\x0304abc\x03d",
			TranslateBBCode("[color=red]a[color=bogus]b[/color]c[/color]d", warn, true))
		require.Len(t, *msgs, 1, "one warn at the open, none at the paired close")
	})
	t.Run("real color under placeholder", func(t *testing.T) {
		assert.Equal(t, "\x0304\x0302x\x0304yz\x03",
			TranslateBBCode("[color=red][color=bogus][color=blue]x[/color]y[/color]z", nil, true))
	})
	t.Run("top level invalid span invisible", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "abc", TranslateBBCode("a[color=bogus]b[/color]c", warn, true))
		require.Len(t, *msgs, 1)
	})
	t.Run("placeholder keeps newline reemit working", func(t *testing.T) {
		// effective color skips placeholders: line 2 re-emits red
		assert.Equal(t, "\x0304a\n\x0304b\x03",
			TranslateBBCode("[color=red][color=bogus]a\nb[/color][/color]", nil, true))
	})
}

func TestTranslateBBCodeAutoClose(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unclosed color", "[color=red]text", "\x0304text\x03"},
		{"unclosed bold", "[b]x", "\x02x\x02"},
		{"toggles close in canonical order", "[i][b]x", "\x1D\x02x\x02\x1D"},
		{"all four", "[s][u][i][b]x", "\x1E\x1F\x1D\x02x\x02\x1D\x1F\x1E"},
		{"nothing open", "plain", "plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, nil, true))
		})
	}
}

// TestTranslateBBCodeStylesDisabled: with styles off, toggle tags are
// prose jurisdiction — they ride the EXISTING unknown-token path
// (verbatim, no warn) exactly like [table] citations — while the color
// path is entirely unaffected. bbcode = true alone is color-only.
func TestTranslateBBCodeStylesDisabled(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bold verbatim", "[b]bold[/b]", "[b]bold[/b]"},
		{"italic verbatim", "[i]italic[/i]", "[i]italic[/i]"},
		{"underline verbatim", "[u]underline[/u]", "[u]underline[/u]"},
		{"strike verbatim", "[s]strike[/s]", "[s]strike[/s]"},
		{"uppercase toggle verbatim", "[B]x[/B]", "[B]x[/B]"},
		{"toggle open with value verbatim", "[b=red]x", "[b=red]x"},
		{"toggle close with value verbatim", "a[/b=x]b", "a[/b=x]b"},
		{"stray toggle close verbatim", "a[/b]b", "a[/b]b"},
		{"unknown close-with-value shape verbatim", "[u]a[/underline=x]b", "[u]a[/underline=x]b"},
		{"colors still translate", "[color=red]x[/color]", "\x0304x\x03"},
		{"mixed doc colors translate toggles verbatim",
			"[b]bold[/b] [color=red]red[/color] [i]it[/i]",
			"[b]bold[/b] \x0304red\x03 [i]it[/i]"},
		{"unclosed color auto-closes with verbatim toggle inside",
			"[color=red]x[b]y", "\x0304x[b]y\x03"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, msgs := bbWarnCapture()
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, warn, false))
			assert.Empty(t, *msgs, "disabled toggles are prose jurisdiction, never a warn")
		})
	}
	t.Run("no toggle codes anywhere", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		got := TranslateBBCode("[b]bold[/b] [i]it[/i] [u]u[/u] [s]s[/s]", warn, false)
		assert.Equal(t, "[b]bold[/b] [i]it[/i] [u]u[/u] [s]s[/s]", got)
		for _, code := range []string{"\x02", "\x1D", "\x1F", "\x1E"} {
			assert.NotContains(t, got, code)
		}
		assert.Empty(t, *msgs)
	})
}

// TestTranslateBBCodeStylesEnabled: styles=true keeps the full current
// behavior (the deep coverage lives in the existing TestTranslateBBCode*
// rows above, which pin the styles-on path; this is the explicit
// flag-on pin).
func TestTranslateBBCodeStylesEnabled(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bold", "[b]x[/b]", "\x02x\x02"},
		{"italic", "[i]x[/i]", "\x1Dx\x1D"},
		{"underline", "[u]x[/u]", "\x1Fx\x1F"},
		{"strike", "[s]x[/s]", "\x1Ex\x1E"},
		{"toggles with color", "[color=red][b]x[/b][/color]", "\x0304\x02x\x02\x03"},
		{"unclosed toggle auto-closes", "[b]x", "\x02x\x02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, nil, true))
		})
	}
}

// TestTranslateBBCodeUnknownCloseWithValue: an unknown close-with-value
// token is UNKNOWN tag-shaped text — verbatim, no warn, and crucially it
// must never be misread as a close of a real tag whose first letter
// matches ([/underline=x] closing [u]) — review finding 1, Oct 2026.
func TestTranslateBBCodeUnknownCloseWithValue(t *testing.T) {
	t.Run("verbatim no warn", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "see [/footnote=1] here", TranslateBBCode("see [/footnote=1] here", warn, true))
		assert.Empty(t, *msgs)
	})
	t.Run("first letter does not close a real toggle", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "\x1Fa[/underline=x]b\x1F",
			TranslateBBCode("[u]a[/underline=x]b", warn, true))
		assert.Empty(t, *msgs)
	})
	t.Run("known close with value still closes", func(t *testing.T) {
		assert.Equal(t, "a\x0304x\x03b", TranslateBBCode("a[color=red]x[/color=red]b", nil, true))
	})
}
