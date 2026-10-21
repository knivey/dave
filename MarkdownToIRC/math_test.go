package markdowntoirc

import (
	mrand "math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Math rendering: LaTeX ($..$, \(..\), \[..\], $$..$$ blocks) → single-line
// Unicode via termtex. Expectations pinned against
// termtex v0.0.0-20260918165034-c407d5cf5251 (Style{Inline: true}).
// Containment is used wherever whitespace is incidental; full equality only
// for exact short forms.

func TestMarkdownToIRCMath(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:       "ArrowInSentence",
			input:      "the reaction $\\rightarrow$ proceeds",
			contain:    []string{"→"},
			notContain: []string{"\\rightarrow"},
		},
		{
			name:       "QuadraticFormulaInline",
			input:      "roots are $x = \\frac{-b \\pm \\sqrt{b^2 - 4ac}}{2a}$ exactly",
			contain:    []string{"(-b ± √(b² - 4ac))/(2a)"},
			notContain: []string{"\\frac", "$"},
		},
		{
			name:    "ProseDollarsUnchanged",
			input:   "it costs $5 and $10 more",
			contain: []string{"$5 and $10"},
		},
		{
			name:       "CodeSpanUnchanged",
			input:      "use `$x^2$` here",
			contain:    []string{"$x^2$"},
			notContain: []string{"x²"},
		},
		{
			name:       "TeXDelimiters",
			input:      "map \\(x \\rightarrow y\\) now",
			contain:    []string{"→"},
			notContain: []string{"\\rightarrow"},
		},
		{
			// single-line $$...$$ block renders on ONE line: the inline-style
			// flat fraction is present, the stacked display fraction bar is not
			name:       "SingleLineBlockOneLine",
			input:      "$$x = \\frac{-b \\pm \\sqrt{b^2 - 4ac}}{2a}$$",
			contain:    []string{"(-b ± √(b² - 4ac))/(2a)"},
			notContain: []string{"─", "\\frac", "$"},
		},
	})
}

func TestMarkdownToIRCMathFallback(t *testing.T) {
	// A valid $$...$$ span whose expression errors at termtex render time:
	// the raw delimiters + expression must survive verbatim (degradation
	// equals the pre-math behavior — nothing is ever lost).
	runTests(t, []mdTest{
		{
			name:    "MalformedBlockFallsBackToRaw",
			input:   "$$\\frac{$$",
			contain: []string{"$$\\frac{$$", "\\frac{"},
		},
		{
			name:    "EmptyExpressionFallsBackToRaw",
			input:   "$$$$",
			contain: []string{"$$$$"},
		},
	})
}

// TestMathBlockMultilineRetainsAllLines pins the termtex-bug regression:
// multi-line $$ blocks must retain EVERY content line (termtex's own
// goldmark block parser double-advances the reader and drops the first
// content line plus every second line after — which is why we ship our own
// parser; see math.go's file comment).
func TestMathBlockMultilineRetainsAllLines(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:  "AllLinesRetained",
			input: "$$\na = b\nb = c\nc = d\n$$",
			// termtex renders the joined expression on one line; whichever
			// path fires (render or raw fallback), every line's text must
			// be present in the output.
			contain:    []string{"a = b", "b = c", "c = d"},
			notContain: []string{},
		},
		{
			name:       "SingleContentLineRenders",
			input:      "$$\np \\rightarrow q\n$$",
			contain:    []string{"→"},
			notContain: []string{"\\rightarrow"},
		},
		{
			// single-line block followed IMMEDIATELY by a paragraph: the
			// paragraph must not be swallowed (the single-line variant of
			// the same reader bug)
			name:       "SingleLineBlockDoesNotEatNextLine",
			input:      "$$a$$\nb = c",
			contain:    []string{"a", "b = c"},
			notContain: []string{},
		},
		{
			name:       "BlankLineInsideBlockRetained",
			input:      "$$\np \\rightarrow q\n\nr \\leftarrow s\n$$",
			contain:    []string{"→", "←"},
			notContain: []string{},
		},
	})
}

