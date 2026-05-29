# Mention Spam Protection

## Summary

Track per-user nick mentions that result in a `no_context` response. After a configurable threshold of no-context mentions, mute the user's ability to get responses via nick mention. This is a "mention mute" — not a full bot ban. Trigger commands continue to work normally throughout.

## Behavior

```
1st no-context mention → no_context notice (with pastebin help link)
                         counter = 1

2nd no-context mention → no_context notice + muted notice
                         muted = true

Subsequent mentions    → silently dropped
                         mute persists until user starts a session

Any trigger command    → resets counter + lifts mute
Mention with session   → resets counter + lifts mute
```

- "no-context mention" = a nick mention where the user has no active chat session (hits the existing `!ContextExists` check in `handleMention`)
- "Mention with session" = a nick mention where the user has an active session (proceeds to queue processing)
- "Trigger command" = any command via the trigger prefix (e.g. `!help`, `!chat`, `!stop`)

## Configuration

New `[mention_spam]` section in `config/config.toml`:

```toml
[mention_spam]
threshold = 2   # no-context mentions before mute (int, default 2)
```

Only one field. No time window — the counter persists until reset by a successful interaction or bot restart. No mute duration — the mute persists until the user demonstrates they can use the bot (trigger command or mention with existing session).

### Config struct

```go
type MentionSpamConfig struct {
    Threshold int `toml:"threshold"`
}
```

Default applied in config loading: if `Threshold == 0`, set to `2`.

## Notice Templates

New `MentionNotices` struct in `notices.go`, under `[mentions]` in `notices.toml`.

### Templates

| Key | Placeholders | Default |
|-----|-------------|---------|
| `no_context` | `{trigger}`, `{help_url}` | `"You need to start a chat session first! See {help_url} for help. Once started, you can reply to my nick to continue the conversation."` |
| `muted` | `{trigger}` | `"Further mentions will be ignored until you start a session. Use {trigger}help to get started."` |

### Existing `no_context` migration

The current `Context.NoContext` notice (in `ContextNotices` struct) is replaced by `MentionNotices.NoContext`. The old field is removed from `ContextNotices` and its default in `setNoticesDefaults`. The new field lives in `MentionNotices` with the updated default text including `{help_url}`.

### Fallback when pastebin fails

If the pastebin upload fails when building the `no_context` message, `{help_url}` is replaced with `"{trigger}help"` (the old instruction). This ensures the notice always has actionable guidance.

## Data Structure

New file: `mention_tracker.go`

```go
type mentionTracker struct {
    mu    sync.Mutex
    users map[string]*mentionState
}

type mentionState struct {
    count int
    muted bool
}
```

- Key: `network:userID` (string concatenation using resolved `User.ID`)
- Uses resolved user ID (not nick) — survives nick changes
- In-memory only — lost on bot restart (acceptable for this transient state)

### Methods

- `recordMention(network string, userID int64) (count int, muted bool)` — increments count, returns current count and whether muted
- `isMuted(network string, userID int64) bool` — checks if user is muted
- `setMuted(network string, userID int64)` — sets muted=true for user
- `reset(network string, userID int64)` — clears count and mute for user (used when user successfully interacts)
- `sweep()` — removes entries where user is not muted and count is 0 (or just all non-muted entries periodically)

### GC

A background goroutine (started alongside the existing rate-limit sweeper) calls `sweep()` every 10 minutes to remove stale non-muted entries. Muted entries are never auto-removed — they persist until the user triggers a reset via a successful interaction.

## Integration Points

### 1. `handleMention` — no-context branch (`irc_handlers.go`)

Currently in `handleMention`, when `!ContextExists(network, channel, userID)`:

```go
// existing code sends no_context notice and returns
```

New behavior:
1. Check `mentionTracker.isMuted(network, userID)` → if true, silently return (no notice at all)
2. Build and send the updated `no_context` notice with pastebin help link (see Pastebin Integration below)
3. Call `mentionTracker.recordMention(network, userID)` → get count
4. If count >= threshold: send the `muted` notice, then call `mentionTracker.setMuted(network, userID)`

With threshold=2: 1st mention sends no_context only. 2nd mention sends no_context + muted notice + sets muted. 3rd+ mentions silently dropped.

### 2. `handleMention` — success branch (has context)

After the ban check passes (line ~183), before rate limiting:

```go
mentionTracker.reset(network, userID)
```

### 3. `handleTrigger` — after successful command dispatch

In the trigger path (`irc_handlers.go`), after user resolution succeeds and the command is dispatched:

```go
mentionTracker.reset(network, userID)
```

This goes in the trigger handler after `resolveIRCUser` succeeds, alongside the ban check area.

### 4. `handleChanMessage` — before mention dispatch

The mute check must happen before the mention is processed. The flow in `handleChanMessage` calls `handleMention`, so the mute check inside `handleMention` is sufficient.

## Pastebin Integration

When building the `no_context` notice:

1. Call the help text builder (extract a helper from `help()` in `help.go` that returns the raw help text string without sending it)
2. Upload to pastebin via `uploadToPastebin()` with title `"Dave Help"`
3. If upload succeeds: `{help_url}` = the pastebin URL
4. If upload fails: `{help_url}` = `"{trigger}help"` (fallback)

The help text generation must be extracted into a reusable function since currently `help()` builds and sends the output directly. The extracted function returns the full help text as a string.

## Files Changed

| File | Change |
|------|--------|
| `mention_tracker.go` | **New** — tracker struct, methods, GC |
| `config.go` | Add `MentionSpamConfig` struct, field on `Config` |
| `notices.go` | Add `MentionNotices` struct, remove `Context.NoContext`, update defaults |
| `irc_handlers.go` | Integrate tracker in `handleMention` (both branches) and `handleTrigger` |
| `help.go` | Extract help text builder into reusable function |
| `main.go` | Start GC goroutine, initialize tracker global |
| `config/config.toml` | Add `[mention_spam]` section with docs |
| `config/notices.toml` | Add `[mentions]` section, remove old `[context]` no_context |
| `mention_tracker_test.go` | **New** — tests for tracker logic |
| `config_test.go` | Add test for `MentionSpamConfig` defaults |
| `notices_test.go` | Update tests for new notice structure |

## Testing

### Unit tests

1. **`mention_tracker_test.go`**:
   - `recordMention` increments count correctly
   - `isMuted` returns false initially, true after `setMuted`
   - `reset` clears count and mute
   - `sweep` removes non-muted entries, keeps muted ones
   - Thread safety (concurrent `recordMention` + `reset`)

2. **`config_test.go`**:
   - `MentionSpamConfig.Threshold` defaults to 2 when zero

3. **`notices_test.go`**:
   - `MentionNotices.NoContext` default includes `{help_url}`
   - `MentionNotices.Muted` default includes `{trigger}`
   - Old `Context.NoContext` field removed

### Integration tests

- Test the full flow: mention → mention → muted → mention (silent) → trigger → mention (un-muted)
