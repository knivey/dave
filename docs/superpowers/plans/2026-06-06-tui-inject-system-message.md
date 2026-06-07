# TUI Inject System Message Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `/reinject` and `/systemmsg` TUI commands that inject RoleSystem messages into sessions without triggering API calls, guaranteed to be delivered on the next turn.

**Architecture:** Two TUI command handlers in `tui_commands.go` following the `/compact` pattern. Both insert a `RoleSystem` message via `sessionMgr.AddMessage` and clear `session.ResponseID` via `sessionMgr.UpdateResponseID` to ensure delivery via the Responses API path. A package-level `botIsInChannel` var enables testing the channel-joined validation. No schema changes, no changes to existing code paths.

**Tech Stack:** Go, existing session/message infrastructure, `text/template` for `/systemmsg`

**Spec:** `docs/superpowers/specs/2026-06-06-tui-inject-system-message-design.md`

---

### Task 1: Implement `/reinject` TUI command

**Files:**
- Modify: `tui_commands.go` (add `botIsInChannel` var, add handler, register in map, update help)

- [ ] **Step 1: Add `"text/template"` to imports**

Add `"text/template"` to the import block in `tui_commands.go`. (Needed for Task 2, but adding now to avoid a build break if committing between tasks.)

- [ ] **Step 2: Add `botIsInChannel` package var**

Add near the top of `tui_commands.go`, after the `tuiCommands` map. This follows the existing codebase pattern of package-level var overrides for testing:

```go
var botIsInChannel = func(bot *Bot, channel string) bool {
	return bot.Client != nil && bot.Client.LookupChannel(channel) != nil
}
```

- [ ] **Step 3: Add `/reinject` to the `tuiCommands` map and help text**

Add `"/reinject": tuiCmdReinject` to the `tuiCommands` map after the `"/compact"` entry.

Add help line in `tuiCmdHelp` after the `/compact` line:

```go
fmt.Fprintf(logView, "  /reinject <session-id>       - Re-render and inject system prompt into session\n")
```

- [ ] **Step 4: Implement `tuiCmdReinject`**

Add the handler function after `tuiCmdCompact`:

```go
func tuiCmdReinject(parts []string, _ string) {
	if len(parts) < 2 {
		fmt.Fprintf(logView, "[yellow]Usage: /reinject <session-id>[white]\n")
		return
	}
	sessionID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		fmt.Fprintf(logView, "[red]Invalid session id: %s[white]\n", parts[1])
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil || session == nil {
		fmt.Fprintf(logView, "[red]Session %d not found[white]\n", sessionID)
		return
	}

	bot, ok := getBot(session.Network)
	if !ok {
		fmt.Fprintf(logView, "[red]No bot connected for network %s[white]\n", session.Network)
		return
	}

	if !botIsInChannel(bot, session.Channel) {
		fmt.Fprintf(logView, "[red]Bot is not joined to %s on %s[white]\n", session.Channel, session.Network)
		return
	}

	if session.UserID == nil {
		fmt.Fprintf(logView, "[red]Session %d has no associated user[white]\n", sessionID)
		return
	}
	u, err := getUserByID(*session.UserID)
	if err != nil || u == nil {
		fmt.Fprintf(logView, "[red]User for session %d not found[white]\n", sessionID)
		return
	}
	userNick := displayNick(u)

	cfg, cfgOk := getSessionConfig(session)
	if !cfgOk {
		fmt.Fprintf(logView, "[red]Chat command %q for session %d no longer exists[white]\n", session.ChatCommand, sessionID)
		return
	}

	rendered := renderFreshSystemPrompt(cfg, bot.Network, bot.Client, session.Channel, userNick, "")
	if rendered == "" {
		fmt.Fprintf(logView, "[red]No system prompt configured for session %d[white]\n", sessionID)
		return
	}

	if session.Status == "completed" {
		fmt.Fprintf(logView, "[yellow]Warning: session %d is completed[white]\n", sessionID)
	}

	if err := sessionMgr.AddMessage(sessionID, ChatMessage{Role: RoleSystem, Content: rendered}); err != nil {
		fmt.Fprintf(logView, "[red]Failed to inject message: %s[white]\n", err)
		return
	}

	if err := sessionMgr.UpdateResponseID(sessionID, nil); err != nil {
		fmt.Fprintf(logView, "[red]Failed to clear response_id: %s[white]\n", err)
		return
	}

	fmt.Fprintf(logView, "[green]Injected system prompt into session %d (%d chars)[white]\n", sessionID, len(rendered))
}
```

- [ ] **Step 5: Build and verify compilation**

Run: `go build -o /dev/null .`
Expected: clean build, no errors

