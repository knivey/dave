# Reload Report Design

## Problem

When `/reload` runs, individual log lines (MCP connections, config loading) scroll past in the TUI and get pushed off-screen by subsequent output. There is no summary of what succeeded and what failed, making it easy to miss errors.

## Solution

Introduce a `ReloadReport` accumulator struct that collects stats and errors during both startup and reload, then prints a two-part summary to the TUI log view at the end.

## New File: `reload_report.go`

```go
type ReloadReport struct {
    label     string      // "startup" or "reload"
    successes []string
    errors    []string
}

func NewReloadReport(label string) *ReloadReport
func (r *ReloadReport) AddSuccess(msg string)
func (r *ReloadReport) AddError(msg string)
func (r *ReloadReport) HasErrors() bool
func (r *ReloadReport) Print(view *tview.TextView)
```

### Print Format

**No errors:**
```
── reload ok ──
  MCPs: 2/3 connected (img-mcp: 8 tools, yt-mcp: 2 tools)
  Config: 4 services, 5 chats, 3 completions, 2 tools, 3 template vars, notices ok
  Ignores: 15 patterns
```
(green header, white body lines)

**With errors — success header turns yellow:**
```
── reload completed with 1 error ──
  MCPs: 2/3 connected (img-mcp: 8 tools, yt-mcp: 2 tools)
  Config: 4 services, 5 chats, 3 completions, 2 tools, 3 template vars, notices ok
  Ignores: 15 patterns
── 1 error ──
  MCP brave-mcp: connection refused
```
(yellow header for summary, red header for errors, red body for error lines)

For startup the label is "startup" instead of "reload".

## Function Changes

### `initMCPClients(r *ReloadReport)` — `mcpClient.go`

Add `*ReloadReport` parameter. For each MCP server:
- On success: `r.AddSuccess(fmt.Sprintf("MCP %s: connected (%d tools)", name, len(srv.Tools)))`
- On failure: `r.AddError(fmt.Sprintf("MCP %s: %s", name, err))`

Keeps existing `logger.Info`/`logger.Error` calls (they go to the log file via the pipe).

### `reloadMCPClients(newMCPs map[string]MCPConfig, r *ReloadReport)` — `mcpClient.go`

Add `*ReloadReport` parameter, pass through to `initMCPClients(r)`.

### `reloadAll()` — `main.go`

```go
func reloadAll() error {
    r := NewReloadReport("reload")
    
    // ... existing lock acquisition ...
    if err := loadReloadableDir(configDir, &config); err != nil {
        r.AddError(fmt.Sprintf("config: %s", err))
        r.Print(logView)
        return err
    }
    
    // Record config stats from loaded config
    r.AddSuccess(fmt.Sprintf("config: %d services, %d chats, %d completions, %d tools, %d template vars, notices ok",
        len(config.Services), len(config.Commands.Chats), len(config.Commands.Completions),
        len(config.Commands.Tools), len(config.TemplateVars)))
    
    // ... casemapping restore, API logger reinit ...
    
    reloadMCPClients(config.MCPs, r)
    
    // ... registerCommandsLocked, queue updates ...
    
    loadIgnores(filepath.Join(configDir, "ignores.txt"))
    // Record ignore count from the loaded patterns
    r.AddSuccess(fmt.Sprintf("ignores: %d patterns", getIgnoreCount()))
    
    r.Print(logView)
    return nil
}
```

The existing success/failure messages printed by `tuiCmdReload` (`[green]Reloaded...[white]` / `[red]Reload failed...[white]`) are removed — the report replaces them.

### `main()` startup path — `main.go`

```go
func main() {
    config = loadConfigDirOrDie(configDir)
    
    r := NewReloadReport("startup")
    r.AddSuccess(...)  // config stats from loaded config
    
    app, _ := initTUI()  // logView now exists
    
    // ... DB init, log writer ...
    
    initMCPClients(r)
    
    // ... queue, commands, ignores ...
    
    loadIgnores(ignorePath)
    r.AddSuccess(fmt.Sprintf("ignores: %d patterns", getIgnoreCount()))
    
    // Print report after all initialization
    r.Print(logView)
    
    // ... rest of startup (bots, TUI run) ...
}
```

## Helper: `getIgnoreCount()`

Small helper in `main.go` that reads `ignorePatterns` under `ignoreMu` and returns the count. Used by both startup and reload paths to record ignore stats without changing `loadIgnores` signature.

## What Does NOT Change

- `loadReloadableDir` — signature unchanged, caller gathers stats from config object
- `loadConfigDir` / `loadConfigDirOrDie` — signature unchanged
- `loadIgnores` — signature unchanged, caller reads count after
- Individual MCP log lines — kept as-is (they go to log file)
- `tuiCmdReload` for `/reload <mcp-name>` — unchanged (single-server reload signal)

## Stats Recorded

| Source | What | Example |
|--------|------|---------|
| Config object (caller) | Service, chat, completion, tool, template var counts | `config: 4 services, 5 chats, 3 completions, 2 tools, 3 template vars, notices ok` |
| `initMCPClients` | Per-server: connected (tools count) or failed (error) | `MCP img-mcp: connected (8 tools)` / `MCP brave-mcp: connection refused` |
| `loadIgnores` (caller) | Pattern count | `ignores: 15 patterns` |

## Testing

- Unit test `ReloadReport.Print` output formatting (with and without errors)
- Integration: existing config tests continue to pass (no signature changes to config loading)
- MCP test subprocess tests unaffected
