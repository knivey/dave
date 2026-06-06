# Aliases Replace Regex Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the regex-based command trigger system with a simpler alias system. Chats and completions gain an `aliases` field; tools use only their section name. Dispatch becomes O(1) map lookup instead of O(n) regex iteration.

**Architecture:** Remove `Regex` from `AIConfig` and `MCPCommandConfig`. Add `Aliases []string` to `AIConfig`. Replace `map[*regexp.Regexp]CmdFunc` dispatch with `map[string]CmdFunc` keyed by trigger word. Update help rendering for multi-line alias display. Builtins keep their regex patterns unchanged.

**Tech Stack:** Go 1.25, BurntSushi/toml, girc, testify

---

## File Map

**Modify:**
- `config.go` — Remove `Regex` fields, add `Aliases`, update validation, collision detection, disabled-command validation
- `main.go` — Replace dispatch tables from regex-keyed to string-keyed maps, update `registerCommandsLocked`, remove `extractSubmatchArgs`
- `irc_handlers.go` — Replace regex iteration with map lookup in `handleTrigger`, update `getServiceForConfigCmd`
- `help.go` — Update `formatCmd`, `formatTable`, `buildHelpText`, `buildPastebinHelpText`, `findCommandHelp`, `matchesCommand`, `sortedAIConfigEntries`, `sortedPastebinEntries`, remove regex references
- `config/chats.toml` — Remove `regex` from docs and live config
- `config/completions.toml` — Remove `regex` from docs and live config, convert `dave?` pattern to aliases
- `config/tools.toml` — Remove `regex` from docs

**Test files to modify:**
- `config_test.go` — Update tests that reference `Regex`, add alias and collision tests
- `help_test.go` — Update tests that reference `Regex`, add alias display tests

---

### Task 1: Remove Regex from AIConfig, Add Aliases field

**Files:**
- Modify: `config.go:231-277`
- Test: `config_test.go`

- [ ] **Step 1: Write failing test for Aliases field**

Add this test to `config_test.go` after `TestChatCommandNameSetting`:

```go
func TestChatCommandAliases(t *testing.T) {
	mainTOML := ``
	servicesTOML := `
[test]
maxtokens = 100
maxhistory = 10
`
	chatsTOML := `
[chat1]
service = "test"
aliases = ["gpt", "ask"]

[chat2]
service = "test"
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"services.toml": servicesTOML,
		"chats.toml":    chatsTOML,
	})
	defer os.RemoveAll(dir)

	config := loadConfigDirOrDie(dir)

	chat1 := config.Commands.Chats["chat1"]
	assert.Equal(t, "chat1", chat1.Name)
	assert.Equal(t, []string{"gpt", "ask"}, chat1.Aliases)

	chat2 := config.Commands.Chats["chat2"]
	assert.Equal(t, "chat2", chat2.Name)
	assert.Nil(t, chat2.Aliases)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestChatCommandAliases ./... -v`
Expected: FAIL — `chat1.Aliases undefined` (field doesn't exist yet) or compilation error.

- [ ] **Step 3: Add Aliases field, remove Regex from AIConfig**

In `config.go`, edit the `AIConfig` struct (around line 231):

Remove this line:
```go
	Regex                string
```

Add after `Description` (around line 252):
```go
	Aliases               []string           `toml:"aliases"`
```

- [ ] **Step 4: Remove regex defaulting from validateAIConfig**

In `config.go`, edit `validateAIConfig` (around line 924). Remove these lines:
```go
	if cfg.Regex == "" {
		cfg.Regex = name
	}
```

The function should just set `cfg.Name = name` and do the service lookup.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -run TestChatCommandAliases ./... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add Aliases field to AIConfig, remove Regex"
```

---

### Task 2: Remove Regex from MCPCommandConfig

**Files:**
- Modify: `config.go:208-222`
- Modify: `config.go:867-871`

- [ ] **Step 1: Remove Regex field from MCPCommandConfig**

In `config.go`, edit the `MCPCommandConfig` struct (around line 208). Remove this line:
```go
	Regex          string
```

- [ ] **Step 2: Remove regex defaulting from validateCommands**

In `config.go`, in the tools validation loop (around line 868), remove these lines:
```go
		if cfg.Regex == "" {
			cfg.Regex = name
		}
```

- [ ] **Step 3: Build to verify no compilation errors in config.go**

Run: `go build ./...`
Expected: Build errors in `main.go`, `irc_handlers.go`, `help.go` referencing `.Regex` — this is expected. Those will be fixed in later tasks. Do NOT commit yet.

Note: Do not commit yet — the build is broken. We'll fix all referencing code in subsequent tasks before committing.

---

### Task 3: Replace dispatch tables from regex-keyed to string-keyed maps

**Files:**
- Modify: `main.go:250-413`

- [ ] **Step 1: Write failing test for string-keyed dispatch**

Add this test to `config_test.go` after `TestRegisterCommandsLocked_PopulatesConfigCmdNames`:

```go
func TestRegisterCommandsLocked_AliasesCreateDispatchEntries(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	cmds := Commands{
		Completions: map[string]AIConfig{
			"comp1": {Name: "comp1", Service: "svc"},
		},
		Chats: map[string]AIConfig{
			"chat1": {Name: "chat1", Aliases: []string{"gpt", "ask"}, Service: "svc"},
		},
		Tools: map[string]MCPCommandConfig{
			"tool1": {Name: "tool1", MCP: "mcp", Tool: "test"},
		},
	}
	err := registerCommandsLocked(cmds)
	require.NoError(t, err)

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()

	// All triggers should be in configCmds
	assert.NotNil(t, configCmds["comp1"], "comp1 trigger")
	assert.NotNil(t, configCmds["chat1"], "chat1 trigger")
	assert.NotNil(t, configCmds["gpt"], "gpt alias trigger")
	assert.NotNil(t, configCmds["ask"], "ask alias trigger")
	assert.NotNil(t, configCmds["tool1"], "tool1 trigger")

	// configCmdNames maps each trigger to canonical name
	assert.Equal(t, "comp1", configCmdNames["comp1"])
	assert.Equal(t, "chat1", configCmdNames["chat1"])
	assert.Equal(t, "chat1", configCmdNames["gpt"])
	assert.Equal(t, "chat1", configCmdNames["ask"])
	assert.Equal(t, "tool1", configCmdNames["tool1"])

	// Aliases for chat1 should be marked as chat commands
	assert.True(t, chatCmds["chat1"])
	assert.True(t, chatCmds["gpt"])
	assert.True(t, chatCmds["ask"])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestRegisterCommandsLocked_AliasesCreateDispatchEntries ./... -v`