---

### Task 2: Implement `/systemmsg` TUI command

**Files:**
- Modify: `tui_commands.go` (add handler, register in map, update help)

- [ ] **Step 1: Add `/systemmsg` to the `tuiCommands` map and help text**

Add `"/systemmsg": tuiCmdSystemMsg` to the `tuiCommands` map after the `"/reinject"` entry.

Add help line in `tuiCmdHelp`:

```go
fmt.Fprintf(logView, "  /systemmsg <session-id> <text> - Inject custom system message (Go template) into session\n")
```

- [ ] **Step 2: Implement `tuiCmdSystemMsg`**

Add the handler function after `tuiCmdReinject`:

```go
func tuiCmdSystemMsg(parts []string, text string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /systemmsg <session-id> <template-text>[white]\n")
		return
	}
	sessionID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		fmt.Fprintf(logView, "[red]Invalid session id: %s[white]\n", parts[1])
		return
	}

	tmplText := strings.TrimSpace(strings.TrimPrefix(text, parts[0]+" "+parts[1]+" "))
	if tmplText == "" {
		fmt.Fprintf(logView, "[red]Empty template text[white]\n")
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil || session == nil {
		fmt.Fprintf(logView, "[red]Session %d not found[white]\n", sessionID)
		return
	}

	bot, ok := getBot(session.Network)
	if !ok {
		fmt.Fprintf(logView, "[red]No bot connected for network %s[white]\n", session.Network)
		return
	}

	if !botIsInChannel(bot, session.Channel) {
		fmt.Fprintf(logView, "[red]Bot is not joined to %s on %s[white]\n", session.Channel, session.Network)
		return
	}

	if session.UserID == nil {
		fmt.Fprintf(logView, "[red]Session %d has no associated user[white]\n", sessionID)
		return
	}
	u, err := getUserByID(*session.UserID)
	if err != nil || u == nil {
		fmt.Fprintf(logView, "[red]User for session %d not found[white]\n", sessionID)
		return
	}
	userNick := displayNick(u)

	tmpl, err := template.New("systemmsg").Parse(tmplText)
	if err != nil {
		fmt.Fprintf(logView, "[red]Template parse error: %s[white]\n", err)
		return
	}

	data := buildSystemPromptData(bot.Network, bot.Client, session.Channel, userNick)
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		fmt.Fprintf(logView, "[red]Template execute error: %s[white]\n", err)
		return
	}
	rendered := buf.String()

	if session.Status == "completed" {
		fmt.Fprintf(logView, "[yellow]Warning: session %d is completed[white]\n", sessionID)
	}

	if err := sessionMgr.AddMessage(sessionID, ChatMessage{Role: RoleSystem, Content: rendered}); err != nil {
		fmt.Fprintf(logView, "[red]Failed to inject message: %s[white]\n", err)
		return
	}

	if err := sessionMgr.UpdateResponseID(sessionID, nil); err != nil {
		fmt.Fprintf(logView, "[red]Failed to clear response_id: %s[white]\n", err)
		return
	}

	fmt.Fprintf(logView, "[green]Injected system message into session %d (%d chars)[white]\n", sessionID, len(rendered))
}
```

- [ ] **Step 3: Build and verify compilation**

Run: `go build -o /dev/null .`
Expected: clean build, no errors

- [ ] **Step 4: Run `go fmt` and `go vet`**

Run: `go fmt ./... && go vet ./...`
Expected: no output (clean)

---

### Task 3: Write tests

**Files:**
- Create: `tui_commands_test.go`

Tests verify message insertion, ResponseID clearing, validation failures, and template rendering. The `botIsInChannel` var is overridden in success-path tests via `setupTUIWithChannel`.

- [ ] **Step 1: Write `tui_commands_test.go`**

