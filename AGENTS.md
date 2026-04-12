# AGENTS.md - dave IRC Bot

## Project Overview
Go IRC chatbot for OpenAI-compatible APIs, Stable Diffusion, ComfyUI image gen. Single binary, TOML-driven config directory.

**Module**: `github.com/knivey/dave`
**Go**: 1.25.0

## Commands
```bash
go build -o dave .

./dave              # config/ directory
./dave prod         # prod/ directory
./dave test         # test/ directory

go test ./...                    # all tests
go test ./MarkdownToIRC/...      # markdown tests only
go test -run TestContextStoreRoundtrip  # one test
go test -v -run TestCodeBlocks   # subtest

go fmt ./...
go vet ./...
go mod tidy
```
No Makefile, no linter config. Use `go fmt` + `go vet`.

## Architecture
- All root `.go` files = `package main` (globals: `config`, `builtInCmds` *CmdMap*, `configCmds` *CmdMap*, `commandsMutex`, `bots`, `wg`, context maps, `logger`).
- `MarkdownToIRC/` (with `irc/`, `tables/`) converts markdown to IRC codes (`\x02`, `\x03`, `\x1D`).
- `main.go`: loads config, registers regex commands, starts girc clients per network, runs TUI.
- `tui.go`: tview-based TUI with scrolling log view and command input (`/reload`, `/quit`).
- `config.go`: directory-based config loading with error-returning validation (reload-safe).
- Config in TOML directory: `config/config.toml` (main), `config/services.toml`, `config/promptenhancements.toml`, `config/completions.toml`, `config/chats.toml`, `config/sd.toml`, `config/comfy.toml`.
  - Missing command/service/promptenhancement files = empty maps (not fatal).
  - `ignores.txt` (see `.example`) for host ignores (wildcard).
  - `contexts.json` (gitignored) for persistent chat history.

## High-Signal Gotchas
- Config validation: `loadConfigDirOrDie` calls `os.Exit(1)` on any error at startup. `loadCommandsDir` and `loadReloadableDir` return errors for hot-reload (no exit).
- Command registration uses `builtInCmds` (stop, help) + `configCmds` (from config). `commandsMutex` (RWMutex) protects concurrent access.
- `/reload` in TUI reloads services, prompt enhancements, and command definitions. Hot-swaps `config.Services`, `config.PromptEnhancements`, and `configCmds` entries.
- Config directory expected as CLI arg (default: `config`). Previously was a single `.toml` file.
- TUI captures stdout/stderr via `os.Pipe()` after config loading. Log output (logxi) displayed in tview TextView with ANSI stripping.
- Command regex: empty = key name; registered as `^<regex> (.+)$` in main.go.
- Networks inherit root `trigger`/`quitmsg`; cycle multiple servers on reconnect.
- Service `maxhistory` defaults to 8.
- ComfyConfig requires `workflow_path`, `clientid`, `output_node`, `prompt_node`.
- Context store: dirty flag, atomic (`.tmp`+rename), timer, age+count cleanup. Tests mock via `persistCfg.FilePath`.
- Tests: table-driven + `t.Run()`, substring `contain`/`notContain` (no testify). MarkdownToIRC uses shared `runTests()` helper. Root has context/config/ai tests. Config tests use `createTestConfigDir` helper for directory-based configs.
- MCP tests (`TestMCPConfigValidation`, `TestMCPConfigTimeoutDefault`) run `go run . <dir>` as subprocess.

## Code Style
- Imports: stdlib, blank, third-party.
- Globals heavily used. Concurrency via `go cmd(...)` + `sync.WaitGroup`.
- Struct fields use TOML snake_case tags.
- Error: `log.Fatalln` at startup only. TUI `/reload` uses error-returning `loadReloadableDir`.

Preserve this file. Update only verified facts.