Expected: FAIL — type mismatch (`configCmds` is still `CmdMap`).

- [ ] **Step 3: Replace dispatch table type declarations**

In `main.go`, replace these lines (around 309-312):

```go
var configCmds CmdMap
var configCmdNames map[*regexp.Regexp]string
var rateExemptCmds map[*regexp.Regexp]bool
var chatCmds map[*regexp.Regexp]bool
```

With:

```go
var configCmds map[string]CmdFunc
var configCmdNames map[string]string
var rateExemptCmds map[string]bool
var chatCmds map[string]bool
var configCmdTakesArgs map[string]bool
```

- [ ] **Step 4: Rewrite registerCommandsLocked**

In `main.go`, replace the entire `registerCommandsLocked` function (lines 360-413) with:

```go
func registerCommandsLocked(cmds Commands) error {
	newConfigCmds := make(map[string]CmdFunc)
	newConfigCmdNames := make(map[string]string)
	newExemptCmds := make(map[string]bool)
	newChatCmds := make(map[string]bool)
	newTakesArgs := make(map[string]bool)

	// Collect all triggers for collision detection
	triggers := make(map[string]string) // trigger -> canonical name

	addTrigger := func(trigger, canonical, section string) error {
		if existing, ok := triggers[trigger]; ok {
			return fmt.Errorf("command trigger %q (%s.%s) conflicts with %q (%s)", trigger, section, canonical, existing, section)
		}
		triggers[trigger] = canonical
		return nil
	}

	for name, c := range cmds.Completions {
		logger.Debug("added Completions command", c)
		if err := addTrigger(name, name, "completions"); err != nil {
			return err
		}
		for _, alias := range c.Aliases {
			if err := addTrigger(alias, name, "completions"); err != nil {
				return err
			}
		}
		handler := func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			completion(network, client, e, c, ctx, output, args...)
		}
		newConfigCmds[name] = handler
		newConfigCmdNames[name] = name
		newTakesArgs[name] = true
		for _, alias := range c.Aliases {
			newConfigCmds[alias] = handler
			newConfigCmdNames[alias] = name
			newTakesArgs[alias] = true
		}
	}

	for name, c := range cmds.Chats {
		logger.Debug("added Chats command", c)
		if err := addTrigger(name, name, "chats"); err != nil {
			return err
		}
		for _, alias := range c.Aliases {
			if err := addTrigger(alias, name, "chats"); err != nil {
				return err
			}
		}
		handler := func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			chat(network, client, e, c, ctx, output, nil, args...)
		}
		newConfigCmds[name] = handler
		newConfigCmdNames[name] = name
		newTakesArgs[name] = true
		newChatCmds[name] = true
		for _, alias := range c.Aliases {
			newConfigCmds[alias] = handler
			newConfigCmdNames[alias] = name
			newTakesArgs[alias] = true
			newChatCmds[alias] = true
		}
	}

	for name, c := range cmds.Tools {
		logger.Debug("added Tools command", c)
		if err := addTrigger(name, name, "tools"); err != nil {
			return err
		}
		handler := func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			mcpCmd(network, client, e, c, ctx, output, args...)
		}
		newConfigCmds[name] = handler
		newConfigCmdNames[name] = name
		takesArgs := c.Arg != ""
		newTakesArgs[name] = takesArgs
		if c.SkipBusy {
			newExemptCmds[name] = true
		}
	}

	configCmds = newConfigCmds
	configCmdNames = newConfigCmdNames
	rateExemptCmds = newExemptCmds
	chatCmds = newChatCmds
	configCmdTakesArgs = newTakesArgs
	return nil
}
```

- [ ] **Step 5: Remove extractSubmatchArgs function**

In `main.go`, remove the `extractSubmatchArgs` function (lines 264-272). It's no longer used by config command dispatch. Builtins use their own capture groups via their regex patterns.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test -run TestRegisterCommandsLocked_AliasesCreateDispatchEntries ./... -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add main.go config_test.go
git commit -m "feat: replace regex-keyed dispatch with string-keyed maps, add alias support"
```

---

### Task 4: Update handleTrigger dispatch in irc_handlers.go

**Files:**
- Modify: `irc_handlers.go:237-425`

- [ ] **Step 1: Write failing test for alias dispatch**

Add this test to `config_test.go`:

```go
func TestHandleTrigger_AliasDispatchesToCanonical(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	// We can't easily test the full handleTrigger without a real IRC connection,
	// but we can verify the dispatch maps are correct for aliases.
	cmds := Commands{
		Chats: map[string]AIConfig{
			"mychat": {Name: "mychat", Aliases: []string{"gpt"}, Service: "svc", Model: "m"},
		},
	}
	err := registerCommandsLocked(cmds)
	require.NoError(t, err)

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()

	// "gpt" should resolve to "mychat" as canonical
	assert.Equal(t, "mychat", configCmdNames["gpt"])
	// Both should have handlers
	assert.NotNil(t, configCmds["gpt"])
	assert.NotNil(t, configCmds["mychat"])
	// Both should be marked as taking args
	assert.True(t, configCmdTakesArgs["gpt"])
	assert.True(t, configCmdTakesArgs["mychat"])
}
```

- [ ] **Step 2: Run test to verify it passes (maps already updated)**

Run: `go test -run TestHandleTrigger_AliasDispatchesToCanonical ./... -v`
Expected: PASS (the maps were already updated in Task 3).

- [ ] **Step 3: Update handleTrigger config command matching**

In `irc_handlers.go`, replace the config command matching loop in `handleTrigger` (around lines 292-300). Replace this block:

```go
	if match == nil {
		for r, cmd := range configCmds {
			if r.Match([]byte(stripped)) {
				name := configCmdNames[r]
				match = &cmdMatch{cmd: cmd, re: r, args: extractSubmatchArgs(r, stripped), disabled: isNetworkCommandDisabled(network, name)}
				break
			}
		}
	}