```go
package main

import (
	"fmt"
	"testing"
	"text/template"

	"github.com/lrstanley/girc"
	"github.com/rivo/tview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTUITest(t *testing.T) {
	t.Helper()
	setupTestDB(t)

	client := girc.New(girc.Config{
		Server: "localhost",
		Port:   6667,
		Nick:   "testbot",
	})
	origBots := bots
	bots = map[string]*Bot{
		"testnet": {Client: client, Network: Network{Name: "testnet", Nick: "testbot"}},
	}
	t.Cleanup(func() { bots = origBots })

	origLogView := logView
	logView = tview.NewTextView()
	t.Cleanup(func() { logView = origLogView })
}

func setupTUIWithChannel(t *testing.T) {
	setupTUITest(t)
	origBotIsInChannel := botIsInChannel
	botIsInChannel = func(_ *Bot, _ string) bool { return true }
	t.Cleanup(func() { botIsInChannel = origBotIsInChannel })
}

func getLogViewText() string {
	if logView == nil {
		return ""
	}
	return logView.GetText(true)
}

func TestTuiCmdReinject_SessionNotFound(t *testing.T) {
	setupTUITest(t)
	tuiCmdReinject([]string{"/reinject", "99999"}, "/reinject 99999")
	assert.Contains(t, getLogViewText(), "not found")
	msgs, _ := loadDBSessionMessages(99999)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_NoBotForNetwork(t *testing.T) {
	setupTUITest(t)
	userID := ensureTestUser(t, "othernet", "testnick")
	sid, err := sessionMgr.CreateSession("othernet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "No bot connected")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_NoSystemPrompt(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "No system prompt configured")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_CompletedSession(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.CompleteSession(sid))

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", System: "static prompt", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "completed")
	assert.Contains(t, getLogViewText(), "Injected system prompt")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Len(t, msgs, 1)
}

func TestTuiCmdReinject_BotNotInChannel(t *testing.T) {
	setupTUITest(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", System: "prompt", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "not joined")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_NilUserID(t *testing.T) {
	setupTUIWithChannel(t)
	sid, err := sessionMgr.CreateSession("testnet", "#test", 0, "chat", "svc", "model")
	require.NoError(t, err)

	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sid).Update("user_id", nil).Error)

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", System: "prompt", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "no associated user")
}

func TestTuiCmdReinject_Success(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	respID := "resp_abc123"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, &respID))

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {
			Name:        "chat",
			System:      "You are a test bot in {{.Channel}}",
			SystemTmpl:  template.Must(template.New("system").Parse("You are a test bot in {{.Channel}}")),
			Service:     "svc",
			Model:       "model",
		},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "Injected system prompt")

	msgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleSystem, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "#test")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err)
	assert.Nil(t, session.ResponseID)
}

func TestTuiCmdSystemMsg_SessionNotFound(t *testing.T) {
	setupTUITest(t)
	tuiCmdSystemMsg([]string{"/systemmsg", "99999", "hello"}, "/systemmsg 99999 hello")
	assert.Contains(t, getLogViewText(), "not found")
}

func TestTuiCmdSystemMsg_TemplateRendering(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	respID := "resp_xyz"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, &respID))

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "Channel: {{.Channel}}, Nick: {{.Nick}}"},
		fmt.Sprintf("/systemmsg %d Channel: {{.Channel}}, Nick: {{.Nick}}", sid),
	)
	assert.Contains(t, getLogViewText(), "Injected system message")

	msgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleSystem, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "#test")
	assert.Contains(t, msgs[0].Content, "testnick")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err)
	assert.Nil(t, session.ResponseID)
}

func TestTuiCmdSystemMsg_TemplateParseError(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "{{.BadField"},
		fmt.Sprintf("/systemmsg %d {{.BadField", sid),
	)
	assert.Contains(t, getLogViewText(), "Template parse error")

	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdSystemMsg_NoBotForNetwork(t *testing.T) {
	setupTUITest(t)
	userID := ensureTestUser(t, "othernet", "testnick")
	sid, err := sessionMgr.CreateSession("othernet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "hello"},
		fmt.Sprintf("/systemmsg %d hello", sid),
	)
	assert.Contains(t, getLogViewText(), "No bot connected")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdSystemMsg_CompletedSession(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.CompleteSession(sid))

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "post-completion instruction"},
		fmt.Sprintf("/systemmsg %d post-completion instruction", sid),
	)
	assert.Contains(t, getLogViewText(), "completed")
	assert.Contains(t, getLogViewText(), "Injected system message")

	msgs, _ := loadDBSessionMessages(sid)
	assert.Len(t, msgs, 1)
}

func TestTuiCmdSystemMsg_PlainTextNoTemplate(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "Be more helpful"},
		fmt.Sprintf("/systemmsg %d Be more helpful", sid),
	)
	assert.Contains(t, getLogViewText(), "Injected system message")

	msgs, _ := loadDBSessionMessages(sid)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Be more helpful", msgs[0].Content)
}
```

- [ ] **Step 2: Run all new tests**

Run: `go test -v -run "TestTuiCmd" .`
Expected: all PASS

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`
Expected: all PASS (no regressions)

- [ ] **Step 4: Run `go fmt` and `go vet`**

Run: `go fmt ./... && go vet ./...`
Expected: no output (clean)

---

### Task 4: Commit

- [ ] **Step 1: Stage and commit**

```bash
git add tui_commands.go tui_commands_test.go
git commit -m "feat: add /reinject and /systemmsg TUI commands for injecting system messages into sessions"
```
