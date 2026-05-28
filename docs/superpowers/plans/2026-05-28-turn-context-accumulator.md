# turnContext Accumulator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the error-prone `append(messages, msg) + cr.addContext(msg)` two-step pattern with a `turnContext` accumulator that atomically appends and persists, making it structurally impossible to add a message during a turn without persisting it.

**Architecture:** A new `turnContext` type owns the working `[]ChatMessage` slice for a single turn. Its `Add()` method both appends to the slice and persists to DB via `sessionMgr.AddMessage()`. All turn-time functions (`runTurn`, `executeToolCalls`, handlers) receive `*turnContext` instead of `[]ChatMessage`. The DB remains the cross-turn source of truth.

**Tech Stack:** Go 1.25, GORM, existing `sessionMgr.AddMessage()`.

**Design doc:** `docs/superpowers/specs/2026-05-28-turn-context-accumulator-design.md`

---

## File Structure

| File | Responsibility |
|---|---|
| `turncontext.go` (new) | `turnContext` type: `newTurnContext`, `Add`, `Messages`, `LastN` |
| `aiCmds.go` | All turn-loop functions: signatures + bodies refactored to use `*turnContext` |
| `jobManager.go:158-162` | `chatRunnerInterface` — `runTurn` signature change |
| `aiCmds_test.go` | Update tests to create `turnContext` instances |
| `clone_test.go` | Update handler tests to use `turnContext` |
| `jobManager_test.go` | Update `mockChatRunner.runTurn` signature |

---

### Task 1: Create `turnContext` type

**Files:**
- Create: `turncontext.go`
- Test: `turncontext_test.go` (new)

- [ ] **Step 1: Write failing tests for turnContext**

Create `turncontext_test.go`:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTurnContext_Add_AppendsAndPersists(t *testing.T) {
	setupTestDB(t)

	sid, err := sessionMgr.CreateSession("net", "#c", 1, "cmd", "svc", "m")
	require.NoError(t, err)

	tc := newTurnContext(sid, nil)
	tc.Add(ChatMessage{Role: RoleUser, Content: "hello"})
	tc.Add(ChatMessage{Role: RoleAssistant, Content: "world"})

	assert.Len(t, tc.Messages(), 2)
	assert.Equal(t, "hello", tc.Messages()[0].Content)
	assert.Equal(t, "world", tc.Messages()[1].Content)

	dbMsgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	assert.Len(t, dbMsgs, 2)
	assert.Equal(t, "hello", dbMsgs[0].Content)
	assert.Equal(t, "world", dbMsgs[1].Content)
}

func TestTurnContext_Add_WithInitialMessages(t *testing.T) {
	setupTestDB(t)

	sid, err := sessionMgr.CreateSession("net", "#c", 1, "cmd", "svc", "m")
	require.NoError(t, err)
	sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"})

	dbMsgs, _ := loadDBSessionMessages(sid)
	tc := newTurnContext(sid, dbMsgs)

	tc.Add(ChatMessage{Role: RoleUser, Content: "hi"})

	assert.Len(t, tc.Messages(), 2)
	assert.Equal(t, "sys", tc.Messages()[0].Content)
	assert.Equal(t, "hi", tc.Messages()[1].Content)

	allMsgs, _ := loadDBSessionMessages(sid)
	assert.Len(t, allMsgs, 2, "initial messages should not be re-persisted")
}

func TestTurnContext_Messages_ReturnsSlice(t *testing.T) {
	tc := newTurnContext(1, []ChatMessage{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "hi"},
	})
	assert.Len(t, tc.Messages(), 2)
}

func TestTurnContext_LastN(t *testing.T) {
	tc := newTurnContext(1, []ChatMessage{
		{Role: RoleSystem, Content: "s"},
		{Role: RoleUser, Content: "u"},
		{Role: RoleAssistant, Content: "a"},
	})

	last2 := tc.LastN(2)
	assert.Len(t, last2, 2)
	assert.Equal(t, "u", last2[0].Content)
	assert.Equal(t, "a", last2[1].Content)

	last5 := tc.LastN(5)
	assert.Len(t, last5, 3, "LastN should return all if n > len")
}

