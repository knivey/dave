# Mention Spam Protection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Track per-user nick mentions that hit no_context, mute mentions after threshold, update no_context notice with pastebin help link.

**Architecture:** In-memory `mentionTracker` (patterned after `checkRate.go`) keyed by `network:userID`. Config via `[mention_spam]` in `config.toml`, notices via `[mentions]` in `notices.toml`. Help text extracted from `help.go` for pastebin upload in no_context.

**Tech Stack:** Go 1.25, existing pastebin/upload infrastructure, existing notices system.

---

### Task 1: MentionSpamConfig struct and defaults

**Files:**
- Modify: `config.go:44` (Config struct)
- Modify: `config.go:513-597` (loadConfigDir defaults)
- Modify: `config_test.go` (add defaults test)

- [ ] **Step 1: Write the failing test**

Add to `config_test.go`:

```go
func TestMentionSpamConfigDefaults(t *testing.T) {
	dir := createTestConfigDir(t, "")
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.Equal(t, 2, cfg.MentionSpam.Threshold)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestMentionSpamConfigDefaults -v`
Expected: FAIL — `MentionSpam` field doesn't exist on `Config`

- [ ] **Step 3: Add MentionSpamConfig struct and field**

In `config.go`, add the struct near `BanConfig` (around line 47):

```go
type MentionSpamConfig struct {
	Threshold int `toml:"threshold"`
}
```

Add field to `Config` struct (line 44, before closing `}`):

```go
MentionSpam MentionSpamConfig `toml:"mention_spam"`
```

Add default in `loadConfigDir` (around line 533, after `config.Compaction.ApplyDefaults()`):

```go
if config.MentionSpam.Threshold == 0 {
	config.MentionSpam.Threshold = 2
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestMentionSpamConfigDefaults -v`
Expected: PASS

- [ ] **Step 5: Run full tests + format + vet**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: All pass

