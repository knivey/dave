package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lrstanley/girc"
)

func getNotices() NoticesConfig {
	var n NoticesConfig
	readConfig(func() { n = config.Notices })
	return n
}

func historySessions(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	n := getNotices()
	var maxHistory int
	readConfig(func() { maxHistory = config.SessionsDisplayLimit })

	casemapping := getCasemapping(network.Name)
	channel := normalizeIRC(e.Params[0], casemapping)

	var arg string
	if len(args) > 0 {
		arg = args[0]
	}

	if arg == "*" {
		if theDB == nil {
			select {
			case output <- errorMsg(n.DB.NotAvailable):
			case <-ctx.Done():
			}
			return
		}
		results, err := getChannelDBSessions(network.Name, channel, maxHistory)
		if err != nil {
			select {
			case output <- errorNotice(n.DB.QuerySessions, map[string]string{"error": err.Error()}):
			case <-ctx.Done():
			}
			return
		}
		if len(results) == 0 {
			select {
			case output <- n.Sessions.None:
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- expandNotice(n.Sessions.AllHeader, map[string]string{"network": network.Name, "channel": channel}):
		case <-ctx.Done():
			return
		}
		sendSessionsLinesWithNick(output, ctx, results, network.Trigger)
		return
	}

	account := ""
	if u := c.LookupUser(e.Source.Name); u != nil {
		account = u.Extras.Account
	}
	resolvedUser, _ := resolveUser(network.Name, e.Source.Name, e.Source.Ident, e.Source.Host, account, casemapping)
	var userID int64
	if resolvedUser != nil {
		userID = resolvedUser.ID
	}

	if arg != "" {
		if theDB == nil {
			select {
			case output <- errorMsg(n.DB.NotAvailable):
			case <-ctx.Done():
			}
			return
		}
		targetUser, err := resolveUserByNick(network.Name, arg, casemapping)
		if err != nil || targetUser == nil {
			select {
			case output <- expandNotice(n.Sessions.OtherNone, map[string]string{"nick": arg}):
			case <-ctx.Done():
			}
			return
		}
		sessions, err := getUserDBSessions(network.Name, channel, targetUser.ID, maxHistory)
		if err != nil {
			select {
			case output <- errorNotice(n.DB.QuerySessions, map[string]string{"error": err.Error()}):
			case <-ctx.Done():
			}
			return
		}
		if len(sessions) == 0 {
			select {
			case output <- expandNotice(n.Sessions.OtherNone, map[string]string{"nick": arg}):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- expandNotice(n.Sessions.OtherHeader, map[string]string{"nick": displayNick(targetUser), "network": network.Name}):
		case <-ctx.Done():
			return
		}
		var swu []SessionWithUser
		for _, s := range sessions {
			swu = append(swu, SessionWithUser{Session: s, OwnerNick: displayNick(targetUser)})
		}
		sendSessionsLinesWithNick(output, ctx, swu, network.Trigger)
		return
	}

	sessions, err := getUserDBSessions(network.Name, channel, userID, maxHistory)
	if err != nil {
		select {
		case output <- errorNotice(n.DB.QuerySessions, map[string]string{"error": err.Error()}):
		case <-ctx.Done():
		}
		return
	}

	if len(sessions) == 0 {
		select {
		case output <- n.Sessions.None:
		case <-ctx.Done():
		}
		return
	}

	select {
	case output <- expandNotice(n.Sessions.Header, map[string]string{"nick": e.Source.Name, "network": network.Name}):
	case <-ctx.Done():
		return
	}

	sendSessionsLinesSelf(output, ctx, sessions, network.Trigger)
}

func sendSessionsLinesSelf(output chan<- string, ctx context.Context, sessions []Session, trigger string) {
	var swu []SessionWithUser
	for _, s := range sessions {
		swu = append(swu, SessionWithUser{Session: s})
	}
	sendSessionsLines(output, ctx, swu, trigger, false)
}

func sendSessionsLinesWithNick(output chan<- string, ctx context.Context, sessions []SessionWithUser, trigger string) {
	sendSessionsLines(output, ctx, sessions, trigger, true)
}

func sendSessionsLines(output chan<- string, ctx context.Context, sessions []SessionWithUser, trigger string, showNick bool) {
	type sessionLine struct {
		icon    string
		nickStr string
		idStr   string
		msgStr  string
		timeStr string
		cmd     string
		preview string
	}

	lines := make([]sessionLine, len(sessions))
	maxID := 0
	maxMsg := 0
	maxTime := 0
	maxNick := 0

	for i, s := range sessions {
		icon := "\x0303●\x0F"
		if s.Status != "active" {
			icon = "\x0304○\x0F"
		}

		var activeMsgs, archivedMsgs int64
		theDB.Model(&Message{}).Where("session_id = ? AND archived = ?", s.ID, false).Count(&activeMsgs)
		// Exclude superseded tail-copy ghosts from the archived count.
		// Those rows duplicate content already covered by an earlier
		// summary; counting them would mislead the user about how much
		// actual conversation got compacted. See compaction.go.
		theDB.Model(&Message{}).
			Where("session_id = ? AND archived = ? AND superseded = ?", s.ID, true, false).
			Count(&archivedMsgs)

		idStr := fmt.Sprintf("#%d", s.ID)
		msgStr := fmt.Sprintf("%d msgs", activeMsgs)
		if archivedMsgs > 0 {
			msgStr = fmt.Sprintf("%d msgs (%d archived)", activeMsgs, archivedMsgs)
		}
		timeStr := formatTimeAgo(s.LastActive)
		nickStr := s.OwnerNick
		preview := strings.ReplaceAll(s.FirstMessage, "\n", " ")
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}

		lines[i] = sessionLine{icon, nickStr, idStr, msgStr, timeStr, s.ChatCommand, preview}

		if l := utf8.RuneCountInString(idStr); l > maxID {
			maxID = l
		}
		if l := utf8.RuneCountInString(msgStr); l > maxMsg {
			maxMsg = l
		}
		if l := utf8.RuneCountInString(timeStr); l > maxTime {
			maxTime = l
		}
		if showNick {
			if l := utf8.RuneCountInString(nickStr); l > maxNick {
				maxNick = l
			}
		}
	}

	for _, l := range lines {
		var line string
		if showNick {
			line = fmt.Sprintf("  %s %s  %s  %s  %s  %s%s",
				l.icon,
				l.nickStr+strings.Repeat(" ", maxNick-utf8.RuneCountInString(l.nickStr)),
				l.idStr+strings.Repeat(" ", maxID-utf8.RuneCountInString(l.idStr)),
				l.msgStr+strings.Repeat(" ", maxMsg-utf8.RuneCountInString(l.msgStr)),
				l.timeStr+strings.Repeat(" ", maxTime-utf8.RuneCountInString(l.timeStr)),
				trigger,
				l.cmd,
			)
		} else {
			line = fmt.Sprintf("  %s %s  %s  %s  %s%s",
				l.icon,
				l.idStr+strings.Repeat(" ", maxID-utf8.RuneCountInString(l.idStr)),
				l.msgStr+strings.Repeat(" ", maxMsg-utf8.RuneCountInString(l.msgStr)),
				l.timeStr+strings.Repeat(" ", maxTime-utf8.RuneCountInString(l.timeStr)),
				trigger,
				l.cmd,
			)
		}
		if l.preview != "" {
			line += " " + l.preview
		}

		for _, wrapped := range wrapLine(line) {
			select {
			case output <- wrapped:
			case <-ctx.Done():
				return
			}
		}
	}
}

func historyShow(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	n := getNotices()
	sendErr := func(msg string) {
		select {
		case output <- msg:
		case <-ctx.Done():
		}
	}
	if theDB == nil {
		sendErr(errorMsg(n.DB.NotAvailable))
		return
	}

	if len(args) == 0 || args[0] == "" {
		sendErr(errorNotice(n.Sessions.HistoryUsage, map[string]string{"trigger": network.Trigger}))
		return
	}

	sessionID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		sendErr(errorMsg(n.Sessions.InvalidID))
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil {
		sendErr(errorMsg(n.Sessions.NotFound))
		return
	}

	if session.Network != network.Name || session.Channel != normalizeIRC(e.Params[0], getCasemapping(network.Name)) {
		sendErr(errorMsg(n.Sessions.NotOwned))
		return
	}
	if session.UserID != nil {
		casemapping := getCasemapping(network.Name)
		account := ""
		if u := c.LookupUser(e.Source.Name); u != nil {
			account = u.Extras.Account
		}
		resolvedUser, _ := resolveUser(network.Name, e.Source.Name, e.Source.Ident, e.Source.Host, account, casemapping)
		if resolvedUser == nil || resolvedUser.ID != *session.UserID {
			sendErr(errorMsg(n.Sessions.NotOwned))
			return
		}
	} else {
		sendErr(errorMsg(n.Sessions.NotOwned))
		return
	}

	messages, err := loadDBSessionMessagesAll(sessionID)
	if err != nil {
		sendErr(errorNotice(n.DB.LoadMessages, map[string]string{"error": err.Error()}))
		return
	}

	var visible []Message
	var archivedCount int
	for _, m := range messages {
		if m.Role == "system" || m.Role == "tool" {
			continue
		}
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && m.ToolCalls != nil {
			continue
		}
		visible = append(visible, m)
		if m.Archived {
			archivedCount++
		}
	}
	activeCount := len(visible) - archivedCount

	archivedSuffix := ""
	if archivedCount > 0 {
		archivedSuffix = fmt.Sprintf(" (%d archived)", archivedCount)
	}

	select {
	case output <- expandNotice(n.Sessions.DetailHeader, map[string]string{
		"id":              fmt.Sprintf("%d", sessionID),
		"command":         session.ChatCommand,
		"count":           fmt.Sprintf("%d", activeCount),
		"active":          fmt.Sprintf("%d", activeCount),
		"archived":        fmt.Sprintf("%d", archivedCount),
		"archived_suffix": archivedSuffix,
		"total":           fmt.Sprintf("%d", len(visible)),
	}):
	case <-ctx.Done():
		return
	}

	var shown []Message
	if len(visible) <= 4 {
		shown = visible
	} else {
		shown = make([]Message, 4)
		copy(shown[:2], visible[:2])
		copy(shown[2:], visible[len(visible)-2:])
	}

	sendHistoryMsg := func(m Message) {
		var roleIcon string
		switch m.Role {
		case "user":
			roleIcon = "\x0312►\x0F"
		case "assistant":
			roleIcon = "\x0303◄\x0F"
		default:
			roleIcon = "·"
		}

		content := m.Content
		if len(content) > 200 {
			content = content[:197] + "..."
		}
		content = strings.ReplaceAll(content, "\n", " ")

		archivedTag := ""
		if m.Archived {
			archivedTag = "\x0314[archived]\x0F "
		}
		line := fmt.Sprintf("  %s %s%s", roleIcon, archivedTag, content)
		for _, wrapped := range wrapLine(line) {
			select {
			case output <- wrapped:
			case <-ctx.Done():
				return
			}
		}
	}

	if len(visible) > 4 {
		sendHistoryMsg(shown[0])
		sendHistoryMsg(shown[1])
		select {
		case output <- expandNotice(n.Sessions.Truncated, map[string]string{"count": fmt.Sprintf("%d", len(visible)-4)}):
		case <-ctx.Done():
			return
		}
		sendHistoryMsg(shown[2])
		sendHistoryMsg(shown[3])
	} else {
		for _, m := range shown {
			sendHistoryMsg(m)
		}
	}
}

func historyStats(network Network, c *girc.Client, e girc.Event, args ...string) {
	n := getNotices()
	if theDB == nil {
		c.Cmd.Reply(e, errorMsg(n.DB.NotAvailable))
		return
	}

	casemapping := getCasemapping(network.Name)
	account := ""
	if u := c.LookupUser(e.Source.Name); u != nil {
		account = u.Extras.Account
	}
	resolvedUser, _ := resolveUser(network.Name, e.Source.Name, e.Source.Ident, e.Source.Host, account, casemapping)
	var userID int64
	if resolvedUser != nil {
		userID = resolvedUser.ID
	}
	channel := normalizeIRC(e.Params[0], casemapping)

	sessionCount, messageCount, err := getUserDBStats(network.Name, channel, userID)
	if err != nil {
		c.Cmd.Reply(e, errorNotice(n.DB.QueryStats, map[string]string{"error": err.Error()}))
		return
	}

	c.Cmd.Reply(e, expandNotice(n.Sessions.StatsFormat, map[string]string{"network": network.Name, "channel": channel, "sessions": fmt.Sprintf("%d", sessionCount), "messages": fmt.Sprintf("%d", messageCount)}))
}

func historyDelete(network Network, c *girc.Client, e girc.Event, args ...string) {
	n := getNotices()
	if theDB == nil {
		c.Cmd.Reply(e, errorMsg(n.DB.NotAvailable))
		return
	}

	if len(args) == 0 || args[0] == "" {
		c.Cmd.Reply(e, errorNotice(n.Sessions.DeleteUsage, map[string]string{"trigger": network.Trigger}))
		return
	}

	sessionID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		c.Cmd.Reply(e, errorMsg(n.Sessions.InvalidID))
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorMsg(n.Sessions.NotFound))
		return
	}

	if session.Network != network.Name || session.Channel != normalizeIRC(e.Params[0], getCasemapping(network.Name)) {
		c.Cmd.Reply(e, errorMsg(n.Sessions.NotOwned))
		return
	}
	if session.UserID != nil {
		casemapping := getCasemapping(network.Name)
		account := ""
		if u := c.LookupUser(e.Source.Name); u != nil {
			account = u.Extras.Account
		}
		resolvedUser, _ := resolveUser(network.Name, e.Source.Name, e.Source.Ident, e.Source.Host, account, casemapping)
		if resolvedUser == nil || resolvedUser.ID != *session.UserID {
			c.Cmd.Reply(e, errorMsg(n.Sessions.NotOwned))
			return
		}
	} else {
		c.Cmd.Reply(e, errorMsg(n.Sessions.NotOwned))
		return
	}

	cancelAsyncJobsForSession(sessionID)

	if err := deleteDBSession(sessionID); err != nil {
		c.Cmd.Reply(e, errorNotice(n.DB.DeleteFailed, map[string]string{"error": err.Error()}))
		return
	}

	c.Cmd.Reply(e, expandNotice(n.Sessions.Deleted, map[string]string{"id": fmt.Sprintf("%d", sessionID)}))
}