func TestTurnContext_Add_WithToolCalls(t *testing.T) {
	setupTestDB(t)

	sid, err := sessionMgr.CreateSession("net", "#c", 1, "cmd", "svc", "m")
	require.NoError(t, err)

	tc := newTurnContext(sid, nil)
	tc.Add(ChatMessage{
		Role:      RoleAssistant,
		ToolCalls: []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "test_tool", Arguments: "{}"}}},
	})
	tc.Add(ChatMessage{Role: RoleTool, Content: "result", ToolCallID: "call_1"})

	assert.Len(t, tc.Messages(), 2)

	incomplete, err := sessionHasIncompleteToolCalls(sid)
	require.NoError(t, err)
	assert.False(t, incomplete, "tool calls persisted via Add should be complete")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -v -run "TestTurnContext" ./... 2>&1 | head -20`
Expected: Compilation error — `turnContext` undefined.

- [ ] **Step 3: Implement turnContext**

Create `turncontext.go`:

```go
package main

type turnContext struct {
	sessionID int64
	messages  []ChatMessage
}

func newTurnContext(sessionID int64, initial []ChatMessage) *turnContext {
	return &turnContext{
		sessionID: sessionID,
		messages:  initial,
	}
}

func (tc *turnContext) Add(msg ChatMessage) {
	tc.messages = append(tc.messages, msg)
	sessionMgr.AddMessage(tc.sessionID, msg)
}

func (tc *turnContext) Messages() []ChatMessage {
	return tc.messages
}

