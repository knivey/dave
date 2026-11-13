package markdowntoirc

// Tests for the BBCode→IRC byte translator (bbcode.go), COLOR-ONLY
// (owner ruling, Nov 2026: style toggles were deleted outright — models
// never used them; `[b]`/`[i]`/`[u]`/`[s]` ride the unknown-token path,
// verbatim and silent, exactly like `[table]` citations).
//
// This is the byte-level path (paste one-shot, legacy completions, and
// plain-mode streaming); the markdown-aware path is the goldmark inline
// parser extension (see docs/superpowers/specs/
// 2026-11-13-bbcode-in-markdown-design.md).
//
// Invariants pinned here (agreed in design, unchanged for colors):
//   - color is a stack (the one thing IRC can't do): [/color] re-emits the
//     enclosing color, or a bare \x03 at top level (color-only reset —
//     markdown-emitted bold/italic survive). \x0F is NEVER emitted.
//   - every color is exactly two digits
//   - after any \x03 emission, text starting with a digit or comma gets a
//     \x02\x02 no-op guard (the irc-formatting-reference workaround —
//     self-restoring in any bold state) so the code can't eat text
//   - an open color re-emits after every newline (the model expects spans
//     to persist across lines; each line is its own PRIVMSG and clients
//     reset formatting per message)
//   - known tag with unusable form (missing/bogus value, stray close) →
//     token stripped, text kept, WARN logged (never silent)
//   - unknown tag-shaped tokens stay VERBATIM — they are prose/markdown
//     jurisdiction ([1] citations, [label](url) links, and since the
//     toggle deletion every b/i/u/s token), not BBCode failures

import (
	"strings"
	"testing"

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
		{"bold verbatim", "[b]x[/b]", "[b]x[/b]"},
		{"italic verbatim", "[i]x[/i]", "[i]x[/i]"},
		{"underline verbatim", "[u]x[/u]", "[u]x[/u]"},
		{"strike verbatim", "[s]x[/s]", "[s]x[/s]"},
		{"uppercase toggle verbatim", "[B]x[/B]", "[B]x[/B]"},
		{"mixed case color tag", "[CoLoR=red]x[/COLOR]", "\x0304x\x03"},
		{"toggles pass through freely", "[b]a[i]b[/i]c[/b]", "[b]a[i]b[/i]c[/b]"},
		{"double toggle open verbatim", "[b][b]x", "[b][b]x"},
		{"markdown untouched", "**bold** and `code`", "**bold** and `code`"},
		{"markdown link untouched", "see [label](http://e.example) now", "see [label](http://e.example) now"},
		{"unknown tag verbatim", "see [table] here", "see [table] here"},
		{"citation verbatim", "source [1] says", "source [1] says"},
		{"empty brackets verbatim", "a[]b[/]c", "a[]b[/]c"},
		{"not tag shaped verbatim", "[col!or]x", "[col!or]x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, nil))
		})
	}
}

// Toggles are gone: verbatim unknown tokens, colors still translate.
func TestTranslateBBCodeTogglesVerbatim(t *testing.T) {
	assert.Equal(t, "[b]bold[/b] and [i]it[/i]",
		TranslateBBCode("[b]bold[/b] and [i]it[/i]", nil))
	assert.Equal(t, "\x0304x\x03", TranslateBBCode("[color=red]x[/color]", nil))
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
			got := TranslateBBCode("[color="+tt.value+"]x[/color]", nil)
			assert.Equal(t, "\x03"+tt.want+"x\x03", got)
		})
	}
}

func TestTranslateBBCodeInvalidTags(t *testing.T) {
	// Color tags with unusable forms: token stripped, text kept, WARN
	// logged — never silent.
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
		{"stray color close", "a[/color]b", "ab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warn, msgs := bbWarnCapture()
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, warn))
			assert.NotEmpty(t, *msgs, "invalid color tags must not be silent")
		})
	}
	// Toggle-shaped tokens (including valued and stray-close forms) are
	// unknown tokens since the toggle deletion: verbatim, never a warn.
	t.Run("toggle tokens verbatim and silent", func(t *testing.T) {
		for _, in := range []string{
			"a[b=x]b",
			"a[/b]b",
			"[b]x[/b][/b]",
		} {
			warn, msgs := bbWarnCapture()
			assert.Equal(t, in, TranslateBBCode(in, warn))
			assert.Empty(t, *msgs, "toggle tokens are prose jurisdiction, not BBCode failures")
		}
	})
	t.Run("close with value is a clean close", func(t *testing.T) {
		// [/color=red] pairs with its open just like [/color] —
		// dropping it would strand the open color until auto-close.
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "a\x0304x\x03b", TranslateBBCode("a[color=red]x[/color=red]b", warn))
		assert.Empty(t, *msgs)
	})
	t.Run("unknown tags do not warn", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "see [table] here", TranslateBBCode("see [table] here", warn))
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
		// [b] is visible text now: the token itself clears color
		// adjacency — a guard is only needed when text DIRECTLY follows
		// the \x03.
		{"unknown token between color and digit", "[color=red][b]1x", "\x0304[b]1x\x03"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, nil))
		})
	}
}

