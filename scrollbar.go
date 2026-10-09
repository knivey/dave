package main

import (
	"github.com/gdamore/tcell/v2"
)

type Scrollbar struct {
	visible    bool
	showAlways bool
	width      int
	color      tcell.Color
	bgColor    tcell.Color
	trackColor tcell.Color
}

func NewScrollbar(cfg TUIScrollbarConfig) *Scrollbar {
	thumbColor := colorFromName(cfg.Color, tcell.ColorGray)
	bgColor := colorFromName(cfg.BackgroundColor, tcell.ColorBlack)
	trackColor := colorFromName(cfg.TrackColor, tcell.ColorDarkGray)

	visible := cfg.Visible == nil || *cfg.Visible
	showAlways := cfg.ShowAlways == nil || *cfg.ShowAlways

	width := cfg.Width
	if width <= 0 {
		width = 1
	}

	return &Scrollbar{
		visible:    visible,
		showAlways: showAlways,
		width:      width,
		color:      thumbColor,
		bgColor:    bgColor,
		trackColor: trackColor,
	}
}

// SetVisible changes scrollbar visibility at runtime (the Ctrl+S TUI toggle).
// Main-goroutine only: tview's event loop serializes the draw func, the app
// input capture, and the input field's done func — every reader and writer of
// this state — so no lock is needed.
func (s *Scrollbar) SetVisible(v bool) {
	s.visible = v
}

// Toggle flips scrollbar visibility. Same concurrency contract as SetVisible.
func (s *Scrollbar) Toggle() {
	s.visible = !s.visible
}

func (s *Scrollbar) Visible() bool {
	return s.visible
}

// ReservedWidth reports how many columns the log view must reserve for the
// scrollbar: its configured width while visible, zero once hidden. The draw
// func consults this every frame (not a startup-captured constant) so a
// runtime toggle returns the column on the next frame — and a config with
// visible = false never reserves it in the first place (before this existed,
// the width was subtracted unconditionally, burning a column while drawing
// nothing).
func (s *Scrollbar) ReservedWidth() int {
	if s.visible {
		return s.width
	}
	return 0
}

func (s *Scrollbar) ShouldDraw(totalLines, visibleHeight int) bool {
	if !s.visible || totalLines == 0 {
		return false
	}
	if s.showAlways {
		return true
	}
	return totalLines > visibleHeight
}

func (s *Scrollbar) Draw(screen tcell.Screen, x, y, width, height, scrollOffset, totalLines int) {
	if !s.visible || totalLines == 0 {
		return
	}

	visibleLines := height
	if visibleLines > totalLines {
		visibleLines = totalLines
	}

	if !s.showAlways && totalLines <= height {
		return
	}

	scrollX := x + width - s.width
	trackStyle := tcell.StyleDefault.Background(s.bgColor).Foreground(s.trackColor)
	thumbStyle := tcell.StyleDefault.Background(s.bgColor).Foreground(s.color)

	maxScroll := totalLines - visibleLines
	scrollRatio := 0.0
	if maxScroll > 0 {
		scrollRatio = float64(scrollOffset) / float64(maxScroll)
	}

	thumbRatio := float64(visibleLines) / float64(totalLines)
	thumbSize := int(float64(height) * thumbRatio)
	if thumbSize < 1 {
		thumbSize = 1
	}
	if thumbSize > height {
		thumbSize = height
	}

	thumbStart := int(float64(height-thumbSize) * scrollRatio)
	thumbEnd := thumbStart + thumbSize

	if thumbStart < 0 {
		thumbStart = 0
	}
	if thumbEnd > height {
		thumbEnd = height
	}

	for i := 0; i < height; i++ {
		if i >= thumbStart && i < thumbEnd {
			for w := 0; w < s.width; w++ {
				screen.SetContent(scrollX+w, y+i, '█', nil, thumbStyle)
			}
		} else {
			for w := 0; w < s.width; w++ {
				screen.SetContent(scrollX+w, y+i, '│', nil, trackStyle)
			}
		}
	}
}

func colorFromName(name string, fallback tcell.Color) tcell.Color {
	if name == "" {
		return fallback
	}
	c, ok := tcell.ColorNames[name]
	if !ok {
		return fallback
	}
	return c
}