```

With:

```go
	if match == nil {
		triggerWord, rest, hasArgs := splitFirstWord(stripped)
		if cmd, ok := configCmds[triggerWord]; ok {
			takesArgs := configCmdTakesArgs[triggerWord]
			// Arg compatibility check: commands that take args must have args, and vice versa
			if takesArgs != hasArgs {
				// No match — wrong arg presence
			} else {
				name := configCmdNames[triggerWord]
				var args []string
				if hasArgs {
					args = []string{rest}
				}
				match = &cmdMatch{
					cmd:      cmd,
					args:     args,
					disabled: isNetworkCommandDisabled(network, name),
					trigger:  triggerWord,
				}
			}
		}
	}
```

- [ ] **Step 4: Update cmdMatch struct**

In `irc_handlers.go`, update the `cmdMatch` struct (around line 274). Replace:

```go
	type cmdMatch struct {
		cmd      CmdFunc
		re       *regexp.Regexp
		args     []string
		builtin  bool
		disabled bool
	}
```

With:

```go
	type cmdMatch struct {
		cmd      CmdFunc
		re       *regexp.Regexp
		args     []string
		builtin  bool
		disabled bool
		trigger  string // config command trigger word (empty for builtins)
	}
```

- [ ] **Step 5: Add splitFirstWord helper function**

Add this function to `main.go` (near where `extractSubmatchArgs` was):

```go
func splitFirstWord(s string) (word, rest string, hasArgs bool) {
	idx := strings.IndexByte(s, ' ')
	if idx == -1 {
		return s, "", false
	}
	return s[:idx], s[idx+1:], true
}
```

- [ ] **Step 6: Update config command execution to use trigger instead of regex**

In `irc_handlers.go`, update the config command execution section (around lines 381-401). Replace all references to `match.re` with `match.trigger`:

Replace:
```go
	if rateExemptCmds[match.re] {
		if chatCmds[match.re] {
```
With:
```go
	if rateExemptCmds[match.trigger] {
		if chatCmds[match.trigger] {
```

Replace:
```go
	if chatCmds[match.re] {
```
With:
```go
	if chatCmds[match.trigger] {
```

Replace:
```go
	svc := getServiceForConfigCmd(match.re)
```
With:
```go
	svc := getServiceForConfigCmd(match.trigger)
```

- [ ] **Step 7: Update getServiceForConfigCmd signature and implementation**

In `irc_handlers.go`, replace `getServiceForConfigCmd` (lines 404-425):

```go
func getServiceForConfigCmd(trigger string) string {
	commandsMutex.RLock()
	defer commandsMutex.RUnlock()
	canonical := configCmdNames[trigger]
	var svc string
	readConfig(func() {
		if c, ok := config.Commands.Completions[canonical]; ok {
			svc = c.Service
			return
		}
		if c, ok := config.Commands.Chats[canonical]; ok {
			svc = c.Service
			return
		}
	})
	return svc
}
```

- [ ] **Step 8: Build to verify compilation**

Run: `go build ./...`
Expected: Build succeeds. If `help.go` still references `.Regex`, those will be fixed in Task 5.

- [ ] **Step 9: Run all tests**

Run: `go test ./...`
Expected: Some tests may fail in `help_test.go` due to remaining `.Regex` references — those will be fixed in Task 5. Focus on `config_test.go` tests passing.

- [ ] **Step 10: Commit**

```bash
git add main.go irc_handlers.go config_test.go
git commit -m "feat: update dispatch to use string-keyed map lookup instead of regex iteration"
```

---

### Task 5: Update help.go — remove all regex references

**Files:**
- Modify: `help.go` (entire file)

This is a large refactor of help.go. Multiple functions reference `Regex` or use regex-based logic. All must be updated.

- [ ] **Step 1: Write failing test for alias display in help table**

Add this test to `help_test.go`:

```go
func TestBuildHelpTextWithAliases(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Aliases: []string{"gpt", "ask"}, Service: "openai", Model: "gpt-4", Description: "general chat"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "!chat")
	assert.Contains(t, text, "!gpt")
	assert.Contains(t, text, "!ask")
	assert.NotContains(t, text, "(regex)")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestBuildHelpTextWithAliases ./... -v`
Expected: FAIL — compilation error (`Aliases` undefined in help.go context) or missing alias display.

- [ ] **Step 3: Update helpEntry struct**

In `help.go`, change the `helpEntry` struct (line 14-19):

Replace:
```go
type helpEntry struct {
	cmd     string
	info    string
	desc    string
	mcpInfo string
}
```

With:
```go
type helpEntry struct {
	cmds    []string // trigger words, canonical first
	info    string
	desc    string
	mcpInfo string
}
```

- [ ] **Step 4: Update formatCmd to return []string**

In `help.go`, replace `formatCmd` (lines 581-587):

Replace:
```go
func formatCmd(trigger, regex, name string) string {
	cmd := trigger + regex
	if regex != name {
		cmd += " (regex)"
	}
	return cmd
}
```

With:
```go
func formatCmds(trigger, name string, aliases []string) []string {
	cmds := []string{trigger + name}
	for _, a := range aliases {
		cmds = append(cmds, trigger+a)
	}
	return cmds
}
```

- [ ] **Step 5: Update formatTable for multi-line cmd column**

In `help.go`, replace `formatTable` (lines 603-635):

Replace the entire function with:

```go
func formatTable(entries []helpEntry) []string {
	if len(entries) == 0 {
		return nil
	}

	maxCmd := 0
	maxInfo := 0
	for _, e := range entries {
		for _, c := range e.cmds {
			cmdLen := utf8.RuneCountInString(c)
			if cmdLen > maxCmd {
				maxCmd = cmdLen
			}
		}
		infoLen := utf8.RuneCountInString(e.info)
		if infoLen > maxInfo {
			maxInfo = infoLen
		}
	}

	var lines []string
	for _, e := range entries {
		infoLen := utf8.RuneCountInString(e.info)
		infoPadding := strings.Repeat(" ", maxInfo-infoLen+2)
		infoCol := ""
		if e.info != "" {
			infoCol = e.info + infoPadding
		} else if maxInfo > 0 {
			infoCol = strings.Repeat(" ", maxInfo+2)
		}

		for i, c := range e.cmds {
			cmdLen := utf8.RuneCountInString(c)
			line := c + strings.Repeat(" ", maxCmd-cmdLen+2)
			if i == 0 {
				line += infoCol + e.desc
			} else {
				if maxInfo > 0 {
					line += strings.Repeat(" ", maxInfo+2)
				}
			}
			lines = append(lines, line)
		}
	}
	return lines
}
```

- [ ] **Step 6: Update buildHelpText**

In `help.go`, edit `buildHelpText` (lines 21-142):

1. Remove line 44 (the regex explanation line):
```go
	lines = append(lines, "Commands marked with (regex) use pattern matching, the trigger can match more than one name.")
```

2. In the tools section (around line 121-125), replace:
```go
		for _, k := range toolKeys {
			c := filteredTools[k]
			entries = append(entries, helpEntry{
				cmd:  formatCmd(trigger, c.Regex, c.Name),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			})
		}
```
With:
```go
		for _, k := range toolKeys {
			c := filteredTools[k]
			entries = append(entries, helpEntry{
				cmds: formatCmds(trigger, c.Name, nil),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			})
		}
```

- [ ] **Step 7: Update buildAIConfigEntry**

In `help.go`, replace `buildAIConfigEntry` (lines 485-492):

Replace:
```go
func buildAIConfigEntry(trigger string, c AIConfig) helpEntry {
	return helpEntry{
		cmd:     formatCmd(trigger, c.Regex, c.Name),
		info:    formatModelInfo(c.Service, c.Model, c.DetectImages),
		desc:    formatDesc(c.Description, false),
		mcpInfo: getMCPServerNames(c.MCPs),
	}
}
```

With:
```go
func buildAIConfigEntry(trigger string, c AIConfig) helpEntry {
	return helpEntry{
		cmds:    formatCmds(trigger, c.Name, c.Aliases),
		info:    formatModelInfo(c.Service, c.Model, c.DetectImages),
		desc:    formatDesc(c.Description, false),
		mcpInfo: getMCPServerNames(c.MCPs),
	}
}
```

- [ ] **Step 8: Update sortedAIConfigEntries**

In `help.go`, replace `sortedAIConfigEntries` (lines 559-579):

Replace the `entries = append(entries, ...)` call inside the loop:
```go
		entries = append(entries, helpEntry{
			cmd:  formatCmd(trigger, c.Regex, c.Name),
			info: formatModelInfo(c.Service, c.Model, c.DetectImages),
			desc: formatDesc(c.Description, false),
		})
```

With:
```go
		entries = append(entries, helpEntry{
			cmds: formatCmds(trigger, c.Name, c.Aliases),
			info: formatModelInfo(c.Service, c.Model, c.DetectImages),
			desc: formatDesc(c.Description, false),
		})
```

- [ ] **Step 9: Update matchesCommand**

In `help.go`, replace `matchesCommand` (lines 474-483):

Replace:
```go
func matchesCommand(cmdName, name, regex string) bool {
	if name == cmdName || regex == cmdName {
		return true
	}
	if regex != name {
		re := regexp.MustCompile("^" + regex + "$")
		return re.MatchString(cmdName)
	}
	return false
}
```

With:
```go
func matchesCommand(cmdName, name string, aliases []string) bool {
	if name == cmdName {
		return true
	}
	for _, a := range aliases {
		if a == cmdName {
			return true
		}
	}
	return false
}
```

- [ ] **Step 10: Update findCommandHelp**

In `help.go`, update `findCommandHelp` (lines 510-557). Replace all calls to `matchesCommand` and all references to `.Regex`:

Replace each occurrence of:
```go
		if matchesCommand(cmdName, c.Name, c.Regex) {
```

With:
```go
		if matchesCommand(cmdName, c.Name, c.Aliases) {
```

(There are 3 occurrences: in the completions loop, chats loop, and tools loop.)

For the tools loop, also replace the helpEntry construction:
```go
			return helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			}, true
```

With:
```go
			return helpEntry{
				cmds: formatCmds(network.Trigger, c.Name, nil),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			}, true
```

- [ ] **Step 11: Update help function to display aliases in per-command help**

In `help.go`, in the `help` function (around line 391), replace:
```go
		lines = append(lines, fmt.Sprintf("Help for %s:", entry.cmd))
```
With:
```go
		lines = append(lines, fmt.Sprintf("Help for %s:", strings.Join(entry.cmds, ", ")))
```

- [ ] **Step 12: Remove regexp import from help.go**

In `help.go`, remove `"regexp"` from the import block (line 6). The file no longer uses regexp directly.

- [ ] **Step 13: Build and run tests**

Run: `go build ./... && go test ./... -v`
Expected: All `help_test.go` tests using `.Regex` will still fail — those are updated in Task 7. Focus on `TestBuildHelpTextWithAliases` passing and compilation succeeding.

- [ ] **Step 14: Commit**

```bash
git add help.go
git commit -m "feat: update help system for aliases, remove regex references"
```

---

### Task 6: Update pastebin help (GFM markdown)

**Files:**
- Modify: `help.go:144-375`

- [ ] **Step 1: Write failing test for pastebin alias display**

Add this test to `help_test.go`:

```go
func TestBuildPastebinHelpTextWithAliases(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Aliases: []string{"gpt", "ask"}, Service: "openai", Model: "gpt-4", Description: "general chat"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "chat<br>gpt<br>ask")
	assert.NotContains(t, text, "| Regex |")
	assert.NotContains(t, text, "✱")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestBuildPastebinHelpTextWithAliases ./... -v`
Expected: FAIL — pastebin still references Regex column.

- [ ] **Step 3: Update pastebinEntry struct**

In `help.go`, replace the `pastebinEntry` struct (lines 144-151):

Replace:
```go
type pastebinEntry struct {
	cmd          string
	regex        bool
	service      string
	model        string
	detectImages bool
	desc         string
}
```

With:
```go
type pastebinEntry struct {
	cmds         []string // trigger words, canonical first
	service      string
	model        string
	detectImages bool
	desc         string
}
```

- [ ] **Step 4: Update sortedPastebinEntries**

In `help.go`, replace `sortedPastebinEntries` (lines 319-341):

Replace the entry construction:
```go
		entries = append(entries, pastebinEntry{
			cmd:          escapeMDPipe(trigger + c.Regex),
			regex:        c.Regex != c.Name,
			service:      c.Service,
			model:        c.Model,
			detectImages: c.DetectImages,
			desc:         formatDesc(c.Description, false),
		})
```

With:
```go
		cmds := []string{trigger + c.Name}
		for _, a := range c.Aliases {
			cmds = append(cmds, trigger+a)
		}
		entries = append(entries, pastebinEntry{
			cmds:         cmds,
			service:      c.Service,
			model:        c.Model,
			detectImages: c.DetectImages,
			desc:         formatDesc(c.Description, false),
		})
```

- [ ] **Step 5: Update writeGFMCmdTable**

In `help.go`, replace `writeGFMCmdTable` (lines 343-360):

Replace the entire function with:

```go
func writeGFMCmdTable(b *strings.Builder, header string, entries []pastebinEntry) {
	b.WriteString(header)
	b.WriteString("\n\n")
	b.WriteString("| Command | Service | Model | Media | Description |\n")
	b.WriteString("|---------|---------|-------|-------|-------------|\n")
	for _, e := range entries {
		cmdParts := make([]string, len(e.cmds))
		for i, c := range e.cmds {
			cmdParts[i] = "`" + escapeMDPipe(c) + "`"
		}
		cmd := strings.Join(cmdParts, "<br>")
		media := ""
		if e.detectImages {
			media = "🖼️"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", cmd, e.service, e.model, media, escapeDescPipe(e.desc))
	}
}
```

- [ ] **Step 6: Update writeGFMToolTable**

In `help.go`, replace `writeGFMToolTable` (lines 362-375):

Replace the entire function with:

```go
func writeGFMToolTable(b *strings.Builder, header string, entries []pastebinEntry) {
	b.WriteString(header)
	b.WriteString("\n\n")
	b.WriteString("| Command | MCP | Tool | Description |\n")
	b.WriteString("|---------|-----|------|-------------|\n")
	for _, e := range entries {
		cmd := "`" + escapeMDPipe(e.cmds[0]) + "`"
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", cmd, e.service, e.model, escapeDescPipe(e.desc))
	}
}
```

- [ ] **Step 7: Update buildPastebinHelpText tools section**

In `help.go`, in `buildPastebinHelpText` (around lines 238-247), replace the tools entry construction:

Replace:
```go
			entries = append(entries, pastebinEntry{
				cmd:     escapeMDPipe(trigger + c.Regex),
				regex:   c.Regex != c.Name,
				service: c.MCP,
				model:   c.Tool,
				desc:    formatDesc(c.Description, false),
			})
```

With:
```go
			entries = append(entries, pastebinEntry{
				cmds:    []string{trigger + c.Name},
				service: c.MCP,
				model:   c.Tool,
				desc:    formatDesc(c.Description, false),
			})
```

- [ ] **Step 8: Run test to verify it passes**

Run: `go test -run TestBuildPastebinHelpTextWithAliases ./... -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add help.go help_test.go
git commit -m "feat: update pastebin help for aliases, drop Regex column"
```

---

### Task 7: Update disabled_commands validation for aliases

**Files:**
- Modify: `config.go:464-487`

- [ ] **Step 1: Write failing test for alias rejection in disabled_commands**

Add this test to `config_test.go`:

```go
func TestLoadConfigDirDisabledCommandsRejectsAlias(t *testing.T) {
	mainTOML := `
[networks.testnet]
nick = "bot"
disabled_commands = ["gpt"]
[[networks.testnet.servers]]
host = "irc.example.com"
`
	servicesTOML := `
[svc]
api_base = "http://localhost"
api_key = "test"
`
	chatsTOML := `
[chat]
service = "svc"
aliases = ["gpt", "ask"]
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"chats.toml":    chatsTOML,
		"services.toml": servicesTOML,
	})
	defer os.RemoveAll(dir)

	_, err := loadConfigDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disabled_commands")
	assert.Contains(t, err.Error(), "gpt")
	assert.Contains(t, err.Error(), "alias")
	assert.Contains(t, err.Error(), "chat")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestLoadConfigDirDisabledCommandsRejectsAlias ./... -v`
Expected: FAIL — validation doesn't check aliases yet.

- [ ] **Step 3: Update validateNetworkDisabledCommands**

In `config.go`, replace `validateNetworkDisabledCommands` (lines 464-487):

Replace the entire function with:

```go
func validateNetworkDisabledCommands(config *Config) error {
	known := make(map[string]bool)
	// Map of alias -> canonical name for error messages
	aliasToCanonical := make(map[string]string)

	for _, name := range builtinCommandNames {
		known[name] = true
	}
	for name, c := range config.Commands.Completions {
		known[name] = true
		for _, alias := range c.Aliases {
			aliasToCanonical[alias] = name
		}
	}
	for name, c := range config.Commands.Chats {
		known[name] = true
		for _, alias := range c.Aliases {
			aliasToCanonical[alias] = name
		}
	}
	for name := range config.Commands.Tools {
		known[name] = true
	}

	for netName, network := range config.Networks {
		for _, cmd := range network.DisabledCommands {
			if canonical, isAlias := aliasToCanonical[cmd]; isAlias {
				return fmt.Errorf("networks.%s disabled_commands: %q is an alias of %q, use the canonical name %q instead", netName, cmd, canonical, canonical)
			}
			if !known[cmd] {
				return fmt.Errorf("networks.%s disabled_commands: %q is not a known command", netName, cmd)
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestLoadConfigDirDisabledCommandsRejectsAlias ./... -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: validate disabled_commands rejects aliases with helpful error"
```

---

### Task 8: Update config TOML files (reference docs and live config)

**Files:**
- Modify: `config/completions.toml`
- Modify: `config/chats.toml`
- Modify: `config/tools.toml`

- [ ] **Step 1: Update completions.toml reference docs**

In `config/completions.toml`:

1. Line 2, replace:
```
# Each section name becomes the command trigger (overridable with `regex`).
```
With:
```
# Each section name becomes the command trigger. Use `aliases` for additional triggers.
```

2. Line 7, replace:
```
#   regex                (string, default: section name) Regex for command trigger (no ^$ anchors)
```
With:
```
#   aliases               (string array, default: [])    Additional trigger names for this command
```

3. Lines 27-28 (in the example block), replace:
```
# regex = "example"
```
With:
```
# aliases = ["alt-name", "another-name"]
```

4. Lines 43-49, replace the live `[dave]` section:
```
[dave]
service = "openai"
model = "gpt-3.5-turbo-instruct"
#optionally you may override the regex for the command name
#in this case both -dave and -dav would trigger this command
#note this regex is used to construct a larger regex so ^$ shouldn't be used
regex = "dave?"
```

With:
```
[dave]
service = "openai"
model = "gpt-3.5-turbo-instruct"
aliases = ["dav", "davee"]
```

- [ ] **Step 2: Update chats.toml reference docs**

In `config/chats.toml`:

1. Line 13, replace:
```
#   regex                (string, default: section name)  Regex for command trigger (no ^$ anchors)
```
With:
```
#   aliases               (string array, default: [])     Additional trigger names for this command
```

2. Line 61 (in the example block), replace:
```
# regex = "chat"
```
With:
```
# aliases = ["alt-name"]
```

- [ ] **Step 3: Update tools.toml reference docs**

In `config/tools.toml`:

1. Line 2, replace:
```
# Each section name becomes the command trigger (overridable with `regex`).
```
With:
```
# Each section name becomes the command trigger.
```

2. Line 12, remove:
```
#   regex        (string, default: section name)  Regex for command trigger (no ^$ anchors)
```

3. Line 39 (in the example block), remove:
```
# regex = "example"
```

- [ ] **Step 4: Build and verify config loads**

Run: `go build ./... && ./dave test 2>&1 | head -20` (or whatever smoke test verifies the test config loads)
Expected: No config parsing errors.

- [ ] **Step 5: Commit**

```bash
git add config/completions.toml config/chats.toml config/tools.toml
git commit -m "docs: update config TOML reference docs for aliases, remove regex"
```

---

### Task 9: Update existing tests (remove all .Regex references)

**Files:**
- Modify: `config_test.go`
- Modify: `help_test.go`

- [ ] **Step 1: Update TestChatCommandNameSetting**

In `config_test.go` (around line 379), update the test:

1. In the `chatsTOML`, replace:
```
[chat2]
service = "test"
regex = "custom"
```
With:
```
[chat2]
service = "test"
aliases = ["custom"]
```

2. In the test struct and assertions (around line 410-428), remove `wantRegex` field and its assertion:
```go
	tests := []struct {
		name      string
		wantName  string
		wantRegex string
		hasTmpl   bool
	}{
		{"chat1", "chat1", "chat1", false},
		{"chat2", "chat2", "custom", false},
		{"chat3", "chat3", "chat3", true},
		{"chat4", "chat4", "chat4", true},
	}
```

Replace with:
```go
	tests := []struct {
		name        string
		wantName    string
		wantAliases []string
		hasTmpl     bool
	}{
		{"chat1", "chat1", nil, false},
		{"chat2", "chat2", []string{"custom"}, false},
		{"chat3", "chat3", nil, true},
		{"chat4", "chat4", nil, true},
	}
```

And replace the assertion:
```go
			assert.Equal(t, tt.wantRegex, cfg.Regex, "Regex")
```
With:
```go
			assert.Equal(t, tt.wantAliases, cfg.Aliases, "Aliases")
```

- [ ] **Step 2: Replace TestRegisterCommandsLocked_InvalidRegex with collision tests**

In `config_test.go`, replace the entire `TestRegisterCommandsLocked_InvalidRegex` function (lines 1490-1550) with:

```go
func TestRegisterCommandsLocked_TriggerCollisions(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	t.Run("canonical-alias collision between chats returns error", func(t *testing.T) {
		cmds := Commands{
			Chats: map[string]AIConfig{
				"chat1": {Name: "chat1", Aliases: []string{"shared"}},
				"chat2": {Name: "chat2", Aliases: []string{"shared"}},
			},
		}
		err := registerCommandsLocked(cmds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shared")
		assert.Contains(t, err.Error(), "conflicts")
	})

	t.Run("canonical name collision returns error", func(t *testing.T) {
		cmds := Commands{
			Completions: map[string]AIConfig{
				"shared": {Name: "shared"},
			},
			Chats: map[string]AIConfig{
				"shared": {Name: "shared"},
			},
		}
		err := registerCommandsLocked(cmds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shared")
	})

	t.Run("tool name collides with chat alias returns error", func(t *testing.T) {
		cmds := Commands{
			Chats: map[string]AIConfig{
				"chat": {Name: "chat", Aliases: []string{"tool1"}},
			},
			Tools: map[string]MCPCommandConfig{
				"tool1": {Name: "tool1", MCP: "mcp", Tool: "test"},
			},
		}
		err := registerCommandsLocked(cmds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tool1")
	})

	t.Run("valid commands with no collisions succeed", func(t *testing.T) {
		cmds := Commands{
			Completions: map[string]AIConfig{
				"comp1": {Name: "comp1"},
			},
			Chats: map[string]AIConfig{
				"chat1": {Name: "chat1", Aliases: []string{"gpt"}},
			},
		}
		err := registerCommandsLocked(cmds)
		require.NoError(t, err)
	})

	t.Run("error does not mutate configCmds", func(t *testing.T) {
		orig := configCmds
		cmds := Commands{
			Chats: map[string]AIConfig{
				"a": {Name: "a", Aliases: []string{"dup"}},
				"b": {Name: "b", Aliases: []string{"dup"}},
			},
		}
		_ = registerCommandsLocked(cmds)
		assert.Equal(t, orig, configCmds, "configCmds should not be mutated on error")
	})
}
```

- [ ] **Step 3: Update TestRegisterCommandsLocked_PopulatesConfigCmdNames**

In `config_test.go` (lines 1606-1637), replace the test:

Replace:
```go
func TestRegisterCommandsLocked_PopulatesConfigCmdNames(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	cmds := Commands{
		Completions: map[string]AIConfig{
			"comp1": {Regex: `comp1`},
		},
		Chats: map[string]AIConfig{
			"chat1": {Regex: `chat1`},
		},
		Tools: map[string]MCPCommandConfig{
			"tool1": {Regex: `tool1`, MCP: "mcp", Tool: "test"},
		},
	}
	err := registerCommandsLocked(cmds)
	require.NoError(t, err)

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()

	names := make(map[string]bool)
	for _, name := range configCmdNames {
		names[name] = true
	}
	assert.True(t, names["comp1"], "configCmdNames should contain comp1")
	assert.True(t, names["chat1"], "configCmdNames should contain chat1")
	assert.True(t, names["tool1"], "configCmdNames should contain tool1")
	assert.Len(t, configCmdNames, 3)
}
```

With:
```go
func TestRegisterCommandsLocked_PopulatesConfigCmdNames(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	cmds := Commands{
		Completions: map[string]AIConfig{
			"comp1": {Name: "comp1"},
		},
		Chats: map[string]AIConfig{
			"chat1": {Name: "chat1"},
		},
		Tools: map[string]MCPCommandConfig{
			"tool1": {Name: "tool1", MCP: "mcp", Tool: "test"},
		},
	}
	err := registerCommandsLocked(cmds)
	require.NoError(t, err)

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()

	assert.Equal(t, "comp1", configCmdNames["comp1"])
	assert.Equal(t, "chat1", configCmdNames["chat1"])
	assert.Equal(t, "tool1", configCmdNames["tool1"])
	assert.Len(t, configCmdNames, 3)
}
```

- [ ] **Step 4: Update TestHandleTrigger_DisabledCommandReturnsEarly**

In `config_test.go` (around line 1639), replace:
```go
		"mychat": {Regex: `mychat`, Service: "svc", Model: "m"},
```
With:
```go
		"mychat": {Name: "mychat", Service: "svc", Model: "m"},
```

- [ ] **Step 5: Update remaining disabled_commands tests**

In `config_test.go`, update `TestLoadConfigDirDisabledCommandsValidation` and `TestLoadReloadableDirDisabledCommands` (around lines 1707-1850). Replace all `regex = "..."` lines in the embedded TOML strings with nothing (remove them). For example:

Replace:
```toml
[mychat]
regex = "mychat"
service = "svc"
model = "m"
```
With:
```toml
[mychat]
service = "svc"
model = "m"
```

Do this for all embedded TOML in these tests. There are multiple instances.

- [ ] **Step 6: Update help_test.go — remove all Regex references**

In `help_test.go`, do a global replacement of all `Regex: "..."` field initializations. For each test struct:

1. `TestBuildHelpTextWithCompletions` (line 219): Remove `Regex: "chat"` — the `Name: "chat"` is sufficient.
2. `TestBuildHelpTextWithChats` (line 229): Remove `Regex: "ask"`.
3. `TestBuildPastebinHelpTextWithChats` (line 340): Remove `Regex: "ask"`.
4. `TestBuildPastebinHelpTextWithCompletions` (line 356): Remove `Regex: "complete"`.
5. `TestBuildPastebinHelpTextWithTools` (line 366): Remove `Regex: "img"`.
6. `TestBuildPastebinHelpTextChatBeforeCompletions` (lines 387, 390): Remove both `Regex:` fields.
7. `TestBuildPastebinHelpTextGFMTables` (line 404): Remove `Regex: "chat"`.
8. `TestBuildPastebinHelpTextRegexMarker` (line 419-433): **Delete this entire test** — regex markers no longer exist.
9. `TestBuildPastebinHelpTextPipeEscaped` (line 437): Remove `Regex: "img|image"`. Change the test to not expect pipe escaping in the command column (tools don't get regex anymore). Replace the test with:
```go
func TestBuildPastebinHelpTextPipeEscaped(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"img": {Name: "img", Service: "openai", Model: "gpt-4", Description: "images"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "`!img`")
}
```
10. `TestBuildPastebinHelpTextDetectImagesCol` (line 448): Remove `Regex: "chat"`. Update the expected line to match the new table format (no Regex column):
Replace:
```go
	assert.Contains(t, text, "| `!chat` |  | openai | gpt-4o | 🖼️ | vision chat |")
```
With:
```go
	assert.Contains(t, text, "| `!chat` | openai | gpt-4o | 🖼️ | vision chat |")
```
11. `TestBuildPastebinHelpTextNoRegexExplanation` (line 477): Keep this test as-is — it asserts regex explanation is NOT present, which is still correct.
12. `TestBuildPastebinHelpTextDescPipeEscaped` (line 484): Remove `Regex: "chat"`.

- [ ] **Step 7: Update TestBuildPastebinHelpTextGFMTables expected output**

In `help_test.go`, `TestBuildPastebinHelpTextGFMTables` (line 402-410):

Replace:
```go
	assert.Contains(t, text, "| Command | Regex | Service | Model | Media | Description |")
	assert.Contains(t, text, "| `!chat` |  | openai | gpt-4 |  | general chat |")
```
With:
```go
	assert.Contains(t, text, "| Command | Service | Model | Media | Description |")
	assert.Contains(t, text, "| `!chat` | openai | gpt-4 |  | general chat |")
```

- [ ] **Step 8: Run all tests**

Run: `go test ./... -v 2>&1 | tail -30`
Expected: All tests pass. If any test still references `.Regex`, fix it.

- [ ] **Step 9: Commit**

```bash
git add config_test.go help_test.go
git commit -m "test: update all tests for aliases, remove regex references"
```

---

### Task 10: Add new tests for alias behaviors

**Files:**
- Modify: `help_test.go`
- Modify: `config_test.go`

- [ ] **Step 1: Add test for help lookup by alias**

Add to `help_test.go`:

```go
func TestFindCommandHelpByAlias(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Aliases: []string{"gpt", "ask"}, Service: "openai", Model: "gpt-4", Description: "general chat"},
	}
	defer func() { config.Commands.Chats = nil }()

	entry, found := findCommandHelp(Network{}, "gpt")
	require.True(t, found, "should find chat via alias 'gpt'")
	assert.Contains(t, entry.cmds[0], "chat")

	entry, found = findCommandHelp(Network{}, "ask")
	require.True(t, found, "should find chat via alias 'ask'")
	assert.Contains(t, entry.cmds[0], "chat")

	entry, found = findCommandHelp(Network{}, "nonexistent")
	assert.False(t, found)
}
```

- [ ] **Step 2: Add test for disabled-by-canonical blocks aliases**

Add to `config_test.go`:

```go
func TestDisabledCanonicalBlocksAliases(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	cmds := Commands{
		Chats: map[string]AIConfig{
			"chat": {Name: "chat", Aliases: []string{"gpt", "ask"}, Service: "svc", Model: "m"},
		},
	}
	err := registerCommandsLocked(cmds)
	require.NoError(t, err)

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()

	network := Network{DisabledCommands: []string{"chat"}}

	// All triggers of chat should be disabled
	assert.True(t, isNetworkCommandDisabled(network, configCmdNames["chat"]))
	assert.True(t, isNetworkCommandDisabled(network, configCmdNames["gpt"]))
	assert.True(t, isNetworkCommandDisabled(network, configCmdNames["ask"]))
}
```

- [ ] **Step 3: Add test for splitFirstWord**

Add to `config_test.go` (or a new test in an appropriate file):

```go
func TestSplitFirstWord(t *testing.T) {
	tests := []struct {
		input    string
		word     string
		rest     string
		hasArgs  bool
	}{
		{"chat hello world", "chat", "hello world", true},
		{"chat", "chat", "", false},
		{"gpt what is ai", "gpt", "what is ai", true},
		{"", "", "", false},
		{"  leading", "", " leading", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			w, r, h := splitFirstWord(tt.input)
			assert.Equal(t, tt.word, w, "word")
			assert.Equal(t, tt.rest, r, "rest")
			assert.Equal(t, tt.hasArgs, h, "hasArgs")
		})
	}
}
```

- [ ] **Step 4: Run all tests**

Run: `go test ./... -v`
Expected: All tests pass.

- [ ] **Step 5: Commit**

```bash
git add help_test.go config_test.go
git commit -m "test: add alias dispatch, help lookup, disabled, and splitFirstWord tests"
```

---

### Task 11: Final cleanup and verification

- [ ] **Step 1: Remove unused regexp import from irc_handlers.go**

Check if `regexp` is still imported in `irc_handlers.go`. If the only use was for the old dispatch (which now uses string maps), remove the import. Builtins still use regex, so it may still be needed. Verify:

Run: `go vet ./...`
Fix any unused import errors.

- [ ] **Step 2: Run go fmt**

Run: `go fmt ./...`

- [ ] **Step 3: Run go vet**

Run: `go vet ./...`
Expected: No issues.

- [ ] **Step 4: Run full test suite**

Run: `go test ./... -v 2>&1 | tail -50`
Expected: All tests pass.

- [ ] **Step 5: Build the binary**

Run: `go build -o dave .`
Expected: Builds successfully.

- [ ] **Step 6: Commit any remaining changes**

```bash
git add -A
git commit -m "chore: cleanup unused imports, fmt, vet"
```

---

## Self-Review Notes

**Spec coverage:**
- [x] Remove Regex from AIConfig — Task 1
- [x] Add Aliases to AIConfig — Task 1
- [x] Remove Regex from MCPCommandConfig — Task 2
- [x] Dispatch map[string]CmdFunc — Task 3
- [x] splitFirstWord helper — Task 4
- [x] Arg compatibility check (configCmdTakesArgs) — Task 3, Task 4
- [x] Collision validation — Task 3
- [x] Update handleTrigger — Task 4
- [x] Update getServiceForConfigCmd — Task 4
- [x] IRC help table multi-line aliases — Task 5
- [x] findCommandHelp with aliases — Task 5
- [x] matchesCommand update — Task 5
- [x] Pastebin help drop Regex column, <br> separator — Task 6
- [x] disabled_commands rejects aliases — Task 7
- [x] Config TOML reference docs — Task 8
- [x] Tests updated — Task 9
- [x] New alias tests — Task 10

**Type consistency:** `configCmdNames`, `chatCmds`, `rateExemptCmds` all changed from `map[*regexp.Regexp]` to `map[string]` consistently. `configCmdTakesArgs` is `map[string]bool`. `cmdMatch.trigger` is `string`.

**No placeholders:** All code blocks contain actual implementation code.
