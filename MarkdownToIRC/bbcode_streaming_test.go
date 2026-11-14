package markdowntoirc

// Streaming BBCode tests (Task 4): the StreamingRenderer renders FRAGMENTS
// (buf[rendered:settled], each block-aligned) instead of re-rendering the
// whole settled prefix, carrying ONE bbRenderState across fragments so a
// color span opened in one fragment is closed (or re-emitted per line) in
// the next. The oracle: streaming concatenation must be byte-identical to
// the one-shot MarkdownToIRCOpts render for bbcode documents, for every
// chunking — the same property TestStreamingChunkInvariance pins for plain
// markdown.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func streamChunks(t *testing.T, opts BBRenderOptions, doc string, chunks []string) string {
	t.Helper()
	var sb strings.Builder
	sr := NewStreamingRendererOpts(opts)
	for _, c := range chunks {
		for _, line := range sr.Process(c) {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	for _, line := range sr.Process("") { // flush
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// Streaming ≡ one-shot byte-equality for bbcode docs, across chunkings.
//
// P-1 note: the reference is MarkdownToIRCStreamOpts (the STREAMING-mode
// whole-document render), not MarkdownToIRCOpts — the two one-shots differ
// on code-block padding by long-standing design (writePlainCodeBlockFixed
// pads to 80 in streaming mode, pinned by TestStreamingCodeBlockWidthCapping
// and streamfuzz's P2 oracle), and doc 3 contains a fenced code block.
// Every doc in this set closes its colors, so the reference's missing
// autoClose tail (the streaming one-shot deliberately omits it) is empty
// and the comparison is exact.
func TestBBCodeStreamChunkInvariance(t *testing.T) {
	docs := []string{
		"[color=red]a\n\nb[/color]",
		"[color=red]para\n\n- item\n- item2\n\nafter[/color]",
		"[color=red]before\n```\ncode\n```\nafter[/color]",
		"[color=red]**bold** and `code`[/color]",
		"[color=bogus]x[/color] and [/color] stray",
	}
	for _, doc := range docs {
		want := MarkdownToIRCStreamOpts(BBRenderOptions{BBCode: true}, doc) + "\n"
		for size := 1; size <= len(doc); size++ {
			var chunks []string
			for i := 0; i < len(doc); i += size {
				e := min(i+size, len(doc))
				chunks = append(chunks, doc[i:e])
			}
			require.Equal(t, want, streamChunks(t, BBRenderOptions{BBCode: true}, doc, chunks), "size %d", size)
		}
	}
}

// The blank-line boundary: state carries, NO auto-close at the fragment edge
// — concatenation is byte-identical to one-shot.
func TestBBCodeStreamSpanCrossingSettleBoundary(t *testing.T) {
	got := streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]a\n\nb[/color]",
		[]string{"[color=red]a\n", "\nb[/color]"})
	assert.Equal(t, "\x0304a\n\x0304b\x03\n", got)
}

// A tag split across deltas within a line: the line never settles until
// complete, so the tag is never frozen literal.
//
// P-1 note: expected is "\x0304x\x03\n", not the brief's "\x0304x\n" —
// the one-shot render of "[color=red]x\n" is "\x0304x\x03" (verified
// against MarkdownToIRCOpts): the source's trailing newline is trimmed by
// the paragraph renderer and never rendered, so no pending-newline is
// armed and the flush auto-close FIRES. Streaming ≡ one-shot is the
// binding invariant; the literal is pinned to the actual correct bytes.
func TestBBCodeStreamTagSplitAcrossDeltas(t *testing.T) {
	got := streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]x\n",
		[]string{"[col", "or=red]x\n"})
	assert.Equal(t, "\x0304x\x03\n", got)
}

// Invalid-color placeholder opened in one fragment, closed in a later one.
func TestBBCodeStreamPlaceholderAcrossFragments(t *testing.T) {
	got := streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]a\n\n[color=bogus]b\n\nc[/color]d[/color]",
		[]string{"[color=red]a\n\n", "[color=bogus]b\n\nc[/color]d[/color]"})
	want := MarkdownToIRCOpts(BBRenderOptions{BBCode: true}, "[color=red]a\n\n[color=bogus]b\n\nc[/color]d[/color]") + "\n"
	assert.Equal(t, want, got)
}

// Flush auto-closes a still-open color (and suppresses after a RENDERED
// newline).
//
// P-1 note: the brief's second case expected "\x0304x\n" for the doc
// "[color=red]x\n", but the one-shot render of that doc is "\x0304x\x03"
// (the paragraph renderer trims the source's trailing newline — nothing
// visible-follows-newline applies, so autoClose fires). Pinned to the
// actual correct bytes. The genuine suppression shape is a doc whose walk
// really ends on a rendered newline — a trailing <br> — asserted third
// (one-shot: "\x0304x\n"; the entry carries the rendered newline, and
// streamChunks' per-entry join adds the final one).
func TestBBCodeStreamFlushAutoClose(t *testing.T) {
	assert.Equal(t, "\x0304x\x03\n", streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]x", []string{"[color=red]x"}))
	assert.Equal(t, "\x0304x\x03\n", streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]x\n", []string{"[color=red]x\n"}))
	assert.Equal(t, "\x0304x\n\n", streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]x<br>\n", []string{"[color=red]x<br>\n"}))
}
