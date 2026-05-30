# Reload Report Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accumulate reload/startup stats and errors into a report that prints a two-part summary to the TUI log view, so errors are always visible at the end.

**Architecture:** A `ReloadReport` struct accumulates success/error entries. Only `initMCPClients` and `reloadMCPClients` get the report parameter (they have per-server success/fail info). Everything else is recorded by the caller from the loaded config object. The report prints to `logView` at the end of startup and reload.

**Tech Stack:** Go, tview color tags, existing project patterns

---

### Task 1: Create ReloadReport struct and Print method

**Files:**
- Create: `reload_report.go`
- Create: `reload_report_test.go`

- [ ] **Step 1: Write failing tests for ReloadReport**

```go
package main

import (
	"strings"
	"testing"

	"github.com/rivo/tview"
	"github.com/stretchr/testify/assert"
)

func TestReloadReport_PrintNoErrors(t *testing.T) {
	r := NewReloadReport("reload")
	r.AddSuccess("config: 3 services, 5 chats")
	r.AddSuccess("ignores: 10 patterns")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── reload ok ──")
	assert.Contains(t, got, "config: 3 services, 5 chats")
	assert.Contains(t, got, "ignores: 10 patterns")
	assert.NotContains(t, got, "── errors ──")
}

func TestReloadReport_PrintWithErrors(t *testing.T) {
	r := NewReloadReport("startup")
	r.AddSuccess("config: 2 services")
	r.AddError("MCP brave-mcp: connection refused")
	r.AddError("MCP fetch-mcp: timeout")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── startup completed with 2 errors ──")
	assert.Contains(t, got, "config: 2 services")
	assert.Contains(t, got, "── 2 errors ──")
	assert.Contains(t, got, "MCP brave-mcp: connection refused")
	assert.Contains(t, got, "MCP fetch-mcp: timeout")
}

func TestReloadReport_PrintEmpty(t *testing.T) {
	r := NewReloadReport("reload")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── reload ok ──")
	assert.NotContains(t, got, "── errors ──")
}

func TestReloadReport_HasErrors(t *testing.T) {
	r := NewReloadReport("reload")
	assert.False(t, r.HasErrors())
	r.AddError("something broke")
	assert.True(t, r.HasErrors())
}

func TestReloadReport_PrintSingleError(t *testing.T) {
	r := NewReloadReport("reload")
	r.AddSuccess("config: ok")
	r.AddError("MCP x: failed")

	view := tview.NewTextView()
	r.Print(view)
	got := view.GetText(true)

	assert.Contains(t, got, "── reload completed with 1 error ──")
	assert.Contains(t, got, "── 1 error ──")
	assert.NotContains(t, got, "1 errors")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestReloadReport -v`
Expected: FAIL (NewReloadReport undefined)

- [ ] **Step 3: Write ReloadReport implementation**

```go
package main

import (
	"fmt"

	"github.com/rivo/tview"
)

type ReloadReport struct {
	label     string
	successes []string
	errors    []string
}

func NewReloadReport(label string) *ReloadReport {
	return &ReloadReport{label: label}
}

func (r *ReloadReport) AddSuccess(msg string) {
	r.successes = append(r.successes, msg)
}

func (r *ReloadReport) AddError(msg string) {
	r.errors = append(r.errors, msg)
}

func (r *ReloadReport) HasErrors() bool {
	return len(r.errors) > 0
}

func (r *ReloadReport) Print(view *tview.TextView) {
	if len(r.errors) == 0 {
		fmt.Fprintf(view, "[green]── %s ok ──[white]\n", r.label)
	} else {
		errNoun := "error"
		if len(r.errors) != 1 {
			errNoun = "errors"
		}
		fmt.Fprintf(view, "[yellow]── %s completed with %d %s ──[white]\n",
			r.label, len(r.errors), errNoun)
	}
	for _, s := range r.successes {
		fmt.Fprintf(view, "  %s\n", s)
	}
	if len(r.errors) > 0 {
		errNoun := "error"
		if len(r.errors) != 1 {
			errNoun = "errors"
		}
		fmt.Fprintf(view, "[red]── %d %s ──[white]\n", len(r.errors), errNoun)
		for _, e := range r.errors {
			fmt.Fprintf(view, "[red]  %s[white]\n", e)
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestReloadReport -v`
Expected: PASS (all 5 tests)

- [ ] **Step 5: Run go fmt and go vet**

Run: `go fmt ./... && go vet ./...`
Expected: no output (clean)

- [ ] **Step 6: Commit**

