# Queue System Plan for dave IRC Bot

## Problem Statement

Currently, when a user sends a command while the bot is already processing their request, they get a "busy" message and are rejected entirely. There's no queuing, no visibility into what's happening, and `-stop` force-kills everything with no continuation. Multiple users competing for the same service have no coordination.

## Design Overview

Replace the per-user `runningPrompts` reference count + busy rejection pattern with a **QueueManager** that provides:

1. **Per-user FIFO queues** - serialize each user's requests instead of rejecting
2. **Per-service concurrency limits** - throttle API calls at the service level
3. **Context-based cancellation** - replace `getRunning()` polling with Go contexts
4. **Automatic scheduling** - a scheduler goroutine dispatches queued items as slots free up
5. **Clear user messaging** - queue position, "now processing" notifications

### Core Architecture: Decouple Execution from Delivery

Separate the **LLM API call** (execution) from the **IRC output** (delivery). This naturally enables prefetching without special logic.

```
User Request
    |
QueueManager.Enqueue()
    |
+-- Scheduler: assigns exec slot, starts goroutine --+
|                                                      |
|  +-- Executor Phase --------------------------+     |
|  |  Acquire exec slot (service semaphore)      |     |
|  |  Run LLM call, write to outputCh ----------+--+  |
|  |  Release exec slot -> wake scheduler        |  |  |
|  +---------------------------------------------+  |  |
|                                                     |  |
|  +-- Delivery Phase --------------------------+     |  |
|  |  Acquire delivery slot (per-channel mutex)  |<----+
|  |  Send "Starting" notification to IRC        |     |
|  |  Read from outputCh -> send to IRC          |     |
|  |  Release delivery slot                      |     |
|  +---------------------------------------------+     |
|                                                      |
|  onComplete -> wake scheduler                        |
+------------------------------------------------------+
```

**Why this is great for `parallel = 1`**: The exec slot is released after the LLM call finishes but BEFORE delivery starts. So the scheduler immediately starts the next user's LLM call while the current response is still being delivered to IRC. The next user's LLM response buffers in the output channel. When delivery finishes, the next user's buffered response is immediately ready - zero LLM latency.

**With `parallel = 2`**: Two LLM calls can overlap, so even more prefetching. But delivery remains serialized per-channel.

### What Gets Replaced

| Current | New |
|---|---|
| `running.go` (entire file, 76 lines) | Absorbed into `queue.go` |
| `getRunning()` checks in `sendIRC`/`sendLoop` | `ctx.Done()` channel checks |
| `startedRunning`/`stoppedRunning` in command funcs | Managed by QueueManager lifecycle |
| `forceStopRunning` in `stop` command | `queueMgr.StopCurrent()` (cancels context) |
| `waitForIdleAndClaim` in jobManager | Queue auto-schedules on completion |
| 3x busy-check blocks in `handleChanMessage` | Single `queueMgr.Enqueue()` call |
| `Config.Busymsgs` | `Config.QueueMsgs` (template-aware) |

---

## Phase 1: `queue.go` - Core Queue Manager (~300 lines)

### Data Structures

```go
type QueueItem struct {
    ID       int64
    Network  string
    Channel  string
    Nick     string
    Service  string
    Enqueued time.Time
    Execute  func(ctx context.Context, output chan<- string)
    outputCh chan string
    ctx      context.Context
    cancel   context.CancelFunc
}

type UserQueue struct {
    current  *QueueItem
    pending  []*QueueItem
}

type execSlot struct {
    max       int
    semaphore chan struct{}
}

type deliverySlot struct {
    mu sync.Mutex
}

type QueueManager struct {
    mu           sync.Mutex
    users        map[string]*UserQueue
    execSlots    map[string]*execSlot
    deliveryMu   sync.Mutex
    deliverySlot map[string]*deliverySlot
    changed      chan struct{}
    idCounter    int64
    maxDepth     int
    queueMsgs    []string
    startedMsg   string
}
```

### Scheduler

Single goroutine, reacts to state changes:

```go
func (qm *QueueManager) scheduler() {
    for range qm.changed {
        qm.schedule()
    }
}

func (qm *QueueManager) schedule() {
    qm.mu.Lock()
    defer qm.mu.Unlock()
    for _, uq := range qm.users {
        if uq.current != nil || len(uq.pending) == 0 {
            continue
        }
        item := uq.pending[0]
        slot := qm.execSlots[item.Service]
        if slot != nil && !slot.tryAcquire() {
            continue
        }
        uq.pending = uq.pending[1:]
        uq.current = item
        go qm.runJob(item)
    }
}
```

### runJob - Execute + Deliver

```go
func (qm *QueueManager) runJob(item *QueueItem) {
    ctx, cancel := context.WithCancel(context.Background())
    item.ctx = ctx
    item.cancel = cancel

    // Phase 1: Execute (LLM call)
    defer close(item.outputCh)
    defer func() {
        if r := recover(); r != nil {
            select {
            case item.outputCh <- errorMsg(fmt.Sprintf("internal error: %v", r)):
            case <-ctx.Done():
            }
        }
    }()
    item.Execute(ctx, item.outputCh)

    // Release exec slot -> scheduler can start next LLM call
    if slot := qm.execSlots[item.Service]; slot != nil {
        slot.release()
    }
    qm.notify()

    // Phase 2: Deliver (serialized per channel)
    ds := qm.getDeliverySlot(item.Network, item.Channel)
    ds.mu.Lock()
    defer ds.mu.Unlock()

    waitTime := time.Since(item.Enqueued)
    if waitTime > time.Second {
        bot := getBotFn(item.Network)
        if bot != nil && bot.Client != nil {
            msg := qm.formatStartedMsg(item.Nick, waitTime)
            bot.Client.Cmd.Message(item.Channel, msg)
        }
    }

    bot := getBotFn(item.Network)
    if bot == nil || bot.Client == nil {
        for range item.outputCh {}
        return
    }
    throttle := bot.Network.Throttle
    for line := range item.outputCh {
        if ctx.Err() != nil {
            for range item.outputCh {}
            break
        }
        bot.Client.Cmd.Message(item.Channel, "\x02\x02"+line)
        time.Sleep(time.Millisecond * throttle)
    }

    // Phase 3: Complete
    qm.mu.Lock()
    key := item.Network + item.Channel + item.Nick
    uq := qm.users[key]
    if uq.current == item {
        uq.current = nil
    }
    qm.mu.Unlock()
    qm.notify()
}
```

### Key Public Methods

```go
func (qm *QueueManager) Enqueue(network, channel, nick, service string, fn func(ctx context.Context, output chan<- string)) int
    // Returns position: 0 = immediate start, 1+ = queued position
    // Checks maxDepth, sends queue notification if position > 0

func (qm *QueueManager) StopCurrent(network, channel, nick string) bool
    // Cancels current item's context -> execution stops -> scheduler auto-starts next

func (qm *QueueManager) IsRunning(network, channel, nick string) bool
    // For async job delivery / backward compat

func (qm *QueueManager) QueueStatus(network, channel, nick string) (current *QueueItem, pending []*QueueItem)
    // For jobs command display

func (qm *QueueManager) UpdateServiceLimits(services map[string]Service)
    // Called on /reload

func (qm *QueueManager) Stop()
    // Graceful shutdown: cancel all running items
```

### Service Semaphore

```go
type execSlot struct {
    max       int
    semaphore chan struct{} // nil when max=0 (unlimited)
}

func (es *execSlot) tryAcquire() bool {
    if es.semaphore == nil { return true }
    select {
    case es.semaphore <- struct{}{}:
        return true
    default:
        return false
    }
}

func (es *execSlot) release() {
    if es.semaphore == nil { return }
    <-es.semaphore
}
```

---

## Phase 2: Config Changes (`config.go`)

### Service struct - add `Parallel`

```go
type Service struct {
    // ... existing fields ...
    Parallel int `toml:"parallel"` // max concurrent LLM calls: 1 (default), 0 = unlimited
}
```

Default is **1** (serialized).

### Config struct - replace busymsgs with queue config

```go
type Config struct {
    // Remove: Busymsgs []string
    // Add:
    QueueMsgs     []string `toml:"queue_msgs"`
    StartedMsg    string   `toml:"started_msg"`
    MaxQueueDepth int      `toml:"max_queue_depth"`
    // ... rest unchanged ...
}
```

