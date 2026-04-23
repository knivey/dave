package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lrstanley/girc"
	gogpt "github.com/sashabaranov/go-openai"
)

func historySessions(network Network, c *girc.Client, e girc.Event, args ...string) {

	sessions, err := getUserDBSessions(network.Name, e.Params[0], e.Source.Name, config.MaxSessionHistory)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("failed to query sessions: "+err.Error()))
		return
	}

	if len(sessions) == 0 {
		c.Cmd.Reply(e, "No session history found.")
		return
	}

	c.Cmd.Reply(e, fmt.Sprintf("\x02Session History (%s on %s):\x02", e.Source.Name, network.Name))

	type sessionLine struct {
		icon    string
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

	for i, s := range sessions {
		icon := "\x0303●\x0F"
		if s.Status != "active" {
			icon = "\x0304○\x0F"
		}

		var msgCount int
		theDB.Get(&msgCount, "SELECT COUNT(*) FROM messages WHERE session_id = ?", s.ID)

		idStr := fmt.Sprintf("#%d", s.ID)
		msgStr := fmt.Sprintf("%d msgs", msgCount)
		timeStr := formatTimeAgo(s.LastActive)

		preview := strings.ReplaceAll(s.FirstMessage, "\n", " ")
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}

		lines[i] = sessionLine{icon, idStr, msgStr, timeStr, s.ChatCommand, preview}

		if l := utf8.RuneCountInString(idStr); l > maxID {
			maxID = l
		}
		if l := utf8.RuneCountInString(msgStr); l > maxMsg {
			maxMsg = l
		}
		if l := utf8.RuneCountInString(timeStr); l > maxTime {
			maxTime = l
		}
	}

	for _, l := range lines {
		line := fmt.Sprintf("  %s %s  %s  %s  %s%s",
			l.icon,
			l.idStr+strings.Repeat(" ", maxID-utf8.RuneCountInString(l.idStr)),
			l.msgStr+strings.Repeat(" ", maxMsg-utf8.RuneCountInString(l.msgStr)),
			l.timeStr+strings.Repeat(" ", maxTime-utf8.RuneCountInString(l.timeStr)),
			network.Trigger,
			l.cmd,
		)
		if l.preview != "" {
			line += " " + l.preview
		}

		for _, wrapped := range wrapLine(line) {
			c.Cmd.Reply(e, "\x02\x02"+wrapped)
			time.Sleep(time.Millisecond * network.Throttle)
		}
	}
}

func historyShow(network Network, c *girc.Client, e girc.Event, args ...string) {
	if theDB == nil {
		c.Cmd.Reply(e, errorMsg("database not available"))
		return
	}

	if len(args) == 0 || args[0] == "" {
		c.Cmd.Reply(e, errorMsg("usage: "+network.Trigger+"history <session-id>"))
		return
	}

	sessionID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("invalid session id"))
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("session not found"))
		return
	}

	if session.Network != network.Name || session.Channel != e.Params[0] || session.Nick != e.Source.Name {
		c.Cmd.Reply(e, errorMsg("that session doesn't belong to you"))
		return
	}

	messages, err := loadDBSessionMessages(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("failed to load messages: "+err.Error()))
		return
	}

	var visible []dbMessage
	for _, m := range messages {
		if m.Role == "system" || m.Role == "tool" {
			continue
		}
		if m.Role == "assistant" && strings.TrimSpace(m.Content) == "" && m.ToolCalls != nil {
			continue
		}
		visible = append(visible, m)
	}

	c.Cmd.Reply(e, fmt.Sprintf("\x02Session #%d (%s) — %d messages:\x02", sessionID, session.ChatCommand, len(visible)))

	var shown []dbMessage
	if len(visible) <= 4 {
		shown = visible
	} else {
		shown = append(visible[:2], visible[len(visible)-2:]...)
	}

	sendHistoryMsg := func(m dbMessage) {
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

		line := fmt.Sprintf("  %s %s", roleIcon, content)
		for _, wrapped := range wrapLine(line) {
			c.Cmd.Reply(e, "\x02\x02"+wrapped)
			time.Sleep(time.Millisecond * network.Throttle)
		}
	}

	if len(visible) > 4 {
		sendHistoryMsg(shown[0])
		sendHistoryMsg(shown[1])
		c.Cmd.Reply(e, fmt.Sprintf("  \x0314... (%d more) ...\x0F", len(visible)-4))
		sendHistoryMsg(shown[2])
		sendHistoryMsg(shown[3])
	} else {
		for _, m := range shown {
			sendHistoryMsg(m)
		}
	}
}