func historyResume(network Network, c *girc.Client, e girc.Event, args ...string) {
	n := getNotices()
	if theDB == nil {
		c.Cmd.Reply(e, errorMsg(n.DB.NotAvailable))
		return
	}

	if len(args) == 0 || args[0] == "" {
		c.Cmd.Reply(e, errorNotice(n.Sessions.ResumeUsage, map[string]string{"trigger": network.Trigger}))
		return
	}

	sessionID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		c.Cmd.Reply(e, errorMsg(n.Sessions.InvalidID))
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorMsg(n.Sessions.NotFound))
		return
	}

	if session.Network != network.Name || session.Channel != normalizeIRC(e.Params[0], getCasemapping(network.Name)) {
		c.Cmd.Reply(e, errorMsg(n.Sessions.NotOwned))
		return
	}
	if session.UserID != nil {
		casemapping := getCasemapping(network.Name)
		account := ""
		if u := c.LookupUser(e.Source.Name); u != nil {
			account = u.Extras.Account
		}
		resolvedUser, _ := resolveUser(network.Name, e.Source.Name, e.Source.Ident, e.Source.Host, account, casemapping)
		if resolvedUser == nil || resolvedUser.ID != *session.UserID {
			c.Cmd.Reply(e, errorMsg(n.Sessions.NotOwned))
			return
		}
	} else {
		c.Cmd.Reply(e, errorMsg(n.Sessions.NotOwned))
		return
	}

	var currentCfg AIConfig
	var cfgOk bool
	readConfig(func() {
		currentCfg, cfgOk = config.Commands.Chats[session.ChatCommand]
	})
	if session.SettingsID != nil {
		settings, err := sessionMgr.GetSessionSettings(*session.SettingsID)
		if err != nil {
			c.Cmd.Reply(e, warnMsg("failed to load stored session config: "+err.Error()))
		} else if settings != nil {
			currentCfg = ApplySettings(settings, currentCfg)
			cfgOk = true
		}
	}
	if !cfgOk {
		c.Cmd.Reply(e, errorNotice(n.Sessions.CommandGone, map[string]string{"command": session.ChatCommand}))
		return
	}

	dbMsgs, err := loadDBSessionMessages(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorNotice(n.DB.LoadMessages, map[string]string{"error": err.Error()}))
		return
	}

	var messages []ChatMessage
	for _, dm := range dbMsgs {
		messages = append(messages, messageFromDB(dm))
	}

	if len(messages) == 0 {
		c.Cmd.Reply(e, errorMsg(n.Sessions.NoMessages))
		return
	}

	messages = TruncateHistory(messages, currentCfg.MaxHistory)

	var resumeUserID int64
	if session.UserID != nil {
		resumeUserID = *session.UserID
	}

	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	oldID, _ := sessionMgr.SwitchActive(network.Name, channel, resumeUserID, sessionID)
	if oldID != 0 {
		c.Cmd.Reply(e, expandNotice(n.Sessions.Paused, map[string]string{"id": fmt.Sprintf("%d", oldID)}))
	}

	apiLogger.RestoreSession(sessionID, network.Name, channel, resumeUserID)

	c.Cmd.Reply(e, expandNotice(n.Sessions.Resumed, map[string]string{"id": fmt.Sprintf("%d", sessionID), "command": session.ChatCommand, "count": fmt.Sprintf("%d", len(messages))}))
}