func (tc *turnContext) LastN(n int) []ChatMessage {
	if n >= len(tc.messages) {
		return tc.messages
	}
	return tc.messages[len(tc.messages)-n:]
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v -run "TestTurnContext" ./...`
Expected: All 5 tests PASS.

- [ ] **Step 5: Run full suite**

Run: `go test ./...`
Expected: All tests pass (no existing code uses `turnContext` yet).

- [ ] **Step 6: Commit**

```bash
git add turncontext.go turncontext_test.go
git commit -m "feat: add turnContext accumulator type with tests"
```

---

### Task 2: Refactor `executeToolCalls` and builtin tool handlers

This is the core change — the functions where the bug class exists. Once these use `turnContext`, forgetting to persist is structurally impossible.

**Files:**
- Modify: `aiCmds.go` (lines 872-1170, 1588-1662)
- Modify: `clone_test.go` (handler tests)

- [ ] **Step 1: Refactor `executeToolCalls` signature and body**

Change `aiCmds.go:872`:

From:
```go
func (cr *chatRunner) executeToolCalls(messages []ChatMessage, toolCalls []ToolCall) ([]ChatMessage, bool) {
```
To:
```go
func (cr *chatRunner) executeToolCalls(turn *turnContext, toolCalls []ToolCall) bool {
```

Replace every `messages = append(messages, toolMsg)` + `cr.addContext(toolMsg)` pair with `turn.Add(toolMsg)`. Change `entry.handler(cr, messages, tc)` to `entry.handler(cr, turn, tc)`. Change final return from `return messages, registeredJob` to `return registeredJob`.

Specific lines:
- Line 910-911: `turn.Add(toolMsg)` (disabled builtin)
- Line 919: `registered = entry.handler(cr, turn, tc)`
- Line 928-929: `turn.Add(toolMsg)` (hidden MCP)
- Line 938-939: `turn.Add(toolMsg)` (arg parse error)
- Line 946-947: `turn.Add(toolMsg)` (MCP error)
- Line 953-954: `turn.Add(toolMsg)` (MCP success)
- Line 956: `return registeredJob`

- [ ] **Step 2: Refactor `handleToolCallResponse` signature and body**

Change `aiCmds.go:413`:

From:
```go
func (cr *chatRunner) handleToolCallResponse(messages []ChatMessage, text string, toolCalls []ToolCall, reasoning string, encryptedReasoning string) []ChatMessage {
```
To:
```go
func (cr *chatRunner) handleToolCallResponse(turn *turnContext, text string, toolCalls []ToolCall, reasoning string, encryptedReasoning string) {
```

- Line 422-423: Replace `messages = append(messages, assistantMsg)` + `cr.addContext(assistantMsg)` with `turn.Add(assistantMsg)`
- Line 434: `cr.executeToolCalls(turn, toolCalls)`
- Line 435: Remove `return messages` (void function now)

- [ ] **Step 3: Refactor `handleBanUser` signature and body**

Change signature from `func (cr *chatRunner) handleBanUser(messages []ChatMessage, tc ToolCall) []ChatMessage` to `func (cr *chatRunner) handleBanUser(turn *turnContext, call ToolCall)`.

For every return path: replace `toolMsg := ... / messages = append(messages, toolMsg) / cr.addContext(toolMsg) / return messages` with `turn.Add(toolMsg) / return`.

- [ ] **Step 4: Refactor `handleCheckBanHistory` signature and body**

Same pattern as `handleBanUser`. Change signature from `func (cr *chatRunner) handleCheckBanHistory(messages []ChatMessage, tc ToolCall) []ChatMessage` to `func (cr *chatRunner) handleCheckBanHistory(turn *turnContext, call ToolCall)`.

Every return path: `turn.Add(toolMsg)` then `return`.

- [ ] **Step 5: Refactor `handleRegisterBackgroundJob` signature and body**

Change signature from `func (cr *chatRunner) handleRegisterBackgroundJob(messages []ChatMessage, tc ToolCall) []ChatMessage` to `func (cr *chatRunner) handleRegisterBackgroundJob(turn *turnContext, call ToolCall) bool`.

Every return path: `turn.Add(toolMsg)` then `return true`.

- [ ] **Step 6: Update `builtinTools` map handler closures**

Update `aiCmds.go:1588-1662`. The `builtinTools` map entries wrap the handlers. Change:

```go
// before (line ~1590):
"register_background_job": {
    handler: func(cr *chatRunner, messages []ChatMessage, tc ToolCall) ([]ChatMessage, bool) {
        return cr.handleRegisterBackgroundJob(messages, tc), true
    },
    ...
},
"ban_user": {
    handler: func(cr *chatRunner, messages []ChatMessage, tc ToolCall) ([]ChatMessage, bool) {
        return cr.handleBanUser(messages, tc), false
    },
    ...
},
"check_ban_history": {
    handler: func(cr *chatRunner, messages []ChatMessage, tc ToolCall) ([]ChatMessage, bool) {
        return cr.handleCheckBanHistory(messages, tc), false
    },
    ...
},
```

To:
```go
"register_background_job": {
    handler: func(cr *chatRunner, turn *turnContext, call ToolCall) bool {
        cr.handleRegisterBackgroundJob(turn, call)
        return true
    },
    ...
},
"ban_user": {
    handler: func(cr *chatRunner, turn *turnContext, call ToolCall) bool {
        cr.handleBanUser(turn, call)
        return false
    },
    ...
},
"check_ban_history": {
    handler: func(cr *chatRunner, turn *turnContext, call ToolCall) bool {
        cr.handleCheckBanHistory(turn, call)
        return false
    },
    ...
},
```

Also update the `builtinToolEntry` handler type if it's declared separately (check the struct — the `handler` field type changes from `func(cr *chatRunner, messages []ChatMessage, tc ToolCall) ([]ChatMessage, bool)` to `func(cr *chatRunner, turn *turnContext, call ToolCall) bool`).

- [ ] **Step 7: Update clone_test.go handler tests**

In `TestHandleBanUser_PersistsToolResult` and `TestHandleCheckBanHistory_PersistsToolResult`:

Replace:
```go
msgs := cr.handleBanUser(nil, tc)
require.Len(t, msgs, 1)
assert.Equal(t, "tool", msgs[0].Role)
```

With:
```go
turn := newTurnContext(sid, nil)
cr.handleBanUser(turn, tc)
require.Len(t, turn.Messages(), 1)
assert.Equal(t, "tool", turn.Messages()[0].Role)
```

Same pattern for `handleCheckBanHistory`.

- [ ] **Step 8: Run tests**

Run: `go test ./... 2>&1 | tail -15`
Expected: All tests pass. This step will fail compilation if any call site was missed.

- [ ] **Step 9: Run go fmt + go vet**

Run: `go fmt ./... && go vet ./...`
Expected: No output (clean).

- [ ] **Step 10: Commit**

```bash
git add aiCmds.go clone_test.go
git commit -m "refactor: migrate executeToolCalls and builtin handlers to turnContext"
```

---

### Task 3: Refactor `runTurn`, `runTurnStream`, and streaming paths

This threads `turnContext` through the Chat Completions turn loop.

**Files:**
- Modify: `aiCmds.go` (lines 413-870)

- [ ] **Step 1: Refactor `runTurn`**

Change signature from `func (cr *chatRunner) runTurn(messages []ChatMessage) ([]ChatMessage, bool)` to `func (cr *chatRunner) runTurn(turn *turnContext) bool`.

Changes inside:
- Line 786: `return cr.runTurnResponses(turn)` (will fail until Task 4, comment out temporarily or do both tasks)
- Line 803: `return true`
- Line 806: `buildChatCompletionParams(cr.cfg, turn.Messages(), mcpTools, cr.renderAPIUser())`
- Line 810: `done, emptyRetries = cr.runTurnStream(ctx, params, turn, iterations, emptyRetries, maxEmptyRetries)`
- Line 814: `return true`
- Line 824: `cr.logAPIIncident(err, turn.Messages(), iterations, "chat_completions")`
- Line 825: `return true`
- Lines 848-852: `turn.Add(ChatMessage{Role: RoleAssistant, Content: content, ReasoningContent: reasoning})` (replaces `cr.addContext(...)`)
- Line 862: `return true`
- Line 868: `cr.handleToolCallResponse(turn, content, toolCalls, reasoning, "")`

- [ ] **Step 2: Refactor `runTurnStream`**

Change signature from `func (cr *chatRunner) runTurnStream(ctx context.Context, params openai.ChatCompletionNewParams, messages []ChatMessage, iterations, emptyRetries, maxEmptyRetries int) ([]ChatMessage, bool, int)` to `func (cr *chatRunner) runTurnStream(ctx context.Context, params openai.ChatCompletionNewParams, turn *turnContext, iterations, emptyRetries, maxEmptyRetries int) (bool, int)`.

Changes inside:
- All `return messages, true, emptyRetries` → `return true, emptyRetries`
- All `return messages, false, emptyRetries` → `return false, emptyRetries`
- Line 625: `cr.logAPIIncident(res.err, turn.Messages(), ...)` (read-only)
- Line 709: `cr.logAPIIncident(timeoutErr, turn.Messages(), ...)` (read-only)
- Line 727-731 (flushStreamedOutput closure): `turn.Add(ChatMessage{Role: RoleAssistant, Content: content, ReasoningContent: reasoningBuffer})` replaces `cr.addContext(...)`
- Line 768: Remove `messages = append(messages, assistantMsg)` — handled by `turn.Add` on next line
- Line 778: `turn.Add(assistantMsg)` replaces `cr.addContext(assistantMsg)`
- Line 780: `cr.executeToolCalls(turn, accumulatedToolCalls)`

- [ ] **Step 3: Compile check**

Run: `go build .`
Expected: Will fail on `runTurnResponses` call in `runTurn` (Task 4) and `chat()` call site (Task 5). These are expected. Fix only compilation errors in the functions changed in this task by temporarily adding adapter code if needed, or proceed to Task 4 immediately.

- [ ] **Step 4: Commit**

```bash
git add aiCmds.go
git commit -m "refactor: migrate runTurn and runTurnStream to turnContext"
```

---

### Task 4: Refactor Responses API paths

Thread `turnContext` through `runTurnResponses` and `runTurnResponsesStream`.

**Files:**
- Modify: `aiCmds.go` (lines 468-565, 1172-1283)

- [ ] **Step 1: Remove `messages` field from `responsesStreamResult`**

In the struct at line 468:
```go
type responsesStreamResult struct {
    done              bool
    emptyRetries      int
    currentResponseID string
    usePrevID         bool
    input             []responses.ResponseInputItemUnionParam
}
```

Remove the `messages []ChatMessage` field. All struct literals that set `messages: messages` or `messages: r.messages` need the field removed.

- [ ] **Step 2: Refactor `runTurnResponsesStream`**

Change signature from `func (cr *chatRunner) runTurnResponsesStream(ctx context.Context, params responses.ResponseNewParams, messages []ChatMessage, iteration int, emptyRetries int, maxEmptyRetries int, currentResponseID string, usePrevID bool) responsesStreamResult` to `func (cr *chatRunner) runTurnResponsesStream(ctx context.Context, params responses.ResponseNewParams, turn *turnContext, iteration int, emptyRetries int, maxEmptyRetries int, currentResponseID string, usePrevID bool) responsesStreamResult`.

Changes inside:
- Line 491: `cr.shouldRetryWithoutResponseID(usePrevID, err, turn.Messages(), ...)`
- Lines 493, 503, 507, 522, 539, 560: Remove `messages: messages,` from result struct literals
- Line 502: `cr.logAPIIncident(err, turn.Messages(), ...)`
- Line 526: `messagesToResponseInputItems(turn.Messages())`
- Lines 532-533: Remove `messages = append(messages, assistantMsg)`, change `cr.addContext(assistantMsg)` to `turn.Add(assistantMsg)`
- Lines 544-545: Same — remove append, change addContext to `turn.Add`
- Line 550: `cr.executeToolCalls(turn, toolCalls)`
- Line 554: `turn.LastN(numToolCalls)` replaces `messages[len(messages)-numToolCalls:]`
- Line 557: `messagesToResponseInputItems(turn.Messages())`

- [ ] **Step 3: Refactor `runTurnResponses`**

Change signature from `func (cr *chatRunner) runTurnResponses(messages []ChatMessage) ([]ChatMessage, bool)` to `func (cr *chatRunner) runTurnResponses(turn *turnContext) bool`.

Changes inside:
- Line 1191: `if len(turn.Messages()) > 0 {`
- Line 1192: `messagesToResponseInputItems(turn.LastN(1))`
- Line 1195: `messagesToResponseInputItems(turn.Messages())`
- Line 1209: `return true`
- Line 1215: `cr.runTurnResponsesStream(ctx, params, turn, ...)`
- Line 1216: Remove `messages = r.messages`
- Line 1224: `return true`
- Line 1232: `cr.shouldRetryWithoutResponseID(usePrevID, err, turn.Messages(), ...)`
- Line 1239: `cr.logAPIIncident(err, turn.Messages(), ...)`
- Line 1240: `return true`
- Line 1261: `turn.Add(assistantMsg)` replaces `cr.addContext(assistantMsg)`
- Line 1271: `return true`
- Line 1277: `cr.handleToolCallResponse(turn, text, toolCalls, reasoning, encReasoning)`
- Line 1280: `turn.LastN(numToolCalls)`
- Line 1283: `messagesToResponseInputItems(turn.Messages())`

- [ ] **Step 4: Compile and test**

Run: `go build . && go test ./...`
Expected: Build succeeds, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add aiCmds.go
git commit -m "refactor: migrate runTurnResponses and runTurnResponsesStream to turnContext"
```

---

### Task 5: Update `chat()` entry point and `chatRunnerInterface`

Wire the accumulator into `chat()` and the async job delivery path.

**Files:**
- Modify: `aiCmds.go` (lines 1523-1556, the `chat()` function)
- Modify: `jobManager.go` (line 160, `chatRunnerInterface`)

- [ ] **Step 1: Update `chat()` main turn call**

In `aiCmds.go`, change lines around 1523-1531:

From:
```go
messages, err := sessionMgr.GetMessages(runner.sessionID, cfg.MaxHistory)
if err != nil {
    runner.logger.Error("failed to load messages", "error", err)
    runner.sendError("failed to load conversation history")
    return
}
runner.logger.Debug("running completion", "summary", summarizeMessages(messages))
messages, _ = runner.runTurn(messages)
```

To:
```go
messages, err := sessionMgr.GetMessages(runner.sessionID, cfg.MaxHistory)
if err != nil {
    runner.logger.Error("failed to load messages", "error", err)
    runner.sendError("failed to load conversation history")
    return
}
runner.logger.Debug("running completion", "summary", summarizeMessages(messages))
turn := newTurnContext(runner.sessionID, messages)
runner.runTurn(turn)
```

- [ ] **Step 2: Update `chat()` async job loop**

Change lines 1549-1554:

From:
```go
messages, err = sessionMgr.GetMessages(runner.sessionID, cfg.MaxHistory)
if err != nil {
    runner.logger.Error("failed to reload messages after job delivery", "error", err)
    break
}
messages, _ = runner.runTurn(messages)
```

To:
```go
messages, err = sessionMgr.GetMessages(runner.sessionID, cfg.MaxHistory)
if err != nil {
    runner.logger.Error("failed to reload messages after job delivery", "error", err)
    break
}
turn = newTurnContext(runner.sessionID, messages)
runner.runTurn(turn)
```

`injectAsyncResultFromDB` still writes directly to DB (it doesn't have access to the turn, and the turn is discarded/recreated here anyway). The new turn loads the injected messages from DB.

- [ ] **Step 3: Update `chatRunnerInterface` in `jobManager.go`**

Change line 160 from:
```go
runTurn(messages []ChatMessage) ([]ChatMessage, bool)
```
To:
```go
runTurn(turn *turnContext) bool
```

- [ ] **Step 4: Update `deliverAsyncResult` in `jobManager.go`**

Change lines 359-361:

From:
```go
messages, _ := sessionMgr.GetMessages(entry.payload.sessionID, currentCfg.MaxHistory)
var done bool
messages, done = runner.runTurn(messages)
```

To:
```go
messages, _ := sessionMgr.GetMessages(entry.payload.sessionID, currentCfg.MaxHistory)
turn := newTurnContext(entry.payload.sessionID, messages)
done := runner.runTurn(turn)
```

- [ ] **Step 5: Update `mockChatRunner` in `jobManager_test.go`**

Change line 23:
```go
runTurnFn func(messages []ChatMessage) ([]ChatMessage, bool)
```
To:
```go
runTurnFn func(turn *turnContext) bool
```

Change `runTurn` method (lines 35-41):
```go
func (m *mockChatRunner) runTurn(turn *turnContext) bool {
    m.runTurnCalled++
    if m.runTurnFn != nil {
        return m.runTurnFn(turn)
    }
    return true
}
```

Update all mock `runTurnFn` closures in tests that reference `messages`:

In each test that sets `runTurnFn`, the closure signature changes from `func(messages []ChatMessage) ([]ChatMessage, bool)` to `func(turn *turnContext) bool`. If the closure reads messages, use `turn.Messages()`.

- [ ] **Step 6: Update `aiCmds_test.go` test call sites**

Update all places that call `executeToolCalls` or `runTurn` directly:

- Lines 67, 120, 177: `go cr.executeToolCalls(nil, toolCalls)` → create a `turnContext` and pass it:
  ```go
  turn := newTurnContext(cr.sessionID, nil)
  go cr.executeToolCalls(turn, toolCalls)
  ```

- Lines 366, 375, 470, 478: `runner.runTurn(messages)` → `runner.runTurn(newTurnContext(runner.sessionID, messages))`

- Lines 682, 695, 708, 718, 728, 738: `msgs := cr.handleRegisterBackgroundJob(nil, tc)` → `cr.handleRegisterBackgroundJob(newTurnContext(cr.sessionID, nil), tc)` — verify via DB or turnContext instead of return value.

- [ ] **Step 7: Compile and test**

Run: `go fmt ./... && go vet ./... && go build . && go test ./...`
Expected: All pass.

- [ ] **Step 8: Commit**

```bash
git add aiCmds.go jobManager.go aiCmds_test.go jobManager_test.go
git commit -m "refactor: wire turnContext into chat(), chatRunnerInterface, and async delivery"
```

---

### Task 6: Remove dead `addContext` calls and clean up

Audit for any remaining `cr.addContext()` calls inside turn-time functions that should now use `turn.Add()`. Remove unused code.

**Files:**
- Modify: `aiCmds.go`

- [ ] **Step 1: Audit remaining `cr.addContext` calls in turn functions**

Search for any remaining `cr.addContext` inside: `runTurn`, `runTurnStream`, `runTurnResponses`, `runTurnResponsesStream`, `executeToolCalls`, `handleToolCallResponse`, `handleBanUser`, `handleCheckBanHistory`, `handleRegisterBackgroundJob`.

Run: `grep -n "cr\.addContext" aiCmds.go`
Expected: No hits inside those functions. Only pre-turn `addContext` calls should remain (system prompt, user message in `chat()`).

- [ ] **Step 2: Add comment to `addContext` explaining when to use it**

Add a comment above `addContext`:

```go
// addContext persists a message to the DB. Use ONLY for pre-turn setup (system
// prompt, user message) and non-turn paths (async injection, compaction).
// During a turn, use turnContext.Add() instead.
func (cr *chatRunner) addContext(msg ChatMessage) {
```

- [ ] **Step 3: Final test run**

Run: `go fmt ./... && go vet ./... && go test ./...`
Expected: All pass.

- [ ] **Step 4: Commit**

```bash
git add aiCmds.go
git commit -m "chore: audit addContext usage, add turn-only documentation"
```

---

## Self-Review

**Spec coverage:** Every section in the design doc maps to a task:
- turnContext type → Task 1
- executeToolCalls + handlers → Task 2
- runTurn + runTurnStream → Task 3
- Responses API paths → Task 4
- chat() + chatRunnerInterface → Task 5
- Cleanup + documentation → Task 6

**Placeholder scan:** No TBDs, TODOs, or "implement later". All steps have code.

**Type consistency:** `turnContext` methods are consistent across all tasks. Handler signatures match between Task 2 (definition) and Task 3/4 (callers). `chatRunnerInterface` updated in Task 5 matches `chatRunner` method in Task 3.
