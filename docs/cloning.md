# Session Cloning/Forking — Design & Implementation Plan

## Overview

Ability to clone (fork) another user's session in the channel. Creates a new session owned by the caller with a copy of the source session's live messages. Also adds the ability to browse other users' sessions so users can discover sessions to clone.

## Commands

| Command | Description |
|---|---|
| `clone <nick>` | Clone target user's active session in the channel |
| `clone <id>` | Clone a specific session by ID (same network+channel only) |
| `sessions` | Your sessions (unchanged) |
| `sessions <nick>` | View another user's sessions |
| `sessions *` | View all sessions in the channel |

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Self-clone | Allowed | Acts as fork/snapshot before trying something risky |
| Clone scope | Both `<nick>` and `<id>` | ID allows cloning completed sessions; nick targets active |
| System prompt | Re-rendered for cloning user | `{{.Nick}}` etc. refer to the new user, not the original |
| Config gating | None | "its not really a secret in the channel" (todo.md) |
| Incomplete tool calls | **Refuse to clone** | Dangling tool_call_ids → API error. Detect and reject with clear notice |
| Responses API chain | Cloned session starts with `response_id = nil` | Original's `previous_response_id` is server-side context; can't be reused. Full history sent on first turn (same as resume on expired ID) |
| Archived messages | Only live (`archived=false`) cloned | Compacted content excluded; clone starts clean |
| Pending jobs | Not cloned | Async goroutines belong to the original session |
| Images | Cloned as-is | Multi-content URLs remain valid |

## Files to Change

| File | Changes |
|---|---|
| `db.go` | Add `cloneDBSession()`, `getChannelDBSessions()`, `sessionHasIncompleteToolCalls()` |
| `historyCmds.go` | Update `historySessions()` for `<nick>`/`*` args; add `historyClone()` |
| `main.go` | Update `sessions_re`; add `clone_re`; register both in `builtInCmds`/`builtInNames` |
| `notices.go` | Add `CloneNotices`; add `OtherHeader`/`OtherNone`/`AllHeader` to `SessionNotices`; defaults |
| `config/notices.toml` | Add `[clone]` section; update `[sessions]` reference |
| `help.go` | Update `sessions` help; add `clone` help |
| `*_test.go` | Tests for clone, sessions-by-user, incomplete-tool-call detection |
| `todo.md` | Update session cloning/forking section |

---

## Implementation Details

### 1. `db.go` — New Functions

#### `sessionHasIncompleteToolCalls(sessionID int64) (bool, error)`

Detects whether a session has assistant messages with `tool_calls` that lack matching `tool`-role response messages. This happens when:

- Bot crashes mid-`executeToolCalls` loop
- User runs `stop` during tool execution
- Context cancellation during tool execution

Algorithm:
1. Load all live messages for the session (`archived=false, superseded=false`)
2. Build a `map[string]struct{}` of all `tool_call_id` values from tool-role messages (the `tool_call_id` column)
3. Iterate assistant messages; for each, unmarshal `tool_calls` JSON → collect all `tool_call.ID` values
4. If any assistant tool call ID is not in the tool-response set → return `true`
5. Return `false` if all pairs are complete

#### `cloneDBSession(sourceSessionID int64, targetNetwork, targetChannel string, targetUserID int64) (int64, error)`

Inside a single DB transaction:

1. Load source session. Return error if not found.
2. Copy the `SessionSetting` row (if `settings_id` is set) — create a new row with identical fields, get new ID.
3. Complete any existing active session for `(targetNetwork, targetChannel, targetUserID)` — same pattern as `CreateSession`:
   ```go
   tx.Model(&Session{}).Where("network = ? AND channel = ? AND user_id = ? AND status = ?",
       targetNetwork, targetChannel, targetUserID, "active").Update("status", "completed")
   ```
4. Create new `Session`:
   - `Network`, `Channel` = caller's network/channel
   - `UserID` = `targetUserID`
   - `ChatCommand`, `Service`, `Model` = copied from source
   - `ConvID` = newly generated
   - `ResponseID` = `nil`
   - `SettingsID` = new settings row ID from step 2
   - `Status` = `"active"`
5. Load live messages from source (`archived=false AND superseded=false`).
6. Skip the first system-role message (the handler will insert a freshly rendered one).
7. Insert remaining messages into the new session with new auto-increment IDs:
   - `SessionID` = new session ID
   - `Archived` = `false`
   - `CompactionID` = `nil`
   - `SourceCompactionID` = `nil`
   - `Superseded` = `false`
   - All other fields (`Role`, `Content`, `ToolCalls`, `ToolCallID`, `ReasoningContent`, `MultiContent`, `IsAsyncResult`) copied as-is
