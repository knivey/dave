package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

var (
	tuiDBNotAvailable = "[red]Database not available[white]\n"

	tuiApp             *tview.Application
	logView            *tview.TextView
	logScrollbar       *Scrollbar
	logScrollbackLines int
	statusBar          *tview.TextView
	sbStatusBar        *tview.TextView
	inputField         *tview.InputField
	shutdownOnce       int32
	autoScroll         = true

	cmdHistory []string
	cmdHistIdx int
	cmdDraft   string

	origStdoutFd int
	origStderrFd int
	logPipeR     *os.File
	logFile      *os.File

	pipeDrainSync chan struct{}

	logBufMu     sync.Mutex
	logBuf       []string
	logFlushStop chan struct{}
	logFlushDone chan struct{}

	statusBarStop chan struct{}
	statusBarDone chan struct{}
)

const cmdHistoryMax = 100

// sbStatusWidth is the fixed width of the right-hand status segment showing
// "^S sb:on/off" — 9 runes plus one cell of breathing room.
const sbStatusWidth = 10

func initTUI() (*tview.Application, error) {
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("creating log pipe: %w", err)
	}

	origStdoutFd, err = syscall.Dup(syscall.Stdout)
	if err != nil {
		pipeR.Close()
		pipeW.Close()
		return nil, fmt.Errorf("dup stdout: %w", err)
	}
	origStderrFd, err = syscall.Dup(syscall.Stderr)
	if err != nil {
		syscall.Close(origStdoutFd)
		pipeR.Close()
		pipeW.Close()
		return nil, fmt.Errorf("dup stderr: %w", err)
	}

	if err := syscall.Dup2(int(pipeW.Fd()), syscall.Stdout); err != nil {
		syscall.Close(origStdoutFd)
		syscall.Close(origStderrFd)
		pipeR.Close()
		pipeW.Close()
		return nil, fmt.Errorf("dup2 stdout: %w", err)
	}
	if err := syscall.Dup2(int(pipeW.Fd()), syscall.Stderr); err != nil {
		syscall.Close(origStdoutFd)
		syscall.Close(origStderrFd)
		pipeR.Close()
		pipeW.Close()
		return nil, fmt.Errorf("dup2 stderr: %w", err)
	}
	pipeW.Close()

	os.Stdout = os.NewFile(uintptr(syscall.Stdout), "/dev/stdout")
	os.Stderr = os.NewFile(uintptr(syscall.Stderr), "/dev/stderr")

	logPipeR = pipeR

	app := tview.NewApplication()
	app.EnableMouse(true)

	scrollbackLines := config.TUI.ScrollbackLines
	if scrollbackLines <= 0 {
		scrollbackLines = 5000
	}
	// Must stay in sync with SetMaxLines below: drawLogView derives purge
	// amounts from logScrollbackLines and assumes it equals the view's cap.
	logScrollbackLines = scrollbackLines

	logView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true).
		SetMaxLines(scrollbackLines).
		SetChangedFunc(func() {
			if autoScroll {
				logView.ScrollToEnd()
			}
		})
	logView.SetBorder(false)

	inputField = tview.NewInputField().
		SetLabel("> ").
		SetFieldWidth(0).
		SetFieldBackgroundColor(tcell.ColorDefault)
	inputField.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			text := inputField.GetText()
			inputField.SetText("")
			if text != "" {
				cmdHistory = append(cmdHistory, text)
				if len(cmdHistory) > cmdHistoryMax {
					cmdHistory = cmdHistory[len(cmdHistory)-cmdHistoryMax:]
				}
			}
			cmdHistIdx = len(cmdHistory)
			cmdDraft = ""
			handleTUICommand(text)
		}
	})

	logScrollbar = NewScrollbar(config.TUI.Scrollbar)
	logContainer := tview.NewBox()
	logContainer.SetDrawFunc(func(screen tcell.Screen, x, y, width, height int) (int, int, int, int) {
		drawLogView(screen, x, y, width, height)
		return x, y, width, height
	})

	logContainer.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseScrollUp:
			wasAutoScroll := autoScroll
			autoScroll = false
			var row int
			if wasAutoScroll {
				totalLines := logView.GetWrappedLineCount()
				_, _, _, h := logView.GetInnerRect()
				if totalLines > h {
					row = totalLines - h
				}
			} else {
				row, _ = logView.GetScrollOffset()
			}
			newRow := row - 3
			if newRow < 0 {
				newRow = 0
			}
			logView.ScrollTo(newRow, 0)
			return tview.MouseConsumed, nil
		case tview.MouseScrollDown:
			if autoScroll {
				logView.ScrollToEnd()
				return tview.MouseConsumed, nil
			}
			row, _ := logView.GetScrollOffset()
			_, _, _, height := logView.GetInnerRect()
			totalLines := logView.GetWrappedLineCount()
			if row+3+height >= totalLines {
				autoScroll = true
				logView.ScrollToEnd()
			} else {
				logView.ScrollTo(row+3, 0)
			}
			return tview.MouseConsumed, nil
		}
		return action, event
	})

	statusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	statusBar.SetBackgroundColor(tcell.ColorBlack)
	statusBar.SetText("[dim]flagged:0[white]")

	// Right-hand segment of the status line: a nano-style shortcut hint plus
	// the live scrollbar state, so the Ctrl+S toggle is always discoverable.
	// Two separate widgets (not one recomposed string) keep the writers
	// independent — pollStatusBar owns this view's flagged peer, the toggle
	// owns this one, and neither needs the other's data to re-render.
	sbStatusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignRight)
	sbStatusBar.SetBackgroundColor(tcell.ColorBlack)
	sbStatusBar.SetText(scrollbarStatusText())

	statusFlex := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(statusBar, 0, 1, false).
		AddItem(sbStatusBar, sbStatusWidth, 0, false)

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(logContainer, 0, 1, true).
		AddItem(statusFlex, 1, 0, false).
		AddItem(inputField, 1, 0, true)

	app.SetRoot(flex, true).SetFocus(inputField)

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			requestShutdown()
			return nil
		}
		switch event.Key() {
		case tcell.KeyCtrlS:
			toggleScrollbar()
			return nil
		case tcell.KeyPgUp:
			wasAutoScroll := autoScroll
			autoScroll = false
			var row int
			_, _, _, height := logView.GetInnerRect()
			if wasAutoScroll {
				totalLines := logView.GetWrappedLineCount()
				if totalLines > height {
					row = totalLines - height
				}
			} else {
				row, _ = logView.GetScrollOffset()
			}
			newRow := row - height
			if newRow < 0 {
				newRow = 0
			}
			logView.ScrollTo(newRow, 0)
			return nil
		case tcell.KeyPgDn:
			if autoScroll {
				logView.ScrollToEnd()
				return nil
			}
			row, _ := logView.GetScrollOffset()
			_, _, _, height := logView.GetInnerRect()
			newRow := row + height
			if newRow+height >= logView.GetWrappedLineCount() {
				autoScroll = true
				logView.ScrollToEnd()
			} else {
				logView.ScrollTo(newRow, 0)
			}
			return nil
		case tcell.KeyUp:
			if cmdHistIdx > 0 {
				if cmdHistIdx == len(cmdHistory) {
					cmdDraft = inputField.GetText()
				}
				cmdHistIdx--
				inputField.SetText(cmdHistory[cmdHistIdx])
			}
			return nil
		case tcell.KeyDown:
			if cmdHistIdx < len(cmdHistory) {
				cmdHistIdx++
				if cmdHistIdx == len(cmdHistory) {
					inputField.SetText(cmdDraft)
				} else {
					inputField.SetText(cmdHistory[cmdHistIdx])
				}
			}
			return nil
		}
		return event
	})

	pipeDrainSync = make(chan struct{})
	go readPipeToView(logPipeR, logView, app)

	if err := openLogFile(); err != nil {
		fmt.Fprintf(logView, "[yellow]Warning: could not open log file: %v[white]\n", err)
	}

	logFlushStop = make(chan struct{})
	logFlushDone = make(chan struct{})
	go flushLogBuf(logView, app, logFlushStop, logFlushDone)

	statusBarStop = make(chan struct{})
	statusBarDone = make(chan struct{})
	go pollStatusBar(app, statusBarStop, statusBarDone)

	tuiApp = app
	return app, nil
}

