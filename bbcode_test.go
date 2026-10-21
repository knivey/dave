package main

// Tests for the BBCode→IRC translator (bbcode.go). The translator is the
// SECOND formatting system (Oct 2026): commands with `bbcode = true` get a
// model instructed to emit BBCode tags; dave converts them to IRC control
// codes at the EMISSION layer only (session storage stays raw), BEFORE the
// markdown renderer on every path.
//
// Invariants pinned here (agreed in design):
//   - tags map 1:1 to IRC codes; b/i/u/s are FLAT INDEPENDENT TOGGLES
//     exactly like IRC itself (no document nesting semantics)
//   - color is a stack (the one thing IRC can't do): [/color] re-emits the
//     enclosing color, or a bare \x03 at top level (color-only reset —
//     markdown-emitted bold/italic survive). \x0F is NEVER emitted.
//   - every color is exactly two digits
//   - after any \x03 emission, text starting with a digit or comma gets a
//     \x02\x02 no-op guard (the irc-formatting-reference workaround —
//     self-restoring in any bold state) so the code can't eat text
//   - open styles re-emit after every newline (the model expects spans to
//     persist across lines; each line is its own PRIVMSG and clients reset
//     formatting per message)
//   - known tag with unusable form (missing/bogus value, stray close) →
//     token stripped, text kept, WARN logged (never silent)
//   - unknown tag-shaped tokens stay VERBATIM — they are prose/markdown
//     jurisdiction ([1] citations, [label](url) links), not BBCode failures

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	markdowntoirc "github.com/knivey/dave/MarkdownToIRC"
	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bbWarnCapture() (func(string), *[]string) {
	var msgs []string
	return func(m string) { msgs = append(msgs, m) }, &msgs
}

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

func TestBBCodeStreaming(t *testing.T) {
	t.Run("tag split across deltas", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "", tr.Process("[col"))
		assert.Equal(t, "\x0304x", tr.Process("or=red]x"))
	})
	t.Run("guard lookahead across deltas", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "\x0304", tr.Process("[color=red]"))
		assert.Equal(t, "\x02\x021. x", tr.Process("1. x"))
	})
	t.Run("decidable text releases immediately", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "ok ", tr.Process("ok "))
		assert.Equal(t, "more", tr.Process("more"))
	})
	t.Run("not-a-tag releases on disqualifying char", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "", tr.Process("[col"))
		assert.Equal(t, "[col!or]x", tr.Process("!or]x"))
	})
	t.Run("flush releases unterminated tag verbatim", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "a", tr.Process("a[color=re"))
		assert.Equal(t, "[color=re", tr.Flush())
		assert.Equal(t, "", tr.Flush(), "spent")
	})
	t.Run("flush auto-closes open styles", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "\x0304x", tr.Process("[color=red]x"))
		assert.Equal(t, "\x03", tr.Flush())
	})
	t.Run("newline re-emit spans deltas", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "\x0304line1\n", tr.Process("[color=red]line1\n"))
		assert.Equal(t, "\x0304line2\x03", tr.Process("line2[/color]"))
	})
	t.Run("empty deltas are no-ops", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "", tr.Process(""))
		assert.Equal(t, "a", tr.Process("a"))
		assert.Equal(t, "", tr.Process(""))
	})
	t.Run("hold cap releases runaway partial", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		long := "[" + strings.Repeat("a", 100)
		got := tr.Process(long)
		assert.Equal(t, long, got, "an over-long unterminated partial is not a tag")
	})
}

func TestBBCodeChunkInvariance(t *testing.T) {
	// Streaming translation must equal the one-shot for every delta split.
	inputs := []string{
		"[b][color=red]line1\nline2[/color]\nb plain[/b]\n" +
			"see [label](http://x.example) and [1]\n" +
			"[color=blue][color=red]x[/color]1\ny[/color]",
		"no tags at all\njust text",
		"[color=red]1. digits[/color],13 commas\n[color=bogus]gone[/color]",
	}
	for _, in := range inputs {
		want := TranslateBBCode(in, nil, true)
		for size := 1; size <= len(in)+1; size++ {
			tr := newBBCodeTranslator(nil, true)
			var got strings.Builder
			for i := 0; i < len(in); i += size {
				end := i + size
				if end > len(in) {
					end = len(in)
				}
				got.WriteString(tr.Process(in[i:end]))
			}
			got.WriteString(tr.Flush())
			require.Equal(t, want, got.String(), "chunk size %d", size)
		}
	}
}

