# turnContext Accumulator Design

## Problem

During an LLM turn, messages exist in two places: an in-memory `[]ChatMessage` slice
(passed through function calls) and the database (via `cr.addContext()` / `sessionMgr.AddMessage()`).
Every new message must be both appended to the slice AND persisted to DB. These are two separate
steps, and forgetting the DB step causes silent data loss — the LLM sees the message during the
turn (via the in-memory slice) but the next turn won't (since it reloads from DB).

Bug example: `handleBanUser` and `handleCheckBanHistory` appended tool results to the in-memory
slice but never called `cr.addContext()`. This caused `sessionHasIncompleteToolCalls()` to
correctly report sessions as having unmatched tool calls, blocking clone operations.

## Current Pattern (error-prone)

```go
// Two separate steps — easy to forget one:
messages = append(messages, toolMsg)  // step 1: in-memory
cr.addContext(toolMsg)                // step 2: DB persist
```

This pattern appears ~20 times across the turn loop. A new builtin tool handler that
forgets step 2 compiles fine, works during the turn, and only manifests as a bug later.

## Design

### Core type

A `turnContext` owns the working message slice for a single turn. The **only** way to add
a message is `Add()`, which atomically appends AND persists:

```go
type turnContext struct {
    sessionID int64
    messages  []ChatMessage
}

func newTurnContext(sessionID int64, initial []ChatMessage) *turnContext {
    return &turnContext{sessionID: sessionID, messages: initial}
}

// Add appends msg to the working set AND persists to DB. The only way to
// add messages during a turn — structurally guarantees DB sync.
func (tc *turnContext) Add(msg ChatMessage) {
    tc.messages = append(tc.messages, msg)
    sessionMgr.AddMessage(tc.sessionID, msg)
}

// Messages returns the current working set (for building API params).
func (tc *turnContext) Messages() []ChatMessage {
    return tc.messages
}

// LastN returns the last n messages. Used by Responses API for
// tool-result-only input when previous_response_id is set.
func (tc *turnContext) LastN(n int) []ChatMessage {
    if n >= len(tc.messages) {
        return tc.messages
    }
    return tc.messages[len(tc.messages)-n:]
}
```

### New pattern (structurally safe)

```go
turn.Add(toolMsg)  // one call, both steps guaranteed
```

### Function signature changes

`[]ChatMessage` parameters and returns replaced with `*turnContext`:

| Function | Before | After |
|---|---|---|
| `runTurn` | `(messages []ChatMessage) ([]ChatMessage, bool)` | `(turn *turnContext) bool` |
| `runTurnStream` | `(ctx, params, messages, ...) ([]ChatMessage, bool, int)` | `(ctx, params, turn, ...) (bool, int)` |
| `runTurnResponses` | `(messages []ChatMessage) ([]ChatMessage, bool)` | `(turn *turnContext) bool` |
| `runTurnResponsesStream` | `(ctx, params, messages, ...) responsesStreamResult` | `(ctx, params, turn, ...) responsesStreamResult` |
| `executeToolCalls` | `(messages, toolCalls) ([]ChatMessage, bool)` | `(turn, toolCalls) bool` |
| `handleToolCallResponse` | `(messages, text, toolCalls, ...) []ChatMessage` | `(turn, text, toolCalls, ...)` |
| `handleBanUser` | `(messages, tc) []ChatMessage` | `(turn, call)` |
| `handleCheckBanHistory` | `(messages, tc) []ChatMessage` | `(turn, call)` |
| `handleRegisterBackgroundJob` | `(messages, tc) []ChatMessage` | `(turn, call) bool` |

Builtin tool entry type:

```go
// before:
handler func(cr *chatRunner, messages []ChatMessage, tc ToolCall) ([]ChatMessage, bool)

// after:
handler func(cr *chatRunner, turn *turnContext, call ToolCall) bool
```

### chat() entry point

```go
// before:
messages, err := sessionMgr.GetMessages(runner.sessionID, cfg.MaxHistory)
messages, _ = runner.runTurn(messages)

// after:
messages, err := sessionMgr.GetMessages(runner.sessionID, cfg.MaxHistory)
turn := newTurnContext(runner.sessionID, messages)
runner.runTurn(turn)
```

### What stays the same

- **`cr.addContext()`** remains for pre-turn setup (system prompt, user message before the
  accumulator is created) and non-turn paths (async result injection in `jobManager.go`,
  compaction). These paths are already DB-only and don't use an in-memory slice.
- **`sessionMgr.AddMessage()`** unchanged — the accumulator calls it internally.
- **Error handling** — `Add()` logs errors the same way `addContext()` does (log, don't
  propagate). No error return from `Add()`.
- **Empty retry paths** — they `continue` without calling `Add()`, same as before.
- **DB as cross-turn source of truth** — unchanged. Accumulator is per-turn, discarded after.

### Conventions

After this refactor, `cr.addContext()` should **not** be called inside `runTurn`,
`executeToolCalls`, or any tool handler. All turn-time message additions go through
`turn.Add()`. If `addContext` appears in those functions, that's a bug.

### Files touched

- New file `turncontext.go` — the `turnContext` type and methods
- `aiCmds.go` — all function signatures and call sites listed above
- `aiCmds_test.go` — update tests to use `turnContext`
- `clone_test.go` — update handler tests

### Out of scope

- No DB schema changes
- No changes to `sessionManager.go`, `db.go`, `compaction.go`, `jobManager.go`
- No changes to async result injection or compaction (already DB-only)
- No performance changes — same number of DB writes per turn