8. Set `first_message` from the first user-role message in the cloned messages.
9. Return new session ID.

#### `SessionWithUser` struct and `getChannelDBSessions`

```go
type SessionWithUser struct {
    Session
    OwnerNick string
}

func getChannelDBSessions(network, channel string, limit int) ([]SessionWithUser, error)
```

- Queries `sessions` JOIN `users` ON `sessions.user_id = users.id`
- Filters by `network`, `channel`, `deleted_at IS NULL`
- Orders by `last_active DESC`
- Returns `OwnerNick` from `users.current_nick`

Note: `users.current_nick` may be stale (user changed nick, bot hasn't seen it). Acceptable for display.

### 2. `historyCmds.go` — Updated Sessions + Clone Handler

#### `historySessions` update

Change regex from `^sessions$` to `^sessions(?:\s+(\S+))?$` (see main.go section).

Dispatch logic:

- **No arg** → existing behavior (caller's sessions)
- **Arg is `*`** → call `getChannelDBSessions`, show all sessions with owner nick column
- **Arg is `<nick>`** → resolve nick to user via `resolveUserByNick`, query their sessions with `getUserDBSessions`, show with nick in header

Display format when showing other users:

```
Sessions for alice:
  ● #42  12 msgs     5m  -yo tell me about cats
```

Display format for `sessions *`:

```
All sessions in #channel:
  ● #42  alice  12 msgs     5m  -yo tell me about cats
  ● #41  bob    3 msgs      2h  -yo hello
  ○ #38  alice  3 msgs      2d  -yo hello
```

The nick column is only shown when the listing includes multiple users. Own sessions remain unchanged (no nick column — it would be redundant).

#### `historyClone` handler

```go
func historyClone(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string)
```

Flow:

1. **Parse arg**: `args[0]` all digits → session ID path; else → nick path.
2. **Resolve calling user**: Already in context from dispatch (`resolvedUserFromCtx`).
3. **Nick path**:
   - Resolve target nick via `resolveUserByNick(network.Name, nick, casemapping)`.
   - If not found → send `target_not_found` notice, return.
   - Call `sessionMgr.GetActiveSession(network.Name, channel, targetUser.ID)`.
   - If nil → send `no_target_session` notice, return.
   - Set `sourceSessionID = activeSession.ID`.
4. **ID path**:
   - Parse `args[0]` as `int64`.
   - Call `getDBSessionByID(sourceSessionID)`.
   - If not found → send `session_not_found` notice, return.
   - Verify `session.Network == network.Name && session.Channel == channel` (normalize channel with casemapping).
   - If mismatch → send `wrong_channel` notice, return.
   - Set `sourceSessionID = parsed ID`.
5. **Check incomplete tool calls**:
   - Call `sessionHasIncompleteToolCalls(sourceSessionID)`.
   - If `true` → send `incomplete_calls` notice, return.
6. **Load chat command config**:
   - Look up `sourceSession.ChatCommand` in current config's `Chats` map.
   - If command no longer exists → send `command_gone` notice, return.
   - If session has `settings_id`, load stored settings and overlay via `ApplySettings`.
7. **Acquire session creation lock**:
   - `mu := getSessionCreationLock(network.Name, channel, callingUserID)`
   - `mu.Lock()` / `defer mu.Unlock()`
   - Prevents concurrent `chat()` from creating a colliding session.
8. **Clone**:
   - Call `cloneDBSession(sourceSessionID, network.Name, channel, callingUserID)`.
   - If error → send error notice, return.
9. **Re-render system prompt**:
   - Execute `cfg.SystemTmpl` with `SystemPromptData{Nick: callingNick, BotNick: client.GetNick(), Channel: channel, Network: network.Name, ChanNicks: ..., Date: ..., Vars: ...}`.
   - Insert as first message in the new session: `sessionMgr.AddMessage(newSessionID, ChatMessage{Role: RoleSystem, Content: renderedPrompt})`.
10. **Restore API logging**: `apiLogger.RestoreSession(newSessionID, network.Name, channel, callingUserID)`.
11. **Send success notice**: `expandNotice(n.Clone.Cloned, map[string]string{"id": ..., "source_nick": ..., "source_id": ..., "count": ...})`.

### 3. `main.go` — Registration

```go
var sessions_re = regexp.MustCompile(`^sessions(?:\\s+(\\S+))?$`)
var clone_re    = regexp.MustCompile(`^clone\\s+(\\S+)$`)
```

Add `clone_re` to `builtInCmds`:

```go
clone_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
    historyClone(n, c, e, ctx, output, s...)
},
```

Add to `builtInNames`:

```go
clone_re: "clone",
```

**Queue integration**: `clone` goes through the queue (like `compact_re`) — it does heavy DB work in a transaction. Add to the dispatch section alongside `compact_re`:

```go
if match.re == clone_re {
    position := queueMgr.Enqueue(network.Name, channel, userID, event.Source.Name, "", msg,
        func(cx context.Context, output chan<- string) {
            match.cmd(network, client, event, ctxWithResolvedUser(cx, resolvedUser), output, match.args...)
        })
    if position > 0 {
        var queueMsg string
        readConfig(func() { queueMsg = config.Notices.QueueMsg(position, 0) })
        client.Cmd.Reply(event, queueMsg)
    }
    return
}
```

The `sessions_re` update changes the capture group count. Verify dispatch submatch extraction still works — the existing loop `for i, m := range r.FindSubmatch(...)` already handles variable groups, so it's fine.

### 4. `notices.go` — New Notice Types

#### CloneNotices

```go
type CloneNotices struct {
    Cloned          string `toml:"cloned"`            // Success. {id}, {source_nick}, {source_id}, {count}
    NoTargetSession string `toml:"no_target_session"` // {nick}
    TargetNotFound  string `toml:"target_not_found"`  // {nick}
    SessionNotFound string `toml:"session_not_found"` // {id}
    WrongChannel    string `toml:"wrong_channel"`     // {id}
    IncompleteCalls string `toml:"incomplete_calls"`  // {id}
    CommandGone     string `toml:"command_gone"`      // {command}
    Usage           string `toml:"usage"`             // {trigger}
}
```

Add to `NoticesConfig`:

```go
Clone CloneNotices `toml:"clone"`
```

Defaults in `setNoticesDefaults()`:

```go
if n.Clone.Cloned == "" {
    n.Clone.Cloned = "\x0303📋 Cloned session #{source_id} → #{id} ({count} messages)\x0F"
}
if n.Clone.NoTargetSession == "" {
    n.Clone.NoTargetSession = "\x0304❗ {nick} has no active session in this channel.\x0F"
}
if n.Clone.TargetNotFound == "" {
    n.Clone.TargetNotFound = "\x0304❗ Nick '{nick}' not found.\x0F"
}
if n.Clone.SessionNotFound == "" {
    n.Clone.SessionNotFound = "\x0304❗ Session #{id} not found.\x0F"
}
if n.Clone.WrongChannel == "" {
    n.Clone.WrongChannel = "\x0304❗ Session #{id} is not in this channel.\x0F"
}
if n.Clone.IncompleteCalls == "" {
    n.Clone.IncompleteCalls = "\x0304❗ Session #{id} has incomplete tool calls and cannot be cloned. Wait for the current turn to finish.\x0F"
}
if n.Clone.CommandGone == "" {
    n.Clone.CommandGone = "\x0304❗ Command '{command}' no longer exists.\x0F"
}
if n.Clone.Usage == "" {
    n.Clone.Usage = "\x0304❗ Usage: {trigger}clone <nick|id>\x0F"
}
```

#### SessionNotices additions

```go
OtherHeader string `toml:"other_header"` // {nick}, {network}
OtherNone   string `toml:"other_none"`   // {nick}
AllHeader   string `toml:"all_header"`   // {network}, {channel}
```

Defaults:

```go
if n.Sessions.OtherHeader == "" {
    n.Sessions.OtherHeader = "Sessions for {nick} on {network}:"
}
if n.Sessions.OtherNone == "" {
    n.Sessions.OtherNone = "\x0304❗ No sessions found for {nick}.\x0F"
}
if n.Sessions.AllHeader == "" {
    n.Sessions.AllHeader = "All sessions in {channel} on {network}:"
}
```

### 5. `config/notices.toml` — Reference Updates

Add to the `[sessions]` reference section:

```toml
#   other_header     (string)    Another user's session list header. Placeholders: {nick}, {network}
#   other_none       (string)    No sessions for target user. Placeholders: {nick}
#   all_header       (string)    All sessions header. Placeholders: {network}, {channel}
```

Add new `[clone]` reference section:

```toml
# [clone] options:
#   cloned            (string)  Clone success. Placeholders: {id}, {source_nick}, {source_id}, {count}
#   no_target_session (string)  Target nick has no active session. Placeholders: {nick}
#   target_not_found  (string)  Target nick not resolved. Placeholders: {nick}
#   session_not_found (string)  Session ID not found. Placeholders: {id}
#   wrong_channel     (string)  Session is in a different channel. Placeholders: {id}
#   incomplete_calls  (string)  Session has incomplete tool calls. Placeholders: {id}
#   command_gone      (string)  Chat command no longer exists. Placeholders: {command}
#   usage             (string)  Usage hint. Placeholders: {trigger}
```

### 6. `help.go`

Update `sessions` help entry to mention `<nick>` and `*` variants.

Add `clone` help entry:

```
clone <nick|id> — Clone another user's session (or your own, to fork it).
  Creates a new session with a copy of the source's message history.
  Use 'sessions <nick>' or 'sessions *' to find sessions to clone.
```

### 7. Tests

#### `sessionHasIncompleteToolCalls` tests

- Session with complete tool call pairs → returns `false`
- Session with orphaned `tool_call_id` (assistant msg has tool_calls, no matching tool response) → returns `true`
- Session with no tool calls → returns `false`
- Session with multiple tool calls in one assistant msg, one missing response → returns `true`
- Session with multiple tool call rounds, all complete → returns `false`
- Session with only the assistant+tool_calls stored (crash before any results) → returns `true`

#### `cloneDBSession` tests

- Clone a session with live + archived messages → only live copied
- Cloned messages preserve content exactly (roles, tool_calls JSON, reasoning_content, multi_content)
- Clone creates new settings row (not shared with source)
- Clone's `response_id` is `nil`
- Clone completes caller's existing active session
- Clone with empty source session (only system prompt) → skips system, no messages copied
- Source session is completely untouched after clone
- `first_message` set correctly from first user-role message

#### `historyClone` handler tests

- Clone by nick (target has active session) → success
- Clone by nick (target has no active session) → `no_target_session` error
- Clone by nick (target nick not found) → `target_not_found` error
- Clone by ID (valid, same channel) → success
- Clone by ID (different channel) → `wrong_channel` error
- Clone by ID (nonexistent ID) → `session_not_found` error
- Clone with incomplete tool calls → `incomplete_calls` error
- Self-clone → works (fork)
- System prompt re-rendered with calling user's nick (verify stored message content)
- New session is the caller's active session after clone
- `sessionCreationMu` lock prevents concurrent collision

#### `historySessions` update tests

- `sessions` (no arg) → own sessions only (existing tests still pass)
- `sessions <nick>` → that user's sessions with nick header
- `sessions <nonexistent_nick>` → `other_none` or similar
- `sessions *` → all channel sessions with nick column in output
- Output format includes nick when viewing other users' sessions
- Output format omits nick when viewing own sessions

---

## Gotchas / Considerations

1. **`sessionCreationMu` lock**: The clone handler must acquire the lock for the caller's `(network, channel, userID)` key. Without it, a concurrent `chat()` could create a session between `GetActiveSession` and `cloneDBSession`, causing the clone to complete the newly-created session instead.

2. **`sessions_re` capture group change**: Going from `^sessions$` (0 capture groups) to `^sessions(?:\s+(\S+))?$` (1 optional capture group). The dispatch loop `for i, m := range r.FindSubmatch(...)` already handles variable groups, so existing logic is fine. But verify that the `builtInNames` lookup still works — it matches on the regex pointer, not the pattern, so registration is fine.

3. **Nick resolution for `sessions <nick>`**: Use `resolveUserByNick(network, nick, casemapping)`. Must handle the case where the nick isn't currently tracked (user left the channel, bot restarted). In that case, the sessions query simply returns no results → `other_none` notice.

4. **`getChannelDBSessions` performance**: Joins `sessions` with `users` on `sessions.user_id = users.id`. The existing `idx_sessions_user` composite index covers `(network, channel, user_id)`. The join should be efficient. `users.current_nick` may be stale — acceptable for display.

5. **Tool call ID reuse in cloned history**: The cloned session copies `tool_call_id` values from the original (e.g., `call_abc123`). These are stored as historical data. When the cloned session sends its first API call, the entire history is sent fresh (no `previous_response_id`), so the API processes them as historical context. This is correct — the IDs don't need to be unique across sessions.

6. **Clone + queue**: The clone command goes through the queue like `compact`. This serializes clones per channel slot and prevents DB contention. The actual clone transaction is fast, but queueing avoids concurrent clones from different users colliding on the same source session.

7. **System prompt re-render edge case**: If the source session's chat command was deleted from config (`command_gone`), the clone is refused. If the command still exists but the template changed since the source session was created, the clone gets the *current* template. This is intentional — the clone is a fresh session with the current config, not a time machine.

8. **`sessions *` vs spammy channels**: In channels with many users and long session histories, `sessions *` could return a lot of lines. The existing `SessionsDisplayLimit` config applies. Consider whether the limit should be per-user or total — for `sessions *` it should be a total limit across all users.

## Future Extensions (not in this change)

- `history <id>` for other users' sessions (read-only access to any session in the channel)
- `clone` with options (e.g., `clone <id> --include-archived`)
- Per-command `allow_clone` config to opt specific personas out of being cloned
- Website clone from admin panel (Phase 6)