// drawLogView renders the log view plus its scrollbar for one frame. It is the
// body of logContainer's DrawFunc, extracted as a package function so tests can
// drive the exact production draw sequence (see TestScrollPurgeKeepsViewAnchored).
//
// DESIGN NOTE: scroll anchoring across SetMaxLines purges. tview's
// TextView.Draw (verified against v0.42.0) drops the oldest wrapped lines once
// the buffer exceeds maxLines and ALSO resets the scroll offset to 0. Two
// consequences we must handle here:
//
//  1. Restoring the saved ABSOLUTE row would re-anchor to the wrong content:
//     the purge shifts every surviving row up by the number of dropped lines,
//     so while scrolled up in history the viewport would creep forward by
//     exactly the incoming line count on every flush — the "new messages push
//     the text I'm reading up the screen" bug. We therefore snapshot the
//     pre-draw wrapped-line total (the index is guaranteed complete here: the
//     scrollbar count at the end of the previous frame parsed it fully, and
//     appends only extend it), derive the purge amount
//     (totalBefore - logScrollbackLines), and subtract it from the saved row
//     so the SAME CONTENT stays on screen. The snapshot's full parse is also
//     load-bearing for purge TIMING, not just measurement: Draw's own
//     incremental parseAhead stops as soon as the viewport is covered, so
//     without this parse the index would be stale-low at Draw's purge check
//     and the purge would fire a frame late.
//  2. At the very top (row 0) no compensation is possible: the oldest lines
//     are being deleted out from under the viewport, so the view clamps there.
//
// The compensation runs AFTER Draw (which paints this frame's content at the
// pre-purge offset — still correct); the adjusted offset takes effect on the
// next frame. This couples to tview's purge-resets-offset behavior; if a
// future tview instead keeps the offset content-anchored across purges, this
// subtraction would double-compensate — re-verify on tview upgrades.
//
// Known limitation: on terminal-resize frames the pre-draw count can mix
// old-width and new-width wrapping for one frame, so the compensation can be
// off by a few rows there. Row numbers are equally approximate across rewraps
// in the clamp-restore path below; accepted.
func drawLogView(screen tcell.Screen, x, y, width, height int) {
	// Re-read every frame: a Ctrl+S toggle (or visible = false config) must
	// give the column back to the log view immediately — a hidden scrollbar
	// reserves nothing.
	sbWidth := logScrollbar.ReservedWidth()
	textW := width - sbWidth
	logView.SetRect(x, y, textW, height)
	logView.SetSize(0, textW)

	var savedRow, totalBefore int
	if !autoScroll && textW > 0 {
		// Must run after SetRect/SetSize so wrapping uses the width Draw will
		// use. Skipped on degenerate widths (container no wider than the
		// scrollbar): Draw early-returns without purging, so a phantom
		// pre-count here would compensate a purge that never happened.
		totalBefore = logView.GetWrappedLineCount()
		savedRow, _ = logView.GetScrollOffset()
	}

	logView.Draw(screen)

	if autoScroll {
		logView.ScrollToEnd()
	} else if totalBefore > logScrollbackLines {
		// A purge happened during Draw: tview dropped
		// (totalBefore - logScrollbackLines) wrapped lines from the top and
		// reset the offset to 0. Re-anchor to the same content.
		logView.ScrollTo(compensateScrollAfterPurge(savedRow, totalBefore, logScrollbackLines), 0)
	} else if savedRow > 0 {
		// No purge this frame. If Draw clamped the offset down (buffer no
		// longer reaches savedRow+height, e.g. after a rewrap), restore it.
		newRow, _ := logView.GetScrollOffset()
		if newRow < savedRow {
			logView.ScrollTo(savedRow, 0)
		}
	}

	totalLines := logView.GetWrappedLineCount()
	row, _ := logView.GetScrollOffset()
	if autoScroll && totalLines > height {
		row = totalLines - height
	}
	if logScrollbar.ShouldDraw(totalLines, height) {
		logScrollbar.Draw(screen, x, y, width, height, row, totalLines)
	}
}

