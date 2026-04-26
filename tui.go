package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

var (
	tuiApp       *tview.Application
	logView      *tview.TextView
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

		var savedRow int
		if !autoScroll {
			savedRow, _ = logView.GetScrollOffset()
		}

		logView.Draw(screen)

		if !autoScroll && savedRow > 0 {
			newRow, _ := logView.GetScrollOffset()
			if newRow < savedRow {
				logView.ScrollTo(savedRow, 0)
			}
		}

		totalLines := logView.GetWrappedLineCount()
		row, _ := logView.GetScrollOffset()
		if scrollbar.ShouldDraw(totalLines, height) {
			scrollbar.Draw(screen, x, y, width, height, row, totalLines)
		}
		return x, y, width, height
	})

	logContainer.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseScrollUp:
			autoScroll = false
			row, _ := logView.GetScrollOffset()
			newRow := row - 3
			if newRow < 0 {
				newRow = 0
			}
			logView.ScrollTo(newRow, 0)
			return tview.MouseConsumed, nil
		case tview.MouseScrollDown:
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

	flex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(logContainer, 0, 1, true).
		AddItem(inputField, 1, 0, true)

	app.SetRoot(flex, true).SetFocus(inputField)

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyCtrlC {
			requestShutdown()
			return nil
		}
		switch event.Key() {
		case tcell.KeyPgUp:
			autoScroll = false
			row, _ := logView.GetScrollOffset()
			_, _, _, height := logView.GetInnerRect()
			newRow := row - height
			if newRow < 0 {
				newRow = 0
			}
			logView.ScrollTo(newRow, 0)
			return nil
		case tcell.KeyPgDn:
			row, _ := logView.GetScrollOffset()
			_, _, _, height := logView.GetInnerRect()
			newRow := row + height
			if newRow+height >= logView.GetWrappedLineCount() {
				autoScroll = true
				logView.ScrollToEnd()
				logView.ScrollTo(logView.GetWrappedLineCount()-height, 0)
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

	switch cmd {
	case "/help":
		fmt.Fprintf(logView, "[white]Commands:\n")
		fmt.Fprintf(logView, "  /help                        - Show this help\n")
		fmt.Fprintf(logView, "  /reload                      - Reload config from disk\n")
		fmt.Fprintf(logView, "  /quit, /exit                 - Shut down\n")
		fmt.Fprintf(logView, "  /join <network> <channel>    - Join a channel\n")
		fmt.Fprintf(logView, "  /part <network> <channel> [message]\n")
		fmt.Fprintf(logView, "                               - Leave a channel\n")
		fmt.Fprintf(logView, "  /nick <network> <nick>       - Change nickname\n")
	case "/reload":
		if err := reloadAll(); err != nil {
			fmt.Fprintf(logView, "[red]Reload failed: %s[white]\n", err)
		} else {
			fmt.Fprintf(logView, "[green]Reloaded commands, services, and prompt enhancements[white]\n")
		}
	case "/quit", "/exit":
		requestShutdown()
	case "/join":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /join <network> <channel>[white]\n")
			break
		}
		network, channel := parts[1], parts[2]
		bot, ok := bots[network]
		if !ok {
			fmt.Fprintf(logView, "[red]Unknown network: %s[white]\n", network)
			break
		}
		bot.mu.Lock()
		if bot.Network.Channels == nil {
			bot.Network.Channels = make(map[string]ChannelConfig)
		}
		_, alreadyJoined := bot.Network.Channels[channel]
		if alreadyJoined {
			bot.mu.Unlock()
			fmt.Fprintf(logView, "[yellow]Already in %s on %s[white]\n", channel, network)
			break
		}
		bot.Network.Channels[channel] = ChannelConfig{}
		bot.mu.Unlock()
		bot.Client.Cmd.Join(channel)
		fmt.Fprintf(logView, "[green]Joined %s on %s[white]\n", channel, network)
	case "/part":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /part <network> <channel> [message][white]\n")
			break
		}
		network, channel := parts[1], parts[2]
		bot, ok := bots[network]
		if !ok {
			fmt.Fprintf(logView, "[red]Unknown network: %s[white]\n", network)
			break
		}
		bot.mu.Lock()
		if bot.Network.Channels == nil {
			bot.Network.Channels = make(map[string]ChannelConfig)
		}
		_, found := bot.Network.Channels[channel]
		if found {
			delete(bot.Network.Channels, channel)
		}
		bot.mu.Unlock()
		if !found {
			fmt.Fprintf(logView, "[yellow]Not in %s on %s[white]\n", channel, network)
			break
		}
		if len(parts) >= 4 {
			bot.Client.Cmd.PartMessage(channel, parts[3])
		} else {
			bot.Client.Cmd.Part(channel)
		}
		fmt.Fprintf(logView, "[green]Parted %s on %s[white]\n", channel, network)
	case "/nick":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /nick <network> <nick>[white]\n")
			break
		}
		network, nick := parts[1], parts[2]
		bot, ok := bots[network]
		if !ok {
			fmt.Fprintf(logView, "[red]Unknown network: %s[white]\n", network)
			break
		}
		bot.mu.Lock()
		bot.Network.Nick = nick
		bot.mu.Unlock()
		bot.Client.Config.Nick = nick
		bot.Client.Cmd.Nick(nick)
		fmt.Fprintf(logView, "[green]Nick change to %s on %s[white]\n", nick, network)
	default:
		fmt.Fprintf(logView, "[yellow]Unknown command: %s[white]\n", text)
	}
	autoScroll = true
	logView.ScrollToEnd()
}

func requestShutdown() {
	if !atomic.CompareAndSwapInt32(&shutdownOnce, 0, 1) {
		return
	}
	go func() {
		logger.Info("Shutdown requested via TUI")

		if logFlushStop != nil {
			close(logFlushStop)
			<-logFlushDone
		}

		StopPendingSave()
		SaveContextStore()
		if apiLogger != nil {
			apiLogger.CloseAll()
		}
		if queueMgr != nil {
			queueMgr.Stop()
		}
		stopJobManager()
		closeMCPClients()

		for _, bot := range bots {
			bot.Quit()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(1 * time.Second) // give IRC connections time to flush the QUIT message

		closeDB(theDB)
		closeLogFile()
		if tuiApp != nil {
			tuiApp.QueueUpdateDraw(func() {
				tuiApp.Stop()
			})
		}
	}()
}

func stopTUI() {
	if tuiApp != nil {
		tuiApp.Stop()
	}
}