func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func historyJobs(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	n := getNotices()
	nick := e.Source.Name
	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	hasOutput := false

	casemapping := getCasemapping(network.Name)
	account := ""
	if u := c.LookupUser(nick); u != nil {
		account = u.Extras.Account
	}
	resolvedUser, _ := resolveUser(network.Name, nick, e.Source.Ident, e.Source.Host, account, casemapping)
	var userID int64
	if resolvedUser != nil {
		userID = resolvedUser.ID
	}

	sendLine := func(line string) bool {
		for _, wrapped := range wrapLine(line) {
			select {
			case output <- wrapped:
			case <-ctx.Done():
				return false
			}
		}
		return true
	}

	if queueMgr != nil {
		current, pending := queueMgr.QueueStatus(network.Name, channel, userID)
		if current != nil || len(pending) > 0 {
			hasOutput = true
			if !sendLine(n.Jobs.QueueHeader) {
				return
			}
			if current != nil {
				elapsed := time.Since(current.Enqueued).Truncate(time.Second)
				desc := current.Description
				if desc == "" {
					desc = "processing"
				}
				line := expandNotice(n.Jobs.QueueRunning, map[string]string{"desc": desc, "elapsed": elapsed.String()})
				if !sendLine(line) {
					return
				}
			}
			if len(pending) > 0 {
				for i, item := range pending {
					wait := time.Since(item.Enqueued).Truncate(time.Second)
					desc := item.Description
					if desc == "" {
						desc = "queued"
					}
					line := expandNotice(n.Jobs.QueuePending, map[string]string{"position": fmt.Sprintf("%d", i+1), "desc": desc, "wait": wait.String()})
					if !sendLine(line) {
						return
					}
				}
			}
		}
	}

	if theDB == nil {
		if !hasOutput {
			select {
			case output <- n.Jobs.NoJobs:
			case <-ctx.Done():
			}
		}
		return
	}

	jobs, err := getPendingJobsForUser(network.Name, channel, userID)
	if err != nil {
		select {
		case output <- errorNotice(n.DB.QueryJobs, map[string]string{"error": err.Error()}):
		case <-ctx.Done():
		}
		return
	}

	if len(jobs) == 0 {
		if !hasOutput {
			select {
			case output <- n.Jobs.NoJobs:
			case <-ctx.Done():
			}
		}
		return
	}

	if !sendLine(n.Jobs.BgHeader) {
		return
	}
	for _, j := range jobs {
		var statusIcon string
		switch j.Status {
		case "pending", "running":
			statusIcon = "\x0303●\x0F"
		case "completed":
			statusIcon = "\x0308◉\x0F"
		default:
			statusIcon = "·"
		}
		elapsed := formatTimeAgo(j.CreatedAt)
		line := expandNotice(n.Jobs.BgLine, map[string]string{"icon": statusIcon, "job_id": j.JobID, "tool": j.ToolName, "server": j.MCPServer, "status": j.Status, "elapsed": elapsed})
		if !sendLine(line) {
			return
		}
	}
}