- [ ] **Step 6: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add MentionSpamConfig with threshold default"
```

---

### Task 2: MentionNotices struct and defaults

**Files:**
- Modify: `notices.go:14-30` (NoticesConfig struct)
- Modify: `notices.go:48-50` (remove ContextNotices.NoContext)
- Modify: `notices.go:197-199` (update defaults)
- Modify: `notices_test.go` (update tests)

- [ ] **Step 1: Write the failing test**

Add to `notices_test.go`:

```go
func TestMentionNoticesDefaults(t *testing.T) {
	var n NoticesConfig
	setNoticesDefaults(&n)
	assert.Contains(t, n.Mentions.NoContext, "{help_url}")
	assert.Contains(t, n.Mentions.Muted, "{trigger}")
	assert.Contains(t, n.Mentions.NoContext, "{trigger}")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestMentionNoticesDefaults -v`
Expected: FAIL — `Mentions` field doesn't exist

- [ ] **Step 3: Add MentionNotices struct**

In `notices.go`, add the struct after `ContextNotices` (after line 50):

```go
type MentionNotices struct {
	NoContext string `toml:"no_context"`
	Muted     string `toml:"muted"`
}
```

Add field to `NoticesConfig` (line 18, after `Context`):

```go
Mentions MentionNotices `toml:"mentions"`
```

Remove `NoContext` from `ContextNotices` (line 49 becomes just an empty struct or remove the struct entirely — check if any other code references `ContextNotices`):

```go
type ContextNotices struct {
}
```

Update `setNoticesDefaults` — replace the Context.NoContext block (lines 197-199) with:

```go
if n.Mentions.NoContext == "" {
	n.Mentions.NoContext = "You need to start a chat session first! See {help_url} for help. Once started, you can reply to my nick to continue the conversation."
}
if n.Mentions.Muted == "" {
	n.Mentions.Muted = "Further mentions will be ignored until you start a session. Use {trigger}help to get started."
}
```

- [ ] **Step 4: Fix all references to `config.Notices.Context.NoContext`**

Search the entire codebase for `Context.NoContext` and update to `Mentions.NoContext`. The main reference is in `irc_handlers.go:177`. Update:

```go
// old:
readConfig(func() { noCtxMsg = config.Notices.Context.NoContext })
// new:
readConfig(func() { noCtxMsg = config.Notices.Mentions.NoContext })
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -run TestMentionNoticesDefaults -v`
Expected: PASS

- [ ] **Step 6: Run full tests + format + vet**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: All pass

- [ ] **Step 7: Commit**

```bash
git add notices.go notices_test.go irc_handlers.go
git commit -m "feat: add MentionNotices, migrate no_context from Context to Mentions"
```

---

### Task 3: Mention tracker data structure and methods

**Files:**
- Create: `mention_tracker.go`
- Create: `mention_tracker_test.go`

- [ ] **Step 1: Write the failing tests**

Create `mention_tracker_test.go`:

```go
package main

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMentionTracker_RecordAndCount(t *testing.T) {
	tr := newMentionTracker()

	count := tr.recordMention("testnet", 1)
	assert.Equal(t, 1, count)
	assert.False(t, tr.isMuted("testnet", 1))

	count = tr.recordMention("testnet", 1)
	assert.Equal(t, 2, count)
}

func TestMentionTracker_SetMuted(t *testing.T) {
	tr := newMentionTracker()

	assert.False(t, tr.isMuted("testnet", 1))
	tr.setMuted("testnet", 1)
	assert.True(t, tr.isMuted("testnet", 1))
}

func TestMentionTracker_Reset(t *testing.T) {
	tr := newMentionTracker()

	tr.recordMention("testnet", 1)
	tr.setMuted("testnet", 1)
	assert.True(t, tr.isMuted("testnet", 1))

	tr.reset("testnet", 1)
	assert.False(t, tr.isMuted("testnet", 1))

	count := tr.recordMention("testnet", 1)
	assert.Equal(t, 1, count)
}

func TestMentionTracker_IsMutedUnknown(t *testing.T) {
	tr := newMentionTracker()
	assert.False(t, tr.isMuted("testnet", 999))
}

func TestMentionTracker_Sweep(t *testing.T) {
	tr := newMentionTracker()

	tr.recordMention("testnet", 1)
	tr.setMuted("testnet", 2)
	tr.recordMention("testnet", 3)

	tr.sweep()

	assert.False(t, tr.isMuted("testnet", 1))
	assert.True(t, tr.isMuted("testnet", 2))
	assert.False(t, tr.isMuted("testnet", 3))
}

func TestMentionTracker_Concurrent(t *testing.T) {
	tr := newMentionTracker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.recordMention("testnet", 1)
			tr.isMuted("testnet", 1)
			tr.reset("testnet", 1)
		}()
	}
	wg.Wait()
}

func TestMentionTracker_DifferentUsers(t *testing.T) {
	tr := newMentionTracker()

	tr.recordMention("testnet", 1)
	count := tr.recordMention("testnet", 2)
	assert.Equal(t, 1, count)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestMentionTracker -v`
Expected: FAIL — `newMentionTracker` undefined

- [ ] **Step 3: Implement mention_tracker.go**

Create `mention_tracker.go`:

```go
package main

import (
	"fmt"
	"sync"
)

type mentionState struct {
	count int
	muted bool
}

type mentionTracker struct {
	mu    sync.Mutex
	users map[string]*mentionState
}

func newMentionTracker() *mentionTracker {
	return &mentionTracker{
		users: make(map[string]*mentionState),
	}
}

func (t *mentionTracker) key(network string, userID int64) string {
	return fmt.Sprintf("%s:%d", network, userID)
}

func (t *mentionTracker) recordMention(network string, userID int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.key(network, userID)
	s, ok := t.users[k]
	if !ok {
		s = &mentionState{}
		t.users[k] = s
	}
	s.count++
	return s.count
}

func (t *mentionTracker) isMuted(network string, userID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.users[t.key(network, userID)]
	if !ok {
		return false
	}
	return s.muted
}

func (t *mentionTracker) setMuted(network string, userID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.users[t.key(network, userID)]
	if !ok {
		s = &mentionState{}
		t.users[t.key(network, userID)] = s
	}
	s.muted = true
}

func (t *mentionTracker) reset(network string, userID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.users, t.key(network, userID))
}