func TestBBCodeIdempotent(t *testing.T) {
	// Translated output must be safe to translate again (guard against
	// double-translation ever changing text): unknown tags survive
	// verbatim, known tags are consumed exactly once.
	in := "see [table] and [b][color=red]1. x[/color][/b]"
	once := TranslateBBCode(in, nil, true)
	assert.Equal(t, once, TranslateBBCode(once, nil, true))

	// Styles off: verbatim toggles survive re-translation — a [b] in
	// translated output must never become a toggle code on a second pass.
	offOnce := TranslateBBCode(in, nil, false)
	assert.Equal(t, offOnce, TranslateBBCode(offOnce, nil, false))
	assert.Contains(t, offOnce, "[b]", "toggles stay verbatim with styles off")
}

// --- bbcode_styles: toggle tags behind their own config ---

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

// TestBBCodeChunkInvarianceStylesDisabled: the styles-off mode must be
// chunk-invariant too — one-shot ≡ byte-streaming for every delta split,
// with toggles verbatim in both (the flag rides the same decidable-token
// machinery, only the tag disposition changes).
func TestBBCodeChunkInvarianceStylesDisabled(t *testing.T) {
	inputs := []struct {
		in       string
		verbatim []string
	}{
		{in: "[b][color=red]line1\nline2[/color]\nb plain[/b]\n" +
			"see [label](http://x.example) and [1]\n" +
			"[color=blue][color=red]x[/color]1\ny[/color]",
			verbatim: []string{"[b]", "[/b]"}},
		{in: "a[i] + b[j] math indexing stays prose\n[color=red]r[/color]",
			verbatim: []string{"[i]", "[j]"}},
		{in: "[s]struck[/s] and [color=red]1. digits[/color],13 commas\n",
			verbatim: []string{"[s]", "[/s]"}},
	}
	for _, tc := range inputs {
		want := TranslateBBCode(tc.in, nil, false)
		for _, v := range tc.verbatim {
			assert.Contains(t, want, v, "toggles verbatim one-shot")
		}
		for size := 1; size <= len(tc.in)+1; size++ {
			tr := newBBCodeTranslator(nil, false)
			var got strings.Builder
			for i := 0; i < len(tc.in); i += size {
				end := i + size
				if end > len(tc.in) {
					end = len(tc.in)
				}
				got.WriteString(tr.Process(tc.in[i:end]))
			}
			got.WriteString(tr.Flush())
			require.Equal(t, want, got.String(), "chunk size %d", size)
		}
	}
}

// --- integration: streamOutput runs the translator BEFORE the renderer ---

