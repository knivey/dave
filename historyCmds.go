package main

import (
	"context"
	"fmt"
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

func verifySessionOwnership(session *Session, network Network, c *girc.Client, e girc.Event) (bool, string) {
	n := getNotices()
	if session.Network != network.Name || session.Channel != normalizeIRC(e.Params[0], getCasemapping(network.Name)) {
		return false, errorMsg(n.Sessions.NotOwned)
	}
	if session.UserID == nil {
		return false, errorMsg(n.Sessions.NotOwned)
	}
	resolvedUser, _ := resolveIRCUser(network, c, e.Source.Name, e.Source)
	if resolvedUser == nil || resolvedUser.ID != *session.UserID {
		return false, errorMsg(n.Sessions.NotOwned)
	}
	return true, ""
}

func parseSessionIDAndVerify(rawID string, network Network, c *girc.Client, e girc.Event) (*Session, string) {
	n := getNotices()
	sessionID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return nil, errorMsg(n.Sessions.InvalidID)
	}
	session, err := getDBSessionByID(sessionID)
	if err != nil {
		return nil, errorMsg(n.Sessions.NotFound)
	}
	if ok, msg := verifySessionOwnership(session, network, c, e); !ok {
		return nil, msg
	}
	return session, ""
}

func resolveIRCUserWithID(network Network, c *girc.Client, e girc.Event) int64 {
	resolvedUser, _ := resolveIRCUser(network, c, e.Source.Name, e.Source)
	if resolvedUser != nil {
		return resolvedUser.ID
	}
	return 0
}

func dbGuard(output chan<- string, ctx context.Context) bool {
	if theDB == nil {
		n := getNotices()
		sendOrDone(ctx, output, errorMsg(n.DB.NotAvailable))
		return false
	}
	return true
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
		if !dbGuard(output, ctx) {
			return
		}
		results, err := getChannelDBSessions(network.Name, channel, maxHistory)
		if err != nil {
			sendOrDone(ctx, output, errorNotice(n.DB.QuerySessions, map[string]string{"error": err.Error()}))
			return
		}
		if len(results) == 0 {
			sendOrDone(ctx, output, n.Sessions.None)
			return
		}
		if !sendOrDone(ctx, output, expandNotice(n.Sessions.AllHeader, map[string]string{"network": network.Name, "channel": channel})) {
			return
		}
		sendSessionsLinesWithNick(output, ctx, results, network.Trigger)
		return
	}

	userID := resolveIRCUserWithID(network, c, e)

	if arg != "" {
		if !dbGuard(output, ctx) {
			return
		}
		targetUser, err := resolveUserByNick(network.Name, arg, casemapping)
		if err != nil || targetUser == nil {
			sendOrDone(ctx, output, expandNotice(n.Sessions.OtherNone, map[string]string{"nick": arg}))
			return
		}
		sessions, err := getUserDBSessions(network.Name, channel, targetUser.ID, maxHistory)
		if err != nil {
			sendOrDone(ctx, output, errorNotice(n.DB.QuerySessions, map[string]string{"error": err.Error()}))
			return
		}
		if len(sessions) == 0 {
			sendOrDone(ctx, output, expandNotice(n.Sessions.OtherNone, map[string]string{"nick": arg}))
			return
		}
		if !sendOrDone(ctx, output, expandNotice(n.Sessions.OtherHeader, map[string]string{"nick": displayNick(targetUser), "network": network.Name})) {
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
		sendOrDone(ctx, output, errorNotice(n.DB.QuerySessions, map[string]string{"error": err.Error()}))
		return
	}

	if len(sessions) == 0 {
		sendOrDone(ctx, output, n.Sessions.None)
		return
	}

	if !sendOrDone(ctx, output, expandNotice(n.Sessions.Header, map[string]string{"nick": e.Source.Name, "network": network.Name})) {
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
			if !sendOrDone(ctx, output, wrapped) {
				return
			}
		}
	}
}