// TestStreamingMathHeldUntilClosed: a multi-line $$ math block spans blank
// lines, so a blank INSIDE the open block must not settle — the block is
// held like an open fence and released by the normal settle rules once
// closed (blank after the closing $$ + first line of the next block).
func TestStreamingMathHeldUntilClosed(t *testing.T) {
	r := NewStreamingRenderer()
	assert.Empty(t, r.Process("intro\n\n"), "blank unclassified: nothing yet")
	// the $$ opener line classifies the blank as a boundary → intro goes
	// out now, then the math block opens
	got := r.Process("$$\n")
	require.Len(t, got, 1, "math opener line must release the intro paragraph")
	assert.Equal(t, "intro", got[0])
	assert.Empty(t, r.Process("p \\rightarrow q\n"), "inside the math block: nothing")
	assert.Empty(t, r.Process("\n"), "blank INSIDE the open math block must NOT settle")
	assert.Empty(t, r.Process("r \\leftarrow s\n"), "still inside: nothing")
	assert.Empty(t, r.Process("$$\n"), "block closed, but normal settle rules apply: nothing yet")
	assert.Empty(t, r.Process("\n"), "blank after close unclassified: nothing yet")
	got = r.Process("after\n")
	require.Len(t, got, 1, "math block must be released when the next block's first line completes")
	assert.Contains(t, got[0], "→", "rendered math must be present")
	assert.Contains(t, got[0], "←", "all lines of the block must be rendered")
	assert.NotContains(t, got[0], "\\rightarrow", "raw LaTeX must not survive")
	assert.Equal(t, "after", joinEntries(r.Process("")), "final block at flush")
}

// TestStreamingMathChunkInvariance: the math-heavy documents must produce
// identical output for every delta chunking (the fuzz generator does not
// emit math, so these dedicated cases cover it).
func TestStreamingMathChunkInvariance(t *testing.T) {
	docs := []struct {
		name string
		doc  string
		// contain pins required rendered fragments of the canonical output
		contain []string
	}{
		{
			name: "Mixed",
			doc: "intro paragraph with $a \\rightarrow b$ inline math.\n\n" +
				"$$e^{i\\pi} + 1 = 0$$\n\n" +
				"$$\np \\rightarrow q\n\nr \\leftarrow s\n$$\n\n" +
				"```text\ncosts $5 and $$10 total\n```\n\n" +
				"outro paragraph here.\n",
			contain: []string{"→"},
		},
		{
			// Adversarial container-opener shape (externally probed in the
			// whole-branch review): the $$ on a LIST-MARKER line opens math
			// in goldmark (the container strips the marker) but must NOT
			// open the settle classifier's math state (trimmed content
			// starts with '-', not $$). The blank line inside the item and
			// the later TOP-LEVEL $$ — which settles the blank and DOES
			// open classifier math — must stay chunk-invariant, and the
			// item's math must render.
			name:    "ContainerOpener",
			doc:     "intro\n\n- $$\n  p \\rightarrow q\n\n$$\n\nafter\n",
			contain: []string{"p → q", "after"},
		},
	}
	for _, tt := range docs {
		t.Run(tt.name, func(t *testing.T) {
			r := mrand.New(mrand.NewPCG(7, 7))
			strats := chunkStrategies(r, tt.doc)
			first := streamCanonical(strats[0].deltas)
			require.NotEmpty(t, first)
			for _, s := range strats[1:] {
				assert.Equal(t, first, streamCanonical(s.deltas), "chunking strategy %q diverged", s.name)
			}
			for _, want := range tt.contain {
				assert.Contains(t, first, want, "canonical output missing %q", want)
			}
		})
	}
}

// TestStreamingMathParityWithNonStreaming: the incremental streaming output
// must equal the non-streaming render for math-bearing documents (same
// normalizeIRC form as the table parity test — empty lines dropped, outer
// newlines trimmed; the docs avoid code fences, which legitimately differ).
func TestStreamingMathParityWithNonStreaming(t *testing.T) {
	docs := []string{
		"intro with $a \\rightarrow b$ inline.\n\nafter\n",
		"$$x = \\frac{-b \\pm \\sqrt{b^2-4ac}}{2a}$$\n\nafter\n",
		"$$\np \\rightarrow q\n\nr \\leftarrow s\n$$\n\nafter\n",
	}
	for _, doc := range docs {
		assert.Equal(t, normalizeIRC(MarkdownToIRC(doc)), normalizeIRC(streamingRender(doc)),
			"streaming and non-streaming math renders diverged for %q", doc)
	}
}