func historyStats(network Network, c *girc.Client, e girc.Event, args ...string) {
	if theDB == nil {
		c.Cmd.Reply(e, errorMsg("database not available"))
		return
	}

	sessionCount, messageCount, err := getUserDBStats(network.Name, e.Params[0], e.Source.Name)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("failed to query stats: "+err.Error()))
		return
	}

	c.Cmd.Reply(e, fmt.Sprintf("\x02Your stats on %s/%s:\x02 %d sessions, %d total messages",
		network.Name, e.Params[0], sessionCount, messageCount))
}

func historyDelete(network Network, c *girc.Client, e girc.Event, args ...string) {
	if theDB == nil {
		c.Cmd.Reply(e, errorMsg("database not available"))
		return
	}

	if len(args) == 0 || args[0] == "" {
		c.Cmd.Reply(e, errorMsg("usage: "+network.Trigger+"delete <session-id>"))
		return
	}

	sessionID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("invalid session id"))
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("session not found"))
		return
	}

	if session.Network != network.Name || session.Channel != e.Params[0] || session.Nick != e.Source.Name {
		c.Cmd.Reply(e, errorMsg("that session doesn't belong to you"))
		return
	}

	if err := deleteDBSession(sessionID); err != nil {
		c.Cmd.Reply(e, errorMsg("failed to delete session: "+err.Error()))
		return
	}

	chatContextsMutex.Lock()
	if ctx, ok := chatContextsMap[session.ContextKey]; ok && ctx.SessionID == sessionID {
		chatContextsMap[session.ContextKey] = ChatContext{}
	}
	chatContextsMutex.Unlock()

	c.Cmd.Reply(e, fmt.Sprintf("Deleted session #%d.", sessionID))
}

func historyResume(network Network, c *girc.Client, e girc.Event, args ...string) {
	if theDB == nil {
		c.Cmd.Reply(e, errorMsg("database not available"))
		return
	}

	if len(args) == 0 || args[0] == "" {
		c.Cmd.Reply(e, errorMsg("usage: "+network.Trigger+"resume <session-id>"))
		return
	}

	sessionID, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("invalid session id"))
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("session not found"))
		return
	}

	if session.Network != network.Name || session.Channel != e.Params[0] || session.Nick != e.Source.Name {
		c.Cmd.Reply(e, errorMsg("that session doesn't belong to you"))
		return
	}

	currentCfg, ok := config.Commands.Chats[session.ChatCommand]
	if !ok {
		c.Cmd.Reply(e, errorMsg(fmt.Sprintf("chat command %q no longer exists, cannot resume", session.ChatCommand)))
		return
	}

	dbMsgs, err := loadDBSessionMessages(sessionID)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("failed to load messages: "+err.Error()))
		return
	}

	var messages []gogpt.ChatCompletionMessage
	for _, dm := range dbMsgs {
		msg := gogpt.ChatCompletionMessage{
			Role:    dm.Role,
			Content: dm.Content,
		}
		if dm.ToolCallID != nil {
			msg.ToolCallID = *dm.ToolCallID
		}
		if dm.ReasoningContent != nil {
			msg.ReasoningContent = *dm.ReasoningContent
		}
		if dm.ToolCalls != nil {
			var toolCalls []gogpt.ToolCall
			if err := json.Unmarshal([]byte(*dm.ToolCalls), &toolCalls); err == nil {
				msg.ToolCalls = toolCalls
			}
		}
		messages = append(messages, msg)
	}

	if len(messages) == 0 {
		c.Cmd.Reply(e, errorMsg("session has no messages"))
		return
	}

	ctxKey := network.Name + e.Params[0] + e.Source.Name

	chatContextsMutex.Lock()
	oldCtx := chatContextsMap[ctxKey]
	if oldCtx.SessionID != 0 && oldCtx.SessionID != sessionID {
		if theDB != nil {
			completeDBSession(oldCtx.SessionID)
		}
		c.Cmd.Reply(e, fmt.Sprintf("Paused your previous session #%d.", oldCtx.SessionID))
	}
	messages = TruncateHistory(messages, currentCfg.MaxHistory)
	chatContextsMap[ctxKey] = ChatContext{
		Messages:  messages,
		Config:    currentCfg,
		SessionID: sessionID,
	}
	chatContextsMutex.Unlock()
	SetContextLastActive(ctxKey)

	if theDB != nil {
		theDB.Exec("UPDATE sessions SET status = 'active' WHERE id = ?", sessionID)
	}

	c.Cmd.Reply(e, fmt.Sprintf("Resumed session #%d (%s) with %d messages.", sessionID, session.ChatCommand, len(messages)))
}

