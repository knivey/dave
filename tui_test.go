package main

import (
	"testing"

	"github.com/rivo/tview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupScrollbarStatusTest swaps the TUI scrollbar/status globals for fresh
// instances (the same pattern setupTUITest uses for logView), so the toggle
// can be exercised without a running application.
func setupScrollbarStatusTest(t *testing.T) {
	t.Helper()
	origSb, origView := logScrollbar, sbStatusBar
	logScrollbar = NewScrollbar(TUIScrollbarConfig{})
	sbStatusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignRight)
	t.Cleanup(func() { logScrollbar, sbStatusBar = origSb, origView })
}

func TestScrollbarStatusText(t *testing.T) {
	setupScrollbarStatusTest(t)

	assert.Equal(t, "[dim]^S sb:on[white]", scrollbarStatusText())
	logScrollbar.Toggle()
	assert.Equal(t, "[dim]^S sb:off[white]", scrollbarStatusText())
}

func TestToggleScrollbar(t *testing.T) {
	setupScrollbarStatusTest(t)

	require.True(t, logScrollbar.Visible())
	toggleScrollbar()
	assert.False(t, logScrollbar.Visible())
	assert.Contains(t, sbStatusBar.GetText(true), "^S sb:off")

	toggleScrollbar()
	assert.True(t, logScrollbar.Visible())
	assert.Contains(t, sbStatusBar.GetText(true), "^S sb:on")
}