// historyCompact is the IRC `^compact$` command handler. Compacts the user's
// active session in the current channel using the LLM associated with that
// session's chat command. Replies with a notice indicating success or
// failure. See compaction.go for the underlying algorithm.
func historyCompact(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	n := getNotices()
	send := func(msg string) {
		select {
		case output <- msg:
		case <-ctx.Done():
		}
	}
	if theDB == nil {
		send(errorMsg(n.DB.NotAvailable))
		return
	}

	var ccfg CompactionConfig
	readConfig(func() { ccfg = config.Compaction })
	if !ccfg.Enabled {
		send(errorMsg(n.Compaction.Disabled))
		return
	}

	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	casemapping := getCasemapping(network.Name)
	account := ""
	if u := c.LookupUser(e.Source.Name); u != nil {
		account = u.Extras.Account
	}
	resolvedUser, err := resolveUser(network.Name, e.Source.Name, e.Source.Ident, e.Source.Host, account, casemapping)
	if err != nil || resolvedUser == nil {
		send(errorMsg(n.Compaction.NoActive))
		return
	}

	session, err := sessionMgr.GetActiveSession(network.Name, channel, resolvedUser.ID)
	if err != nil || session == nil {
		send(errorMsg(n.Compaction.NoActive))
		return
	}

	var cfg AIConfig
	var cfgOk bool
	readConfig(func() {
		cfg, cfgOk = config.Commands.Chats[session.ChatCommand]
	})
	if session.SettingsID != nil {
		settings, sErr := sessionMgr.GetSessionSettings(*session.SettingsID)
		if sErr == nil && settings != nil {
			cfg = ApplySettings(settings, cfg)
			cfgOk = true
		}
	}
	if !cfgOk {
		send(errorNotice(n.Sessions.CommandGone, map[string]string{"command": session.ChatCommand}))
		return
	}

	send(n.Compaction.Started)

	res, cErr := sessionMgr.CompactSession(ctx, CompactSessionInputs{
		SessionID: session.ID,
		Network:   network,
		Channel:   channel,
		UserNick:  e.Source.Name,
		Client:    c,
		Trigger:   "manual",
	}, cfg)
	if cErr != nil {
		switch cErr {
		case ErrCompactionTooShort:
			send(errorMsg(n.Compaction.TooShort))
		case ErrCompactionInProgress:
			send(errorMsg(n.Compaction.InProgress))
		case ErrCompactionEmptyResult:
			send(errorNotice(n.Compaction.Failed, map[string]string{"error": "summarizer returned empty content"}))
		default:
			send(errorNotice(n.Compaction.Failed, map[string]string{"error": cErr.Error()}))
		}
		return
	}

	send(expandNotice(n.Compaction.Completed, compactionNoticeVars(res, session.ID)))
}

