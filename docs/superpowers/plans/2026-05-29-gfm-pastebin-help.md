# GFM Pastebin Help Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace plain-text pastebin help with GFM markdown, reorder sections to prioritize chat commands, and add an example session chatlog.

**Architecture:** Add a new `buildPastebinHelpText` function that generates GFM markdown (headings, tables, code blocks) completely separate from the existing IRC `buildHelpText`. The `help()` function and `irc_handlers.go` no_context path switch to the new builder for pastebin uploads. IRC in-channel output is untouched.

**Tech Stack:** Go stdlib (`fmt`, `strings`, `sort`). No new dependencies.

---

### Task 1: Add `buildPastebinHelpText` function

**Files:**
- Modify: `help.go` (add new function after `buildHelpText`)

- [ ] **Step 1: Write the new `buildPastebinHelpText` function**

Add the following function to `help.go` after the existing `buildHelpText` (after line 142). This function builds a GFM markdown document with the new section order: Chats → Example → Completions → Tools → History → Built-ins → MCP Servers.

```go
func buildPastebinHelpText(botnick, trigger string, network Network) string {
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

	var b strings.Builder

	fmt.Fprintf(&b, "# %s Help\n\n", botnick)
	fmt.Fprintf(&b, "I'm %s! Use my commands below to chat or generate images.\n", botnick)
	fmt.Fprintf(&b, "Only **Chat commands** start a persistent context. After starting one, reply to my nick (e.g. `%s, your message here`) to continue that context without using a command.\n", botnick)
	fmt.Fprintf(&b, "Commands marked with `(regex)` use pattern matching, the trigger can match more than one name.\n\n")
	b.WriteString("---\n\n")

	filteredChats := make(map[string]AIConfig)
	for k, v := range chats {
		if !isNetworkCommandDisabled(network, k) {
			filteredChats[k] = v
		}
	}
	if len(filteredChats) > 0 {
		entries := sortedAIConfigEntries(trigger, filteredChats)
		writeGFMTable(&b, "## Chat Commands\n", "| Command | Service/Model | Description |", entries)
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Example Session\n\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "<alice> %schat hey %s, what's a good recipe for pasta?\n", trigger, botnick)
	fmt.Fprintf(&b, "<%s> Hey Alice! Here's a classic — Cacio e Pepe. You'll need...\n", botnick)
	fmt.Fprintf(&b, "<alice> %s, can you make it vegetarian?\n", botnick)
	fmt.Fprintf(&b, "<%s> Sure! Swap the pecorino for a good vegetarian alternative...\n", botnick)
	fmt.Fprintf(&b, "<alice> %sstop\n", trigger)
	fmt.Fprintf(&b, "<%s> Session paused.\n", botnick)
	fmt.Fprintf(&b, "```\n\n")

	filteredCompletions := make(map[string]AIConfig)
	for k, v := range completions {
		if !isNetworkCommandDisabled(network, k) {
			filteredCompletions[k] = v
		}
	}
	if len(filteredCompletions) > 0 {
		entries := sortedAIConfigEntries(trigger, filteredCompletions)
		writeGFMTable(&b, "## Completions\n", "| Command | Service/Model | Description |", entries)
		b.WriteString("\n")
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
		writeGFMTable(&b, "## Tool Commands\n", "| Command | MCP/Tool | Description |", entries)
		b.WriteString("\n")
	}

	if theDB != nil {
		var rows []string
		histBuiltins := []struct {
			name string
			line string
		}{
			{"sessions", fmt.Sprintf("`%ssessions [nick|*]` | List sessions (yours, another user's, or all)", trigger)},
			{"history", fmt.Sprintf("`%shistory <id>` | Show messages from a session", trigger)},
			{"resume", fmt.Sprintf("`%sresume <id>` | Resume a previous session", trigger)},
			{"delete", fmt.Sprintf("`%sdelete <id>` | Delete a session", trigger)},
			{"mystats", fmt.Sprintf("`%smystats` | Show your session/message stats", trigger)},
			{"jobs", fmt.Sprintf("`%sjobs` | List your chat queue and background jobs", trigger)},
			{"compact", fmt.Sprintf("`%scompact` | Summarize old messages to free context", trigger)},
			{"clone", fmt.Sprintf("`%sclone <nick|id>` | Clone another user's session", trigger)},
		}
		for _, h := range histBuiltins {
			if !isNetworkCommandDisabled(network, h.name) {
				rows = append(rows, h.line)
			}
		}
		if len(rows) > 0 {
			b.WriteString("## History & Sessions\n\n")
			b.WriteString("| Command | Description |\n")
			b.WriteString("|---------|-------------|\n")
			for _, r := range rows {
				fmt.Fprintf(&b, "| %s |\n", r)
			}
			b.WriteString("\n")
		}
	}

	var builtinRows []string
	builtinItems := []struct {
		name string
		desc string
	}{
		{"stop", "Stop text generation"},
		{"support", "Support dave's development"},
	}
	for _, bi := range builtinItems {
		if !isNetworkCommandDisabled(network, bi.name) {
			builtinRows = append(builtinRows, fmt.Sprintf("`%s%s` | %s", trigger, bi.name, bi.desc))
		}
	}
	if len(builtinRows) > 0 {
		b.WriteString("## Built-in Commands\n\n")
		b.WriteString("| Command | Description |\n")
		b.WriteString("|---------|-------------|\n")
		for _, r := range builtinRows {
			fmt.Fprintf(&b, "| %s |\n", r)
		}
		b.WriteString("\n")
	}

	mcpServers := getAllMCPServerInfo()
	if len(mcpServers) > 0 {
		b.WriteString("## MCP Servers\n\n")
		for _, l := range mcpServers {
			stripped := strings.ReplaceAll(l, "\x02", "**")
			fmt.Fprintf(&b, "- %s\n", stripped)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func writeGFMTable(b *strings.Builder, header string, colHeader string, entries []helpEntry) {
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString(colHeader)
	b.WriteString("\n")
	b.WriteString("|---------|--------------|-------------|\n")
	for _, e := range entries {
		fmt.Fprintf(b, "| `%s` | %s | %s |\n", e.cmd, e.info, e.desc)
	}
}

- [ ] **Step 2: Run `go vet` and `go build` to verify compilation**

Run: `go vet ./... && go build -o /dev/null .`
Expected: Clean compilation, no errors.

---

### Task 2: Update `help()` function to use new pastebin builder

**Files:**
- Modify: `help.go:144-237` (the `help()` function)

- [ ] **Step 1: Update the pastebin upload path in `help()`**

Replace the current pastebin upload block in `help()` (lines 179-225). The key changes:
1. Use `buildPastebinHelpText` instead of wrapping `buildHelpText` in code fences
2. Remove the 3-line preview before the link — just post the link
3. The non-pastebin IRC path (lines 228-236) stays unchanged

Find this block in `help()`:

```go
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
```

Replace with:

```go
	if chCfg.Pastebin {
		wrappedLines := wrapForIRC(rawText)
		if len(wrappedLines) >= chCfg.GetMaxLines() {
			mdText := buildPastebinHelpText(botnick, network.Trigger, network)
			url, err := uploadToPastebin(mdText, "Dave's Help")
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
			select {
			case output <- expandNotice(n.Pastebin.Link, map[string]string{"url": url}):
			case <-ctx.Done():
				return
			}
			return
		}
	}
```

Changes from original:
- Line 182: `uploadToPastebin("```\n"+rawText+"\n```", ...)` → `uploadToPastebin(mdText, ...)` where `mdText` comes from new builder
- Lines 208-218 (the 3-line preview block) removed entirely — just post the link
- Error fallback still shows preview lines (unchanged behavior on upload failure)

- [ ] **Step 2: Run `go vet` and `go build`**

Run: `go vet ./... && go build -o /dev/null .`
Expected: Clean compilation.

---

### Task 3: Update `irc_handlers.go` no_context notice to use new builder

**Files:**
- Modify: `irc_handlers.go:179-187`

- [ ] **Step 1: Update the no_context pastebin upload**

Find in `irc_handlers.go` (around line 179):

```go
		helpURL := ""
		helpText := buildHelpText(client.GetNick(), network.Trigger, network)
		url, err := uploadToPastebin("```\n"+helpText+"\n```", "Dave Help")
```

Replace with:

```go
		helpURL := ""
		mdText := buildPastebinHelpText(client.GetNick(), network.Trigger, network)
		url, err := uploadToPastebin(mdText, "Dave Help")
```

- [ ] **Step 2: Run `go vet` and `go build`**

Run: `go vet ./... && go build -o /dev/null .`
Expected: Clean compilation.

---

### Task 4: Add tests for `buildPastebinHelpText`

**Files:**
- Modify: `help_test.go`

- [ ] **Step 1: Write tests for the new builder**

Add the following tests to `help_test.go`:

```go
func TestBuildPastebinHelpText(t *testing.T) {
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "# testbot Help")
	assert.Contains(t, text, "!stop")
	assert.Contains(t, text, "!support")
	assert.Contains(t, text, "## Example Session")
	assert.Contains(t, text, "<alice>")
	assert.Contains(t, text, "!chat")
	assert.Contains(t, text, "!stop")
	assert.NotEmpty(t, text)
}

func TestBuildPastebinHelpTextUsesBotnickAndTrigger(t *testing.T) {
	text := buildPastebinHelpText("mybot", ".", Network{})
	assert.Contains(t, text, "# mybot Help")
	assert.Contains(t, text, "mybot, your message here")
	assert.Contains(t, text, ".chat")
	assert.Contains(t, text, ".stop")
	assert.Contains(t, text, "<mybot>")
}

func TestBuildPastebinHelpTextWithChats(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"ask": {Name: "ask", Regex: "ask", Service: "openai", Model: "gpt-4", Description: "ask questions"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "## Chat Commands")
	assert.Contains(t, text, "!ask")
	assert.Contains(t, text, "gpt-4")
	assert.Contains(t, text, "ask questions")
	assert.Contains(t, text, "## Example Session")
	chatIdx := strings.Index(text, "## Chat Commands")
	exampleIdx := strings.Index(text, "## Example Session")
	assert.True(t, chatIdx < exampleIdx, "Chat Commands should come before Example Session")
}

func TestBuildPastebinHelpTextWithCompletions(t *testing.T) {
	config.Commands.Completions = map[string]AIConfig{
		"complete": {Name: "complete", Regex: "complete", Service: "openai", Model: "gpt-4"},
	}
	defer func() { config.Commands.Completions = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "## Completions")
	assert.Contains(t, text, "!complete")
}

func TestBuildPastebinHelpTextWithTools(t *testing.T) {
	config.Commands.Tools = map[string]MCPCommandConfig{
		"img": {Name: "img", Regex: "img", MCP: "img-mcp", Tool: "generate_image", Description: "generate an image"},
	}
	defer func() { config.Commands.Tools = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "## Tool Commands")
	assert.Contains(t, text, "!img")
	assert.Contains(t, text, "img-mcp/generate_image")
}

func TestBuildPastebinHelpTextDisabledCommands(t *testing.T) {
	network := Network{
		DisabledCommands: []string{"stop"},
	}
	text := buildPastebinHelpText("testbot", "!", network)
	assert.NotContains(t, text, "!stop")
	assert.Contains(t, text, "!support")
}

func TestBuildPastebinHelpTextChatBeforeCompletions(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat", Service: "openai", Model: "gpt-4"},
	}
	config.Commands.Completions = map[string]AIConfig{
		"complete": {Name: "complete", Regex: "complete", Service: "openai", Model: "gpt-4"},
	}
	defer func() {
		config.Commands.Chats = nil
		config.Commands.Completions = nil
	}()
	text := buildPastebinHelpText("testbot", "!", Network{})
	chatIdx := strings.Index(text, "## Chat Commands")
	compIdx := strings.Index(text, "## Completions")
	assert.True(t, chatIdx < compIdx, "Chat Commands should come before Completions")
}

func TestBuildPastebinHelpTextGFMTables(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat", Service: "openai", Model: "gpt-4", Description: "general chat"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "| Command | Service/Model | Description |")
	assert.Contains(t, text, "|---------|--------------|-------------|")
	assert.Contains(t, text, "| `!chat` | [openai/gpt-4] | general chat |")
}

func TestBuildPastebinHelpTextExampleUsesTrigger(t *testing.T) {
	text := buildPastebinHelpText("dave", "dave:", Network{})
	assert.Contains(t, text, "dave:chat")
	assert.Contains(t, text, "dave:stop")
	assert.Contains(t, text, "<dave>")
}
```

- [ ] **Step 2: Run tests to verify they pass**

Run: `go test -v -run TestBuildPastebinHelpText ./...`
Expected: All tests PASS.

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`
Expected: All tests PASS (existing tests unchanged, new tests pass).

---

### Task 5: Run `go fmt` and `go vet` on all changes

**Files:**
- All modified files

- [ ] **Step 1: Format and vet**

Run: `go fmt ./... && go vet ./...`
Expected: Clean output, no issues.

- [ ] **Step 2: Final full test run**

Run: `go test ./...`
Expected: All tests PASS.
