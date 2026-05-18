# AGENTS.md - dave IRC Bot

## Project Overview
Go IRC chatbot for OpenAI-compatible APIs, Stable Diffusion, ComfyUI image gen. Single binary, TOML-driven config directory.

**Module**: `github.com/knivey/dave`
**Go**: 1.25.0

**NEVER** use `git add -f` to force-add files that are in `.gitignore`. The `.gitignore` exists for a reason (secret keys, environment configs, build artifacts). If `git add` refuses to track a file, respect that — do not override it.

**NEVER skip code reviews.** Code reviews via the `requesting-code-review` skill are MANDATORY after completing any task — no exceptions. "It's simple", "it's too small", "I already tested it", or "the tests pass" are NOT valid reasons to skip review. Every completed task MUST be reviewed before moving on or declaring work done. If you are tempted to skip a review, stop and do it anyway.

**Shutdown path is single-source.** The only shutdown logic lives in the `shutdown()` function in `main.go`. The SIGINT signal handler and TUI `requestShutdown()` in `tui.go` both call `shutdown()` — they do NOT duplicate cleanup code. Any new cleanup steps (closing connections, flushing buffers, etc.) must be added to `shutdown()` in `main.go` ONLY. Do not create parallel shutdown paths.

## Design Docs
- [Queue & Session System](docs/queue-and-sessions.md) — how queue delivery, parallel execution, sessions, and async background tasks are intended to work

