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

	tuiApp       *tview.Application
	logView      *tview.TextView
	statusBar    *tview.TextView
	inputField   *tview.InputField
	shutdownOnce int32
	autoScroll   = true

	cmdHistory []string
	cmdHistIdx int
	cmdDraft   string

	origStdoutFd int
	origStderrFd int
	logPipeR     *os.File
	logFile      *os.File

	logBufMu     sync.Mutex
	logBuf       []string
	logFlushStop chan struct{}
	logFlushDone chan struct{}

	statusBarStop chan struct{}
	statusBarDone chan struct{}
)

const cmdHistoryMax = 100

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

	scrollbar := NewScrollbar(config.TUI.Scrollbar)
	sbWidth := scrollbar.width
	logContainer := tview.NewBox()
	logContainer.SetDrawFunc(func(screen tcell.Screen, x, y, width, height int) (int, int, int, int) {
		logView.SetRect(x, y, width-sbWidth, height)
		logView.SetSize(0, width-sbWidth)

		var savedRow int
		if !autoScroll {
			savedRow, _ = logView.GetScrollOffset()
		}

		logView.Draw(screen)

		if autoScroll {
			logView.ScrollToEnd()
		} else if savedRow > 0 {
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
		if scrollbar.ShouldDraw(totalLines, height) {
			scrollbar.Draw(screen, x, y, width, height, row, totalLines)
		}
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

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(logContainer, 0, 1, true).
		AddItem(statusBar, 1, 0, false).
		AddItem(inputField, 1, 0, true)

	app.SetRoot(flex, true).SetFocus(inputField)

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			requestShutdown()
			return nil
		}
		switch event.Key() {
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