func historyShow(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	n := getNotices()
	if !dbGuard(output, ctx) {
		return
	}

	if len(args) == 0 || args[0] == "" {
		sendOrDone(ctx, output, errorNotice(n.Sessions.HistoryUsage, map[string]string{"trigger": network.Trigger}))
		return
	}

	session, errMsg := parseSessionIDAndVerify(args[0], network, c, e)
	if errMsg != "" {
		sendOrDone(ctx, output, errMsg)
		return
	}

	messages, err := loadDBSessionMessagesAll(session.ID)
	if err != nil {
		sendOrDone(ctx, output, errorNotice(n.DB.LoadMessages, map[string]string{"error": err.Error()}))
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

	if !sendOrDone(ctx, output, expandNotice(n.Sessions.DetailHeader, map[string]string{
		"id":              fmt.Sprintf("%d", session.ID),
		"command":         session.ChatCommand,
		"count":           fmt.Sprintf("%d", activeCount),
		"active":          fmt.Sprintf("%d", activeCount),
		"archived":        fmt.Sprintf("%d", archivedCount),
		"archived_suffix": archivedSuffix,
		"total":           fmt.Sprintf("%d", len(visible)),
	})) {
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
			if !sendOrDone(ctx, output, wrapped) {
				return
			}
		}
	}

	if len(visible) > 4 {
		sendHistoryMsg(shown[0])
		sendHistoryMsg(shown[1])
		if !sendOrDone(ctx, output, expandNotice(n.Sessions.Truncated, map[string]string{"count": fmt.Sprintf("%d", len(visible)-4)})) {
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

	userID := resolveIRCUserWithID(network, c, e)
	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))

	sessionCount, messageCount, err := getUserDBStats(userID, network.Name, channel)
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

	session, errMsg := parseSessionIDAndVerify(args[0], network, c, e)
	if errMsg != "" {
		c.Cmd.Reply(e, errMsg)
		return
	}

	cancelAsyncJobsForSession(session.ID)

	if err := deleteDBSession(session.ID); err != nil {
		c.Cmd.Reply(e, errorNotice(n.DB.DeleteFailed, map[string]string{"error": err.Error()}))
		return
	}

	c.Cmd.Reply(e, expandNotice(n.Sessions.Deleted, map[string]string{"id": fmt.Sprintf("%d", session.ID)}))
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

	session, errMsg := parseSessionIDAndVerify(args[0], network, c, e)
	if errMsg != "" {
		c.Cmd.Reply(e, errMsg)
		return
	}

	var currentCfg AIConfig
	var cfgOk bool
	currentCfg, cfgOk = getSessionConfig(session)
	if !cfgOk {
		c.Cmd.Reply(e, errorNotice(n.Sessions.CommandGone, map[string]string{"command": session.ChatCommand}))
		return
	}

	dbMsgs, err := loadDBSessionMessages(session.ID)
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
	oldID, _ := sessionMgr.SwitchActive(network.Name, channel, resumeUserID, session.ID)
	if oldID != 0 {
		c.Cmd.Reply(e, expandNotice(n.Sessions.Paused, map[string]string{"id": fmt.Sprintf("%d", oldID)}))
	}

	apiLogger.RestoreSession(session.ID, network.Name, channel, resumeUserID)

	c.Cmd.Reply(e, expandNotice(n.Sessions.Resumed, map[string]string{"id": fmt.Sprintf("%d", session.ID), "command": session.ChatCommand, "count": fmt.Sprintf("%d", len(messages))}))
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
	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	hasOutput := false

	userID := resolveIRCUserWithID(network, c, e)

	sendLine := func(line string) bool {
		for _, wrapped := range wrapLine(line) {
			if !sendOrDone(ctx, output, wrapped) {
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
			sendOrDone(ctx, output, n.Jobs.NoJobs)
		}
		return
	}

	jobs, err := getPendingJobsForUser(network.Name, channel, userID)
	if err != nil {
		sendOrDone(ctx, output, errorNotice(n.DB.QueryJobs, map[string]string{"error": err.Error()}))
		return
	}

	if len(jobs) == 0 {
		if !hasOutput {
			sendOrDone(ctx, output, n.Jobs.NoJobs)
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
	if !dbGuard(output, ctx) {
		return
	}

	var ccfg CompactionConfig
	readConfig(func() { ccfg = config.Compaction })
	if !ccfg.Enabled {
		sendOrDone(ctx, output, errorMsg(n.Compaction.Disabled))
		return
	}

	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	resolvedUser, err := resolveIRCUser(network, c, e.Source.Name, e.Source)
	if err != nil || resolvedUser == nil {
		sendOrDone(ctx, output, errorMsg(n.Compaction.NoActive))
		return
	}

	session, err := sessionMgr.GetActiveSession(network.Name, channel, resolvedUser.ID)
	if err != nil || session == nil {
		sendOrDone(ctx, output, errorMsg(n.Compaction.NoActive))
		return
	}

	var cfg AIConfig
	var cfgOk bool
	cfg, cfgOk = getSessionConfig(session)
	if !cfgOk {
		sendOrDone(ctx, output, errorNotice(n.Sessions.CommandGone, map[string]string{"command": session.ChatCommand}))
		return
	}

	sendOrDone(ctx, output, n.Compaction.Started)

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
			sendOrDone(ctx, output, errorMsg(n.Compaction.TooShort))
		case ErrCompactionInProgress:
			sendOrDone(ctx, output, errorMsg(n.Compaction.InProgress))
		case ErrCompactionEmptyResult:
			sendOrDone(ctx, output, errorNotice(n.Compaction.Failed, map[string]string{"error": "summarizer returned empty content"}))
		default:
			sendOrDone(ctx, output, errorNotice(n.Compaction.Failed, map[string]string{"error": cErr.Error()}))
		}
		return
	}

	sendOrDone(ctx, output, expandNotice(n.Compaction.Completed, compactionNoticeVars(res, session.ID)))
}

func historyClone(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	n := getNotices()
	if !dbGuard(output, ctx) {
		return
	}

	if len(args) == 0 || args[0] == "" {
		sendOrDone(ctx, output, errorNotice(n.Clone.Usage, map[string]string{"trigger": network.Trigger}))
		return
	}

	casemapping := getCasemapping(network.Name)
	channel := normalizeIRC(e.Params[0], casemapping)

	resolvedUser, err := resolveIRCUser(network, c, e.Source.Name, e.Source)
	if err != nil || resolvedUser == nil {
		sendOrDone(ctx, output, errorNotice(n.Clone.Usage, map[string]string{"trigger": network.Trigger}))
		return
	}
	callingUserID := resolvedUser.ID

	var sourceSession *Session
	var sourceNick string

	if isAllDigits(args[0]) {
		sessionID, parseErr := strconv.ParseInt(args[0], 10, 64)
		if parseErr != nil {
			sendOrDone(ctx, output, errorMsg(n.Sessions.InvalidID))
			return
		}
		src, dbErr := getDBSessionByID(sessionID)
		if dbErr != nil {
			sendOrDone(ctx, output, errorNotice(n.Clone.SessionNotFound, map[string]string{"id": args[0]}))
			return
		}
		normChannel := normalizeIRC(src.Channel, casemapping)
		if src.Network != network.Name || normChannel != channel {
			sendOrDone(ctx, output, errorNotice(n.Clone.WrongChannel, map[string]string{"id": args[0]}))
			return
		}
		sourceSession = src
	} else {
		nick := args[0]
		targetUser, rErr := resolveUserByNick(network.Name, nick, casemapping)
		if rErr != nil || targetUser == nil {
			sendOrDone(ctx, output, errorNotice(n.Clone.TargetNotFound, map[string]string{"nick": nick}))
			return
		}
		activeSession, aErr := sessionMgr.GetActiveSession(network.Name, channel, targetUser.ID)
		if aErr != nil || activeSession == nil {
			sendOrDone(ctx, output, errorNotice(n.Clone.NoTargetSession, map[string]string{"nick": displayNick(targetUser)}))
			return
		}
		sourceSession = activeSession
		sourceNick = displayNick(targetUser)
	}

	incomplete, icErr := sessionHasIncompleteToolCalls(sourceSession.ID)
	if icErr != nil {
		sendOrDone(ctx, output, errorNotice(n.DB.QuerySessions, map[string]string{"error": icErr.Error()}))
		return
	}
	if incomplete {
		sendOrDone(ctx, output, errorNotice(n.Clone.IncompleteCalls, map[string]string{"id": fmt.Sprintf("%d", sourceSession.ID)}))
		return
	}

	var cfg AIConfig
	var cfgOk bool
	cfg, cfgOk = getSessionConfig(sourceSession)
	if !cfgOk {
		sendOrDone(ctx, output, errorNotice(n.Clone.CommandGone, map[string]string{"command": sourceSession.ChatCommand}))
		return
	}

	mu := getSessionCreationLock(network.Name, channel, callingUserID)
	mu.Lock()
	defer mu.Unlock()

	systemContent := renderFreshSystemPrompt(cfg, network, c, channel, e.Source.Name, cfg.System)

	var resolvedSourceNick string
	if sourceNick != "" {
		resolvedSourceNick = sourceNick
	} else if sourceSession.UserID != nil {
		var srcUser User
		if err := theDB.Where("id = ?", *sourceSession.UserID).First(&srcUser).Error; err == nil && srcUser.ID != 0 {
			resolvedSourceNick = displayNick(&srcUser)
		}
	}

	newSessionID, cloneErr := cloneDBSession(sourceSession.ID, network.Name, channel, callingUserID, systemContent, resolvedSourceNick)
	if cloneErr != nil {
		sendOrDone(ctx, output, errorNotice(n.DB.InternalError, map[string]string{"error": cloneErr.Error()}))
		return
	}

	apiLogger.RestoreSession(newSessionID, network.Name, channel, callingUserID)

	vars := map[string]string{
		"id":          fmt.Sprintf("%d", newSessionID),
		"source_id":   fmt.Sprintf("%d", sourceSession.ID),
		"count":       "0",
		"source_nick": resolvedSourceNick,
	}
	newMsgs, _ := loadDBSessionMessages(newSessionID)
	vars["count"] = fmt.Sprintf("%d", len(newMsgs))

	sendOrDone(ctx, output, expandNotice(n.Clone.Cloned, vars))
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