## Commands
```bash
go build -o dave .
go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp
go build -o mcps/yt-mcp/yt-mcp ./mcps/yt-mcp
go build -o tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql ./tools/migrate-sqlite-to-pgsql

./dave              # config/ directory
./dave prod         # prod/ directory
./dave test         # test/ directory

./mcps/img-mcp/img-mcp              # uses mcps/img-mcp/config.toml (stdio, all tools)
./mcps/img-mcp/img-mcp prod.toml    # uses specified config (relative to binary dir)
./mcps/img-mcp/img-mcp --http       # HTTP mode (dual paths: /sync + /async)

./tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql --sqlite data/dave.db --postgres "postgres://user:pass@localhost:5432/dave?sslmode=disable"
./tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql --dry-run --sqlite data/dave.db --postgres "postgres://user:pass@localhost:5432/dave?sslmode=disable"

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
- `mcpClient.go`: MCP client lifecycle — `initMCPClients`, `closeMCPClients`, `closeAndClearMCPClients`, `reloadMCPClients`, `signalMCPServer`. Automatic reconnection with exponential backoff on tool/resource/prompt call failure. `connectMCPServer` creates a single server connection (stores `exec.Cmd` for stdio processes); `reconnectMCPServer` handles reconnect with per-server mutex and backoff state. `connectMCPServerImpl` is a package-level var (overridable in tests). `signalMCPServer` sends SIGHUP to stdio processes or POSTs to the HTTP admin reload endpoint.
- Config in TOML directory: `config/config.toml` (main), `config/services.toml`, `config/promptenhancements.toml`, `config/mcps.toml`, `config/completions.toml`, `config/chats.toml`, `config/sd.toml`, `config/comfy.toml`, `config/notices.toml`.
  - Missing command/service/promptenhancement/mcps/notices files = empty maps/defaults (not fatal).
  - `ignores.txt` (see `.example`) for host ignores (wildcard).
- `notices.go`: Templatizable user-facing notice/reply system. All IRC messages (errors, warnings, queue status, session management, tool calls, pastebin, etc.) use `{placeholder}` templates loaded from `notices.toml`. Missing fields use hardcoded defaults via `setNoticesDefaults()`. Reloadable via `/reload`. `getNotices()` helper reads config under RLock. `expandNotice(tmpl, vars)` does `{key}` → value replacement. `errorMsg()`/`warnMsg()` use cached prefix globals (`noticeErrorPrefix`/`noticeWarnPrefix`) updated on config load/reload.
- MCP servers are self-contained packages in `mcps/<mcp-name>/`. Each MCP includes its own source code, binary, config files, and resources (e.g., workflows).
  - `mcps/img-mcp/`: ComfyUI image generation MCP with prompt enhancement.
    - Binary: `mcps/img-mcp/img-mcp`
    - Config files: `config.toml` (default), `prod.toml`, `test.toml`, `example.toml`
    - Workflows: `mcps/img-mcp/workflows/*.json`
    - Config path is relative to binary directory by default
    - Built via: `go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp`
    - **HTTP mode**: serves two MCP servers on one port with hardcoded path routing (`/sync` and `/async`). Both share the same `JobQueue`. Sync path exposes blocking tools (`generate_image`, `enhance_and_generate`). Async path exposes non-blocking tools (`generate_image_async`, `wait_for_job`, `cancel_job`, etc.). Stdio mode exposes all tools (backward compatible). Admin endpoint at `/admin/reload`.
    - **Config reload**: `SIGHUP` triggers hot reload of `comfy.*`, `upload.*`, `enhancements`, `workflows`, `queue.result_ttl`. Non-reloadable fields (`server.*`, `database.*`, `queue.max_workers`, `queue.max_depth`) are preserved from startup; warnings logged if they changed. HTTP mode also exposes `POST /admin/reload`.
    - `server.go`: three server builders — `createSyncServer`, `createAsyncServer`, `createFullServer` (stdio).
    - Config access: `ToolHandlers` and `JobQueue` use `getConfig()`/`setConfig()` with `sync.RWMutex` for atomic config swaps during reload.
    - Dave connects via two MCP entries in `mcps.toml`: `[img-mcp]` (sync path) and `[img-mcp-async]` (async path).
  - `mcps/yt-mcp/`: YouTube transcript and video metadata MCP.
    - Binary: `mcps/yt-mcp/yt-mcp`
    - Config files: `config.toml` (default), `example.toml`
    - Config path is relative to binary directory by default
    - Built via: `go build -o mcps/yt-mcp/yt-mcp ./mcps/yt-mcp`
    - **Stdio only** — no HTTP mode, no database, no job queue.
    - **HTTP mode**: `--http` flag serves Streamable HTTP MCP on `server.addr`. Requires `auth.api_key`. Admin endpoint at `POST /admin/reload`.
    - **Config reload**: `SIGHUP` triggers hot reload of `ytdlp.*` settings. HTTP mode also exposes `POST /admin/reload`.
    - Tools: `get_transcript` (fetches YouTube auto-captions via yt-dlp json3 format, parses to plain text), `get_video_info` (fetches video metadata via `--dump-json`). YouTube-only — tool descriptions enforce this so the LLM won't call on non-YouTube URLs.
    - Temp files named `<videoID>.<lang>.json3` to avoid collisions.
    - Dave connects via `[yt-mcp]` entry in `mcps.toml` (stdio transport).
  - MCPs are referenced in dave's config via `mcps.toml` (transport: stdio/http, command path, timeout) and `tools.toml` (mcp server name, tool name, args).

## High-Signal Gotchas
- Config validation: `loadConfigDirOrDie` calls `os.Exit(1)` on any error at startup. `loadCommandsDir` and `loadReloadableDir` return errors for hot-reload (no exit).
- Command registration: `builtInCmds` contains static built-in commands (stop, help), never modified. `configCmds` is atomically replaced on reload. Dispatch merges both maps (built-ins first for priority). `commandsMutex` (RWMutex) protects concurrent access.
- `/reload` in TUI reloads MCPs, services, prompt enhancements, notices, and command definitions. Hot-swaps `config.MCPs`, `config.Services`, `config.PromptEnhancements`, `config.Notices`, and atomically replaces `configCmds` (no in-place mutation). MCP clients are closed and reconnected on reload. Queue manager notices updated via `UpdateNotices()`. `/reload <mcp-name>` sends a reload signal to a specific MCP server (SIGHUP for stdio, HTTP POST to `/admin/reload` for HTTP).
- Config directory expected as CLI arg (default: `config`). Previously was a single `.toml` file.
- TUI captures stdout/stderr via `os.Pipe()` after config loading. Log output (logxi) displayed in tview TextView with ANSI stripping.
- `tview.TranslateANSI()` is used for log output in TUI, preceded by `tview.Escape()` to prevent IRC text with brackets from being interpreted as tview color tags.
- Command regex: empty = key name; registered as `^<regex> (.+)$` in main.go.
- Networks inherit root `trigger`/`quitmsg`; cycle multiple servers on reconnect. TUI commands (`/join`, `/part`, `/nick`) update `bot.Network` in-memory (not persisted); `bot.mu` (sync.Mutex) protects access. RPL_WELCOME and reconnect loop reference `bot.Network` (not captured `network` value) so runtime changes survive reconnect. SASL PLAIN auth via `[networks.<name>.sasl]` (user+pass); girc handles CAP negotiation automatically.
- Service `maxhistory` defaults to 8.
- **Builtin LLM tools**: `register_background_job`, `ban_user`, `check_ban_history` — auto-available when MCP tools exist. Controlled by two config options:
  - `disabled_builtin_tools` (root/services/chats): which tools are **unavailable** to the model. Cascade: `nil` = inherit from service/root, `[]` = none disabled (override). Default: `nil` (all enabled). Tool definition filtering in `getBuiltinToolDefs(disabled)`, dispatch guard in `executeToolCalls` returns error message for disabled tools.
  - `hidden_tools` (root only): which tool names skip IRC call notifications. Default: `["register_background_job", "check_ban_history"]`. Controlled by `isToolHidden()` in `executeToolCalls`; previously all builtins were hardcoded-hidden.
- ComfyConfig requires `workflow_path`, `clientid`, `output_node`, `prompt_node`.
- `db.go`: GORM database layer. `DatabaseConfig` has `Driver` (sqlite/postgres), `Path` (sqlite), `DSN` (postgres), `MaxAgeDays`. `initDB` opens with appropriate GORM dialector and runs AutoMigrate. All query functions use GORM API (no raw SQL except complex updates). Structs: `Session`, `Message`, `PendingJob`, `TurnUsage` (with `time.Time` timestamps). `theDB` is `*gorm.DB` global.
- Tests: table-driven + `t.Run()`, using `github.com/stretchr/testify` (`assert`/`require`). MarkdownToIRC uses shared `runTests()` helper with contain/notContain checks. Root has context/config/ai tests. Config tests use `createTestConfigDir` helper for directory-based configs. Hand-rolled mocks (`mockChatRunner`, `mockContextStore`, `mockRateLimiter`) use function fields for per-test behavior — not converted to `testify/mock` since function-field pattern is more flexible for dynamic behavior.
- MCP tests (`TestMCPConfigValidation`, `TestMCPConfigTimeoutDefault`) run `go run . <dir>` as subprocess. MCP config is in `mcps.toml` (not in `config.toml`).
- MCP reconnection: `callMCPTool`, `readMCPResource`, `getMCPPrompt` all retry once after reconnect on failure. Backoff: `2^count * 1s` with jitter, capped at 60s. SDK `KeepAlive` (default 30s for HTTP) proactively detects dead sessions. `MCPServer.reconnectMu` serializes reconnect attempts per server. `reconnectCount` resets to 0 on success. `MCPConfig.KeepAlive` field in `mcps.toml`.
- Responses API: `responses_api` (AIConfig, per-command in `chats.toml`) enables `POST /v1/responses` instead of Chat Completions. Supported by OpenAI and xAI/Grok. `previous_response_id` chains responses via server-side context storage (only sends new messages). Both default `false`. `encrypted_reasoning` (bool, default false) requests encrypted reasoning content in the API response; only supported by reasoning models (o-series), non-reasoning models return 400. Implementation in `responses.go` bypasses `sashabaranov/go-openai` SDK (which lacks Responses API support) and makes direct HTTP calls via `chatRunner.httpClient` (shares `daveTransport` for extra_body/header injection and API logging). Response ID stored in `ChatContext.ResponseID` and persisted in `sessions.response_id` column. Tool call loop retained locally — `function_call` output items mapped to/from `gogpt.ToolCall`. Graceful fallback on expired/invalid `previous_response_id`.
- **Never normalize config keys at load time** (e.g. channel names in `Network.Channels` map). Config keys must stay exactly as the admin typed them in TOML. Normalization happens at lookup time using the per-network casemapping (`Network.Casemapping`, set at runtime from ISUPPORT). See `GetChannelConfig` for the correct pattern: normalize the lookup key, then iterate config keys normalizing each for comparison. This is because the correct casemapping is only known after connecting to the IRC server, not at config load time.
- Session compacting: `compaction.go` summarizes the first ~2/3 of session history into a single `RoleSystem` summary message. Originals stay in the `messages` table flagged `archived=true` with FK to a `compactions` row. `loadDBSessionMessages` filters `archived=false` for live history; `loadDBSessionMessagesAll` returns everything for the history viewer except superseded ghosts. Triggered manually via `^compact$` IRC and TUI `/compact`, or automatically post-turn when prompt tokens cross `[compaction] auto_threshold * context_window`. Compaction inserts new rows in this order: fresh re-rendered system prompt → summary → re-inserted preserved-tail copies (originals also archived) so `ORDER BY id ASC` produces correct logical ordering. Resets `Session.ResponseID` so any Responses API chain restarts on the next turn. Per-session `compactionMu sync.Map` with `mu.TryLock()` prevents concurrent compactions on the same session. Images in the archived range are stripped to `[image]` placeholder for the summarizer call. CRITICAL: `pickCompactionCutTurn` enforces that the preserved tail begins with `RoleUser` so the post-compaction chain never goes `system → assistant` (some providers like xAI/Grok reject that). If no safe cut exists (e.g. async-injected `RoleSystem` rows at every candidate boundary), compaction refuses with `ErrCompactionTooShort`.
- Session compacting tail-copy supersedure: every re-inserted preserved-tail row is tagged with `Message.SourceCompactionID = comp.ID` of the compaction that produced it. On the NEXT compaction, any row carrying `SourceCompactionID` (whether it lands in the archived range or in the new preserved tail) is marked `superseded=true, archived=true` instead of being counted as fresh archived material — its content is already covered by the earlier summary. Superseded rows are excluded from `loadDBSessionMessagesAll`, from the `historySessions` archived-count query, and from the user-facing `CompactionResult.ArchivedCount`. They still exist on disk; there is no per-row GC and the rows are eventually removed only when the whole session is cleaned up by `MaxAgeDays`. The Compaction audit row's `FirstArchivedID`/`LastArchivedID`/`ArchivedCount` still describe the contiguous range that the event touched (including superseded rows) so the bookkeeping stays internally consistent. **Future direction (not implemented):** Option A — a `Message.LogicalSeq` column queried via `ORDER BY logical_seq` would eliminate the need to re-insert tail rows entirely (zero duplication on disk). It requires migrating every `ORDER BY id` on messages and backfilling existing rows; deferred until the codebase grows other ordering needs or production data shows pathological growth. See the comment block atop `compaction.go`.
- User identity resolution graceful degradation: `resolveUser` wraps `resolveUserOnce` with retry+fallback. Transient DB errors (lock contention etc, detected via `isTransientDBErr`) retry 2× with 50ms+150ms jittered backoff; if still failing it returns `*ErrUserResolveTransient` to the caller. UNIQUE-constraint failures that survive retries (detected via `isUniqueConstraintErr`) trigger `resolveUserFallback` which inserts a **flagged row** with the real `normalized_nick` (the partial unique index `idx_users_nick_active` excludes flagged rows, so duplicates are allowed). Flagged rows have `Flagged=true` and `FlaggedReason="collision_unique_nick"`. They are skipped by `getUserByAccount` (filter), `getActiveUserByNormalizedNick` (filter), `resolveUserByNick` (calls `getActiveUserByNormalizedNick`), and `recoverByKnownHost` (JOIN filter `users.flagged = false`) — the JOIN filter is critical because `resolveUserFallback` calls `upsertKnownHost` so the flagged row inherits the legitimate owner's `(ident, host)` and would otherwise be re-surfaced via host recovery on the next message. Callers consume results via `handleResolveResult` in `users_resolve_helper.go` which sends one of two `warnMsg()` notices: `[users] resolve_transient` ("try again") or `[users] resolve_persistent` ("data conflict, admin should investigate"). Admin awareness: ERROR-level log line `flagged_user_created_admin_attention_required` includes full context; TUI status bar between log view and input shows `flagged:N` (5s poll, yellow when >0; logs `status bar countFlaggedUsers failed` on DB error and renders `flagged:?`); TUI `/flagged [network]` lists current flagged rows. Cleanup is manual via the existing TUI `/usermerge` (which calls `mergeUser`). Flagged users intentionally still proceed through ban checks (using their throwaway ID) — bans keyed on the real user_id are briefly bypassed during the fallback window; they are restored automatically once DB state clears or admin merges. `claimNickFor` is exposed as `claimNickForFn` package var to allow test injection of collision scenarios. Migration #5 `add_users_flagged_columns` adds the `flagged` + `flagged_reason` columns and `idx_users_flagged` index.

- User row release & partial unique index: when a tracked user QUITs, PARTs the last visible channel, is KICKed, or is displaced by `handleNickCollision`, `releaseUserNick(userID)` flips `Released=true` on the row (current_nick + normalized_nick are preserved). The unique constraint on `(network, normalized_nick)` is enforced by a **partial** unique index `idx_users_nick_active` created by migration #7: `UNIQUE (network, normalized_nick) WHERE released = false AND flagged = false`. Released and flagged rows are excluded from uniqueness so another active user can claim the same nick, and the released row is still available for host-based recovery (`recoverByKnownHost`) and account-based recovery (`getUserByAccount`) when the user returns — on match `resolveUserOnce` calls `reactivateIfReleased` to flip `Released=false` again. `getActiveUserByNormalizedNick` is the standard "who owns this nick right now" lookup and filters on `released=false AND flagged=false`. The old `,quit,<id>` / `,flagged,<network>,<norm>,<unixNano>,<seq>` sentinel-in-`normalized_nick` scheme was replaced in migration #7 (`convert_sentinels_to_released_column`) — sentinels never carried meaningful payload, the `<id>` was just a uniqueness salt because the previous full UNIQUE index couldn't tolerate two `,quit,` rows; the new partial index removes the need for that encoding. Migration #7 also drops the transitional `last_nick` column added by #6 (which existed only to give #7 a source of nick history when restoring sentinel rows on upgrade).

- Released-nick fallback in `resolveUserOnce`: a third-tier identity recovery (after account match and host recovery) implemented via `getMostRecentReleasedUserByNormalizedNick`. When a user rejoins from a previously-unseen host on an accountless network (common with mobile networks, ISP DHCP, VPN), neither active nick lookup nor host recovery will find them. The fallback looks for released rows on `(network, normalized_nick)` and re-activates the most recently updated match. Multiple matches log a WARN (`released-nick fallback: multiple released rows match, picking newest`) so admins can monitor whether this happens in practice; the picker is `ORDER BY updated_at DESC, id DESC LIMIT 1`. Flagged rows are explicitly excluded. The fallback also runs in the account branch (after account miss + host miss) to handle the corner case of a user gaining IRC services account between disconnects. Successful matches log at INFO with `method=released_nick_fallback` (or `released_nick_fallback+account_link`). Security note: on zero-trust networks this extends nick continuity across disconnects, so an attacker re-using a released nick inherits the previous owner's sessions/bans/history — same trust posture the bot already had via in-channel nick continuity, just preserved across QUIT/PART/KICK. Involuntarily-released rows (`handleNickCollision` displacement) are also eligible for the fallback; the precondition (two active rows on the same normalized_nick) is hard to reach on real IRC servers — realistic sources are netsplit corner cases or DB-state drift, not user-triggered. Full mitigation deferred to Phase 5 account system. Tests in `users_test.go` covering same-nick-different-host, different-ident, host-priority, multiple-match selection, no-match fallthrough, account-branch fallback, and flagged-row exclusion.

## Code Style
- Imports: stdlib, blank, third-party.
- Globals heavily used. Concurrency via `go cmd(...)` + `sync.WaitGroup`.
- **Logger initialization**: Every `logxi.New()` logger MUST call `SetLevel(logxi.LevelAll)` — without it, debug messages are silently dropped. Package-level loggers do this in an `init()` block; function-scoped loggers do it inline right after creation.
- Struct fields use TOML snake_case tags.
- Error: `log.Fatalln` at startup only. TUI `/reload` uses error-returning `loadReloadableDir`.
- **Design comments**: Preserve and maintain block comments that explain non-obvious design decisions (e.g. "DESIGN NOTE", "Rationale", multi-line comments explaining why code is structured a certain way). When you encounter code that could be misunderstood or that implements a multi-layer defense, add a comment explaining the full picture. These comments are critical for maintaining correctness across future edits. See `isResponseIDError()` in `responses.go` for an example.

## Review Process
- When implementing multi-task plans, perform **both spec compliance review AND code quality review** after every task, no matter how trivial. Never skip either review.
- Spec review verifies the code matches the task requirements (nothing missing, nothing extra).
- Quality review verifies the code is well-built (correct patterns, no bugs, no race conditions, clean style).
- Fix all issues found in reviews before moving to the next task.

## Testing
- All new features and changes MUST be covered by tests.
- Run `go test ./...` after every change to verify nothing is broken.
- Config methods: test with table-driven tests using struct literals (see `TestGetPastebinPreviewLines`, `TestAIConfigApplyDefaults`).
- Config loading: use `createTestConfigDir` helper to build temporary config dirs from TOML strings (see `TestLoadConfigDirNetworkEnabledDefault`).
- Use `github.com/stretchr/testify` (`assert`/`require`). Pointer helpers: `boolPtr`, `intPtr` in `config_test.go`.
- Run `go fmt ./...` and `go vet ./...` after writing tests.

## Config Documentation Convention
- All TOML config files in `config/` MUST have a **reference section** at the top listing every available option with type and default value.
- A **commented-out `[section]` block** with all fields set to example/default values must follow the reference list, directly copy-pasteable.
- When adding a new config field (Go struct `toml` tag), update the corresponding config file's reference list AND commented example block.
- Duration fields use TOML string format: `"30s"`, `"2m"`, `"1m30s"`, `"750ms"`. Ban duration fields (`[bans]`) also support days: `"7d"`.
- Ban durations are parsed by `parseBanDuration` in `bans.go` which converts `d` suffix to hours. TUI `/ban` and LLM `ban_user` both enforce `max_duration` from config.
- Keep all existing live config sections untouched below the documentation blocks.
- The `ignores.txt.example` file should document the wildcard pattern format.

Preserve this file. Update only verified facts.