func TestTranslateBBCodeColorStack(t *testing.T) {
	t.Run("close re-emits enclosing", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x0302b\x0304c\x03d",
			TranslateBBCode("[color=red]a[color=blue]b[/color]c[/color]d", nil))
	})
	t.Run("auto close all colors single reset", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x0302b\x03",
			TranslateBBCode("[color=red]a[color=blue]b", nil))
	})
	t.Run("unclosed inner with closed outer text", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x0302b\x0304c\x03",
			TranslateBBCode("[color=red]a[color=blue]b[/color]c", nil))
	})
	t.Run("reset then new color", func(t *testing.T) {
		assert.Equal(t, "\x0304a\x03\x0302b\x03",
			TranslateBBCode("[color=red]a[/color][color=blue]b", nil))
	})
	t.Run("placeholder pairs silently", func(t *testing.T) {
		assert.Equal(t, "abc",
			TranslateBBCode("a[color=bogus]b[/color]c", nil))
	})
}

func TestTranslateBBCodeLinePersistence(t *testing.T) {
	t.Run("color spans lines", func(t *testing.T) {
		assert.Equal(t, "\x0304line1\n\x0304line2still\x03",
			TranslateBBCode("[color=red]line1\nline2still[/color]", nil))
	})
	t.Run("color re-emits after newline, toggle verbatim", func(t *testing.T) {
		assert.Equal(t, "[b]\x0304line1\n\x0304line2\x03\nb plain",
			TranslateBBCode("[b][color=red]line1\nline2[/color]\nb plain", nil))
	})
	t.Run("no color no re-emit", func(t *testing.T) {
		assert.Equal(t, "a\nb", TranslateBBCode("a\nb", nil))
	})
	t.Run("blank line keeps waiting for visible text", func(t *testing.T) {
		// the empty middle line must not produce a codes-only message
		assert.Equal(t, "\x0304a\n\n\x0304b\x03",
			TranslateBBCode("[color=red]a\n\nb[/color]", nil))
	})
	t.Run("re-emit guard on digit line start", func(t *testing.T) {
		assert.Equal(t, "\x0304a\n\x0304\x02\x021. b\x03",
			TranslateBBCode("[color=red]a\n1. b[/color]", nil))
	})
	t.Run("close on next line after newline", func(t *testing.T) {
		// close tag right after the newline: no re-emit sandwich AND no
		// close code — the close is deferred (state-only) and 'b' needs
		// nothing (color closed, nothing open). Review finding B trace.
		assert.Equal(t, "\x0304a\nb",
			TranslateBBCode("[color=red]a\n[/color]b", nil))
	})
}

func TestTranslateBBCodeInvalidColorTracking(t *testing.T) {
	// An invalid color open is TRACKED as a transparent placeholder so its
	// eventual [/color] pairs with it — an untracked open would desync
	// every later close onto the wrong span (design review, Oct 2026).
	t.Run("invalid open pairs with its close", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "\x0304abc\x03d",
			TranslateBBCode("[color=red]a[color=bogus]b[/color]c[/color]d", warn))
		require.Len(t, *msgs, 1, "one warn at the open, none at the paired close")
	})
	t.Run("real color under placeholder", func(t *testing.T) {
		assert.Equal(t, "\x0304\x0302x\x0304yz\x03",
			TranslateBBCode("[color=red][color=bogus][color=blue]x[/color]y[/color]z", nil))
	})
	t.Run("top level invalid span invisible", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "abc", TranslateBBCode("a[color=bogus]b[/color]c", warn))
		require.Len(t, *msgs, 1)
	})
	t.Run("placeholder keeps newline reemit working", func(t *testing.T) {
		// effective color skips placeholders: line 2 re-emits red
		assert.Equal(t, "\x0304a\n\x0304b\x03",
			TranslateBBCode("[color=red][color=bogus]a\nb[/color][/color]", nil))
	})
}

func TestTranslateBBCodeAutoClose(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unclosed color", "[color=red]text", "\x0304text\x03"},
		{"unclosed bold verbatim", "[b]x", "[b]x"},
		{"toggles never auto-close", "[i][b]x", "[i][b]x"},
		{"all four verbatim", "[s][u][i][b]x", "[s][u][i][b]x"},
		{"nothing open", "plain", "plain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, TranslateBBCode(tt.in, nil))
		})
	}
}