```bash
git add reload_report.go reload_report_test.go
git commit -m "feat: add ReloadReport accumulator for startup/reload summaries"
```

---

### Task 2: Add getIgnoreCount helper

**Files:**
- Modify: `main.go:128-129`

- [ ] **Step 1: Write failing test**

```go
func TestGetIgnoreCount(t *testing.T) {
	ignoreMu.Lock()
	ignorePatterns = []string{"a", "b", "c"}
	ignoreMu.Unlock()
	assert.Equal(t, 3, getIgnoreCount())

	ignoreMu.Lock()
	ignorePatterns = nil
	ignoreMu.Unlock()
	assert.Equal(t, 0, getIgnoreCount())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestGetIgnoreCount -v`
Expected: FAIL (getIgnoreCount undefined)

- [ ] **Step 3: Add getIgnoreCount to main.go after the ignorePatterns/ignoreMu declarations (after line 129)**

```go
func getIgnoreCount() int {
	ignoreMu.RLock()
	defer ignoreMu.RUnlock()
	return len(ignorePatterns)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestGetIgnoreCount -v`
Expected: PASS

- [ ] **Step 5: Run go fmt and go vet**

Run: `go fmt ./... && go vet ./...`
Expected: no output (clean)

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "feat: add getIgnoreCount helper for reload report"
```

---

### Task 3: Thread ReloadReport through initMCPClients and reloadMCPClients

**Files:**
- Modify: `mcpClient.go:158-189` (initMCPClients)
- Modify: `mcpClient.go:215-219` (reloadMCPClients)

- [ ] **Step 1: Modify initMCPClients to accept and populate a ReloadReport**

Change `mcpClient.go:158` signature and body. The existing logger calls stay — the report adds summary entries.

Replace the function at line 158:

```go
func initMCPClients(r *ReloadReport) {
	mcpServersMu.Lock()
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)
	mcpServersMu.Unlock()

	if len(config.MCPs) == 0 {
		return
	}

	for name, mcpCfg := range config.MCPs {
		logger.Info("connecting MCP server", "name", name, "transport", mcpCfg.Transport)

		srv, err := connectMCPServer(name, mcpCfg)
		if err != nil {
			logger.Error("failed to connect MCP server", "name", name, "error", err)
			if r != nil {
				r.AddError(fmt.Sprintf("MCP %s: %s", name, err))
			}
			continue
		}

		logger.Info("MCP server connected", "name", name,
			"tools", len(srv.Tools),
			"resources", len(srv.Resources),
			"prompts", len(srv.Prompts))

		if r != nil {
			r.AddSuccess(fmt.Sprintf("MCP %s: connected (%d tools)", name, len(srv.Tools)))
		}

		mcpServersMu.Lock()
		mcpServers[name] = srv
		for _, tool := range srv.Tools {
			mcpToolToServer[tool.Name] = name
		}
		mcpServersMu.Unlock()
	}
}
```

Key changes:
- Added `r *ReloadReport` parameter (nil-safe so tests and other callers still work)
- On connect failure: `r.AddError(...)` 
- On connect success: `r.AddSuccess(...)`

- [ ] **Step 2: Modify reloadMCPClients to pass report through**

Replace `mcpClient.go:215-219`:

```go
func reloadMCPClients(newMCPs map[string]MCPConfig, r *ReloadReport) {
	closeAndClearMCPClients()
	config.MCPs = newMCPs
	initMCPClients(r)
}
```

- [ ] **Step 3: Run go fmt and go vet**

Run: `go fmt ./... && go vet ./...`
Expected: compile errors from callers not yet updated (main.go:487, main.go:431). These will be fixed in Task 4.

- [ ] **Step 4: Commit**

```bash
git add mcpClient.go
git commit -m "feat: thread ReloadReport through initMCPClients and reloadMCPClients"
```

---

### Task 4: Wire ReloadReport into reloadAll and tuiCmdReload

**Files:**
- Modify: `main.go:409-441` (reloadAll)
- Modify: `tui_commands.go:88-93` (tuiCmdReload success/failure messages)

- [ ] **Step 1: Update reloadAll to create report, populate it, and print it**

Replace `main.go:409-441`:

```go
func reloadAll() error {
	r := NewReloadReport("reload")

	commandsMutex.Lock()
	defer commandsMutex.Unlock()
	configMu.Lock()
	defer configMu.Unlock()
	if err := loadReloadableDir(configDir, &config); err != nil {
		r.AddError(fmt.Sprintf("config: %s", err))
		r.Print(logView)
		return err
	}
	r.AddSuccess(fmt.Sprintf("config: %d services, %d chats, %d completions, %d tools, %d template vars, notices ok",
		len(config.Services), len(config.Commands.Chats), len(config.Commands.Completions),
		len(config.Commands.Tools), len(config.TemplateVars)))

	for name, bot := range snapshotBots() {
		bot.mu.Lock()
		cm := bot.casemapping
		bot.mu.Unlock()
		if net, ok := config.Networks[name]; ok {
			net.Casemapping = cm
			config.Networks[name] = net
		}
	}
	if apiLogger != nil {
		apiLogger.CloseAll()
	}
	initAPILogger(config, configDir)
	initIncidentLogger(config)
	reloadMCPClients(config.MCPs, r)
	if err := registerCommandsLocked(config.Commands); err != nil {
		r.AddError(fmt.Sprintf("commands: %s", err))
		r.Print(logView)
		return err
	}
	if queueMgr != nil {
		queueMgr.UpdateServiceLimits(config.Services)
		queueMgr.UpdateNotices(config.Notices)
	}
	loadIgnores(filepath.Join(configDir, "ignores.txt"))
	r.AddSuccess(fmt.Sprintf("ignores: %d patterns", getIgnoreCount()))

	r.Print(logView)
	return nil
}
```

- [ ] **Step 2: Remove old success/failure messages from tuiCmdReload**

In `tui_commands.go`, replace lines 88-93 (the `else` branch after `/reload` with no args):

```go
	} else {
		reloadAll()
	}
