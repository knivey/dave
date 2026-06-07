# TUI Inject System Message Commands

## Problem

There is no way to inject a mid-conversation system prompt into an existing session from the TUI. This is useful when:
- The system prompt template has been updated and the admin wants to push the new version into an active session
- The admin wants to steer an ongoing conversation with an arbitrary system message

The injected message must sit in the database and be delivered on the next API turn (user action or async tool delivery), without triggering an immediate API call.

## Design

Two new TUI commands in `tui_commands.go`, registered in the `tuiCommands` map, following the `/compact` pattern of session-ID-based targeting.

### `/reinject <session-id>`

Re-renders the session's system prompt template with fresh data (current channel nicks, date, etc.) and inserts it as a `RoleSystem` message at the end of the session history.

1. Parse session ID from args
2. Load session via `getDBSessionByID` — not found → error, no insert
3. Get bot via `getBot(session.Network)` — not found → error, no insert (bot not connected)
4. Validate bot is joined to `session.Channel` via `bot.Client.LookupChannel(session.Channel)` — nil → error, no insert (not in channel)
5. Get config via `getSessionConfig(session)`
6. Resolve user nick from `session.UserID` → `getUserByID` → `displayNick(u)`. Nil user ID or user not found → error, no insert
7. Call `renderFreshSystemPrompt(cfg, bot.Network, bot.Client, session.Channel, userNick, "")` — same function compaction uses
8. If the rendered content is empty (no template and no static system prompt) → error, no insert
9. Insert via `sessionMgr.AddMessage(sessionID, ChatMessage{Role: RoleSystem, Content: rendered})`
10. Clear `ResponseID` via `sessionMgr.UpdateResponseID(sessionID, nil)` — ensures delivery via Responses API
11. Print confirmation to `logView`
12. If session status is completed → print warning before insert (still insert)

### `/systemmsg <session-id> <template-text>`

Parses the rest-of-line after the session ID as a Go template, renders it with `SystemPromptData`, and inserts the result as a `RoleSystem` message.

1. Parse session ID, rest-of-line is the template text — empty text → error, no insert
2. Load session via `getDBSessionByID` — not found → error, no insert
3. Get bot via `getBot(session.Network)` — not found → error, no insert
4. Validate bot is joined to `session.Channel` via `bot.Client.LookupChannel(session.Channel)` — nil → error, no insert
5. Resolve user nick from `session.UserID` → `getUserByID` → `displayNick(u)`. Nil user ID or user not found → error, no insert
6. Parse text as Go template: `template.New("systemmsg").Parse(text)` — parse error → error, no insert
7. Build `SystemPromptData` via `buildSystemPromptData(bot.Network, bot.Client, session.Channel, userNick)`
8. Execute template with data — execute error → error, no insert
9. Insert via `sessionMgr.AddMessage(sessionID, ChatMessage{Role: RoleSystem, Content: rendered})`
10. Clear `ResponseID` via `sessionMgr.UpdateResponseID(sessionID, nil)`
11. Print confirmation to `logView`
12. If session status is completed → print warning before insert (still insert)

### Message Delivery Guarantee

Injected messages are regular `Message` rows in the database. On the next API turn:

- **Chat Completions path**: `loadDBSessionMessages` returns all non-archived messages → injected message included in full history send
- **Responses API path (no `previous_response_id`)**: full history sent → injected message included
- **Responses API path (with `previous_response_id`)**: only `LastN(1)` would be sent, skipping the injected message. **Fix**: clearing `session.ResponseID` to nil forces the next turn to send full history instead of chaining. Costs one turn of prompt caching but guarantees delivery.

### Connected/Joined Validation

Both commands require the bot to be connected to the session's network and joined to the session's channel. This ensures template data (`ChanNicks`, `BotNick`) is accurate. If either check fails, the command errors out with no insert.

### Compaction Interaction

Compaction already handles multiple `RoleSystem` rows (the summary is `RoleSystem`). No changes needed:

- Injected message in archived range → summarized, content preserved in summary
- Injected message in preserved tail → re-inserted verbatim as a fresh row

### Edge Cases

| Condition | Behavior |
|-----------|----------|
| Session not found | Error, no insert |
| Session completed | Warning + insert |
| Bot not connected to network | Error, no insert |
| Bot not joined to channel | Error, no insert |
| No system prompt template or static string (`/reinject`) | Error, no insert |
| Empty template text (`/systemmsg`) | Error, no insert |
| Template parse error (`/systemmsg`) | Error, no insert |
| Template execute error (`/systemmsg`) | Error, no insert |
| User ID nil or user not found | Error, no insert |

### Files Changed

- `tui_commands.go`: Add `tuiCmdReinject` and `tuiCmdSystemMsg` handlers, register in `tuiCommands` map, update help text
- No schema changes, no changes to existing code paths

### Help Text Addition

```
/reinject <session-id>              - Re-render and inject system prompt at end of session
/systemmsg <session-id> <text>      - Inject custom system message (Go template) into session
```
