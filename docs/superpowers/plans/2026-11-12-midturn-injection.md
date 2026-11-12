# Mid-Turn Message Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Same-session continuation messages arriving while a session's turn is running are absorbed into that live turn (persisted as plain user rows after the pending tool results) instead of queueing a second turn; async-delivery queue items stop emitting phantom "Processing your request" notices.

**Architecture:** A per-session `turnMailbox` registry hands messages from `handleMention` dispatch to the running turn. The turn drains the mailbox at the one seam all four loop variants share (`executeToolCalls`, after the existing `view_image` injection flush) and in a quiescent-close post-turn loop inside `chat()`. A `Silent` flag on `QueueItem` suppresses the started notice for async-delivery items.

**Tech Stack:** Go 1.25, testify, table-driven tests, no new dependencies.

**Spec:** `docs/superpowers/specs/2026-11-12-midturn-injection-design.md` (read it first — the plan argues from it)

## Global Constraints

- No config knob — absorption is always on for same-session mentions (spec: owner decision).
- Absorbed messages are plain `RoleUser` rows — no marker, no prefix.
- Every accepted `push` is drained or persisted exactly once — never lost, never duplicated.
- The receipt notice default is `"\x0314📎 {nick}: added to the running turn\x0f"`, key `absorbed` under `[mentions]`.
- Loggers created with `logxi.New()` must call `SetLevel(logxi.LevelAll)`.
- Tests use `github.com/stretchr/testify` (`assert`/`require`); helpers `setupTestDB`, `setupNoticesDefaults`, `ensureTestUser`, `makeTestAIConfig` live in `testHelpers_test.go`; `createTestSession`, `insertTestMessage`, `setupTestJobManager` in `jobManager_test.go`; `chatGateClient`, `chatGateEvent`, `chatGateUser`, `chatGateCompletion`, `setChatGateServices` in `sessionGate_test.go`; `newImageViewTestRunner`, `newTestImageServer` in `image_detect_test.go`; `setupDeliveryTest` in `queue_test.go`. All test code in this plan is same-package (`package main`).
- After every task: `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...` must pass. NEVER use `git add -f`.
- All work happens in the current worktree (already isolated).

## Review Focus

The five failure modes the spec implies but individual happy-path tests don't exercise, each pinned by a test in the owning task:

1. **Push racing `closeAndDrain` must be exactly-once** — a message accepted by `push` must appear in exactly one drain result, never zero, never twice. → `TestTurnMailboxPushVsCloseExactlyOnce` (Task 2).
2. **Image-budget failure must not drop an absorbed message** — chat() drops on limit failure, but an absorbed message was receipted; it must persist text-only. → `TestDrainAbsorbedOverBudgetPersistsTextOnly` (Task 6).
3. **Message absorbed during the final API response still gets a turn** — no tool round follows to drain it; the post-turn loop must. → `TestChatAbsorbsMessageDuringFinalResponse` (Task 7).
4. **Cancelled turn (^stop) with undrained absorbed messages** — persisted as text rows, nothing sent. → `TestChatCancelPersistsUndrainedAbsorbed` (Task 7).
5. **No-op async delivery must emit no started notice** (the actual production incident). → `TestOnAsyncJobCompletedEnqueuesSilentDelivery` + `shouldSendStarted` silent predicate (Task 1).

---

### Task 1: Silent queue items for async-result deliveries

The bounded side-fix from the spec. Async-delivery queue items inherit the generic
"▶ Processing your request (waited X)..." started notice which fires from the
delivery machinery before `deliverAsyncResult` can know it will no-op (first-wins
inline delivery). "Processing your request" is also semantically wrong — the user
made no request.

**Files:**
- Modify: `queue.go` (QueueItem struct ~line 14; runJob started-notice block ~line 535-543; new `EnqueueSilent` near `Enqueue` ~line 180)
- Modify: `jobManager.go:300` (`onAsyncJobCompletedWithMgr`) and `jobManager.go:636` (`recoverPendingJobs` completed-job branch)
- Test: `queue_test.go`, `jobManager_test.go`

