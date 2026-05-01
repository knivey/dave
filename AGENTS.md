# AGENTS.md - dave IRC Bot

## Project Overview
Go IRC chatbot for OpenAI-compatible APIs, Stable Diffusion, ComfyUI image gen. Single binary, TOML-driven config directory.

**Module**: `github.com/knivey/dave`
**Go**: 1.25.0

## Commands
```bash
go build -o dave .
go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp

./dave              # config/ directory
./dave prod         # prod/ directory
./dave test         # test/ directory

./mcps/img-mcp/img-mcp              # uses mcps/img-mcp/config.toml (stdio, all tools)
./mcps/img-mcp/img-mcp prod.toml    # uses specified config (relative to binary dir)
./mcps/img-mcp/img-mcp --http       # HTTP mode (dual paths: /sync + /async)

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
- `tui.go`: tview-based TUI with scrolling log view and command input (`/help`, `/reload`, `/join`, `/part`, `/nick`, `/quit`).
- `config.go`: directory-based config loading with error-returning validation (reload-safe).
- `mcpClient.go`: MCP client lifecycle — `initMCPClients`, `closeMCPClients`, `closeAndClearMCPClients`, `reloadMCPClients`. Automatic reconnection with exponential backoff on tool/resource/prompt call failure. `connectMCPServer` creates a single server connection; `reconnectMCPServer` handles reconnect with per-server mutex and backoff state. `connectMCPServerImpl` is a package-level var (overridable in tests).
- Config in TOML directory: `config/config.toml` (main), `config/services.toml`, `config/promptenhancements.toml`, `config/mcps.toml`, `config/completions.toml`, `config/chats.toml`, `config/sd.toml`, `config/comfy.toml`.
  - Missing command/service/promptenhancement/mcps files = empty maps (not fatal).
  - `ignores.txt` (see `.example`) for host ignores (wildcard).
- MCP servers are self-contained packages in `mcps/<mcp-name>/`. Each MCP includes its own source code, binary, config files, and resources (e.g., workflows).
  - `mcps/img-mcp/`: ComfyUI image generation MCP with prompt enhancement.
    - Binary: `mcps/img-mcp/img-mcp`
    - Config files: `config.toml` (default), `prod.toml`, `test.toml`, `example.toml`
    - Workflows: `mcps/img-mcp/workflows/*.json`
    - Config path is relative to binary directory by default
    - Built via: `go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp`
    - **HTTP mode**: serves two MCP servers on one port with path routing (`/sync` and `/async`, configurable). Both share the same `JobQueue`. Sync path exposes blocking tools (`generate_image`, `enhance_and_generate`). Async path exposes non-blocking tools (`generate_image_async`, `wait_for_job`, `cancel_job`, etc.). Stdio mode exposes all tools (backward compatible).
    - `server.go`: three server builders — `createSyncServer`, `createAsyncServer`, `createFullServer` (stdio).
    - Dave connects via two MCP entries in `mcps.toml`: `[img-mcp]` (sync path) and `[img-mcp-async]` (async path).
  - MCPs are referenced in dave's config via `mcps.toml` (transport: stdio/http, command path, timeout) and `tools.toml` (mcp server name, tool name, args).

## High-Signal Gotchas
- Config validation: `loadConfigDirOrDie` calls `os.Exit(1)` on any error at startup. `loadCommandsDir` and `loadReloadableDir` return errors for hot-reload (no exit).
- Command registration: `builtInCmds` contains static built-in commands (stop, help), never modified. `configCmds` is atomically replaced on reload. Dispatch merges both maps (built-ins first for priority). `commandsMutex` (RWMutex) protects concurrent access.
- `/reload` in TUI reloads MCPs, services, prompt enhancements, and command definitions. Hot-swaps `config.MCPs`, `config.Services`, `config.PromptEnhancements`, and atomically replaces `configCmds` (no in-place mutation). MCP clients are closed and reconnected on reload.
- Config directory expected as CLI arg (default: `config`). Previously was a single `.toml` file.
- TUI captures stdout/stderr via `os.Pipe()` after config loading. Log output (logxi) displayed in tview TextView with ANSI stripping.
- `tview.TranslateANSI()` is used for log output in TUI, preceded by `tview.Escape()` to prevent IRC text with brackets from being interpreted as tview color tags.
- Command regex: empty = key name; registered as `^<regex> (.+)$` in main.go.
- Networks inherit root `trigger`/`quitmsg`; cycle multiple servers on reconnect. TUI commands (`/join`, `/part`, `/nick`) update `bot.Network` in-memory (not persisted); `bot.mu` (sync.Mutex) protects access. RPL_WELCOME and reconnect loop reference `bot.Network` (not captured `network` value) so runtime changes survive reconnect.
- Service `maxhistory` defaults to 8.
- ComfyConfig requires `workflow_path`, `clientid`, `output_node`, `prompt_node`.
- Context store: dirty flag, atomic (`.tmp`+rename), timer, age+count cleanup. Tests mock via `persistCfg.FilePath`.
- Tests: table-driven + `t.Run()`, substring `contain`/`notContain` (no testify). MarkdownToIRC uses shared `runTests()` helper. Root has context/config/ai tests. Config tests use `createTestConfigDir` helper for directory-based configs.
- MCP tests (`TestMCPConfigValidation`, `TestMCPConfigTimeoutDefault`) run `go run . <dir>` as subprocess. MCP config is in `mcps.toml` (not in `config.toml`).
- MCP reconnection: `callMCPTool`, `readMCPResource`, `getMCPPrompt` all retry once after reconnect on failure. Backoff: `2^count * 1s` with jitter, capped at 60s. SDK `KeepAlive` (default 30s for HTTP) proactively detects dead sessions. `MCPServer.reconnectMu` serializes reconnect attempts per server. `reconnectCount` resets to 0 on success. `MCPConfig.KeepAlive` field in `mcps.toml`.
- Responses API: `responses_api` (AIConfig, per-command in `chats.toml`) enables `POST /v1/responses` instead of Chat Completions. Supported by OpenAI and xAI/Grok. `previous_response_id` chains responses via server-side context storage (only sends new messages). Both default `false`. Implementation in `responses.go` bypasses `sashabaranov/go-openai` SDK (which lacks Responses API support) and makes direct HTTP calls via `chatRunner.httpClient` (shares `daveTransport` for extra_body/header injection and API logging). Response ID stored in `ChatContext.ResponseID` and persisted in `sessions.response_id` column (DB migration 004). Tool call loop retained locally — `function_call` output items mapped to/from `gogpt.ToolCall`. Graceful fallback on expired/invalid `previous_response_id`.

## Code Style
- Imports: stdlib, blank, third-party.
- Globals heavily used. Concurrency via `go cmd(...)` + `sync.WaitGroup`.
- Struct fields use TOML snake_case tags.
- Error: `log.Fatalln` at startup only. TUI `/reload` uses error-returning `loadReloadableDir`.

## Config Documentation Convention
- All TOML config files in `config/` MUST have a **reference section** at the top listing every available option with type and default value.
- A **commented-out `[section]` block** with all fields set to example/default values must follow the reference list, directly copy-pasteable.
- When adding a new config field (Go struct `toml` tag), update the corresponding config file's reference list AND commented example block.
- Duration fields use TOML string format: `"30s"`, `"2m"`, `"1m30s"`, `"750ms"`.
- Keep all existing live config sections untouched below the documentation blocks.
- The `ignores.txt.example` file should document the wildcard pattern format.

Preserve this file. Update only verified facts.