// compensateScrollAfterPurge returns the content-anchored scroll row for a
// frame in which tview's SetMaxLines purge dropped (totalBefore - maxLines)
// wrapped lines from the top of the buffer: the content previously displayed
// at savedRow sits (totalBefore - maxLines) rows higher afterwards. Clamps at
// 0 — when the top of the viewport was itself purged, the oldest surviving
// line is the best available anchor. Returns savedRow unchanged when no purge
// happened (totalBefore <= maxLines).
func compensateScrollAfterPurge(savedRow, totalBefore, maxLines int) int {
	if totalBefore <= maxLines {
		return savedRow
	}
	dropped := totalBefore - maxLines
	if target := savedRow - dropped; target > 0 {
		return target
	}
	return 0
}

func restoreStdoutStderr() {
	if origStdoutFd > 0 {
		syscall.Dup2(origStdoutFd, syscall.Stdout)
		syscall.Close(origStdoutFd)
		origStdoutFd = 0
	}
	if origStderrFd > 0 {
		syscall.Dup2(origStderrFd, syscall.Stderr)
		syscall.Close(origStderrFd)
		origStderrFd = 0
	}
	os.Stdout = os.NewFile(uintptr(syscall.Stdout), "/dev/stdout")
	os.Stderr = os.NewFile(uintptr(syscall.Stderr), "/dev/stderr")
}