func historyClone(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	n := getNotices()
	send := func(msg string) {
		select {
		case output <- msg:
		case <-ctx.Done():
		}
	}
	if theDB == nil {
		send(errorMsg(n.DB.NotAvailable))
		return
	}

	if len(args) == 0 || args[0] == "" {
		send(errorNotice(n.Clone.Usage, map[string]string{"trigger": network.Trigger}))
		return
	}

	casemapping := getCasemapping(network.Name)
	channel := normalizeIRC(e.Params[0], casemapping)

	account := ""
	if u := c.LookupUser(e.Source.Name); u != nil {
		account = u.Extras.Account
	}
	resolvedUser, err := resolveUser(network.Name, e.Source.Name, e.Source.Ident, e.Source.Host, account, casemapping)
	if err != nil || resolvedUser == nil {
		send(errorNotice(n.Clone.Usage, map[string]string{"trigger": network.Trigger}))
		return
	}
	callingUserID := resolvedUser.ID

	var sourceSession *Session
	var sourceNick string

	if isAllDigits(args[0]) {
		sessionID, parseErr := strconv.ParseInt(args[0], 10, 64)
		if parseErr != nil {
			send(errorMsg(n.Sessions.InvalidID))
			return
		}
		src, dbErr := getDBSessionByID(sessionID)
		if dbErr != nil {
			send(errorNotice(n.Clone.SessionNotFound, map[string]string{"id": args[0]}))
			return
		}
		normChannel := normalizeIRC(src.Channel, casemapping)
		if src.Network != network.Name || normChannel != channel {
			send(errorNotice(n.Clone.WrongChannel, map[string]string{"id": args[0]}))
			return
		}
		sourceSession = src
	} else {
		nick := args[0]
		targetUser, rErr := resolveUserByNick(network.Name, nick, casemapping)
		if rErr != nil || targetUser == nil {
			send(errorNotice(n.Clone.TargetNotFound, map[string]string{"nick": nick}))
			return
		}
		activeSession, aErr := sessionMgr.GetActiveSession(network.Name, channel, targetUser.ID)
		if aErr != nil || activeSession == nil {
			send(errorNotice(n.Clone.NoTargetSession, map[string]string{"nick": displayNick(targetUser)}))
			return
		}
		sourceSession = activeSession
		sourceNick = displayNick(targetUser)
	}

	incomplete, icErr := sessionHasIncompleteToolCalls(sourceSession.ID)
	if icErr != nil {
		send(errorNotice(n.DB.QuerySessions, map[string]string{"error": icErr.Error()}))
		return
	}
	if incomplete {
		send(errorNotice(n.Clone.IncompleteCalls, map[string]string{"id": fmt.Sprintf("%d", sourceSession.ID)}))
		return
	}

	var cfg AIConfig
	var cfgOk bool
	readConfig(func() {
		cfg, cfgOk = config.Commands.Chats[sourceSession.ChatCommand]
	})
	if sourceSession.SettingsID != nil {
		settings, sErr := sessionMgr.GetSessionSettings(*sourceSession.SettingsID)
		if sErr == nil && settings != nil {
			cfg = ApplySettings(settings, cfg)
			cfgOk = true
		}
	}
	if !cfgOk {
		send(errorNotice(n.Clone.CommandGone, map[string]string{"command": sourceSession.ChatCommand}))
		return
	}

	mu := getSessionCreationLock(network.Name, channel, callingUserID)
	mu.Lock()
	defer mu.Unlock()

	newSessionID, cloneErr := cloneDBSession(sourceSession.ID, network.Name, channel, callingUserID)
	if cloneErr != nil {
		send(errorNotice(n.DB.InternalError, map[string]string{"error": cloneErr.Error()}))
		return
	}

	var systemContent string
	if cfg.SystemTmpl != nil {
		templateVars := copyTemplateVars()
		data := SystemPromptData{
			Nick:    e.Source.Name,
			BotNick: c.GetNick(),
			Channel: channel,
			Network: network.Name,
			Date:    time.Now().Format("2006-01-02"),
			Vars:    templateVars,
		}
		ch := c.LookupChannel(channel)
		var nicks []string
		if ch != nil {
			for _, u := range ch.Users(c) {
				nicks = append(nicks, u.Nick)
			}
			sort.Strings(nicks)
		}
		data.ChanNicks = `["` + strings.Join(nicks, `","`) + `"]`

		var buf strings.Builder
		if execErr := cfg.SystemTmpl.Execute(&buf, data); execErr != nil {
			systemContent = cfg.System
		} else {
			systemContent = buf.String()
		}
	} else {
		systemContent = cfg.System
	}
	sessionMgr.AddMessage(newSessionID, ChatMessage{
		Role:    RoleSystem,
		Content: systemContent,
	})

	apiLogger.RestoreSession(newSessionID, network.Name, channel, callingUserID)

	vars := map[string]string{
		"id":        fmt.Sprintf("%d", newSessionID),
		"source_id": fmt.Sprintf("%d", sourceSession.ID),
		"count":     "0",
	}
	if sourceNick != "" {
		vars["source_nick"] = sourceNick
	} else {
		if sourceSession.UserID != nil {
			var srcUser User
			if err := theDB.Where("id = ?", *sourceSession.UserID).First(&srcUser).Error; err == nil && srcUser.ID != 0 {
				vars["source_nick"] = displayNick(&srcUser)
			}
		}
	}
	newMsgs, _ := loadDBSessionMessages(newSessionID)
	vars["count"] = fmt.Sprintf("%d", len(newMsgs))

	send(expandNotice(n.Clone.Cloned, vars))
}

func isAllDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
