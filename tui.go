package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

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

	switch cmd {
	case "/help":
		fmt.Fprintf(logView, "[white]Commands:\n")
		fmt.Fprintf(logView, "  /help                        - Show this help\n")
		fmt.Fprintf(logView, "  /reload                      - Reload config from disk\n")
		fmt.Fprintf(logView, "  /reload <mcp-name>           - Reload MCP server config (SIGHUP/HTTP)\n")
		fmt.Fprintf(logView, "  /quit, /exit                 - Shut down\n")
		fmt.Fprintf(logView, "  /join <network> <channel>    - Join a channel\n")
		fmt.Fprintf(logView, "  /part <network> <channel> [message]\n")
		fmt.Fprintf(logView, "                               - Leave a channel\n")
		fmt.Fprintf(logView, "  /nick <network> <nick>       - Change nickname\n")
		fmt.Fprintf(logView, "  /ban <network> <nick> <duration> [reason]\n")
		fmt.Fprintf(logView, "                               - Ban a user\n")
		fmt.Fprintf(logView, "  /unban <network> <nick>      - Unban a user\n")
		fmt.Fprintf(logView, "  /bans [network]              - List active bans\n")
		fmt.Fprintf(logView, "  /banhistory <network> <nick>  - Show ban history for a user\n")
		fmt.Fprintf(logView, "  /user <network> <nick|id>    - Show user details\n")
		fmt.Fprintf(logView, "  /usersearch <network> <query> - Search users by nick/account/host\n")
		fmt.Fprintf(logView, "  /usermerge <ghost_id> <target_id> [hash]\n")
		fmt.Fprintf(logView, "                               - Merge ghost user into target\n")
		fmt.Fprintf(logView, "  /userrelease <network> <nick|id>\n")
		fmt.Fprintf(logView, "                               - Release a user's nick claim\n")
		fmt.Fprintf(logView, "  /flagged [network]           - List flagged users (resolveUser fallback rows)\n")
		fmt.Fprintf(logView, "  /sessions <network> <nick|id> [channel]\n")
		fmt.Fprintf(logView, "                               - List sessions for a user\n")
		fmt.Fprintf(logView, "  /compact <session-id>        - Summarize old messages of a session\n")
	case "/reload":
		if len(parts) >= 2 {
			mcpName := parts[1]
			result, err := signalMCPServer(mcpName)
			if err != nil {
				fmt.Fprintf(logView, "[red]Reload %s failed: %s[white]\n", mcpName, err)
			} else {
				fmt.Fprintf(logView, "[green]Reload signal sent to %s[white]\n", mcpName)
				for _, w := range result.Warnings {
					fmt.Fprintf(logView, "[yellow]Warning: %s[white]\n", w)
				}
			}
		} else {
			if err := reloadAll(); err != nil {
				fmt.Fprintf(logView, "[red]Reload failed: %s[white]\n", err)
			} else {
				fmt.Fprintf(logView, "[green]Reloaded commands, services, and prompt enhancements[white]\n")
			}
		}
	case "/quit", "/exit":
		requestShutdown()
	case "/join":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /join <network> <channel>[white]\n")
			break
		}
		network, channel := parts[1], parts[2]
		bot, ok := getBot(network)
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
		bot, ok := getBot(network)
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
		bot, ok := getBot(network)
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
	case "/ban":
		if len(parts) < 4 {
			fmt.Fprintf(logView, "[yellow]Usage: /ban <network> <nick> <duration> [reason][white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		parts = strings.SplitN(text, " ", 5)
		network, banNick := parts[1], parts[2]
		durationStr := parts[3]
		reason := "manual ban"
		if len(parts) >= 5 {
			reason = parts[4]
		}
		duration, err := parseBanDuration(durationStr)
		if err != nil {
			fmt.Fprintf(logView, "[red]Invalid duration: %s[white]\n", err)
			break
		}
		var maxDur time.Duration
		readConfig(func() {
			maxDur, _ = parseBanDuration(config.Bans.MaxDuration)
		})
		if maxDur > 0 && duration > maxDur {
			fmt.Fprintf(logView, "[yellow]Capping ban duration from %s to max %s[white]\n", formatDuration(duration), formatDuration(maxDur))
			duration = maxDur
		}
		cm := getCasemapping(network)
		user, err := resolveUserByNick(network, banNick, cm)
		if err != nil || user == nil {
			fmt.Fprintf(logView, "[red]User %s not found on %s[white]\n", banNick, network)
			break
		}
		_, err = createBan(theDB, user.ID, network, "", "", reason, duration, nil, "tui")
		if err != nil {
			fmt.Fprintf(logView, "[red]Failed to ban: %s[white]\n", err)
			break
		}
		fmt.Fprintf(logView, "[green]Banned %s on %s for %s: %s[white]\n", banNick, network, formatDuration(duration), reason)
	case "/unban":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /unban <network> <nick>[white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		network, unbanNick := parts[1], parts[2]
		cm := getCasemapping(network)
		user, err := resolveUserByNick(network, unbanNick, cm)
		if err != nil || user == nil {
			fmt.Fprintf(logView, "[red]User %s not found on %s[white]\n", unbanNick, network)
			break
		}
		if err := deactivateBansForUser(theDB, user.ID, network); err != nil {
			fmt.Fprintf(logView, "[red]Failed to unban: %s[white]\n", err)
			break
		}
		fmt.Fprintf(logView, "[green]Unbanned %s on %s[white]\n", unbanNick, network)
	case "/bans":
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		network := ""
		if len(parts) >= 2 {
			network = parts[1]
		}
		if network == "" {
			var bans []Ban
			theDB.Where("active = ?", true).Order("created_at DESC").Find(&bans)
			if len(bans) == 0 {
				fmt.Fprintf(logView, "[white]No active bans.[white]\n")
				break
			}
			for _, b := range bans {
				fmt.Fprintf(logView, "[white]#%d %s/%d %s expires %s[white]\n", b.ID, b.Network, b.UserID, b.Reason, b.ExpiresAt.Format("2006-01-02 15:04"))
			}
			break
		}
		bans, err := getActiveBans(theDB, network)
		if err != nil {
			fmt.Fprintf(logView, "[red]Failed to list bans: %s[white]\n", err)
			break
		}
		if len(bans) == 0 {
			fmt.Fprintf(logView, "[white]No active bans on %s.[white]\n", network)
			break
		}
		for _, b := range bans {
			var user User
			theDB.First(&user, b.UserID)
			fmt.Fprintf(logView, "[white]#%d %s (%s) %s expires %s[white]\n", b.ID, tview.Escape(displayNick(&user)), formatDuration(b.Duration), tview.Escape(b.Reason), b.ExpiresAt.Format("2006-01-02 15:04"))
		}
	case "/banhistory":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /banhistory <network> <nick>[white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		network, histNick := parts[1], parts[2]
		cm := getCasemapping(network)
		user, err := resolveUserByNick(network, histNick, cm)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error looking up user: %s[white]\n", err)
			break
		}
		if user == nil {
			fmt.Fprintf(logView, "[yellow]User %s not found on %s[white]\n", histNick, network)
			break
		}
		bans, err := getBanHistory(theDB, user.ID)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error fetching ban history: %s[white]\n", err)
			break
		}
		if len(bans) == 0 {
			fmt.Fprintf(logView, "[white]No ban history for %s (id: %d).[white]\n", tview.Escape(displayNick(user)), user.ID)
			break
		}
		fmt.Fprintf(logView, "[white]Ban history for %s (id: %d):[white]\n", tview.Escape(displayNick(user)), user.ID)
		for _, b := range bans {
			status := "expired"
			if b.Active {
				status = "ACTIVE"
			}
			fmt.Fprintf(logView, "[white]#%d %s %s (%s) by %s, %s ago[white]\n", b.ID, status, tview.Escape(b.Reason), formatDuration(b.Duration), tview.Escape(b.BannerNick), formatDuration(time.Since(b.CreatedAt)))
		}
	case "/user":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /user <network> <nick|id>[white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		network, userRef := parts[1], parts[2]
		var info *UserInfo
		if id, err := strconv.ParseInt(userRef, 10, 64); err == nil {
			var infoErr error
			info, infoErr = getUserInfo(id)
			if infoErr != nil {
				fmt.Fprintf(logView, "[red]Error: %s[white]\n", infoErr)
				break
			}
		} else {
			cm := getCasemapping(network)
			user, err := resolveUserByNick(network, userRef, cm)
			if err != nil {
				fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
				break
			}
			if user == nil {
				fmt.Fprintf(logView, "[yellow]User %s not found on %s[white]\n", userRef, network)
				break
			}
			info, err = getUserInfo(user.ID)
			if err != nil {
				fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
				break
			}
		}
		if info == nil {
			fmt.Fprintf(logView, "[yellow]User not found[white]\n")
			break
		}
		printUserInfo(logView, info)
	case "/usersearch":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /usersearch <network> <query>[white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		network, query := parts[1], parts[2]
		results, err := searchUsers(network, query)
		if err != nil {
			fmt.Fprintf(logView, "[red]Search error: %s[white]\n", err)
			break
		}
		if len(results) == 0 {
			fmt.Fprintf(logView, "[white]No users matching %q on %s.[white]\n", query, network)
			break
		}
		fmt.Fprintf(logView, "[white]%d user(s) matching %q on %s:[white]\n", len(results), query, network)
		for _, r := range results {
			released := ""
			if r.Released {
				released = " [red](released)[white]"
			}
			account := ""
			if r.IRCAccount != "" {
				account = fmt.Sprintf(" account:%s", tview.Escape(r.IRCAccount))
			}
			fmt.Fprintf(logView, "[white]  #%d %s hosts:%d sessions:%d%s%s[white]\n",
				r.ID, tview.Escape(r.DisplayName()), r.HostCount, r.SessionCount, account, released)
		}
	case "/usermerge":
		parts = strings.SplitN(text, " ", 5)
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /usermerge <ghost_id> <target_id> [hash][white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		ghostID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			fmt.Fprintf(logView, "[red]Invalid ghost user ID: %s[white]\n", parts[1])
			break
		}
		targetID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			fmt.Fprintf(logView, "[red]Invalid target user ID: %s[white]\n", parts[2])
			break
		}
		if ghostID == targetID {
			fmt.Fprintf(logView, "[red]Cannot merge a user into itself[white]\n")
			break
		}
		ghost, err := getUserByID(ghostID)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
			break
		}
		if ghost == nil {
			fmt.Fprintf(logView, "[red]Ghost user #%d not found[white]\n", ghostID)
			break
		}
		target, err := getUserByID(targetID)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
			break
		}
		if target == nil {
			fmt.Fprintf(logView, "[red]Target user #%d not found[white]\n", targetID)
			break
		}

		if len(parts) >= 4 && parts[3] != "" {
			expected := computeMergeHash(ghost, target)
			if parts[3] != expected {
				fmt.Fprintf(logView, "[red]Hash mismatch. Users may have changed. Re-run without hash to verify.[white]\n")
				break
			}
			if err := mergeUser(ghostID, targetID); err != nil {
				fmt.Fprintf(logView, "[red]Merge failed: %s[white]\n", err)
				break
			}
			fmt.Fprintf(logView, "[green]Merged user #%d (%s) into #%d (%s)[white]\n",
				ghostID, tview.Escape(displayNick(ghost)), targetID, tview.Escape(displayNick(target)))
		} else {
			fmt.Fprintf(logView, "[white]Ghost (will be deleted):[white]\n")
			ghostInfo, _ := getUserInfo(ghostID)
			if ghostInfo != nil {
				printUserInfo(logView, ghostInfo)
			}
			fmt.Fprintf(logView, "[white]Target (will survive):[white]\n")
			targetInfo, _ := getUserInfo(targetID)
			if targetInfo != nil {
				printUserInfo(logView, targetInfo)
			}
			hash := computeMergeHash(ghost, target)
			fmt.Fprintf(logView, "[yellow]Confirm: /usermerge %d %d %s[white]\n", ghostID, targetID, hash)
		}
	case "/userrelease":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /userrelease <network> <nick|id>[white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		network, userRef := parts[1], parts[2]
		var user *User
		if id, err := strconv.ParseInt(userRef, 10, 64); err == nil {
			user, err = getUserByID(id)
			if err != nil {
				fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
				break
			}
		} else {
			cm := getCasemapping(network)
			var err error
			user, err = resolveUserByNick(network, userRef, cm)
			if err != nil {
				fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
				break
			}
		}
		if user == nil {
			fmt.Fprintf(logView, "[yellow]User not found[white]\n")
			break
		}
		if user.Released {
			fmt.Fprintf(logView, "[yellow]User #%d nick is already released[white]\n", user.ID)
			break
		}
		oldNick := displayNick(user)
		if err := releaseUserNick(user.ID); err != nil {
			fmt.Fprintf(logView, "[red]Failed to release nick: %s[white]\n", err)
			break
		}
		fmt.Fprintf(logView, "[green]Released nick %q for user #%d on %s[white]\n",
			tview.Escape(oldNick), user.ID, network)
	case "/flagged":
		// /flagged [network] — list current flagged users (resolveUser
		// fallback rows). Helps admin find and merge them via /usermerge.
		var netFilter string
		if len(parts) >= 2 {
			netFilter = parts[1]
		}
		flagged, err := getFlaggedUsers(netFilter)
		if err != nil {
			fmt.Fprintf(logView, "[red]Failed to list flagged users: %s[white]\n", err)
			break
		}
		if len(flagged) == 0 {
			if netFilter == "" {
				fmt.Fprintf(logView, "[dim]No flagged users.[white]\n")
			} else {
				fmt.Fprintf(logView, "[dim]No flagged users on %s.[white]\n", tview.Escape(netFilter))
			}
			break
		}
		header := "Flagged users:"
		if netFilter != "" {
			header = fmt.Sprintf("Flagged users on %s:", netFilter)
		}
		fmt.Fprintf(logView, "[white]%s[white]\n", header)
		for _, u := range flagged {
			account := ""
			if u.IRCAccount != "" {
				account = fmt.Sprintf(" account:%s", tview.Escape(u.IRCAccount))
			}
			fmt.Fprintf(logView, "[yellow]  #%d[white] %s%s reason:%s network:%s created:%s\n",
				u.ID,
				tview.Escape(displayNick(&u)),
				account,
				tview.Escape(u.FlaggedReason),
				tview.Escape(u.Network),
				u.CreatedAt.Format("2006-01-02 15:04:05"))
		}
	case "/sessions":
		if len(parts) < 3 {
			fmt.Fprintf(logView, "[yellow]Usage: /sessions <network> <nick|id> [channel][white]\n")
			break
		}
		if theDB == nil {
			fmt.Fprint(logView, tuiDBNotAvailable)
			break
		}
		network, userRef := parts[1], parts[2]
		cm := getCasemapping(network)
		var userID int64
		if id, err := strconv.ParseInt(userRef, 10, 64); err == nil {
			userID = id
		} else {
			user, err := resolveUserByNick(network, userRef, cm)
			if err != nil {
				fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
				break
			}
			if user == nil {
				fmt.Fprintf(logView, "[yellow]User %s not found on %s[white]\n", tview.Escape(userRef), network)
				break
			}
			userID = user.ID
		}
		var limit int
		readConfig(func() { limit = config.SessionsDisplayLimit })
		var sessions []Session
		var err error
		showChannel := false
		if len(parts) >= 4 {
			channel := normalizeIRC(parts[3], cm)
			sessions, err = getUserDBSessions(network, channel, userID, limit)
		} else {
			sessions, err = getUserDBSessionsByNetwork(network, userID, limit)
			showChannel = true
		}
		if err != nil {
			fmt.Fprintf(logView, "[red]Error querying sessions: %s[white]\n", err)
			break
		}
		if len(sessions) == 0 {
			fmt.Fprintf(logView, "[white]No sessions found for %s on %s[white]\n", tview.Escape(userRef), network)
			break
		}
		var trigger string
		readConfig(func() { trigger = config.Trigger })
		if trigger == "" {
			trigger = "!"
		}
		type sessionLine struct {
			icon    string
			idStr   string
			channel string
			msgStr  string
			timeStr string
			cmd     string
			preview string
		}
		lines := make([]sessionLine, len(sessions))
		maxID := 0
		maxChan := 0
		maxMsg := 0
		maxTime := 0
		for i, s := range sessions {
			icon := "[green]●[white]"
			if s.Status != "active" {
				icon = "[red]○[white]"
			}
			var activeMsgs, archivedMsgs int64
			theDB.Model(&Message{}).Where("session_id = ? AND archived = ?", s.ID, false).Count(&activeMsgs)
			theDB.Model(&Message{}).
				Where("session_id = ? AND archived = ? AND superseded = ?", s.ID, true, false).
				Count(&archivedMsgs)
			idStr := fmt.Sprintf("#%d", s.ID)
			msgStr := fmt.Sprintf("%d msgs", activeMsgs)
			if archivedMsgs > 0 {
				msgStr = fmt.Sprintf("%d msgs (%d archived)", activeMsgs, archivedMsgs)
			}
			timeStr := formatTimeAgo(s.LastActive)
			preview := strings.ReplaceAll(s.FirstMessage, "\n", " ")
			if len(preview) > 80 {
				preview = preview[:77] + "..."
			}
			lines[i] = sessionLine{icon, idStr, tview.Escape(s.Channel), msgStr, timeStr, tview.Escape(s.ChatCommand), tview.Escape(preview)}
			if l := utf8.RuneCountInString(idStr); l > maxID {
				maxID = l
			}
			if showChannel {
				if l := utf8.RuneCountInString(s.Channel); l > maxChan {
					maxChan = l
				}
			}
			if l := utf8.RuneCountInString(msgStr); l > maxMsg {
				maxMsg = l
			}
			if l := utf8.RuneCountInString(timeStr); l > maxTime {
				maxTime = l
			}
		}
		for _, l := range lines {
			var line string
			if showChannel {
				line = fmt.Sprintf("  %s %s  %s  %s  %s  %s%s",
					l.icon,
					l.idStr+strings.Repeat(" ", maxID-utf8.RuneCountInString(l.idStr)),
					l.channel+strings.Repeat(" ", maxChan-utf8.RuneCountInString(l.channel)),
					l.msgStr+strings.Repeat(" ", maxMsg-utf8.RuneCountInString(l.msgStr)),
					l.timeStr+strings.Repeat(" ", maxTime-utf8.RuneCountInString(l.timeStr)),
					tview.Escape(trigger),
					l.cmd,
				)
			} else {
				line = fmt.Sprintf("  %s %s  %s  %s  %s%s",
					l.icon,
					l.idStr+strings.Repeat(" ", maxID-utf8.RuneCountInString(l.idStr)),
					l.msgStr+strings.Repeat(" ", maxMsg-utf8.RuneCountInString(l.msgStr)),
					l.timeStr+strings.Repeat(" ", maxTime-utf8.RuneCountInString(l.timeStr)),
					tview.Escape(trigger),
					l.cmd,
				)
			}
			if l.preview != "" {
				line += " " + l.preview
			}
			fmt.Fprintln(logView, line)
		}
	case "/compact":
		if len(parts) < 2 {
			fmt.Fprintf(logView, "[yellow]Usage: /compact <session-id>[white]\n")
			break
		}
		sessionID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			fmt.Fprintf(logView, "[red]Invalid session id: %s[white]\n", parts[1])
			break
		}
		session, err := getDBSessionByID(sessionID)
		if err != nil || session == nil {
			fmt.Fprintf(logView, "[red]Session %d not found[white]\n", sessionID)
			break
		}
		bot, ok := getBot(session.Network)
		if !ok {
			fmt.Fprintf(logView, "[red]Unknown network for session: %s[white]\n", session.Network)
			break
		}
		var cfg AIConfig
		var cfgOk bool
		readConfig(func() { cfg, cfgOk = config.Commands.Chats[session.ChatCommand] })
		if session.SettingsID != nil {
			if settings, sErr := sessionMgr.GetSessionSettings(*session.SettingsID); sErr == nil && settings != nil {
				cfg = ApplySettings(settings, cfg)
				cfgOk = true
			}
		}
		if !cfgOk {
			fmt.Fprintf(logView, "[red]Chat command %q for session %d no longer exists[white]\n", session.ChatCommand, sessionID)
			break
		}
		userNick := ""
		if session.UserID != nil {
			if u, err := getUserByID(*session.UserID); err == nil && u != nil {
				userNick = displayNick(u)
			}
		}
		fmt.Fprintf(logView, "[white]Compacting session #%d...[white]\n", sessionID)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			res, err := sessionMgr.CompactSession(ctx, CompactSessionInputs{
				SessionID: sessionID,
				Network:   bot.Network,
				Channel:   session.Channel,
				UserNick:  userNick,
				Client:    bot.Client,
				Trigger:   "manual",
			}, cfg)
			tuiApp.QueueUpdateDraw(func() {
				if err != nil {
					fmt.Fprintf(logView, "[red]Compaction failed for session %d: %s[white]\n", sessionID, err)
					return
				}
				vars := compactionNoticeVars(res, sessionID)
				fmt.Fprintf(logView, "[green]Compacted session %d: %s messages, total: %s tokens, cached: %s, %dms[white]\n",
					sessionID, vars["count"], vars["total"], vars["cached"], res.DurationMs)
			})
		}()
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

		if statusBarStop != nil {
			close(statusBarStop)
			<-statusBarDone
		}

		if apiLogger != nil {
			apiLogger.CloseAll()
		}
		if queueMgr != nil {
			queueMgr.Stop()
		}
		stopJobManager()
		stopToolJobManager()
		closeMCPClients()

		for _, bot := range snapshotBots() {
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