func openLogFile() error {
	if err := os.MkdirAll("logs", 0755); err != nil {
		return fmt.Errorf("creating logs directory: %w", err)
	}
	name := fmt.Sprintf("logs/dave-%s.log", time.Now().Format("2006-01-02"))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("opening log file: %w", err)
	}
	logFile = f
	return nil
}

func closeLogFile() {
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
}

func readPipeToView(reader *os.File, view *tview.TextView, app *tview.Application) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimRight(line, "\r")
		if line == "\x00PIPE_DRAIN\x00" {
			if pipeDrainSync != nil {
				pipeDrainSync <- struct{}{}
			}
			continue
		}
		if logFile != nil {
			logFile.WriteString(line + "\n")
		}
		escaped := tview.Escape(line)
		translated := tview.TranslateANSI(escaped)
		logBufMu.Lock()
		logBuf = append(logBuf, translated)
		logBufMu.Unlock()
	}
}

func flushLogBuf(view *tview.TextView, app *tview.Application, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			logBufMu.Lock()
			batch := logBuf
			logBuf = nil
			logBufMu.Unlock()
			if len(batch) > 0 {
				app.QueueUpdateDraw(func() {
					var b strings.Builder
					for _, line := range batch {
						b.WriteString(line)
						b.WriteByte('\n')
					}
					fmt.Fprint(view, b.String())
				})
			}
			return
		case <-ticker.C:
			logBufMu.Lock()
			if len(logBuf) == 0 {
				logBufMu.Unlock()
				continue
			}
			batch := logBuf
			logBuf = nil
			logBufMu.Unlock()
			app.QueueUpdateDraw(func() {
				var b strings.Builder
				for _, line := range batch {
					b.WriteString(line)
					b.WriteByte('\n')
				}
				fmt.Fprint(view, b.String())
			})
		}
	}
}

func drainPipe() {
	if pipeDrainSync == nil {
		return
	}
	fmt.Fprintln(os.Stdout, "\x00PIPE_DRAIN\x00")
	os.Stdout.Sync()
	<-pipeDrainSync
}

// scrollbarStatusText renders the right-hand status segment: the ^S shortcut
// hint (nano-style, so the key is never forgotten) plus the live state.
// Only valid after initTUI has built the scrollbar global.
func scrollbarStatusText() string {
	state := "off"
	if logScrollbar.Visible() {
		state = "on"
	}
	return fmt.Sprintf("[dim]^S sb:%s[white]", state)
}

// toggleScrollbar is the Ctrl+S handler: flips visibility and refreshes the
// status segment. Runs on the tview main goroutine (input capture), so a
// direct SetText is safe and the redraw lands on the same frame.
//
// The toggle is a runtime-only override of [tui.scrollbar] visible: it is not
// persisted, deliberately survives /reload (reloadAll never rebuilds TUI
// widgets), and resets to the config value on restart.
func toggleScrollbar() {
	logScrollbar.Toggle()
	sbStatusBar.SetText(scrollbarStatusText())
}