Defaults:
```go
QueueMsgs:     []string{"queued (position {position})"},
StartedMsg:    "\x0306\u25b6 {nick}: Processing your request (waited {wait})...\x0f",
MaxQueueDepth: 5,
```

Template variables: `{position}`, `{nick}`, `{wait}`, `{eta}`

### Config file changes

```toml
# config.toml - replace busymsgs with:
queue_msgs = [
    "queued (position {position}, ~{eta})",
    "you're #{position} in line, hang tight"
]
started_msg = "\x0306\u25b6 {nick}: Processing your request (waited {wait})...\x0f"
max_queue_depth = 5

# services.toml - add parallel to services:
[openai]
# parallel = 1    # default: one LLM call at a time (others queue)
                   # set higher to allow concurrent LLM calls
                   # set 0 for unlimited

[local]
parallel = 1       # local model can only handle one at a time
```

---

## Phase 3: Delete `running.go`

The entire file (76 lines) is absorbed into `queue.go`. All globals removed:
- `runningPrompts` -> `qm.users[key].current != nil`
- `runningMutex` -> `qm.mu`
- `runningChanged` -> `qm.changed`

All functions removed:
- `startedRunning`/`stoppedRunning` -> internal to runJob lifecycle
- `getRunning` -> `qm.IsRunning()`
- `forceStopRunning` -> `qm.StopCurrent()`
- `waitForIdleAndClaim` -> eliminated entirely (scheduler handles this)
- `notifyRunningChanged` -> internal `qm.notify()`

---

## Phase 4: Modify `aiCmds.go`

### chatRunner changes

```go
type chatRunner struct {
    // ... existing fields ...
    ctx      context.Context
    outputCh chan<- string
}

func (cr *chatRunner) sendIRC(out string) {
    for _, line := range wrapForIRC(out) {
        if len(line) <= 0 { continue }
        select {
        case cr.outputCh <- line:
        case <-cr.ctx.Done():
            return
        }
    }
}

func (cr *chatRunner) sendError(msg string) {
    select {
    case cr.outputCh <- errorMsg(msg):
    case <-cr.ctx.Done():
    }
}

func (cr *chatRunner) sendWarning(msg string) {
    select {
    case cr.outputCh <- warnMsg(msg):
    case <-cr.ctx.Done():
    }
}
```

### chat() function

```go
// Before:
func chat(network Network, c *girc.Client, e girc.Event, cfg AIConfig, args ...string)

// After:
func chat(network Network, c *girc.Client, e girc.Event, cfg AIConfig, ctx context.Context, output chan<- string, args ...string)
```

Remove `startedRunning`/`defer stoppedRunning`. The queue manages lifecycle. Pass `ctx` through to all API calls and use `output` channel for all IRC output.

### completion() function

Same pattern: add `ctx` and `output` params, remove running state management.

### Streaming in runTurn

Replace `getRunning()` check with `ctx.Done()`:

```go
select {
case <-cr.ctx.Done():
    stream.Close()
    return messages, true
case chunk := <-streamCh:
    // process
}
```

---

## Phase 5: Modify `mcpCmds.go`

Add `ctx context.Context` and `output chan<- string` parameters. Remove running state management. Write results to output channel.

---

## Phase 6: Modify `handleChanMessage` in `main.go`

Replace all busy-check blocks with a single enqueue pattern. SkipBusy tools bypass the queue. stop/help/jobs remain synchronous.

---

## Phase 7: Stop Command

```go
func stop(network Network, _ *girc.Client, m girc.Event, _ ...string) {
    logger.Info("stop requested")
    queueMgr.StopCurrent(network.Name, m.Params[0], m.Source.Name)
}
```

Flow:
1. `StopCurrent()` calls `item.cancel()` on user's current item
2. Executor sees `ctx.Done()` -> stops LLM call, returns
3. `outputCh` is closed (defer in runJob)
4. Exec slot released -> scheduler wakes -> starts next queued item's LLM call
5. Delivery phase sees `ctx.Err() != nil` -> drains output, skips IRC sending
6. Delivery slot released
7. Next queued item auto-started by scheduler

---

## Phase 8: Job Manager Simplification (`jobManager.go`)

Replace `waitForIdleAndClaim` with simple Enqueue:

```go
func onAsyncJobCompleted(job *asyncJob, resultText string) {
    // ...
    queueMgr.Enqueue(job.Network, job.Channel, job.Nick, "",
        func(ctx context.Context, output chan<- string) {
            deliverAsyncResult(job, ctx, output)
        })
}
```

Simplified `deliverAsyncResult` - no more startedRunning/stoppedRunning, no more waitForIdleAndClaim.

---

## Phase 9: Enhanced `jobs` Command (`historyCmds.go`)

Show queue status alongside async background jobs:

```
Running: chat "what is Go?" (15s elapsed)
Queued (2):
  1. chat "tell me a joke" (waiting 8s)
  2. completion "hello world" (waiting 3s)
Background: img-qwen-abc123 (pending, 30s)
```

---

## Phase 10: Initialization & Shutdown

### Startup
```go
queueMgr = NewQueueManager(config.QueueMsgs, config.StartedMsg, config.MaxQueueDepth)
queueMgr.UpdateServiceLimits(config.Services)
queueMgr.Start()
```

### Shutdown
```go
queueMgr.Stop()     // cancel all running items
```

### Reload
```go
queueMgr.UpdateServiceLimits(config.Services)
```

---

## Phase 11: MCP Tool Call Context Propagation

Add context-aware variants:
```go
func callMCPToolWithContext(ctx context.Context, toolName string, args map[string]any) (*mcp.ToolResult, error)
func callMCPToolWithTimeoutContext(ctx context.Context, toolName string, args map[string]any, timeout time.Duration) (*mcp.ToolResult, error)
```

---

## Phase 12: CmdFunc Signature Change

```go
// Before:
type CmdFunc func(Network, *girc.Client, girc.Event, ...string)

// After:
type CmdFunc func(Network, *girc.Client, girc.Event, context.Context, chan<- string, ...string)
```

For synchronous commands (stop, help, jobs), pass `context.Background()` and `nil`.

---

## File Change Summary

| File | Action | Description |
|---|---|---|
| `queue.go` | **CREATE** | QueueManager, scheduler, runJob, service slots, delivery slots (~300 lines) |
| `queue_test.go` | **CREATE** | Unit tests (~200 lines) |
| `running.go` | **DELETE** | Absorbed into queue.go |
| `running_test.go` | **DELETE** | Rewritten as queue_test.go |
| `config.go` | **MODIFY** | Add Parallel to Service, replace Busymsgs with queue config |
| `main.go` | **MODIFY** | Replace busy checks with Enqueue, update stop, init queue, CmdFunc type |
| `aiCmds.go` | **MODIFY** | Add ctx+outputCh to chatRunner, update sendIRC/sendError, update chat/completion |
| `mcpCmds.go` | **MODIFY** | Add ctx+output params, use output channel |
| `mcpClient.go` | **MODIFY** | Add context-aware tool call variants |
| `jobManager.go` | **MODIFY** | Replace waitForIdleAndClaim with Enqueue, simplify deliverAsyncResult |
| `historyCmds.go` | **MODIFY** | Enhance jobs command with queue status |

---

## Test Plan

| Test | Description |
|---|---|
| `TestEnqueueImmediate` | Idle user -> position 0, runs immediately |
| `TestEnqueueQueued` | Busy user -> position 1+, queued |
| `TestFIFOOrder` | Multiple items enqueued -> dequeued in order |
| `TestStopCurrent` | Stop cancels execution, next starts automatically |
| `TestStopEmptyQueue` | Stop when no current -> no-op |
| `TestMaxQueueDepth` | Enqueue beyond limit -> rejected with message |
| `TestServiceParallel1` | Two users, parallel=1 -> serialized at service level |
| `TestServiceParallel2` | Two users, parallel=2 -> both can execute |
| `TestServiceParallel0` | parallel=0 -> no service-level limit |
| `TestCancellationPropagation` | Stop -> ctx.Done() propagates to sendIRC, API calls |
| `TestSchedulerFairness` | Multiple users queued -> FIFO across users |
| `TestUpdateServiceLimits` | Reload changes limits, running jobs unaffected |
| `TestDeliverySerialization` | Two users in same channel -> delivery is sequential |
| `TestSkipBusyBypass` | SkipBusy commands bypass queue entirely |