func TestStreamOutputBBCode(t *testing.T) {
	t.Run("plain mode split tag across deltas", func(t *testing.T) {
		so := streamOutput{bb: newBBCodeTranslator(nil, true)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[col", send)
		so.HandleDelta("or=red]line1\n", send)
		assert.Equal(t, []string{"\x0304line1\n"}, sent)
		so.HandleDelta("line2[/color] tail", send)
		so.Flush(send)
		assert.Equal(t, []string{"\x0304line1\n", "\x0304line2\x03 tail"}, sent)
	})
	t.Run("renderer mode gets translated codes", func(t *testing.T) {
		so := streamOutput{renderer: markdowntoirc.NewStreamingRenderer(), bb: newBBCodeTranslator(nil, true)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[color=red]red **bold**[/color]\n\nplain para\n\n", send)
		so.Flush(send)
		joined := strings.Join(sent, "\n")
		assert.Contains(t, joined, "\x0304", "the color survives into the rendered markdown")
		assert.NotContains(t, joined, "[color", "tags never reach the renderer")
	})
	t.Run("flush releases auto-close tail", func(t *testing.T) {
		so := streamOutput{bb: newBBCodeTranslator(nil, true)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[color=red]tail", send)
		assert.Empty(t, sent, "partial line held")
		so.Flush(send)
		assert.Equal(t, []string{"\x0304tail\x03"}, sent)
	})
	t.Run("nil translator passes deltas through raw", func(t *testing.T) {
		so := streamOutput{}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[b]x[/b]\n", send)
		assert.Equal(t, []string{"[b]x[/b]\n"}, sent)
	})
	t.Run("styles off keeps toggles verbatim, colors translate", func(t *testing.T) {
		so := streamOutput{bb: newBBCodeTranslator(nil, false)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[b]bold[/b] [color=red]line\n", send)
		so.HandleDelta("more[/color]", send)
		so.Flush(send)
		joined := strings.Join(sent, "\n")
		assert.Contains(t, joined, "[b]bold[/b]", "toggles verbatim with styles off")
		assert.Contains(t, joined, "\x0304", "colors still translate")
		assert.NotContains(t, joined, "\x02", "no bold codes with styles off")
	})
}

// --- integration: emission paths translate raw model output ---

func TestSendRenderedBBCode(t *testing.T) {
	newRunner := func(cfg AIConfig) (*chatRunner, chan string) {
		outputCh := make(chan string, 8)
		return &chatRunner{
			cfg:      cfg,
			network:  Network{Name: "testnet"},
			channel:  "#t",
			ctx:      context.Background(),
			outputCh: outputCh,
			logger:   newTestLogger(),
		}, outputCh
	}
	t.Run("off passes raw through", func(t *testing.T) {
		cr, outputCh := newRunner(AIConfig{})
		cr.sendRendered("[color=red]x[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		assert.Equal(t, []string{"[color=red]x[/color]"}, lines)
	})
	t.Run("on translates before rendering", func(t *testing.T) {
		cr, outputCh := newRunner(AIConfig{BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)})
		cr.sendRendered("[color=red]x[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		assert.Equal(t, []string{"\x0304x\x03"}, lines)
	})
	t.Run("markdown and bbcode combine", func(t *testing.T) {
		cr, outputCh := newRunner(AIConfig{RenderMarkdown: true, BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)})
		cr.sendRendered("**bold** [color=red]red[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "\x02bold\x02", "markdown still converts")
		assert.Contains(t, joined, "\x0304", "bbcode still translates")
		assert.NotContains(t, joined, "[color")
	})
	t.Run("styles off color-only, toggles verbatim", func(t *testing.T) {
		// bbcode = true with bbcode_styles unset (false): the flag is
		// threaded through sendRendered, not just the color decision.
		cr, outputCh := newRunner(AIConfig{BBCode: boolPtr(true)})
		cr.sendRendered("[b]bold[/b] [color=red]red[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "[b]bold[/b]", "toggles stay verbatim when bbcode_styles is off")
		assert.Contains(t, joined, "\x0304red\x03", "colors still translate")
		assert.NotContains(t, joined, "\x02", "no bold codes with styles off")
	})
}

// TestRunTurnStreamBBCode: end to end over the SSE harness — tags split
// across chunk boundaries; IRC receives codes only.
func TestRunTurnStreamBBCode(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	bbStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"[col"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"or=red]line1\n"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"line2[/color] tail"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, bbStream)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: false, StreamTimeout: 5 * time.Second,
		BBCode: boolPtr(true), BBCodeStyles: boolPtr(true),
	}, sid, outputCh)

	done := make(chan struct{})
	go func() { cr.runTurn(newTurnContext(sid, messages)); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304line1", "line 1 carries its own color (per-message reset)")
	assert.Contains(t, joined, "\x0304line2\x03 tail", "line 2 re-emits the span color")
	assert.NotContains(t, joined, "[color", "no tag reaches IRC")
}

// TestRunTurnStreamBBCodeStylesOff: bbcode = true with bbcode_styles
// off — the streaming hook threads the styles flag: colors translate,
// toggles stay verbatim on the wire.
func TestRunTurnStreamBBCodeStylesOff(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	bbStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"[b]bo"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ld[/b] [color=red]line1\n"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"line2[/color] tail"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, bbStream)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: false, StreamTimeout: 5 * time.Second,
		BBCode: boolPtr(true), // bbcode_styles unset = color-only
	}, sid, outputCh)

	done := make(chan struct{})
	go func() { cr.runTurn(newTurnContext(sid, messages)); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]bold[/b]", "toggle tags stay verbatim with styles off")
	assert.Contains(t, joined, "\x0304line2\x03 tail", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes with styles off")
}

// TestRunTurnResponsesStreamBBCode: the Responses-API streaming path is
// separately pinned so a refactor cannot drop one path.
func TestRunTurnResponsesStreamBBCode(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#r", "shrew", "testcmd", "svc", "test-model")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	completedPayload := map[string]any{
		"id": "resp-r", "object": "response", "model": "test-model",
		"output": []any{
			map[string]any{
				"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "[color=red]done"}},
			},
		},
	}
	completed, err := json.Marshal(map[string]any{"type": "response.completed", "response": completedPayload})
	require.NoError(t, err)
	stream := streamChunk(`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"[col"}`) +
		streamChunk(`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"or=red]done"}`) +
		"data: " + string(completed) + "\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer server.Close()

	outputCh := make(chan string, 16)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "test-model", ResponsesAPI: true, Streaming: true,
		Timeout: 10 * time.Second, StreamTimeout: 5 * time.Second,
		RenderMarkdown: false, BBCode: boolPtr(true), BBCodeStyles: boolPtr(true),
	}, sid, outputCh)
	cr.ctx = context.Background()

	done := make(chan struct{})
	go func() {
		cr.runTurn(newTurnContext(sid, messages))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304done\x03", "responses path translates")
	assert.NotContains(t, joined, "[color")
}

// TestRunTurnResponsesStreamBBCodeStylesOff: the Responses-API streaming
// hook threads the styles flag too — colors translate, toggles verbatim.
func TestRunTurnResponsesStreamBBCodeStylesOff(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#r", "shrew", "testcmd", "svc", "test-model")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	completedPayload := map[string]any{
		"id": "resp-r", "object": "response", "model": "test-model",
		"output": []any{
			map[string]any{
				"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "[b]bold[/b] [color=red]done"}},
			},
		},
	}
	completed, err := json.Marshal(map[string]any{"type": "response.completed", "response": completedPayload})
	require.NoError(t, err)
	stream := streamChunk(`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"[b]bold[/b] [col"}`) +
		streamChunk(`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"or=red]done"}`) +
		"data: " + string(completed) + "\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer server.Close()

	outputCh := make(chan string, 16)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "test-model", ResponsesAPI: true, Streaming: true,
		Timeout: 10 * time.Second, StreamTimeout: 5 * time.Second,
		RenderMarkdown: false, BBCode: boolPtr(true), // bbcode_styles unset = color-only
	}, sid, outputCh)
	cr.ctx = context.Background()

	done := make(chan struct{})
	go func() {
		cr.runTurn(newTurnContext(sid, messages))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]bold[/b]", "toggle tags stay verbatim with styles off")
	assert.Contains(t, joined, "\x0304done\x03", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes with styles off")
}

