package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompensateScrollAfterPurge(t *testing.T) {
	tests := []struct {
		name        string
		savedRow    int
		totalBefore int
		maxLines    int
		want        int
	}{
		{name: "no purge at cap returns saved row", savedRow: 120, totalBefore: 5000, maxLines: 5000, want: 120},
		{name: "below cap returns saved row", savedRow: 120, totalBefore: 4000, maxLines: 5000, want: 120},
		{name: "purge mid history subtracts dropped", savedRow: 120, totalBefore: 5010, maxLines: 5000, want: 110},
		{name: "purge one over cap", savedRow: 2500, totalBefore: 5001, maxLines: 5000, want: 2499},
		{name: "saved row equals dropped clamps to zero", savedRow: 50, totalBefore: 100, maxLines: 50, want: 0},
		{name: "saved row inside purged range clamps to zero", savedRow: 5, totalBefore: 100, maxLines: 50, want: 0},
		{name: "top of view stays at top", savedRow: 0, totalBefore: 100, maxLines: 50, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, compensateScrollAfterPurge(tt.savedRow, tt.totalBefore, tt.maxLines))
		})
	}
}

// setupScrollView installs a production-shaped log view (wrap + maxLines +
// scrollbar + scrollback cap globals) for scroll tests and returns an
// initialized simulation screen. drawLogView operates on the package globals,
// matching how the TUI runs.
func setupScrollView(t *testing.T, maxLines int) tcell.SimulationScreen {
	t.Helper()
	origView, origSB, origAuto, origMax := logView, logScrollbar, autoScroll, logScrollbackLines
	t.Cleanup(func() {
		logView, logScrollbar, autoScroll, logScrollbackLines = origView, origSB, origAuto, origMax
	})

	logView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true).
		SetMaxLines(maxLines)
	logScrollbar = NewScrollbar(TUIScrollbarConfig{})
	autoScroll = true
	logScrollbackLines = maxLines

	screen := tcell.NewSimulationScreen("UTF-8")
	require.NoError(t, screen.Init())
	t.Cleanup(screen.Fini)
	return screen
}

// writeScrollLines appends n unique single-screen-row entries ("line-%04d").
// With the fixed test width every logical line is exactly one wrapped row, so
// buffer offsets map 1:1 onto line numbers. NOTE: every entry ends in '\n' (as
// production flushes do), and tview counts that trailing newline as one extra
// empty wrapped entry — so after W writes the view holds W+1 entries. The
// purge base arithmetic in the tests below accounts for it.
func writeScrollLines(n, start int) {
	for i := 0; i < n; i++ {
		fmt.Fprintf(logView, "line-%04d\n", start+i)
	}
}

// screenTopLine returns the text painted on screen row 0, excluding the
// scrollbar column(s).
func screenTopLine(t *testing.T, screen tcell.SimulationScreen, width int) string {
	t.Helper()
	textW := width - logScrollbar.width
	var sb strings.Builder
	for x := 0; x < textW; x++ {
		r, _, _, _ := screen.GetContent(x, 0)
		sb.WriteRune(r)
	}
	return strings.TrimSpace(sb.String())
}

// TestScrollPurgeKeepsViewAnchored is the regression test for the
// "new messages push the text I'm reading up the screen" bug: with the
// scrollback buffer full and the view scrolled up in history, arriving log
// lines purge the oldest lines and must NOT move the viewed content.
func TestScrollPurgeKeepsViewAnchored(t *testing.T) {
	const (
		maxLines = 50
		viewW    = 30
		viewH    = 10
	)
	screen := setupScrollView(t, maxLines)

	// Overfill past the cap and reach the steady full state. Two frames are
	// needed: the frame-1 scrollbar count completes the line index, so the
	// purge fires from frame 2 onward (mirrors production, where the
	// scrollbar count runs every frame).
	writeScrollLines(60, 0)
	drawLogView(screen, 0, 0, viewW, viewH)
	drawLogView(screen, 0, 0, viewW, viewH)
	require.Equal(t, maxLines, logView.GetWrappedLineCount())

	// Scroll up 20 rows above the bottom, like the mouse-wheel handler.
	autoScroll = false
	total := logView.GetWrappedLineCount()
	logView.ScrollTo(total-viewH-20, 0)
	drawLogView(screen, 0, 0, viewW, viewH) // settle: paint the scrolled view
	// 60 lines written + 1 trailing-empty entry = 61; the purge kept 50, so
	// the buffer starts at line 11 and row 20 is absolute line 31.
	require.Equal(t, "line-0031", screenTopLine(t, screen, viewW))

	// Flush arrives: 5 new lines push 5 old lines out of the top.
	writeScrollLines(5, 60)
	drawLogView(screen, 0, 0, viewW, viewH)

	// The purge dropped 5 lines; the offset must track them downward so the
	// same content stays at the top of the screen. The frame that purged
	// painted at the pre-purge offset (still correct); the compensated offset
	// takes effect on the next frame.
	rowAfter, _ := logView.GetScrollOffset()
	assert.Equal(t, 15, rowAfter)
	drawLogView(screen, 0, 0, viewW, viewH)
	assert.Equal(t, "line-0031", screenTopLine(t, screen, viewW))

	// A second flush keeps the anchor too (compensation chains).
	writeScrollLines(5, 65)
	drawLogView(screen, 0, 0, viewW, viewH)
	drawLogView(screen, 0, 0, viewW, viewH)
	rowFinal, _ := logView.GetScrollOffset()
	assert.Equal(t, 10, rowFinal)
	assert.Equal(t, "line-0031", screenTopLine(t, screen, viewW))
}

