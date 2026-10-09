package markdowntoirc

// Latency semantics of the settled-prefix streaming renderer, pinned:
//
//   - a completed paragraph is emitted as soon as the FIRST LINE of the
//     next block completes (that line is what classifies the blank line as
//     a real block boundary rather than list/code continuation) — the
//     paragraph does NOT wait for the next block to finish, or for a
//     second blank line;
//   - a top-level fenced code block is emitted at its CLOSING fence line,
//     without waiting for the blank line after it;
//   - a tight list is held until the list ends (no blank lines interior) —
//     rendering items individually would break list structure;
//   - the final block of a stream is emitted by the flush.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func joinEntries(entries []string) string {
	return strings.Join(entries, "\n")
}

func TestStreamingLatencyParagraphReleasedAtNextBlockStart(t *testing.T) {
	r := NewStreamingRenderer()
	assert.Empty(t, r.Process("para one line"), "incomplete line: nothing yet")
	assert.Empty(t, r.Process("\n"), "paragraph line complete but block still open: nothing yet")
	// the blank line arrives — still nothing: whether this blank is a block
	// boundary is only decidable from the NEXT line (list/code continuation?)
	assert.Empty(t, r.Process("\n"), "blank arrived, unclassified: nothing yet")
	// first line of the next block completes → the blank is a boundary →
	// the finished paragraph must go out NOW
	got := r.Process("para two line\n")
	require.Len(t, got, 1, "paragraph must be emitted when the next block's first line completes")
	assert.Equal(t, "para one line", got[0])
	// flush delivers the rest
	tail := r.Process("")
	assert.Equal(t, "para two line", joinEntries(tail))
}

func TestStreamingLatencyCodeBlockReleasedAtClosingFence(t *testing.T) {
	r := NewStreamingRenderer()
	assert.Empty(t, r.Process("intro\n\n"), "blank not yet classified: nothing")
	// the fence OPEN line is itself a non-blank line: it classifies the
	// preceding blank as a boundary, releasing the paragraph immediately
	got := r.Process("```go\n")
	require.Len(t, got, 1, "paragraph must be released when the fence line arrives")
	assert.Equal(t, "intro", got[0])
	assert.Empty(t, r.Process("code line\n"), "still inside fence: nothing")
	// the closing fence completes the block — no waiting for a blank line
	got = r.Process("```\n")
	require.Len(t, got, 1, "code block must be emitted at the closing fence line")
	assert.Contains(t, got[0], "code line")
	assert.Empty(t, r.Process(""), "flush has nothing left")
}

func TestStreamingLatencyListHeldUntilListEnds(t *testing.T) {
	r := NewStreamingRenderer()
	assert.Empty(t, r.Process("- a\n- b\n\n"), "blank unclassified: list possibly continuing")
	// the following paragraph's first line ends the list and releases it whole
	got := r.Process("after\n")
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "a")
	assert.Contains(t, got[0], "b")
	assert.Equal(t, "after", joinEntries(r.Process("")))
}

func TestStreamingLatencyMarkerLineFence(t *testing.T) {
	// "- ```py" — fence opening ON the marker line. The scanner must track
	// this fence; before it did, the block's real closer ("  ```") was
	// misread as a NEW top-level opener, poisoning fence state and holding
	// every subsequent block until flush (silent batch degradation).
	r := NewStreamingRenderer()
	var emitted []string
	feed := func(s string) { emitted = append(emitted, r.Process(s)...) }
	feed("p1\n\np2\n\n")
	assert.Equal(t, []string{"p1"}, emitted, "p2's line released p1")
	feed("- ```py\n  x\n  ```\n")
	assert.Equal(t, []string{"p1"}, emitted, "inside the list fence: nothing yet")
	feed("\np4\n")
	require.Len(t, emitted, 2, "the list+fence block must NOT wait for the flush")
	assert.Contains(t, emitted[1], "p2")
	assert.Contains(t, emitted[1], "x", "fence content must be rendered as code")
	feed("\np5\n")
	assert.Equal(t, []string{"p1", emitted[1], "p4"}, emitted, "p4 released by p5's line")
	feed("")
	require.Len(t, emitted, 4)
	assert.Equal(t, "p5", emitted[3], "final block at flush")
}

func TestStreamingLatencyFinalBlockReleasedByFlush(t *testing.T) {
	r := NewStreamingRenderer()
	assert.Empty(t, r.Process("only paragraph, no blank after"))
	got := r.Process("")
	assert.Equal(t, "only paragraph, no blank after", joinEntries(got))
}

// TestStreamingLatencyManyParagraphsNoBacklog: successive paragraphs must
// each go out when the following block starts — output must never wait for
// two or more completed blocks.
func TestStreamingLatencyManyParagraphsNoBacklog(t *testing.T) {
	r := NewStreamingRenderer()
	var emitted []string
	feed := func(s string) { emitted = append(emitted, r.Process(s)...) }
	feed("p1\n\n")
	assert.Empty(t, emitted, "p1's blank unclassified yet")
	feed("p2\n\n")
	assert.Equal(t, []string{"p1"}, emitted, "p2's line released p1")
	feed("p3\n\n")
	assert.Equal(t, []string{"p1", "p2"}, emitted, "p3's line released p2")
	feed("p4\n")
	assert.Equal(t, []string{"p1", "p2", "p3"}, emitted, "p4's line released p3; p4 itself waits for flush")
	feed("")
	assert.Equal(t, []string{"p1", "p2", "p3", "p4"}, emitted)
}