func TestBBCodeStreaming(t *testing.T) {
	t.Run("tag split across deltas", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "", tr.Process("[col"))
		assert.Equal(t, "\x0304x", tr.Process("or=red]x"))
	})
	t.Run("guard lookahead across deltas", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "\x0304", tr.Process("[color=red]"))
		assert.Equal(t, "\x02\x021. x", tr.Process("1. x"))
	})
	t.Run("decidable text releases immediately", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "ok ", tr.Process("ok "))
		assert.Equal(t, "more", tr.Process("more"))
	})
	t.Run("not-a-tag releases on disqualifying char", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "", tr.Process("[col"))
		assert.Equal(t, "[col!or]x", tr.Process("!or]x"))
	})
	t.Run("flush releases unterminated tag verbatim", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "a", tr.Process("a[color=re"))
		assert.Equal(t, "[color=re", tr.Flush())
		assert.Equal(t, "", tr.Flush(), "spent")
	})
	t.Run("flush auto-closes open color", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "\x0304x", tr.Process("[color=red]x"))
		assert.Equal(t, "\x03", tr.Flush())
	})
	t.Run("newline re-emit spans deltas", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "\x0304line1\n", tr.Process("[color=red]line1\n"))
		assert.Equal(t, "\x0304line2\x03", tr.Process("line2[/color]"))
	})
	t.Run("empty deltas are no-ops", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "", tr.Process(""))
		assert.Equal(t, "a", tr.Process("a"))
		assert.Equal(t, "", tr.Process(""))
	})
	t.Run("hold cap releases runaway partial", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		long := "[" + strings.Repeat("a", 100)
		got := tr.Process(long)
		assert.Equal(t, long, got, "an over-long unterminated partial is not a tag")
	})
}

func TestBBCodeChunkInvariance(t *testing.T) {
	// Streaming translation must equal the one-shot for every delta split.
	// The inputs deliberately carry toggle tokens: verbatim in BOTH modes
	// is part of the invariance contract.
	inputs := []string{
		"[b][color=red]line1\nline2[/color]\nb plain[/b]\n" +
			"see [label](http://x.example) and [1]\n" +
			"[color=blue][color=red]x[/color]1\ny[/color]",
		"no tags at all\njust text",
		"[color=red]1. digits[/color],13 commas\n[color=bogus]gone[/color]",
	}
	for _, in := range inputs {
		want := TranslateBBCode(in, nil)
		for size := 1; size <= len(in)+1; size++ {
			tr := NewBBCodeStream(nil)
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
	// double-translation ever changing text): unknown tags — toggles
	// included — survive verbatim, color tags are consumed exactly once.
	in := "see [table] and [b][color=red]1. x[/color][/b]"
	once := TranslateBBCode(in, nil)
	assert.Equal(t, once, TranslateBBCode(once, nil))
	assert.Contains(t, once, "[b]", "toggle tokens stay verbatim")
}

// TestTranslateBBCodeUnknownCloseWithValue: an unknown close-with-value
// token is UNKNOWN tag-shaped text — verbatim, no warn. Historically it
// had to not be misread as a close of a real toggle whose first letter
// matched ([/underline=x] closing [u]); with toggles deleted the whole
// input is verbatim, and the wire shape stays pinned — review finding 1,
// Oct 2026.
func TestTranslateBBCodeUnknownCloseWithValue(t *testing.T) {
	t.Run("verbatim no warn", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "see [/footnote=1] here", TranslateBBCode("see [/footnote=1] here", warn))
		assert.Empty(t, *msgs)
	})
	t.Run("toggle input fully verbatim", func(t *testing.T) {
		warn, msgs := bbWarnCapture()
		assert.Equal(t, "[u]a[/underline=x]b",
			TranslateBBCode("[u]a[/underline=x]b", warn))
		assert.Empty(t, *msgs)
	})
	t.Run("known close with value still closes", func(t *testing.T) {
		assert.Equal(t, "a\x0304x\x03b", TranslateBBCode("a[color=red]x[/color=red]b", nil))
	})
}

// TestBBCodeValueHoldCap: value growth HOLDS (unbounded) — a mid-value
// release cap breaks chunk invariance (re-review finding A reverted the
// caps). Invariance itself is pinned by TestBBCodeValueHoldInvariance;
// these rows pin the streaming hold observable directly.
func TestBBCodeValueHoldCap(t *testing.T) {
	t.Run("open value growth holds until terminator or flush", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "", tr.Process("[color="+strings.Repeat("a", 100)))
		assert.Equal(t, "[color="+strings.Repeat("a", 100), tr.Flush(),
			"unterminated candidate releases verbatim at flush")
	})
	t.Run("close value growth holds", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		assert.Equal(t, "", tr.Process("[/color="+strings.Repeat("a", 100)))
		assert.Equal(t, "[/color="+strings.Repeat("a", 100), tr.Flush(),
			"unterminated candidate releases verbatim at flush")
	})
	t.Run("name growth still releases at the cap", func(t *testing.T) {
		tr := NewBBCodeStream(nil)
		got := tr.Process("[" + strings.Repeat("a", 100))
		assert.Equal(t, "["+strings.Repeat("a", 100), got,
			"name-only growth releases (unknown names are verbatim — the cap is safe there)")
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
	want := TranslateBBCode(in, nil)
	assert.Equal(t, "x tail", want, "complete tag with invalid value is consumed (placeholder)")
	for size := 1; size <= len(in)+1; size++ {
		tr := NewBBCodeStream(nil)
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