func TestScrollPurgeAtVeryTopStaysAtTop(t *testing.T) {
	const (
		maxLines = 50
		viewW    = 30
		viewH    = 10
	)
	screen := setupScrollView(t, maxLines)
	writeScrollLines(60, 0)
	drawLogView(screen, 0, 0, viewW, viewH)
	drawLogView(screen, 0, 0, viewW, viewH)

	autoScroll = false
	logView.ScrollTo(0, 0)
	drawLogView(screen, 0, 0, viewW, viewH)
	// 61 entries purged down to 50: the buffer starts at line 11.
	require.Equal(t, "line-0011", screenTopLine(t, screen, viewW))

	// Purge under a top-pinned view: the oldest lines are gone, so no
	// compensation is possible — the view clamps at the top and shows the
	// oldest survivors.
	writeScrollLines(5, 60)
	drawLogView(screen, 0, 0, viewW, viewH)
	drawLogView(screen, 0, 0, viewW, viewH)
	row, _ := logView.GetScrollOffset()
	assert.Equal(t, 0, row)
	assert.Equal(t, "line-0016", screenTopLine(t, screen, viewW))
}

func TestScrollPurgeAutoScrollStaysPinnedToEnd(t *testing.T) {
	const (
		maxLines = 50
		viewW    = 30
		viewH    = 10
	)
	screen := setupScrollView(t, maxLines)
	writeScrollLines(60, 0)
	drawLogView(screen, 0, 0, viewW, viewH)
	drawLogView(screen, 0, 0, viewW, viewH)

	writeScrollLines(5, 60)
	drawLogView(screen, 0, 0, viewW, viewH)
	// The frame that purged leaves the offset at 0 (tview resets it after
	// painting); trackEnd re-pins on the following frame — the same lag the
	// scrollbar block's display-row override covers in production.
	drawLogView(screen, 0, 0, viewW, viewH)

	row, _ := logView.GetScrollOffset()
	assert.Equal(t, maxLines-viewH, row)
	// 65 lines written, 16 purged total (11 + 5); the end view (10 rows of
	// the 50 entries, last of which is the empty tail) starts at line 56.
	assert.Equal(t, "line-0056", screenTopLine(t, screen, viewW))
}

func TestScrollBelowCapKeepsOffset(t *testing.T) {
	const (
		maxLines = 50
		viewW    = 30
		viewH    = 10
	)
	screen := setupScrollView(t, maxLines)

	// Buffer below the cap: appending while scrolled up must not move the
	// view at all (no purge, no clamp).
	writeScrollLines(30, 0)
	drawLogView(screen, 0, 0, viewW, viewH)
	autoScroll = false
	logView.ScrollTo(10, 0)
	drawLogView(screen, 0, 0, viewW, viewH)
	require.Equal(t, "line-0010", screenTopLine(t, screen, viewW))

	writeScrollLines(5, 30)
	drawLogView(screen, 0, 0, viewW, viewH)

	row, _ := logView.GetScrollOffset()
	assert.Equal(t, 10, row)
	assert.Equal(t, "line-0010", screenTopLine(t, screen, viewW))
}

// TestScrollDegenerateWidthSkipsCompensation covers the guard on the pre-draw
// snapshot: when the container is no wider than the scrollbar, Draw
// early-returns without purging, so the purge compensation must not fire on a
// phantom pre-count (that would snap the view to the top).
func TestScrollDegenerateWidthSkipsCompensation(t *testing.T) {
	const (
		maxLines = 50
		viewW    = 30
		viewH    = 10
	)
	screen := setupScrollView(t, maxLines)
	writeScrollLines(60, 0)
	drawLogView(screen, 0, 0, viewW, viewH)
	drawLogView(screen, 0, 0, viewW, viewH)

	autoScroll = false
	total := logView.GetWrappedLineCount()
	logView.ScrollTo(total-viewH-20, 0)

	writeScrollLines(5, 60)
	drawLogView(screen, 0, 0, logScrollbar.width, viewH) // text width 0

	row, _ := logView.GetScrollOffset()
	assert.Equal(t, total-viewH-20, row)
}