func (t *mentionTracker) sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range t.users {
		if !s.muted {
			delete(t.users, k)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestMentionTracker -v`
Expected: All PASS

- [ ] **Step 5: Run full tests + format + vet**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: All pass

- [ ] **Step 6: Commit**

```bash
git add mention_tracker.go mention_tracker_test.go
git commit -m "feat: add mention tracker with record/mute/reset/sweep"
```

---

### Task 4: Extract buildHelpText from help()

**Files:**
- Modify: `help.go:21-169` (extract text builder)
- Modify: `help.go:171-231` (refactor help() to use builder)
- Add test in `help_test.go` (if exists) or `mention_tracker_test.go`

- [ ] **Step 1: Write the failing test**

Add to `mention_tracker_test.go` (or a new test file — check if `help_test.go` exists):

```go
func TestBuildHelpText(t *testing.T) {
	cfg := Config{
		Trigger: "!",
		Commands: Commands{
			Completions: map[string]AIConfig{
				"test": {Model: "gpt-4", Service: "openai"},
			},
		},
	}
	readConfig(func() { config = cfg })
	defer readConfig(func() { config = Config{} })

	text := buildHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "testbot")
	assert.Contains(t, text, "!stop")
}
```

Note: This test may need adjustment based on what `readConfig`/`config` globals look like. The test verifies the extracted function returns non-empty help text containing the botnick and trigger.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestBuildHelpText -v`
Expected: FAIL — `buildHelpText` undefined

- [ ] **Step 3: Extract buildHelpText**

In `help.go`, add a new function before `help()`:

```go
func buildHelpText(botnick, trigger string, network Network) string {
	var completions map[string]AIConfig
	var chats map[string]AIConfig
	var tools map[string]MCPCommandConfig
	readConfig(func() {
		completions = make(map[string]AIConfig, len(config.Commands.Completions))
		for k, v := range config.Commands.Completions {
			completions[k] = v
		}
		chats = make(map[string]AIConfig, len(config.Commands.Chats))
		for k, v := range config.Commands.Chats {
			chats[k] = v
		}
		tools = make(map[string]MCPCommandConfig, len(config.Commands.Tools))
		for k, v := range config.Commands.Tools {
			tools[k] = v
		}
	})

	var lines []string

	lines = append(lines, fmt.Sprintf("I'm %s! Use my commands below to chat or generate images.", botnick))
	lines = append(lines, fmt.Sprintf("Only Chat commands start a persistent context. After starting one, reply with my nick (e.g. \"%s, your message here\") to continue that context without using a command.", botnick))
	lines = append(lines, "Commands marked with (regex) use pattern matching, the trigger can match more than one name.")
	if !isNetworkCommandDisabled(network, "stop") {
		lines = append(lines, fmt.Sprintf("  %sstop — Stop text generation (including this help message)", trigger))
	}
	if !isNetworkCommandDisabled(network, "support") {
		lines = append(lines, fmt.Sprintf("  %ssupport — Support dave's development", trigger))
	}

	if theDB != nil {
		var histLines []string
		histBuiltins := []struct {
			name string
			line string
		}{
			{"sessions", fmt.Sprintf("  %ssessions [nick|*] — List sessions (yours, another user's, or all)", trigger)},
			{"history", fmt.Sprintf("  %shistory <id> — Show messages from a session", trigger)},
			{"resume", fmt.Sprintf("  %sresume <id> — Resume a previous session", trigger)},
			{"delete", fmt.Sprintf("  %sdelete <id> — Delete a session", trigger)},
			{"mystats", fmt.Sprintf("  %smystats — Show your session/message stats", trigger)},
			{"jobs", fmt.Sprintf("  %sjobs — List your chat queue and background jobs", trigger)},
			{"compact", fmt.Sprintf("  %scompact — Summarize old messages in your active session to free context", trigger)},
			{"clone", fmt.Sprintf("  %sclone <nick|id> — Clone another user's session (or your own, to fork it)", trigger)},
		}
		for _, h := range histBuiltins {
			if !isNetworkCommandDisabled(network, h.name) {
				histLines = append(histLines, h.line)
			}
		}
		if len(histLines) > 0 {
			lines = append(lines, "\x02History:\x02")
			lines = append(lines, histLines...)
		}
	}

	filteredCompletions := make(map[string]AIConfig)
	for k, v := range completions {
		if !isNetworkCommandDisabled(network, k) {
			filteredCompletions[k] = v
		}
	}
	if len(filteredCompletions) > 0 {
		lines = append(lines, "\x02Completions:\x02")
		for _, l := range formatTable(sortedAIConfigEntries(trigger, filteredCompletions)) {
			lines = append(lines, "  "+l)
		}
	}

	filteredChats := make(map[string]AIConfig)
	for k, v := range chats {
		if !isNetworkCommandDisabled(network, k) {
			filteredChats[k] = v
		}
	}
	if len(filteredChats) > 0 {
		lines = append(lines, "\x02Chats:\x02")
		for _, l := range formatTable(sortedAIConfigEntries(trigger, filteredChats)) {
			lines = append(lines, "  "+l)
		}
	}

	filteredTools := make(map[string]MCPCommandConfig)
	for k, v := range tools {
		if !isNetworkCommandDisabled(network, k) {
			filteredTools[k] = v
		}
	}
	if len(filteredTools) > 0 {
		var entries []helpEntry
		toolKeys := make([]string, 0, len(filteredTools))
		for k := range filteredTools {
			toolKeys = append(toolKeys, k)
		}
		sort.Slice(toolKeys, func(i, j int) bool {
			return toolKeys[i] < toolKeys[j]
		})
		for _, k := range toolKeys {
			c := filteredTools[k]
			entries = append(entries, helpEntry{
				cmd:  formatCmd(trigger, c.Regex, c.Name),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			})
		}
		lines = append(lines, "\x02Tool commands:\x02")
		for _, l := range formatTable(entries) {
			lines = append(lines, "  "+l)
		}
	}

	mcpLines := getAllMCPServerInfo()
	if len(mcpLines) > 0 {
		lines = append(lines, "\x02MCP Servers:\x02")
		for _, l := range mcpLines {
			lines = append(lines, l)
		}
	}

	return strings.Join(lines, "\n")
}
```

Now refactor `help()` to use `buildHelpText`. The `help()` function becomes:

```go
func help(network Network, client *girc.Client, event girc.Event, ctx context.Context, output chan<- string, args ...string) {
	botnick := client.GetNick()

	if len(args) > 0 && args[0] != "" {
		cmdName := args[0]
		entry, found := findCommandHelp(network, cmdName)
		if !found {
			select {
			case output <- fmt.Sprintf("\x0304❗ Command '%s' not found. Use %shelp to see all commands.", cmdName, network.Trigger):
			case <-ctx.Done():
			}
			return
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("Help for %s:", entry.cmd))
		if entry.info != "" {
			lines = append(lines, "  "+entry.info)
		}
		lines = append(lines, "  "+entry.desc)
		if entry.mcpInfo != "" {
			lines = append(lines, "  "+entry.mcpInfo)
		}
		for _, line := range lines {
			select {
			case output <- line:
			case <-ctx.Done():
				return
			}
		}
		return
	}

	rawText := buildHelpText(botnick, network.Trigger, network)

	chCfg := network.GetChannelConfig(event.Params[0])
	if chCfg.Pastebin {
		wrappedLines := wrapForIRC(rawText)
		if len(wrappedLines) >= chCfg.GetMaxLines() {
			url, err := uploadToPastebin("```\n"+rawText+"\n```", "Dave's Help")
			n := getNotices()
			if err != nil {
				select {
				case output <- errorNotice(n.DB.PastebinUpload, map[string]string{"error": err.Error()}):
				case <-ctx.Done():
					return
				}
				preview := chCfg.GetMaxLines()
				if preview > len(wrappedLines) {
					preview = len(wrappedLines)
				}
				for i := 0; i < preview; i++ {
					select {
					case output <- wrappedLines[i]:
					case <-ctx.Done():
						return
					}
				}
				select {
				case output <- n.Pastebin.Failed:
				case <-ctx.Done():
					return
				}
				return
			}
			preview := 3
			if preview > len(wrappedLines) {
				preview = len(wrappedLines)
			}
			for i := 0; i < preview; i++ {
				select {
				case output <- wrappedLines[i]:
				case <-ctx.Done():
					return
				}
			}
			select {
			case output <- expandNotice(n.Pastebin.Link, map[string]string{"url": url}):
			case <-ctx.Done():
				return
			}
			return
		}
	}

	for _, line := range strings.Split(rawText, "\n") {
		for _, wrapped := range wrapLine(line) {
			select {
			case output <- wrapped:
			case <-ctx.Done():
				return
			}
		}
	}
}
```

Note: The original `help()` iterated `lines` and called `wrapLine` per line. The refactored version splits `rawText` back into lines and does the same. The per-line help output section (args > 0) stays inline since it doesn't benefit from extraction.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: All pass

- [ ] **Step 5: Commit**

```bash
git add help.go
git commit -m "refactor: extract buildHelpText from help() for reuse"
```

---

### Task 5: Initialize global tracker and start GC goroutine

**Files:**
- Modify: `main.go:499-507` (add tracker init + GC goroutine)

- [ ] **Step 1: Add global tracker variable**

In `main.go`, near other global declarations (near the rate limiter globals or bot declarations), add:

```go
var theMentionTracker = newMentionTracker()
```

- [ ] **Step 2: Add GC goroutine**

In `main.go`, after `startRateLimitGC()` (line 499), add:

```go
go func() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		theMentionTracker.sweep()
	}
}()
```

- [ ] **Step 3: Run full tests + format + vet**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: All pass

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "feat: initialize global mention tracker and start GC goroutine"
```

---

### Task 6: Integrate tracker in irc_handlers.go

**Files:**
- Modify: `irc_handlers.go:159-210` (handleMention)
- Modify: `irc_handlers.go:212-375` (handleTrigger)

- [ ] **Step 1: Update handleMention — no-context branch**

Replace the no-context block in `handleMention` (lines 174-180). The current code:

```go
if !ContextExists(network.Name, channel, userID) {
    logger.Info("Ignoring message due to no existing chat context")
    var noCtxMsg string
    readConfig(func() { noCtxMsg = config.Notices.Mentions.NoContext })
    noCtxMsg = expandNotice(noCtxMsg, map[string]string{"trigger": network.Trigger})
    client.Cmd.Reply(event, warnMsg(noCtxMsg))
    return
}
```

Becomes:

```go
if !ContextExists(network.Name, channel, userID) {
    logger.Info("Ignoring message due to no existing chat context")
    if theMentionTracker.isMuted(network.Name, userID) {
        return
    }
    helpURL := ""
    helpText := buildHelpText(client.GetNick(), network.Trigger, network)
    url, err := uploadToPastebin("```\n"+helpText+"\n```", "Dave Help")
    if err != nil {
        logger.Warn("failed to upload help to pastebin for no_context notice", "error", err)
        helpURL = network.Trigger + "help"
    } else {
        helpURL = url
    }
    var noCtxMsg string
    readConfig(func() { noCtxMsg = config.Notices.Mentions.NoContext })
    noCtxMsg = expandNotice(noCtxMsg, map[string]string{"trigger": network.Trigger, "help_url": helpURL})
    client.Cmd.Reply(event, warnMsg(noCtxMsg))
    count := theMentionTracker.recordMention(network.Name, userID)
    var threshold int
    readConfig(func() { threshold = config.MentionSpam.Threshold })
    if count >= threshold {
        var mutedMsg string
        readConfig(func() { mutedMsg = config.Notices.Mentions.Muted })
        mutedMsg = expandNotice(mutedMsg, map[string]string{"trigger": network.Trigger})
        client.Cmd.Reply(event, warnMsg(mutedMsg))
        theMentionTracker.setMuted(network.Name, userID)
    }
    return
}
```

- [ ] **Step 2: Update handleMention — success branch**

Add tracker reset after the ban check passes (line 173, after `isBanned` check), before the `ContextExists` check:

```go
theMentionTracker.reset(network.Name, userID)
```

Insert after line 172 (after `isBanned` return block, before `ContextExists` check).

- [ ] **Step 3: Update handleTrigger — after ban check**

In `handleTrigger`, after the ban check (line 296, after the `isBanned` block), add:

```go
theMentionTracker.reset(network.Name, userID)
```

This resets the mention counter whenever a user successfully runs any trigger command.

- [ ] **Step 4: Run full tests + format + vet**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: All pass

- [ ] **Step 5: Commit**

```bash
git add irc_handlers.go
git commit -m "feat: integrate mention tracker in handleMention and handleTrigger"
```

---

### Task 7: Update config files

**Files:**
- Modify: `config/config.toml` (add `[mention_spam]` section)
- Modify: `config/notices.toml` (add `[mentions]` section, update `[context]`)

- [ ] **Step 1: Add [mention_spam] to config/config.toml**

Add the reference documentation and commented example block in the appropriate section (near `[bans]`):

```toml
# [mention_spam] — Mention spam protection
# threshold    (int)    — No-context mentions before muting (default: 2)
#
# [mention_spam]
# threshold = 2
```

Add the live config block:

```toml
[mention_spam]
threshold = 2
```

- [ ] **Step 2: Update config/notices.toml**

Add `[mentions]` section with reference and defaults:

```toml
# [mentions] — Mention spam notice templates
# no_context  (string) — Sent when user mentions bot without a session.
#                         Placeholders: {trigger}, {help_url}
# muted       (string) — Sent when user hits mention threshold.
#                         Placeholders: {trigger}
#
# [mentions]
# no_context = "You need to start a chat session first! See {help_url} for help. Once started, you can reply to my nick to continue the conversation."
# muted = "Further mentions will be ignored until you start a session. Use {trigger}help to get started."
```

Remove or update the `[context]` section's `no_context` field (since it moved to `[mentions]`).

- [ ] **Step 3: Run full tests + format + vet**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: All pass

- [ ] **Step 4: Commit**

```bash
git add config/config.toml config/notices.toml
git commit -m "docs: add mention_spam config and notices documentation"
```

---

### Task 8: Final verification

**Files:**
- All modified files

- [ ] **Step 1: Run full test suite**

Run: `go test ./...`
Expected: All pass

- [ ] **Step 2: Run format and vet**

Run: `go fmt ./... && go vet ./...`
Expected: Clean

- [ ] **Step 3: Build binary**

Run: `go build -o dave .`
Expected: Clean build

- [ ] **Step 4: Verify no_context flow manually (optional)**

If a test IRC environment is available:
1. Mention bot without a session → should get no_context with pastebin link
2. Mention again → should get no_context + muted notice
3. Mention again → should be silently ignored
4. Use `!help` → should reset tracker
5. Mention again → should get no_context again (counter reset)