// pollStatusBar refreshes the TUI status bar every 5 seconds with the current
// flagged-user count. Runs until stop is closed. statusBar always reflects
// the latest DB count; yellow when >0 to catch admin attention, dim grey
// when zero. DB query is a single indexed COUNT — negligible load.
func pollStatusBar(app *tview.Application, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	render := func() {
		n, err := countFlaggedUsers()
		if err != nil {
			logger.Warn("status bar countFlaggedUsers failed", "error", err.Error())
			app.QueueUpdateDraw(func() {
				statusBar.SetText("[red]flagged:?[white]")
			})
			return
		}
		var text string
		if n > 0 {
			text = fmt.Sprintf("[yellow]flagged:%d[white]", n)
		} else {
			text = "[dim]flagged:0[white]"
		}
		app.QueueUpdateDraw(func() {
			statusBar.SetText(text)
		})
	}

	// Render immediately on start so the bar reflects DB state at launch.
	render()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			render()
		}
	}
}

func printUserInfo(view *tview.TextView, info *UserInfo) {
	u := info.User
	released := ""
	if u.Released {
		released = " [red](released)[white]"
	}
	fmt.Fprintf(view, "[white]  ID: #%d  Nick: %s  NormNick: %s%s[white]\n",
		u.ID, tview.Escape(displayNick(&u)), tview.Escape(u.NormalizedNick), released)
	fmt.Fprintf(view, "[white]  Network: %s  Account: %s[white]\n",
		tview.Escape(u.Network), tview.Escape(u.IRCAccount))
	fmt.Fprintf(view, "[white]  Created: %s  Updated: %s[white]\n",
		u.CreatedAt.Format("2006-01-02 15:04"), u.UpdatedAt.Format("2006-01-02 15:04"))
	fmt.Fprintf(view, "[white]  Sessions: %d  Messages: %d[white]\n",
		info.SessionCount, info.MessageCount)

	if len(info.Hosts) > 0 {
		fmt.Fprintf(view, "[white]  Known hosts:[white]\n")
		for _, h := range info.Hosts {
			fmt.Fprintf(view, "[white]    %s@%s (first: %s, last: %s)[white]\n",
				tview.Escape(h.Ident), tview.Escape(h.Host),
				h.FirstSeen.Format("2006-01-02"), h.LastSeen.Format("2006-01-02"))
		}
	} else {
		fmt.Fprintf(view, "[white]  Known hosts: none[white]\n")
	}

	if len(info.ActiveBans) > 0 {
		fmt.Fprintf(view, "[white]  Active bans:[white]\n")
		for _, b := range info.ActiveBans {
			fmt.Fprintf(view, "[white]    #%d %s %s expires %s[white]\n",
				b.ID, tview.Escape(b.Reason), formatDuration(b.Duration),
				b.ExpiresAt.Format("2006-01-02 15:04"))
		}
	}

	if len(info.NickChanges) > 0 {
		fmt.Fprintf(view, "[white]  Recent nick changes (%d):[white]\n", len(info.NickChanges))
		for _, nc := range info.NickChanges {
			fmt.Fprintf(view, "[white]    %s -> %s (%s)[white]\n",
				tview.Escape(nc.OldNick), tview.Escape(nc.NewNick),
				nc.CreatedAt.Format("2006-01-02 15:04"))
		}
	}
}

func handleTUICommand(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	if !strings.HasPrefix(text, "/") {
		fmt.Fprintf(logView, "[yellow]Unknown command: %s[white]\n", text)
		autoScroll = true
		logView.ScrollToEnd()
		return
	}

	parts := strings.SplitN(text, " ", 4)
	cmd := strings.ToLower(parts[0])

	if handler, ok := tuiCommands[cmd]; ok {
		handler(parts, text)
	} else {
		fmt.Fprintf(logView, "[yellow]Unknown command: %s[white]\n", text)
	}
	autoScroll = true
	logView.ScrollToEnd()
}

func requestShutdown() {
	logger.Info("Shutdown requested via TUI")
	go shutdown()
}