func formatTimeAgo(dbTime string) string {
	for _, layout := range []string{
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
		time.RFC3339,
		time.RFC3339Nano,
	} {
		if t, err := time.Parse(layout, dbTime); err == nil {
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
	}
	return dbTime
}

func historyJobs(network Network, c *girc.Client, e girc.Event, args ...string) {
	nick := e.Source.Name
	channel := e.Params[0]
	hasOutput := false

	if queueMgr != nil {
		current, pending := queueMgr.QueueStatus(network.Name, channel, nick)
		if current != nil || len(pending) > 0 {
			hasOutput = true
			c.Cmd.Reply(e, "\x02Queue:\x02")
			if current != nil {
				elapsed := time.Since(current.Enqueued).Truncate(time.Second)
				desc := current.Description
				if desc == "" {
					desc = "processing"
				}
				line := fmt.Sprintf("  \x0303▶\x0F %s (%s elapsed)", desc, elapsed)
				for _, wrapped := range wrapLine(line) {
					c.Cmd.Reply(e, "\x02\x02"+wrapped)
					time.Sleep(time.Millisecond * network.Throttle)
				}
			}
			if len(pending) > 0 {
				for i, item := range pending {
					wait := time.Since(item.Enqueued).Truncate(time.Second)
					desc := item.Description
					if desc == "" {
						desc = "queued"
					}
					line := fmt.Sprintf("  \x0308%d.\x0F %s (waiting %s)", i+1, desc, wait)
					for _, wrapped := range wrapLine(line) {
						c.Cmd.Reply(e, "\x02\x02"+wrapped)
						time.Sleep(time.Millisecond * network.Throttle)
					}
				}
			}
		}
	}

	if theDB == nil {
		if !hasOutput {
			c.Cmd.Reply(e, "No active jobs or queue items.")
		}
		return
	}

	jobs, err := getPendingJobsForUser(network.Name, channel, nick)
	if err != nil {
		c.Cmd.Reply(e, errorMsg("failed to query jobs: "+err.Error()))
		return
	}

	if len(jobs) == 0 {
		if !hasOutput {
			c.Cmd.Reply(e, "No active jobs or queue items.")
		}
		return
	}

	c.Cmd.Reply(e, "\x02Background jobs:\x02")
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
		line := fmt.Sprintf("  %s %s [%s/%s] %s, %s ago", statusIcon, j.JobID, j.ToolName, j.MCPServer, j.Status, elapsed)
		for _, wrapped := range wrapLine(line) {
			c.Cmd.Reply(e, "\x02\x02"+wrapped)
			time.Sleep(time.Millisecond * network.Throttle)
		}
	}
}