```

The report handles its own output now. `reloadAll` still returns an error for logging purposes, but `tuiCmdReload` no longer prints its own message. (If you want to know if it failed, the error is in the report.)

- [ ] **Step 3: Run go fmt and go vet**

Run: `go fmt ./... && go vet ./...`
Expected: no errors (startup call at main.go:487 `initMCPClients()` needs updating — that's Task 5)

Wait — actually `initMCPClients()` is now `initMCPClients(r *ReloadReport)`. The call at main.go:487 will fail. Fix it now with a nil argument temporarily:

Change `main.go:487` from:
```go
	initMCPClients()
```
to:
```go
	initMCPClients(nil)
```

This will be replaced properly in Task 5.

Run: `go fmt ./... && go vet ./...`
Expected: clean

- [ ] **Step 4: Run full test suite**

Run: `go test ./...`
Expected: all tests pass

- [ ] **Step 5: Commit**

```bash
git add main.go tui_commands.go
git commit -m "feat: wire ReloadReport into reloadAll, remove old tuiCmdReload messages"
```

---

### Task 5: Wire ReloadReport into startup path in main()

**Files:**
- Modify: `main.go:443-500` (startup sequence)

- [ ] **Step 1: Update main() to create and print a startup report**

Replace the startup sequence starting at line 443. The key changes are:
1. Create report after config load, record config stats
2. Pass report to `initMCPClients(r)` instead of `initMCPClients(nil)`
3. Record ignore count after `loadIgnores`
4. Print report to logView after all init

In `main()`, after line 450 (`config = loadConfigDirOrDie(configDir)`), add:

```go
	r := NewReloadReport("startup")
	r.AddSuccess(fmt.Sprintf("config: %d networks, %d services, %d chats, %d completions, %d tools, %d MCPs, %d template vars",
		len(config.Networks), len(config.Services), len(config.Commands.Chats),
		len(config.Commands.Completions), len(config.Commands.Tools),
		len(config.MCPs), len(config.TemplateVars)))
```

Change line 487 from `initMCPClients(nil)` to:
```go
	initMCPClients(r)
```

After line 499 (`loadIgnores(ignorePath)`), add:
```go
	r.AddSuccess(fmt.Sprintf("ignores: %d patterns", getIgnoreCount()))
```

After line 500 (`watchIgnores(ignorePath)`), add:
```go
	r.Print(logView)
```

Note: `r.Print(logView)` is safe here because `initTUI()` has already been called and `logView` is set up.

- [ ] **Step 2: Run go fmt and go vet**

Run: `go fmt ./... && go vet ./...`
Expected: clean

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`
Expected: all tests pass

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "feat: add startup report summary in main()"
```

---

### Task 6: Verify end-to-end and run all tests

**Files:** None (verification only)

- [ ] **Step 1: Run go fmt**

Run: `go fmt ./...`
Expected: no output (clean)

- [ ] **Step 2: Run go vet**

Run: `go vet ./...`
Expected: no output (clean)

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`
Expected: all tests pass

- [ ] **Step 4: Build binary to verify compilation**

Run: `go build -o /tmp/dave-test .`
Expected: builds successfully, no errors