// TestHandleToolCallResponseBBCode: intermediate tool-loop text translates
// before its inline render.
func TestHandleToolCallResponseBBCode(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#t", "shrew", "testcmd", "svc", "m")
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	outputCh := make(chan string, 16)
	cr := &chatRunner{
		cfg:       AIConfig{BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)},
		network:   Network{Name: "testnet"},
		channel:   "#t",
		nick:      "shrew",
		ctx:       context.Background(),
		outputCh:  outputCh,
		logger:    newTestLogger(),
		sessionID: sid,
	}

	cr.handleToolCallResponse(newTurnContext(sid, messages),
		"[color=red]1. TILLANDSIA",
		[]ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "nonexistent_tool", Arguments: "{}"}}},
		reasoningParts{})

	lines := drainOutput(t, outputCh, 8, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304\x02\x021. TILLANDSIA\x03", "tool-loop text translates (guard on digit text)")
	assert.NotContains(t, joined, "[color")
}

// TestHandleToolCallResponseBBCodeStylesOff: the tool-loop emission hook
// threads the styles flag — colors translate, toggles verbatim.
func TestHandleToolCallResponseBBCodeStylesOff(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#t", "shrew", "testcmd", "svc", "m")
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	outputCh := make(chan string, 16)
	cr := &chatRunner{
		cfg:       AIConfig{BBCode: boolPtr(true)}, // bbcode_styles unset = color-only
		network:   Network{Name: "testnet"},
		channel:   "#t",
		nick:      "shrew",
		ctx:       context.Background(),
		outputCh:  outputCh,
		logger:    newTestLogger(),
		sessionID: sid,
	}

	cr.handleToolCallResponse(newTurnContext(sid, messages),
		"[b]x[/b] [color=red]red text",
		[]ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "nonexistent_tool", Arguments: "{}"}}},
		reasoningParts{})

	lines := drainOutput(t, outputCh, 8, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]x[/b]", "toggle tags stay verbatim with styles off")
	assert.Contains(t, joined, "\x0304red text\x03", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes with styles off")
}