**Interfaces:**
- Consumes: existing `Enqueue`/`enqueueItem`/`newQueueItem`.
- Produces: `QueueItem.Silent bool`; `func (qm *QueueManager) EnqueueSilent(network, channel string, userID int64, nick, service, desc string, fn func(ctx context.Context, output chan<- string)) int`; `func shouldSendStarted(item *QueueItem, now time.Time) bool` (used by runJob and Task 1's tests only).

- [ ] **Step 1: Write the failing tests**

Append to `queue_test.go`:

```go
// shouldSendStarted is the started-notice predicate runJob applies after
// acquiring the delivery turn (extracted so the silent-flag contract is
// directly testable): silent items never announce; non-silent items
// announce when they waited for delivery or when execution outran the
// 1s grace window.
func TestShouldSendStarted(t *testing.T) {
	now := time.Now()
	t.Run("silent never sends", func(t *testing.T) {
		item := &QueueItem{Silent: true, Enqueued: now.Add(-5 * time.Minute), deliveryWaited: true}
		assert.False(t, shouldSendStarted(item, now))
	})
	t.Run("non-silent waited sends", func(t *testing.T) {
		item := &QueueItem{Enqueued: now, deliveryWaited: true}
		assert.True(t, shouldSendStarted(item, now))
	})
	t.Run("non-silent slow start sends", func(t *testing.T) {
		item := &QueueItem{Enqueued: now.Add(-5 * time.Second)}
		assert.True(t, shouldSendStarted(item, now))
	})
	t.Run("non-silent fast does not send", func(t *testing.T) {
		item := &QueueItem{Enqueued: now}
		assert.False(t, shouldSendStarted(item, now))
	})
}

func TestEnqueueSilentMarksItem(t *testing.T) {
	qm := setupDeliveryTest(t, 1)

	ran := make(chan struct{})
	pos := qm.EnqueueSilent("net", "#chan", 1, "u", "svc", "",
		func(ctx context.Context, output chan<- string) { close(ran) })
	require.GreaterOrEqual(t, pos, 0, "item dropped at enqueue")

	qm.mu.Lock()
	var item *QueueItem
	for _, it := range qm.channels[channelKey("net", "#chan")].running {
		item = it
	}
	qm.mu.Unlock()
	require.NotNil(t, item)
	assert.True(t, item.Silent, "EnqueueSilent must mark the item silent")

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("silent item never executed")
	}
}
```

Append to `jobManager_test.go` (this is the incident regression — the delivery item MUST be silent):

```go
func TestOnAsyncJobCompletedEnqueuesSilentDelivery(t *testing.T) {
	setupTestDB(t)
	setupTestJobManager(t)

	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")
	require.NoError(t, createPendingJob(sid, "job-s1", "generate_image_async", "img-mcp"))
	require.NoError(t, completePendingJob("job-s1", "result text"))

	qm := setupDeliveryTest(t, 1)
	// Occupy the ""-service exec slot so the delivery item sits in pending
	// deterministically instead of racing through deliverAsyncResult.
	qm.UpdateServiceLimits(map[string]Service{"": {Parallel: 1}})
	block := make(chan struct{})
	qm.Enqueue("testnet", "#test", 1, "blocker", "", "",
		func(ctx context.Context, output chan<- string) { <-block })
	t.Cleanup(func() { close(block) })

	mgr := newGenericJobMgr[asyncJobPayload]()
	entry := &jobEntry[asyncJobPayload]{
		jobID:     "job-s1",
		payload:   asyncJobPayload{sessionID: sid},
		toolName:  "generate_image_async",
		mcpServer: "img-mcp",
		network:   "testnet",
		channel:   "#test",
		nick:      "testuser",
	}
	onAsyncJobCompletedWithMgr(mgr, qm, entry, "result text")

	qm.mu.Lock()
	cq := qm.channels[channelKey("testnet", "#test")]
	var item *QueueItem
	if cq != nil {
		for _, it := range cq.pending {
			item = it
		}
	}
	qm.mu.Unlock()
	require.NotNil(t, item, "delivery item was never enqueued")
	assert.True(t, item.Silent, "async delivery items must be silent (incident 2026-11-12: phantom Processing notices)")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestShouldSendStarted|TestEnqueueSilentMarksItem|TestOnAsyncJobCompletedEnqueuesSilentDelivery' . -v`
Expected: FAIL — `shouldSendStarted` / `EnqueueSilent` undefined.

- [ ] **Step 3: Implement**

In `queue.go`, add the field to `QueueItem` (after `deliveryWaited`, keeping unexported fields last):

```go
	// Silent suppresses the "Processing your request" started notice at
	// delivery time. Async-result delivery items set it: the user made no
	// request, the item may no-op (first-wins delivery elsewhere), and
	// everything a real delivery says (switch notice, commentary,
	// deferrals) flows through outputCh in order.
	Silent bool
```

Add the enqueue method next to `Enqueue` (~line 180):

```go
// EnqueueSilent is Enqueue for items whose delivery must not announce
// itself with the started notice (see QueueItem.Silent).
func (qm *QueueManager) EnqueueSilent(network, channel string, userID int64, nick, service, desc string, fn func(ctx context.Context, output chan<- string)) int {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	item := qm.newQueueItem(network, channel, userID, nick, service, desc, time.Now(), fn)
	if item == nil {
		return -1
	}
	item.Silent = true
	pos, _ := qm.enqueueItem(item)
	return pos
}
```

Extract the predicate near `runJob` and use it in `runJob`'s notice block (~line 540), replacing `if item.deliveryWaited || time.Since(item.Enqueued) > time.Second {`:

```go
// shouldSendStarted is the started-notice predicate runJob applies after
// acquiring the delivery turn.
func shouldSendStarted(item *QueueItem, now time.Time) bool {
	if item.Silent {
		return false
	}
	return item.deliveryWaited || now.Sub(item.Enqueued) > time.Second
}
```

```go
	if shouldSendStarted(item, time.Now()) {
```

In `jobManager.go`, `onAsyncJobCompletedWithMgr` (~line 300): change `qm.Enqueue(entry.network, ...)` to `qm.EnqueueSilent(entry.network, ...)`. In `recoverPendingJobs` (~line 636): change `queueMgr.Enqueue(session.Network, ...)` to `queueMgr.EnqueueSilent(session.Network, ...)`. Do NOT touch the `EnqueueAtWithPrompt` call (~line 741) — the image-gen boot-recovery path keeps its own "▶ Processed your image request..." override.

- [ ] **Step 4: Run tests and full suite**

Run: `go test -run 'TestShouldSendStarted|TestEnqueueSilentMarksItem|TestOnAsyncJobCompletedEnqueuesSilentDelivery' . -v` — expect PASS.
Run: `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...` — expect all pass.

- [ ] **Step 5: Commit**

```bash
git add queue.go jobManager.go queue_test.go jobManager_test.go
git commit -m "queue: silent started notice for async-delivery items"
```

---

### Task 2: The turnMailbox primitive

The concurrency core: one mailbox per session, race-freedom in one mutex.

**Files:**
- Create: `midturn.go`
- Create: `midturn_test.go`

**Interfaces:**
- Produces: `type turnMailbox struct`; `func openTurnMailbox(sessionID int64) *turnMailbox`; `func turnMailboxFor(sessionID int64) *turnMailbox` (nil when none); `func (mb *turnMailbox) push(msg string) bool`; `func (mb *turnMailbox) drain() []string`; `func (mb *turnMailbox) closeAndDrain() []string`; package var `turnMailboxes sync.Map`.

- [ ] **Step 1: Write the failing tests**

Create `midturn_test.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTurnMailboxPushDrainClose(t *testing.T) {
	mb := openTurnMailbox(42)
	t.Cleanup(func() { mb.closeAndDrain() })

	assert.True(t, mb.push("one"))
	assert.True(t, mb.push("two"))
	assert.Equal(t, []string{"one", "two"}, mb.drain())
	assert.Empty(t, mb.drain(), "drain clears pending")

	assert.Empty(t, mb.closeAndDrain())
	assert.False(t, mb.push("three"), "closed mailbox rejects pushes")
}

func TestTurnMailboxForUnknownSession(t *testing.T) {
	assert.Nil(t, turnMailboxFor(999999))
}

func TestTurnMailboxForReturnsOpened(t *testing.T) {
	mb := openTurnMailbox(7)
	t.Cleanup(func() { mb.closeAndDrain() })
	require.Same(t, mb, turnMailboxFor(7))
}

func TestTurnMailboxOpenReplacesStale(t *testing.T) {
	first := openTurnMailbox(7)
	assert.True(t, first.push("old"))
	second := openTurnMailbox(7)
	t.Cleanup(func() { second.closeAndDrain() })
	require.NotSame(t, first, second)
	assert.False(t, first.push("late"), "stale mailbox must reject after replacement")
	assert.Empty(t, second.drain(), "replacement starts fresh")
}

// Review Focus #1: every successful push appears in exactly one drain —
// the absorption obligation. Pushers race a single closeAndDrain; the
// count of accepted pushes must equal the returned stragglers (no drain
// happens before the close in this test), and no push may succeed after
// the close completed.
func TestTurnMailboxPushVsCloseExactlyOnce(t *testing.T) {
	mb := openTurnMailbox(11)
	const pushers = 32
	var accepted atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	closed := make(chan []string, 1)

	for i := 0; i < pushers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if mb.push(fmt.Sprintf("m%d", i)) {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		closed <- mb.closeAndDrain()
	}()
	close(start)
	wg.Wait()

	drained := <-closed
	assert.Equal(t, accepted.Load(), int64(len(drained)),
		"accepted pushes must equal drained messages — exactly once")
	assert.False(t, mb.push("late"), "push after close must fail")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestTurnMailbox . -v`
Expected: FAIL — `turnMailbox` undefined.

- [ ] **Step 3: Implement**

Create `midturn.go`:

```go
package main

import (
	"sync"
)

// Mid-turn message absorption (spec:
// docs/superpowers/specs/2026-11-12-midturn-injection-design.md).
//
// A same-session continuation (mention) arriving while its session's turn
// is running is handed to that live turn instead of queueing a second
// turn behind it: the running model is blind to queued input (it keeps
// executing a plan the user already superseded), and the queue
// choreography around a second turn (Queued → Processing → turn_wait)
// reads as the bot hallucinating.
//
// The mailbox is the hand-off point. dispatch (handleMention) pushes raw
// message text; the turn drains at round boundaries (executeToolCalls,
// after the view_image injection flush — the same wire shape view_image
// already proved in production) and in chat()'s post-turn quiescent-close
// loop. Race-freedom lives in one mutex: a push that succeeds while the
// mailbox is open is guaranteed to appear in a subsequent drain or in the
// closeAndDrain that flipped it closed — the only accepted-then-lost case
// is turn cancellation, where chat()'s deferred closeAndDrain persists
// the remainder as text rows.
//
// Registry entries are never deleted (closed mailboxes stay registered
// and reject pushes); the map is bounded by sessions since boot, exactly
// like the session-gate registry (sessionGate.go).
type turnMailbox struct {
	mu      sync.Mutex
	open    bool
	pending []string
}

var turnMailboxes sync.Map // int64 session id -> *turnMailbox

// openTurnMailbox opens a fresh mailbox for the session, closing any
// stale entry (a defensive replace — chat() holds the session gate, so
// two opens cannot legitimately race).
func openTurnMailbox(sessionID int64) *turnMailbox {
	mb := &turnMailbox{open: true}
	if old, loaded := turnMailboxes.Swap(sessionID, mb); loaded {
		omb := old.(*turnMailbox)
		omb.mu.Lock()
		omb.open = false
		omb.pending = nil
		omb.mu.Unlock()
	}
	return mb
}

// turnMailboxFor returns the session's mailbox, or nil when no turn has
// one open.
func turnMailboxFor(sessionID int64) *turnMailbox {
	mb, ok := turnMailboxes.Load(sessionID)
	if !ok {
		return nil
	}
	return mb.(*turnMailbox)
}

// push returns true when the turn has taken on the obligation to
// drain-or-persist the message.
func (mb *turnMailbox) push(msg string) bool {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if !mb.open {
		return false
	}
	mb.pending = append(mb.pending, msg)
	return true
}

// drain returns and clears pending; the mailbox stays open.
func (mb *turnMailbox) drain() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	out := mb.pending
	mb.pending = nil
	return out
}

// closeAndDrain atomically closes and returns pending: a racing push
// either lands in the returned slice or fails against the closed mailbox.
func (mb *turnMailbox) closeAndDrain() []string {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.open = false
	out := mb.pending
	mb.pending = nil
	return out
}
```

- [ ] **Step 4: Run tests**

Run: `go test -race -run TestTurnMailbox . -v` — expect PASS (race detector on: the exactly-once test is the point).
Run: `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...`

- [ ] **Step 5: Commit**

```bash
git add midturn.go midturn_test.go
git commit -m "midturn: per-session turn mailbox primitive"
```

---

### Task 3: The absorbed receipt notice

**Files:**
- Modify: `notices.go` (MentionNotices struct ~line 60; setNoticesDefaults ~line 292)
- Modify: `config/notices.toml` (reference list + commented block of the `[mentions]` section)
- Test: `notices_test.go`

**Interfaces:**
- Produces: `MentionNotices.Absorbed string` (toml `absorbed`); default `"\x0314📎 {nick}: added to the running turn\x0f"`.

- [ ] **Step 1: Write the failing test**

Append to `notices_test.go`:

```go
func TestNoticesMentionsAbsorbedDefault(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)
	assert.Equal(t, "\x0314📎 {nick}: added to the running turn\x0f", n.Mentions.Absorbed)

	out := expandNotice(n.Mentions.Absorbed, map[string]string{"nick": "shrew"})
	assert.Contains(t, out, "shrew")
	assert.NotContains(t, out, "{nick}")
}
```

(If `setNoticesDefaults` takes a value receiver or different name, match its real signature in `notices.go` — the assertions are the contract.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestNoticesMentionsAbsorbedDefault . -v`
Expected: FAIL — `Absorbed` undefined.

- [ ] **Step 3: Implement**

In `notices.go`, add to `MentionNotices` next to `NoContext`/`Muted`:

```go
	// Absorbed is the receipt sent when a mention is handed to the
	// session's live turn instead of queueing a second turn (midturn.go).
	Absorbed string `toml:"absorbed"`
```

In `setNoticesDefaults`, next to the other Mentions defaults:

```go
	if n.Mentions.Absorbed == "" {
		n.Mentions.Absorbed = "\x0314📎 {nick}: added to the running turn\x0f"
	}
```

In `config/notices.toml`: add `# absorbed = "\x0314📎 {nick}: added to the running turn\x0f"` to the `[mentions]` reference list AND to the commented-out example block, per the config documentation convention (comment above it: `# Receipt sent when the message is absorbed into the session's running turn instead of queueing a new one`).

- [ ] **Step 4: Run tests**

Run: `go test -run TestNoticesMentionsAbsorbedDefault . -v` — PASS. Then full suite: `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...`

- [ ] **Step 5: Commit**

```bash
git add notices.go notices_test.go config/notices.toml
git commit -m "notices: mentions absorbed receipt notice"
```

---

### Task 4: Dispatch absorption in handleMention

**Files:**
- Modify: `midturn.go` (add `tryAbsorbMention`)
- Modify: `irc_handlers.go:446-452` (between the "Running chat completion with existing context" log and `EnqueueWithStart`)
- Test: `midturn_test.go`

**Interfaces:**
- Consumes: Task 2 mailbox, Task 3 notice.
- Produces: `func tryAbsorbMention(sessionID int64, msg, nick string) (string, bool)` — returns the receipt notice text and true when absorbed.

- [ ] **Step 1: Write the failing tests**

Append to `midturn_test.go`:

```go
func TestTryAbsorbMention(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	t.Run("open mailbox absorbs and returns receipt", func(t *testing.T) {
		mb := openTurnMailbox(50)
		t.Cleanup(func() { mb.closeAndDrain() })
		txt, ok := tryAbsorbMention(50, "make it look like this", "hastur")
		assert.True(t, ok)
		assert.Contains(t, txt, "hastur")
		assert.Equal(t, []string{"make it look like this"}, mb.drain())
	})

	t.Run("no mailbox falls back", func(t *testing.T) {
		_, ok := tryAbsorbMention(999999, "x", "hastur")
		assert.False(t, ok)
	})

	t.Run("closed mailbox falls back", func(t *testing.T) {
		mb := openTurnMailbox(51)
		mb.closeAndDrain()
		_, ok := tryAbsorbMention(51, "x", "hastur")
		assert.False(t, ok)
	})
}

func TestHandleMentionAbsorbsIntoLiveTurn(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "absorbuser")
	sid := createTestSession(t, "testnet", "#absorb", "absorbuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "you are helpful")

	// handleMention -> getSessionConfig needs the chat command registered.
	configMu.Lock()
	old := config
	c := config
	c.Commands.Chats = map[string]AIConfig{"testchat": makeTestAIConfig()}
	config = c
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		config = old
		configMu.Unlock()
	})

	// Fresh queue: the absorbed path must never touch it.
	origQM := queueMgr
	qm := NewQueueManager(NoticesConfig{}, 5)
	qm.Start()
	queueMgr = qm
	t.Cleanup(func() { queueMgr = origQM; qm.Stop() })

	// Fresh mention tracker so earlier tests can't mute us.
	origTracker := theMentionTracker
	theMentionTracker = newMentionTracker()
	t.Cleanup(func() { theMentionTracker = origTracker })

	mb := openTurnMailbox(sid)
	t.Cleanup(func() { mb.closeAndDrain() })

	network := Network{Name: "testnet"}
	client := chatGateClient()
	event := chatGateEvent("absorbuser", "#absorb")

	handleMention(network, client, event, "#absorb", "just make it look like this")

	got := mb.drain()
	require.Len(t, got, 1, "message was not absorbed")
	assert.Equal(t, "just make it look like this", got[0])
	cur, pending := qm.QueueStatus("testnet", "#absorb", userID)
	assert.Nil(t, cur, "absorbed mention must not enqueue a queue item")
	assert.Empty(t, pending)
}

func TestHandleMentionFallsBackWhenNoMailbox(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "absorbuser2")
	sid := createTestSession(t, "testnet", "#absorb2", "absorbuser2", "testchat", "", "")
	insertTestMessage(t, sid, "system", "you are helpful")

	configMu.Lock()
	old := config
	c := config
	c.Commands.Chats = map[string]AIConfig{"testchat": makeTestAIConfig()}
	config = c
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		config = old
		configMu.Unlock()
	})

	origQM := queueMgr
	qm := NewQueueManager(NoticesConfig{}, 5)
	qm.Start()
	queueMgr = qm
	t.Cleanup(func() { queueMgr = origQM; qm.Stop() })

	origTracker := theMentionTracker
	theMentionTracker = newMentionTracker()
	t.Cleanup(func() { theMentionTracker = origTracker })

	network := Network{Name: "testnet"}
	client := chatGateClient()
	event := chatGateEvent("absorbuser2", "#absorb2")

	handleMention(network, client, event, "#absorb2", "queue me the old way")

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		cur, _ := qm.QueueStatus("testnet", "#absorb2", userID)
		if cur != nil {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(t, found, "closed-mailbox mention must fall back to the queue path")
}
```

Add `import "time"` to `midturn_test.go` if not present.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestTryAbsorbMention|TestHandleMentionAbsorbs|TestHandleMentionFallsBack' . -v`
Expected: FAIL — `tryAbsorbMention` undefined.

- [ ] **Step 3: Implement**

Append to `midturn.go`:

```go
// tryAbsorbMention hands a continuation message to the session's live
// turn. Returns the receipt notice text and true when absorbed; ("", false)
// when no mailbox is open (dispatch falls back to the normal queue path).
func tryAbsorbMention(sessionID int64, msg, nick string) (string, bool) {
	mb := turnMailboxFor(sessionID)
	if mb == nil || !mb.push(msg) {
		return "", false
	}
	n := getNotices()
	return expandNotice(n.Mentions.Absorbed, map[string]string{"nick": nick}), true
}
```

In `irc_handlers.go` `handleMention`, insert between `logger.Info("Running chat completion with existing context")` (line ~446) and the `EnqueueWithStart` call:

```go
	// Mid-turn absorption (midturn.go): this session has a live turn that
	// can still take messages — hand the message over instead of queueing
	// a second turn behind it. No queue item, no queue notices; one
	// immediate receipt line.
	if txt, absorbed := tryAbsorbMention(session.ID, msg, event.Source.Name); absorbed {
		client.Cmd.Reply(event, txt)
		return
	}
```

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestTryAbsorbMention|TestHandleMentionAbsorbs|TestHandleMentionFallsBack' . -v` — PASS. Then `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...`

- [ ] **Step 5: Commit**

```bash
git add midturn.go midturn_test.go irc_handlers.go
git commit -m "mentions: absorb same-session continuations into the live turn"
```

---

### Task 5: Shared user-message preprocessing (`buildUserChatMessage`)

Extract chat()'s DetectImages block into a helper both chat() and the drain use. chat()'s behavior is unchanged (warn + drop on failure); the drain deviates deliberately (warn + text-only persist) per the spec.

**Files:**
- Modify: `aiCmds.go` (new `buildUserChatMessage` near `chat()`; replace chat()'s preprocessing block at lines ~2300-2344; move `runner.sessionID = session.ID` from line ~2348 to before the preprocessing)
- Test: `aiCmds_test.go`

**Interfaces:**
- Consumes: `detectImageURLs`, `buildImageMessage`, `countContextImages` (image_detect.go).
- Produces: `func buildUserChatMessage(cr *chatRunner, raw string, existingImages int) (msg ChatMessage, fallbackText string, ok bool)`. Sends its own warnings via `cr.sendWarning`/`cr.sendError`. `ok=false` ⇒ use `fallbackText` (the raw text) if the caller must not drop the message.

- [ ] **Step 1: Write the failing test**

Append to `aiCmds_test.go`:

```go
func TestBuildUserChatMessage(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	t.Run("detectimages off is plain text", func(t *testing.T) {
		cr := newImageViewTestRunner(t, AIConfig{}, 9)
		msg, fallback, ok := buildUserChatMessage(cr, "hello there", 0)
		assert.True(t, ok)
		assert.Empty(t, fallback)
		assert.Equal(t, RoleUser, msg.Role)
		assert.Equal(t, "hello there", msg.Content)
		assert.Empty(t, msg.MultiContent)
	})

	t.Run("no urls stays plain", func(t *testing.T) {
		cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 5}, 9)
		msg, _, ok := buildUserChatMessage(cr, "no links here", 0)
		assert.True(t, ok)
		assert.Equal(t, "no links here", msg.Content)
		assert.Empty(t, msg.MultiContent)
	})

	t.Run("image url builds multicontent", func(t *testing.T) {
		config.ImagesDir = t.TempDir()
		t.Cleanup(func() { config.ImagesDir = "" })
		config.TrustedImageHosts = []string{"127.0.0.1"}
		t.Cleanup(func() { config.TrustedImageHosts = nil })
		srv := newTestImageServer(t)
		t.Cleanup(srv.Close)

		cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 5, ImageFormat: "jpg", ImageQuality: 75}, 9)
		msg, fallback, ok := buildUserChatMessage(cr, "look "+srv.URL+"/pic.jpg", 0)
		assert.True(t, ok)
		assert.Empty(t, fallback)
		assert.Equal(t, RoleUser, msg.Role)
		require.NotEmpty(t, msg.MultiContent, "image URL should attach image parts")
	})

	t.Run("over budget warns and returns fallback text", func(t *testing.T) {
		cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 1}, 9)
		msg, fallback, ok := buildUserChatMessage(cr, "see https://img.example.com/x.jpg", 1)
		assert.False(t, ok)
		assert.Equal(t, "see https://img.example.com/x.jpg", fallback)
		assert.Empty(t, msg.MultiContent)
		select {
		case line := <-cr.outputCh:
			assert.NotEmpty(t, line, "a warning notice must go out on the output channel")
		default:
			t.Fatal("expected a warning notice on the output channel")
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestBuildUserChatMessage . -v`
Expected: FAIL — `buildUserChatMessage` undefined.

- [ ] **Step 3: Implement**

Add to `aiCmds.go` above `chat()`:

```go
// buildUserChatMessage is the shared user-message preprocessing for
// chat() and mid-turn absorption (midturn.go): detectimages URL handling
// against the session image budget. Warnings are sent by the helper via
// the runner. ok=false means images could not be attached (budget or
// processing failure) — the caller decides: chat() drops the message
// (historical behavior), the absorption drain persists fallbackText as a
// plain text row (the user was already told their message was absorbed;
// dropping is not acceptable).
func buildUserChatMessage(cr *chatRunner, raw string, existingImages int) (ChatMessage, string, bool) {
	if !cr.cfg.DetectImages {
		return ChatMessage{Role: RoleUser, Content: raw}, "", true
	}
	_, imageUrls := detectImageURLs(raw)
	if len(imageUrls) == 0 {
		return ChatMessage{Role: RoleUser, Content: raw}, "", true
	}
	remaining := cr.cfg.MaxContextImages - existingImages
	n := getNotices()
	if remaining <= 0 {
		cr.sendWarning(expandNotice(n.Images.LimitReached, map[string]string{"max": fmt.Sprintf("%d", cr.cfg.MaxContextImages)}))
		return ChatMessage{}, raw, false
	}
	if len(imageUrls) > remaining {
		cr.sendWarning(expandNotice(n.Images.PartialLimit, map[string]string{
			"remaining": fmt.Sprintf("%d", remaining),
			"used":      fmt.Sprintf("%d", existingImages),
			"max":       fmt.Sprintf("%d", cr.cfg.MaxContextImages),
		}))
		return ChatMessage{}, raw, false
	}
	var maxImagePixels int
	readConfig(func() { maxImagePixels = config.MaxImagePixels })
	msg, err := buildImageMessage(raw, imageUrls, cr.cfg.MaxImages, cr.cfg.ImageFormat, cr.cfg.ImageQuality, cr.cfg.MaxImageWidth, cr.cfg.MaxImageHeight, maxImagePixels, cr.sessionID)
	if err != nil {
		cr.sendError(expandNotice(n.DB.ProcessImages, map[string]string{"error": err.Error()}))
		return ChatMessage{}, raw, false
	}
	return msg, "", true
}
```

Then refactor `chat()` (aiCmds.go ~2300-2348):
1. Move `runner.sessionID = session.ID` (currently line ~2348) to immediately after `defer releaseTurn()` — the helper needs it for image-ref registration.
2. Replace the whole `var userMsg ChatMessage; if cfg.DetectImages { ... } else { ... }` block with:

```go
	var existingImages int
	if cfg.DetectImages {
		messages, err := sessionMgr.GetMessages(session.ID)
		if err != nil {
			runner.logger.Error("failed to load messages for image detection", "error", err)
			messages = nil
		}
		existingImages = countContextImages(messages)
	}
	userMsg, _, msgOK := buildUserChatMessage(runner, args[0], existingImages)
	if !msgOK {
		return // warning already sent by the helper; message dropped (historical behavior)
	}
```

3. Delete the now-duplicate `runner.sessionID = session.ID` at its old location (keep `runner.convID` handling where it is).

- [ ] **Step 4: Run tests**

Run: `go test -run TestBuildUserChatMessage . -v` — PASS. Then the full suite (chat()-image tests must stay green: `go test -run 'TestChat|TestDetect|TestSession' . -v` then everything): `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...`

- [ ] **Step 5: Commit**

```bash
git add aiCmds.go aiCmds_test.go
git commit -m "ai: extract shared user-message preprocessing"
```

---

### Task 6: Round-boundary drain in executeToolCalls

**Files:**
- Modify: `midturn.go` (add `drainAbsorbedIntoTurn`)
- Modify: `aiCmds.go:1627-1632` (after the `pendingInjections` flush)
- Test: `midturn_test.go`

**Interfaces:**
- Consumes: Task 2 mailbox, Task 5 `buildUserChatMessage`.
- Produces: `func drainAbsorbedIntoTurn(cr *chatRunner, turn *turnContext)`.

- [ ] **Step 1: Write the failing tests**

Append to `midturn_test.go`:

```go
// Review Focus #2: an over-budget absorbed message must persist text-only
// instead of being dropped (chat() drops; the drain may not — the user
// got the receipt).
func TestDrainAbsorbedOverBudgetPersistsTextOnly(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)
	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 1}, 9)
	turn := newTurnContext(9, nil)
	turn.Add(ChatMessage{Role: RoleUser, MultiContent: []MessagePart{
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/jpeg;base64,AAA"}},
	}}) // 1 existing image against a budget of 1 → remaining 0

	mb := openTurnMailbox(9)
	t.Cleanup(func() { mb.closeAndDrain() })
	require.True(t, mb.push("see https://img.example.com/x.jpg now"))

	drainAbsorbedIntoTurn(cr, turn)
	last := turn.Messages()[len(turn.Messages())-1]
	assert.Equal(t, RoleUser, last.Role)
	assert.Equal(t, "see https://img.example.com/x.jpg now", last.Content,
		"text kept when the image budget is exhausted")
	assert.Empty(t, last.MultiContent)
}

func TestExecuteToolCallsDrainsAbsorbedAfterToolResults(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)
	cr := newImageViewTestRunner(t, AIConfig{}, 9)
	turn := newTurnContext(9, nil)
	turn.Add(ChatMessage{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1"}}})

	mb := openTurnMailbox(9)
	t.Cleanup(func() { mb.closeAndDrain() })
	require.True(t, mb.push("just make it look like this"))

	cr.executeToolCalls(turn, []ToolCall{
		{ID: "call_1", Type: "function", Function: FunctionCall{Name: "unknown_tool_zz", Arguments: "{}"}},
	})

	msgs := turn.Messages()
	require.NotEmpty(t, msgs)
	last := msgs[len(msgs)-1]
	assert.Equal(t, RoleUser, last.Role, "absorbed message flushed after tool results")
	assert.Equal(t, "just make it look like this", last.Content)

	// Persisted too (non-ephemeral turn), same as tool rows.
	dbMsgs, err := sessionMgr.GetMessages(9)
	require.NoError(t, err)
	assert.Equal(t, RoleUser, dbMsgs[len(dbMsgs)-1].Role, "absorbed message persisted to the session")
}

func TestDrainNoMailboxIsNoop(t *testing.T) {
	setupTestDB(t)
	cr := newImageViewTestRunner(t, AIConfig{}, 12345)
	turn := newTurnContext(12345, nil)
	before := len(turn.Messages())
	drainAbsorbedIntoTurn(cr, turn)
	assert.Len(t, turn.Messages(), before)
}

func TestDrainSequentialBudget(t *testing.T) {
	// Two absorbed image messages against a 2-image budget: both attach.
	// (DetectImages with a working image server.)
	setupTestDB(t)
	setupNoticesDefaults(t)
	config.ImagesDir = t.TempDir()
	t.Cleanup(func() { config.ImagesDir = "" })
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })
	srv := newTestImageServer(t)
	t.Cleanup(srv.Close)

	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 2, ImageFormat: "jpg", ImageQuality: 75}, 9)
	turn := newTurnContext(9, nil)

	mb := openTurnMailbox(9)
	t.Cleanup(func() { mb.closeAndDrain() })
	require.True(t, mb.push("one "+srv.URL+"/pic.jpg"))
	require.True(t, mb.push("two "+srv.URL+"/pic.jpg"))

	drainAbsorbedIntoTurn(cr, turn)
	msgs := turn.Messages()
	require.Len(t, msgs, 2)
	for _, m := range msgs {
		assert.Equal(t, RoleUser, m.Role)
		assert.NotEmpty(t, m.MultiContent, "both messages should attach their image")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestDrainAbsorbed|TestExecuteToolCallsDrainsAbsorbed|TestDrainNoMailbox|TestDrainSequentialBudget' . -v`
Expected: FAIL — `drainAbsorbedIntoTurn` undefined.

- [ ] **Step 3: Implement**

Append to `midturn.go`:

```go
// drainAbsorbedIntoTurn flushes absorbed messages into the turn at a
// round boundary: after all tool results and after view_image's
// pendingInjections, so the newest human input sits closest to the
// generation prompt. Row order: tool results → view_image injections →
// absorbed user messages (mirrors the flush order in executeToolCalls).
func drainAbsorbedIntoTurn(cr *chatRunner, turn *turnContext) {
	if cr.sessionID == 0 {
		return
	}
	mb := turnMailboxFor(cr.sessionID)
	if mb == nil {
		return
	}
	for _, raw := range mb.drain() {
		msg, fallback, ok := buildUserChatMessage(cr, raw, countContextImages(turn.Messages()))
		if !ok {
			// The warning already went out via the helper. The user was
			// told their message was absorbed — never drop it; keep the
			// text, skip the images.
			turn.Add(ChatMessage{Role: RoleUser, Content: fallback})
			continue
		}
		turn.Add(msg)
	}
}
```

In `aiCmds.go` `executeToolCalls`, immediately after the `pendingInjections` flush loop (line ~1632 `cr.pendingInjections = nil`), before `return registeredJob`:

```go
	// Mid-turn absorption (midturn.go): messages a user sent while this
	// turn was running, handed over by dispatch. Drained AFTER tool
	// results and view_image injections — same seam, same wire shape the
	// view_image injections already proved in production.
	drainAbsorbedIntoTurn(cr, turn)
```

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestDrain|TestExecuteToolCallsDrainsAbsorbed' . -v` — PASS. Then `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...`

- [ ] **Step 5: Commit**

```bash
git add midturn.go midturn_test.go aiCmds.go
git commit -m "midturn: drain absorbed messages at the tool-round boundary"
```

---

### Task 7: chat() lifecycle — open, quiescent-close loop, cancellation persistence

**Files:**
- Modify: `midturn.go` (add `persistUndrainedAbsorbed`)
- Modify: `aiCmds.go` chat() — open the mailbox after the gate; replace the post-turn inline loop (lines ~2369-2398) with the quiescent-close loop.
- Test: `midturn_test.go`

**Interfaces:**
- Consumes: Tasks 2, 5, 6.
- Produces: `func persistUndrainedAbsorbed(runner *chatRunner, mb *turnMailbox)`.

- [ ] **Step 1: Write the failing tests**

Append to `midturn_test.go`:

```go
// Review Focus #3: a message absorbed during the model's final API
// response has no tool round left to drain it — chat()'s post-turn loop
// must give it a continuation turn.
func TestChatAbsorbsMessageDuringFinalResponse(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "absorber")
	resolved := chatGateUser(userID, "absorber")

	var mu sync.Mutex
	var bodies []string
	firstHit := make(chan struct{})
	var once sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		once.Do(func() { close(firstHit) })
		<-release // block every request until released; closed chan returns instantly
		chatGateCompletion(w, "first answer")
	}))
	t.Cleanup(func() { server.Close() })
	t.Cleanup(doRelease)
	setChatGateServices(t, server.URL+"/v1")

	network := Network{Name: "testnet"}
	client := chatGateClient()
	cfg := makeTestAIConfig()

	out := make(chan string, 50)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		chat(network, client, chatGateEvent("absorber", "#abs3"), cfg, context.Background(), out, resolved, "first question")
	}()

	<-firstHit // primary turn is inside its (final) API call

	sess, err := sessionMgr.GetActiveSession("testnet", "#abs3", userID)
	require.NoError(t, err)
	require.NotNil(t, sess)

	txt, ok := tryAbsorbMention(sess.ID, "actually use the reference image", "absorber")
	require.True(t, ok, "mention during the final response must be absorbed")
	assert.Contains(t, txt, "absorber")

	doRelease()
	<-done

	// The continuation request carries the absorbed message after the
	// primary answer.
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(bodies), 2, "absorbed message never produced a continuation request")
	assert.Contains(t, bodies[1], "actually use the reference image")
	assert.Contains(t, bodies[1], "first answer")

	// The mailbox is closed when chat() returns — later mentions enqueue.
	mb := turnMailboxFor(sess.ID)
	require.NotNil(t, mb)
	assert.False(t, mb.push("after close"), "mailbox must be closed when chat() returns")

	// Absorbed messages never wait on the gate — no turn_wait notice.
	sawWait := false
	for l := range out {
		if strings.Contains(l, "still working") {
			sawWait = true
		}
	}
	assert.False(t, sawWait, "absorbed message must not produce a turn_wait notice")
}

// Review Focus #4: a cancelled turn (^stop) with undrained absorbed
// messages persists them as text rows and sends nothing further.
func TestChatCancelPersistsUndrainedAbsorbed(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "absorber")
	resolved := chatGateUser(userID, "absorber")

	firstHit := make(chan struct{})
	var once sync.Once
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(firstHit) })
		<-release
		chatGateCompletion(w, "first answer")
	}))
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { close(release) })
	setChatGateServices(t, server.URL+"/v1")

	network := Network{Name: "testnet"}
	client := chatGateClient()
	cfg := makeTestAIConfig()

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan string, 50)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		chat(network, client, chatGateEvent("absorber", "#abs4"), cfg, ctx, out, resolved, "first question")
	}()

	<-firstHit

	sess, err := sessionMgr.GetActiveSession("testnet", "#abs4", userID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	_, ok := tryAbsorbMention(sess.ID, "words that must survive", "absorber")
	require.True(t, ok)

	cancel()
	<-done

	msgs, err := sessionMgr.GetMessages(sess.ID)
	require.NoError(t, err)
	found := false
	for _, m := range msgs {
		if m.Role == RoleUser && strings.Contains(m.Content, "words that must survive") {
			found = true
		}
	}
	assert.True(t, found, "cancelled turn must persist undrained absorbed messages as text rows")
}
```

Add imports to `midturn_test.go` as needed: `context`, `io`, `net/http`, `net/http/httptest`, `strings`, `time` (time only if still unused after Task 4 — remove then).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestChatAbsorbsMessageDuringFinalResponse|TestChatCancelPersistsUndrainedAbsorbed' . -v`
Expected: FAIL — no mailbox is open during chat() (`tryAbsorbMention` returns false).

- [ ] **Step 3: Implement**

Append to `midturn.go`:

```go
// persistUndrainedAbsorbed is the loss-tail of the absorption obligation:
// every accepted push must be drained or persisted — exactly once, on
// every exit path. Two paths reach it with a non-empty mailbox: a
// cancelled turn (stop/shutdown — the post-turn loop bails on ctx.Done)
// and chat()'s early returns after the mailbox opened (image-budget
// drop, history-load failure). Either way the messages are written as
// plain text-only user rows so the user's words survive into their next
// turn; nothing is sent (the turn is over — cancelled turns were
// announced by the queue, early returns already sent their own error).
// On a normal exit the quiescent-close loop already closed the mailbox
// and this is a no-op. DB writes do not depend on the queue ctx.
func persistUndrainedAbsorbed(runner *chatRunner, mb *turnMailbox) {
	remaining := mb.closeAndDrain()
	if len(remaining) == 0 {
		return
	}
	for _, raw := range remaining {
		sessionMgr.AddMessage(runner.sessionID, ChatMessage{Role: RoleUser, Content: raw})
	}
	runner.logger.Warn("mid-turn absorption: persisted undrained messages after turn end",
		"session", runner.sessionID, "count", len(remaining))
}
```

In `aiCmds.go` `chat()`:

1. Right after `defer releaseTurn()` (line ~2298), add:

```go
	// Mid-turn absorption (midturn.go): the mailbox is open for the whole
	// gate hold — executeToolCalls drains it at round boundaries, and the
	// post-turn loop below drains it between continuation turns.
	mb := openTurnMailbox(session.ID)
	defer persistUndrainedAbsorbed(runner, mb)
```

2. Replace the post-turn inline loop block (lines ~2369-2398, from `if theDB != nil && sessionMgr.IsSessionActive(runner.sessionID) {` through its closing brace) with:

```go
	if theDB != nil && sessionMgr.IsSessionActive(runner.sessionID) {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			absorbed := mb.drain()
			completedJobs, err := getCompletedPendingJobs(runner.sessionID)
			if err != nil {
				runner.logger.Error("failed to query completed pending jobs", "error", err)
			}
			if len(absorbed) == 0 && (err != nil || len(completedJobs) == 0) {
				// Quiescent: close under the mailbox mutex so a racing
				// push either lands in the returned stragglers (processed
				// by exactly one extra cycle) or fails against the closed
				// mailbox (dispatch enqueues normally). See midturn.go.
				stragglers := mb.closeAndDrain()
				if len(stragglers) == 0 {
					break
				}
				absorbed = stragglers
			}
			for _, raw := range absorbed {
				if msg, fallback, ok := buildUserChatMessage(runner, raw, countContextImages(turn.Messages())); ok {
					turn.Add(msg)
				} else {
					// Absorbed messages are never dropped (the user got
					// the receipt): keep the text, skip the images.
					turn.Add(ChatMessage{Role: RoleUser, Content: fallback})
				}
			}
			for _, cj := range completedJobs {
				injectAsyncResultFromDB(runner.sessionID, cfg, cj, network.Name, channel, e.Source.Name)
				markPendingJobDelivered(cj.JobID)
			}
			messages, err = sessionMgr.GetMessages(runner.sessionID)
			if err != nil {
				runner.logger.Error("failed to reload messages after job delivery", "error", err)
				break
			}
			turn = newTurnContext(runner.sessionID, messages)
			runner.runTurn(turn)
			// Same api_log pointer the primary turn logs — continuation
			// turns (absorbed messages, async job delivery) write to the
			// same api log file and deserve the same breadcrumb.
			runner.logger.Debug("continuation finished", "api_log", apiLogger.GetSessionFilePath(runner.sessionID))
		}
	}
```

(This preserves the old loop's async-job behavior byte-for-byte — same query, same injection, same reload, same runTurn, same log line — and adds the absorbed drain + quiescent close. The `messages, err =` assignment reuses chat()'s existing `messages` variable.)

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestChatAbsorbsMessageDuringFinalResponse|TestChatCancelPersistsUndrainedAbsorbed' . -v` — PASS. Then the full suite: `go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...` (existing chat/gate/async-delivery tests must stay green — especially `TestChatSessionGate*`, `TestDeliverAsyncResult*`, `TestRecoverPendingJobs*`).

- [ ] **Step 5: Commit**

```bash
git add midturn.go midturn_test.go aiCmds.go
git commit -m "chat: open the turn mailbox and drain absorbed messages in the post-turn loop"
```

---

### Task 8: Wire-shape test — absorbed row rides after tool results

The end-to-end pin that the next API request contains the tool result followed by the absorbed user message, through the real `chat()` path (chat completions, non-streaming). The Responses-path delta builder carrying injected user rows is already pinned by the existing `TestToolLoopMsgsToInputItemsCarriesInjectedUser`; absorbed rows are identical in shape (plain `RoleUser`), so no new Responses e2e is needed.

**Files:**
- Test: `midturn_test.go`

**Interfaces:**
- Consumes: Tasks 4, 6, 7 (full absorption pipeline).

- [ ] **Step 1: Write the failing test**

Append to `midturn_test.go`:

```go
// The wire pin: round 1 returns a tool call; the absorbed message is
// pushed while round 1 is being answered; round 2's REQUEST must carry
// the tool result followed by the absorbed user row, in that order. An
// unknown tool name is deliberate — executeToolCalls turns it into an
// error tool result, which is all the wire shape needs (no MCP/gating
// setup required).
func TestChatAbsorbedRidesAfterToolResultsWire(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "wireuser")
	resolved := chatGateUser(userID, "wireuser")

	var mu sync.Mutex
	var bodies []string
	reqNo := 0
	firstHit := make(chan struct{})
	var once sync.Once
	pushed := make(chan struct{})

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqNo++
		n := reqNo
		bodies = append(bodies, string(raw))
		mu.Unlock()
		once.Do(func() { close(firstHit) })
		if n == 1 {
			<-pushed // hold the round-1 answer until the message is absorbed
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"unknown_tool_zz","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		chatGateCompletion(w, "final answer")
	}))
	t.Cleanup(func() { api.Close() })
	t.Cleanup(func() { close(pushed) })
	setChatGateServices(t, api.URL+"/v1")

	network := Network{Name: "testnet"}
	client := chatGateClient()
	cfg := makeTestAIConfig()

	out := make(chan string, 50)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		chat(network, client, chatGateEvent("wireuser", "#wire"), cfg, context.Background(), out, resolved, "first question")
	}()

	<-firstHit
	sess, err := sessionMgr.GetActiveSession("testnet", "#wire", userID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	_, ok := tryAbsorbMention(sess.ID, "now change the plan", "wireuser")
	require.True(t, ok)
	close(pushed)
	<-done

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(bodies), 2)
	round2 := bodies[1]
	assert.Contains(t, round2, "unknown_tool_zz", "round 2 must carry the tool round-trip")
	assert.Contains(t, round2, "now change the plan", "round 2 must carry the absorbed message")
	toolIdx := strings.Index(round2, `"unknown_tool_zz"`)
	absorbedIdx := strings.Index(round2, "now change the plan")
	require.GreaterOrEqual(t, toolIdx, 0)
	require.GreaterOrEqual(t, absorbedIdx, 0)
	assert.Less(t, toolIdx, absorbedIdx, "the tool result must precede the absorbed user row")
}
```

- [ ] **Step 2: Run test**

Run: `go test -run TestChatAbsorbedRidesAfterToolResultsWire . -v`
Expected: PASS with Tasks 4+6+7 in place (this test exists to pin the contract; if it fails, the ordering in `drainAbsorbedIntoTurn`'s call site is wrong). If run before Task 7 it fails at `tryAbsorbMention` returning false — that is the failing-first demonstration.

- [ ] **Step 3: Full suite**

`go build -o /tmp/opencode/dave-check . && go fmt ./... && go vet ./... && go test ./...`

- [ ] **Step 4: Commit**

```bash
git add midturn_test.go
git commit -m "midturn: wire test — absorbed row rides after tool results"
```

---

### Task 9: Documentation

**Files:**
- Modify: `docs/queue-and-sessions.md`
- Modify: `AGENTS.md`

**Interfaces:**
- Consumes: the finished implementation (verify claims against code before writing).

- [ ] **Step 1: Add the docs section to `docs/queue-and-sessions.md`**

After the "Async Background Tasks" section:

```markdown
## Mid-turn absorption

When a user sends a same-session continuation (a mention) while their
session's turn is still running — including while the model is mid
tool-loop — the message is handed to the live turn instead of queueing a
second turn behind it:

1. The user gets one quiet receipt line: `📎 {nick}: added to the running
   turn` (notices `[mentions] absorbed`). No Queued/Processing notices —
   the message created no queue item.
2. The message is persisted as a plain user row and sent to the model
   after the pending tool results of the current round (the same wire
   position view_image injections use), so the model can pivot
   immediately instead of finishing a superseded plan.
3. Messages arriving during the model's final response get a
   continuation turn after it; messages racing turn shutdown are
   persisted as text rows and picked up by the user's next message.
4. A message that cannot attach its images (session image budget
   exhausted) is kept as text with a warning — absorbed messages are
   never dropped.
5. Fresh trigger commands are never absorbed: they complete the old
   session and start a new one, and queue as before. Async-result
   deliveries queue as before too (silently — no started notice).

The mailbox (`midturn.go`) guarantees every accepted message is drained
or persisted exactly once. If the turn is already finishing (mailbox
closed), dispatch falls back to the queue-a-new-turn behavior unchanged.
```

- [ ] **Step 2: Update `AGENTS.md`**

Add a bullet to the architecture list summarizing (verify each claim against the shipped code first — AGENTS.md carries verified facts only): the midturn.go mailbox (one per session, opened by chat() under the session gate, drain seam in executeToolCalls after pendingInjections, quiescent-close loop, cancellation persistence), the handleMention absorption point + `[mentions] absorbed` receipt, no-knob always-on scope, same-session-continuations-only eligibility, and the Task 1 `QueueItem.Silent` fix for the phantom async-delivery notices (naming the 2026-11-12 incident). Keep it in the style of the neighboring bullets (dense, mechanism + rationale + guard-test names).

- [ ] **Step 3: Final verification**

```bash
go build -o /tmp/opencode/dave-check .
go fmt ./...
go vet ./...
go test ./...
```

All must pass.

- [ ] **Step 4: Commit**

```bash
git add docs/queue-and-sessions.md AGENTS.md
git commit -m "docs: mid-turn absorption"
```

---

## Final whole-branch review

After Task 9, dispatch a fresh reviewer over the entire branch (subagent-driven-development does this automatically as its final phase) covering: spec compliance against `docs/superpowers/specs/2026-11-12-midturn-injection-design.md`, the Review Focus list above, concurrency (`go test -race -run TestTurnMailbox .`), and that no enqueue path besides the two async-delivery sites gained silence.