// TestCompletionBBCode: the legacy completions path translates too.
func TestCompletionBBCode(t *testing.T) {
	setupNoticesDefaults(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions":
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"[color=red]1. TILLANDSIA[/color]","index":0,"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"localsvc": {BaseURL: server.URL + "/v1"}}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Name: "c", Service: "localsvc", Model: "m", Timeout: 10 * time.Second, BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)}
	cfg.ApplyDefaults(config.Services["localsvc"])

	outputCh := make(chan string, 4)
	e := girc.Event{Source: &girc.Source{Name: "shrew"}, Params: []string{"#101"}}

	completion(Network{Name: "testnet"}, nil, e, cfg, context.Background(), outputCh, "hello")

	lines := drainOutput(t, outputCh, 4, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304\x02\x021. TILLANDSIA\x03")
	assert.NotContains(t, joined, "[color")
}

// TestCompletionBBCodeStylesOff: the legacy completions hook threads the
// styles flag — colors translate, toggles verbatim.
func TestCompletionBBCodeStylesOff(t *testing.T) {
	setupNoticesDefaults(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions":
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"[b]bold[/b] [color=red]TILLANDSIA[/color]","index":0,"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"localsvc": {BaseURL: server.URL + "/v1"}}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Name: "c", Service: "localsvc", Model: "m", Timeout: 10 * time.Second, BBCode: boolPtr(true)}
	cfg.ApplyDefaults(config.Services["localsvc"])

	outputCh := make(chan string, 4)
	e := girc.Event{Source: &girc.Source{Name: "shrew"}, Params: []string{"#101"}}

	completion(Network{Name: "testnet"}, nil, e, cfg, context.Background(), outputCh, "hello")

	lines := drainOutput(t, outputCh, 4, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]bold[/b]", "toggle tags stay verbatim with styles off")
	assert.Contains(t, joined, "\x0304TILLANDSIA\x03", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes with styles off")
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

// TestBBCodeNoCodesOnlyLine: a trailing newline with open styles must not
// produce a codes-only PRIVMSG from the auto-close tail (each output line
// is its own message; codes alone render as a blank bot message) —
// review finding 2, Oct 2026. Drives the translated output through the
// production wrapForIRC splitter.
func TestBBCodeNoCodesOnlyLine(t *testing.T) {
	assertNoCodesOnly := func(t *testing.T, in string) {
		t.Helper()
		codeOnly := regexp.MustCompile(`^([\x02\x1D\x1F\x1E]|\x03[0-9]{0,2}(,[0-9]{0,2})?)*$`)
		for _, line := range wrapForIRC(TranslateBBCode(in, nil, true)) {
			if line == "" {
				continue // trailing-newline split artifact; sendToOutput skips these too
			}
			assert.False(t, codeOnly.MatchString(line),
				"line %q is codes-only (blank PRIVMSG) for input %q", line, in)
		}
	}
	t.Run("trailing newline with open styles", func(t *testing.T) {
		assertNoCodesOnly(t, "[b][color=red]x\n")
	})
	t.Run("trailing newline unclosed color only", func(t *testing.T) {
		assertNoCodesOnly(t, "[color=red]x\n")
	})
	t.Run("trailing newline after visible text still colored", func(t *testing.T) {
		// suppression must not lose the auto-close when visible text
		// follows the newline on the same line
		assert.Equal(t, "\x0304x\n\x0304y\x03", TranslateBBCode("[color=red]x\ny", nil, true))
	})
	t.Run("no trailing newline keeps auto-close", func(t *testing.T) {
		assert.Equal(t, "\x02\x0304x\x02\x03", TranslateBBCode("[b][color=red]x", nil, true))
	})
}

// TestBBCodeValueHoldCap: value growth HOLDS (unbounded) — a mid-value
// release cap breaks chunk invariance (re-review finding A reverted the
// caps). Invariance itself is pinned by TestBBCodeValueHoldInvariance;
// these rows pin the streaming hold observable directly.
func TestBBCodeValueHoldCap(t *testing.T) {
	t.Run("open value growth holds until terminator or flush", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "", tr.Process("[color="+strings.Repeat("a", 100)))
		assert.Equal(t, "[color="+strings.Repeat("a", 100), tr.Flush(),
			"unterminated candidate releases verbatim at flush")
	})
	t.Run("close value growth holds", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		assert.Equal(t, "", tr.Process("[/color="+strings.Repeat("a", 100)))
		assert.Equal(t, "[/color="+strings.Repeat("a", 100), tr.Flush(),
			"unterminated candidate releases verbatim at flush")
	})
	t.Run("name growth still releases at the cap", func(t *testing.T) {
		tr := newBBCodeTranslator(nil, true)
		got := tr.Process("[" + strings.Repeat("a", 100))
		assert.Equal(t, "["+strings.Repeat("a", 100), got,
			"name-only growth releases (unknown names are verbatim in both modes — the cap is safe there)")
	})
}

// TestBBCodeValueHoldInvariance: an over-length [color=<value>] candidate
// must HOLD until its ']' — a mid-value release cap breaks chunk
// invariance (one-shot classifies the complete tag; byte-streaming would
// leak it verbatim). Re-review finding A, Oct 2026: the hold is bounded
// by the stream itself (the full text is buffered by the stream loop
// regardless), so no cap is needed here.
func TestBBCodeValueHoldInvariance(t *testing.T) {
	in := "[color=" + strings.Repeat("a", 70) + "]x tail"
	want := TranslateBBCode(in, nil, true)
	assert.Equal(t, "x tail", want, "complete tag with invalid value is consumed (placeholder)")
	for size := 1; size <= len(in)+1; size++ {
		tr := newBBCodeTranslator(nil, true)
		var got strings.Builder
		for i := 0; i < len(in); i += size {
			end := i + size
			if end > len(in) {
				end = len(in)
			}
			got.WriteString(tr.Process(in[i:end]))
		}
		got.WriteString(tr.Flush())
		require.Equal(t, want, got.String(), "chunk size %d", size)
	}
}

// TestBBCodeTagOnlyLineNoCodes: tags on their own line after a newline
// emit no codes there — the codes would be a codes-only PRIVMSG; the
// next visible character's re-emit reconstructs the post-tag state.
// Re-review finding B, Oct 2026.
func TestBBCodeTagOnlyLineNoCodes(t *testing.T) {
	t.Run("close tag alone on line", func(t *testing.T) {
		assert.Equal(t, "\x02a\nc", TranslateBBCode("[b]a\n[/b]c", nil, true))
	})
	t.Run("close tag alone at end", func(t *testing.T) {
		assert.Equal(t, "\x0304a\n", TranslateBBCode("[color=red]a\n[/color]", nil, true))
	})
	t.Run("open tag alone on line", func(t *testing.T) {
		assert.Equal(t, "\x0304a\n\x0302\x02b\x02\x03", TranslateBBCode("[color=red]a\n[color=blue][b]b", nil, true))
	})
	t.Run("wire level never codes-only", func(t *testing.T) {
		codeOnly := regexp.MustCompile(`^([\x02\x1D\x1F\x1E]|\x03[0-9]{0,2}(,[0-9]{0,2})?)*$`)
		for _, in := range []string{
			"[b]a\n[/b]",
			"[color=red]a\n[/color]",
			"[color=red]a\n[color=blue]",
			"[i]a\n[/i]\n",
			"[b][color=red]x\n",
		} {
			for _, line := range wrapForIRC(TranslateBBCode(in, nil, true)) {
				if line == "" {
					continue
				}
				assert.False(t, codeOnly.MatchString(line),
					"line %q is codes-only (blank PRIVMSG) for input %q", line, in)
			}
		}
	})
}

// TestBBCodeInstructionNamesCovered: the 16 canonical color names taught
// by the {{.Vars.BBCodeFormatting}} instruction var (config/
// templatevars.toml) must each be a DIRECT bbColorNames key — the
// instructions promise one unambiguous name per color (no numerics, no
// aliases), so a name that only worked via an alias would be a doc bug.
func TestBBCodeInstructionNamesCovered(t *testing.T) {
	for _, name := range []string{
		"white", "black", "blue", "green", "red", "brown", "purple",
		"orange", "yellow", "lightgreen", "cyan", "lightcyan",
		"lightblue", "pink", "grey", "silver",
	} {
		_, ok := bbColorNames[name]
		assert.True(t, ok, "instruction name %q must be a direct table key", name)
	}
}

// TestBBCodeStylesTemplateVars: the shipped instruction vars split the
// vocabulary exactly (bbcode_styles, Oct 2026) — BBCodeFormatting teaches
// ONLY color tags (every tag-shaped token is [color=…], every taught
// color value resolves in bbColorNames), BBCodeStylesFormatting teaches
// ONLY toggle tags (every taught tag name is a bbToggleCode key), for
// admins who enable bbcode_styles = true.
func TestBBCodeStylesTemplateVars(t *testing.T) {
	var vars map[string]string
	_, err := toml.DecodeFile("config/templatevars.toml", &vars)
	require.NoError(t, err, "shipped config/templatevars.toml must decode")
	colorsVar := vars["BBCodeFormatting"]
	stylesVar := vars["BBCodeStylesFormatting"]
	require.NotEmpty(t, colorsVar, "BBCodeFormatting var must exist")
	require.NotEmpty(t, stylesVar, "BBCodeStylesFormatting var must exist")

	tagRe := regexp.MustCompile(`\[(/?)([a-zA-Z]+)(=[^\]\[]*)?\]`)

	for _, m := range tagRe.FindAllStringSubmatch(colorsVar, -1) {
		assert.Equal(t, "color", strings.ToLower(m[2]),
			"BBCodeFormatting must teach only color tags, found %q", m[0])
	}
	// Every taught color name is a direct table key (no aliases/numerics
	// taught — one unambiguous name per color).
	vals := regexp.MustCompile(`Supported colors: ([^\n]+)`).FindStringSubmatch(colorsVar)
	require.NotNil(t, vals, "BBCodeFormatting lists its supported colors")
	for _, name := range strings.Split(strings.TrimSuffix(vals[1], "."), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		_, ok := bbColorNames[strings.ToLower(name)]
		assert.True(t, ok, "taught color %q must be a direct bbColorNames key", name)
	}

	for _, m := range tagRe.FindAllStringSubmatch(stylesVar, -1) {
		name := strings.ToLower(m[2])
		require.Len(t, name, 1, "taught toggle tag %q must be a single letter", name)
		_, ok := bbToggleCode[name[0]]
		assert.True(t, ok, "taught toggle tag %q must be a bbToggleCode key", name)
	}
	for _, tag := range []string{"[b]", "[i]", "[u]", "[s]"} {
		assert.Contains(t, stylesVar, tag, "the four canonical toggles are taught")
	}
	assert.NotContains(t, stylesVar, "[color", "BBCodeStylesFormatting teaches only toggles")
}
