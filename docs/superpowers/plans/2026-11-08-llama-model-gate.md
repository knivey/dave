# llama-server Model Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serialize whole dave turns across *distinct* models on single-model llama-server routers (discovered via `/props`), while same-model turns keep sharing the server — ending mid-generation model swaps.

**Architecture:** A package-level gate registry keyed by normalized baseurl (mirroring `sessionGate.go`) with FIFO waiters and same-model pass; capacity auto-detected from the router's `GET /props` (`role`/`max_instances`), cached per baseurl, overridable per service via `models_max`; acquired for the whole turn at three hook sites (`runTurn`, legacy `completion`, compaction's `callSummarizer`), each ordered before its model-load notice probe; a liveness valve mirrors the session gate's (the same delivery-FIFO wedge applies).

**Tech Stack:** Go 1.25 (stdlib `sync`, `net/http`, `encoding/json`), testify, logxi, httptest for fake llama servers.

**Spec:** `docs/superpowers/specs/2026-11-08-llama-model-gate-design.md` — read it before starting; this plan argues from it.

## Global Constraints

- Module `github.com/knivey/dave`, Go 1.25.0. Build: `go build -o dave .`
- Tests use `github.com/stretchr/testify` `assert`/`require` only; table-driven + `t.Run` per repo convention.
- **Every `logxi.New()` logger MUST call `SetLevel(logxi.LevelAll)`** or debug output is silently dropped.
- TOML struct tags are snake_case; every new config field gets a reference-list line AND a commented example line in the corresponding `config/*.toml` (Config Documentation Convention in AGENTS.md).
- Never `git add -f`. Never skip the review step after a task.
- Before every commit: the task's tests pass, plus `go fmt ./...` and `go vet ./...` are clean. Before the final task's commit: `go test ./...` passes.
- Preserve/extend block comments that explain non-obvious design decisions (`sessionGate.go` is the style model).
- Test globals note: package-level vars (`modelGateWaitLimit`, capacity cache) may be swapped in tests with `t.Cleanup` restore — the repo's established pattern (see `sessionTurnWaitLimit` swaps in `jobManager_test.go`). Not safe under `t.Parallel()`; do not add `t.Parallel()`.

## Review Focus

The five failure modes most likely to bite, each pinned by a test in its owning task:

1. **Admitted-in-the-same-instant race leaks a resident count** — a waiter's ctx cancels (or its valve fires) in the same instant the wake pass admits it; the admission must roll back or the gate blocks that model forever. → `TestModelGateCancelledWaiterAbortsAndDoesNotBlockLater` + `TestModelGateValveFires` (Task 1: both drive the exact timeout/cancel-while-admitted interleaving through the `w.admitted` rollback).
2. **Wake pass must share a just-admitted model** — queue `[B, B2]` behind resident A: releasing A must admit BOTH B and B2 in one pass (same-model sharing), never just one. → `TestModelGateWakePassSharesSameModel` (Task 1).
3. **Probe failure must not disable gating forever, and success must not probe per-turn** — a down `/props` gets a short-TTL failure entry (retried later); a success is cached for the process lifetime (exactly one HTTP hit across repeated resolutions). → `TestResolveModelGateCapacityFailureTTLRetries` + `TestResolveModelGateCapacityCachesSuccess` (Task 2).
4. **Override engagement corners** — `models_max = -1` disables even a `type = "llama"` service; `models_max > 0` engages even a non-llama service (the llama-swap escape hatch). → `TestResolveModelGateCapacityOverrides` rows + `TestRunTurnModelGateDisabledByOverride` (Tasks 2 and 4).
5. **Hook order: gate BEFORE the load-notice probe** — a queued turn must not emit a misleading "loading model" notice (or even hit `/v1/models`) while still waiting for another model. → `TestRunTurnModelGateSerializesModels` asserts zero `/v1/models` hits while B is queued (Task 4).

---

### Task 1: Model gate core (`modelGate.go`)

**Files:**
- Create: `modelGate.go`
- Test: `modelGate_test.go`

**Interfaces:**
- Consumes: nothing new (stdlib only; `newLogger` helper already exists).
- Produces (Task 2/4/5/6 rely on these exact names):
  - `normalizeModelGateKey(baseURL string) string`
  - `acquireModelSlot(ctx context.Context, key, model string, capacity int, waitLimit time.Duration) (release func(), ok bool)`
  - `modelGateBusy(key, model string, capacity int) (blocked bool, held string)` — `held` is comma-joined resident models
  - `modelGateWaitLimit time.Duration` (package var, 10m floor)
  - `modelGateWaitLimitFor(timeout time.Duration) time.Duration`
  - `loggerMG logxi.Logger`

- [ ] **Step 1: Write the failing tests**

Create `modelGate_test.go`:

```go
package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Gate tests use unique keys per test: registry entries are never
// deleted (mirrors sessionGate), so cross-test state must not collide.
// No require/assert inside goroutines — results cross via channels.

func TestModelGateSameModelShares(t *testing.T) {
	key := "gate://same-share"
	releaseA, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok)
	releaseB, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok, "same model must share the slot")
	releaseA()
	releaseB()
	blocked, held := modelGateBusy(key, "modelA", 1)
	assert.False(t, blocked)
	assert.Empty(t, held, "gate must be empty after both released")
}

func TestModelGateDifferentModelsSerialize(t *testing.T) {
	key := "gate://diff"
	releaseA, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok)

	acquiredB := make(chan struct{})
	failedB := make(chan struct{})
	go func() {
		releaseB, ok := acquireModelSlot(context.Background(), key, "modelB", 1, time.Minute)
		if ok {
			close(acquiredB)
			releaseB()
		} else {
			close(failedB)
		}
	}()

	select {
	case <-acquiredB:
		t.Fatal("modelB acquired while modelA resident on a capacity-1 gate")
	case <-failedB:
		t.Fatal("modelB waiter failed instead of waiting")
	case <-time.After(50 * time.Millisecond):
	}

	blocked, held := modelGateBusy(key, "modelB", 1)
	assert.True(t, blocked)
	assert.Equal(t, "modelA", held)

	releaseA()
	select {
	case <-acquiredB:
	case <-failedB:
		t.Fatal("modelB waiter failed after modelA released")
	case <-time.After(5 * time.Second):
		t.Fatal("modelB never acquired after modelA released")
	}
}

func TestModelGateFIFOOrder(t *testing.T) {
	key := "gate://fifo"
	releaseA, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok)

	bDone, cDone := make(chan struct{}), make(chan struct{})
	var releaseB, releaseC func()
	go func() {
		releaseB, _ = acquireModelSlot(context.Background(), key, "modelB", 1, time.Minute)
		close(bDone)
	}()
	time.Sleep(50 * time.Millisecond) // B queues first
	go func() {
		releaseC, _ = acquireModelSlot(context.Background(), key, "modelC", 1, time.Minute)
		close(cDone)
	}()
	time.Sleep(50 * time.Millisecond) // C queues second

	releaseA()
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("first waiter never acquired")
	}
	select {
	case <-cDone:
		t.Fatal("second waiter jumped the queue while the first held the slot")
	case <-time.After(50 * time.Millisecond):
	}
	require.NotNil(t, releaseB)
	releaseB()
	select {
	case <-cDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second waiter never acquired after first released")
	}
	require.NotNil(t, releaseC)
	releaseC()
}

func TestModelGateWakePassSharesSameModel(t *testing.T) {
	key := "gate://wake-pass"
	releaseA, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok)

	b1Done, b2Done := make(chan struct{}), make(chan struct{})
	var r1, r2 func()
	go func() {
		r1, _ = acquireModelSlot(context.Background(), key, "modelB", 1, time.Minute)
		close(b1Done)
	}()
	time.Sleep(50 * time.Millisecond)
	go func() {
		r2, _ = acquireModelSlot(context.Background(), key, "modelB", 1, time.Minute)
		close(b2Done)
	}()
	time.Sleep(50 * time.Millisecond)

	releaseA()
	<-b1Done
	<-b2Done // both admitted in the same wake pass: same-model sharing
	require.NotNil(t, r1)
	require.NotNil(t, r2)
	r1()
	r2()
}

func TestModelGateCancelledWaiterAbortsAndDoesNotBlockLater(t *testing.T) {
	key := "gate://cancel"
	releaseA, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok)

	ctx, cancel := context.WithCancel(context.Background())
	bDone := make(chan bool)
	go func() {
		_, ok := acquireModelSlot(ctx, key, "modelB", 1, time.Minute)
		bDone <- ok
	}()
	time.Sleep(50 * time.Millisecond) // let B queue
	cancel()
	require.False(t, <-bDone, "cancelled waiter must return ok=false")

	// The cancelled waiter must not block a later different-model waiter.
	cDone := make(chan struct{})
	var releaseC func()
	go func() {
		releaseC, _ = acquireModelSlot(context.Background(), key, "modelC", 1, time.Minute)
		close(cDone)
	}()
	time.Sleep(50 * time.Millisecond)
	releaseA()
	select {
	case <-cDone:
	case <-time.After(5 * time.Second):
		t.Fatal("later waiter blocked by a cancelled waiter's stale queue entry")
	}
	require.NotNil(t, releaseC)
	releaseC()
}

func TestModelGateValveFires(t *testing.T) {
	key := "gate://valve"
	releaseA, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok)
	defer releaseA()

	_, ok = acquireModelSlot(context.Background(), key, "modelB", 1, 40*time.Millisecond)
	require.False(t, ok, "valve must abort a waiter at its limit")

	// After the valve fired, the gate must still work for a new waiter
	// (the aborted waiter's entry removed, resident state intact).
	blocked, held := modelGateBusy(key, "modelB", 1)
	assert.True(t, blocked)
	assert.Equal(t, "modelA", held)
}

func TestModelGateUngatedPassthrough(t *testing.T) {
	release, ok := acquireModelSlot(context.Background(), "gate://off", "modelA", 0, time.Minute)
	require.True(t, ok, "capacity 0 must be an ungated no-op")
	release() // must not panic
}

func TestModelGateReleaseIdempotent(t *testing.T) {
	key := "gate://idem"
	release, ok := acquireModelSlot(context.Background(), key, "modelA", 1, time.Minute)
	require.True(t, ok)
	release()
	release() // no panic, no double-release effect
	release2, ok := acquireModelSlot(context.Background(), key, "modelB", 1, time.Minute)
	require.True(t, ok, "slot must be free after release")
	release2()
}

func TestModelGateCapacityTwo(t *testing.T) {
	key := "gate://cap2"
	rA, ok := acquireModelSlot(context.Background(), key, "mA", 2, time.Minute)
	require.True(t, ok)
	rB, ok := acquireModelSlot(context.Background(), key, "mB", 2, time.Minute)
	require.True(t, ok, "second distinct model fits capacity 2")

	cDone := make(chan bool)
	go func() {
		_, ok := acquireModelSlot(context.Background(), key, "mC", 2, time.Minute)
		cDone <- ok
	}()
	select {
	case ok := <-cDone:
		t.Fatalf("third distinct model admitted (ok=%v) at capacity 2", ok)
	case <-time.After(100 * time.Millisecond):
	}
	rA()
	select {
	case ok := <-cDone:
		require.True(t, ok, "third model must acquire after a slot freed")
	case <-time.After(5 * time.Second):
		t.Fatal("third model never admitted after a slot freed")
	}
	rB()
}

func TestNormalizeModelGateKey(t *testing.T) {
	assert.Equal(t,
		normalizeModelGateKey("http://host:8000/v1/"),
		normalizeModelGateKey("http://host:8000/v1"),
		"trailing slash and /v1 must not split the gate key")
	assert.Equal(t, "http://host:8000", normalizeModelGateKey("HTTP://HOST:8000/v1"),
		"scheme and host are lowercased, /v1 trimmed")
}

func TestModelGateWaitLimitFor(t *testing.T) {
	orig := modelGateWaitLimit
	t.Cleanup(func() { modelGateWaitLimit = orig })

	modelGateWaitLimit = 10 * time.Minute
	assert.Equal(t, 10*time.Minute, modelGateWaitLimitFor(30*time.Second),
		"short timeouts take the floor")
	assert.Equal(t, 45*time.Minute, modelGateWaitLimitFor(15*time.Minute),
		"long timeouts take 3x")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestModelGate|TestNormalizeModelGate' . -v`
Expected: FAIL — `undefined: acquireModelSlot` (and the other gate symbols).

- [ ] **Step 3: Write `modelGate.go`**

```go
package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

var loggerMG = newLogger("modelGate")

// DESIGN NOTE — why this exists: the llama-server multi-model router
// (GET /props → {"role":"router","max_instances":1,...}) can hold only
// max_instances models in memory. dave's `parallel` is a per-SERVICE
// execution knob and the model is a per-COMMAND cascade, so with
// parallel = 2 a command for model B dispatches while model A is
// mid-generation — the router preempts A (unload A, load B, finish B,
// reload A, re-prefill A). Costs: a long mid-stream stall for A's user,
// A's KV cache destroyed (full re-prefill — the slow-prefill regime
// behind the historical stream deaths, and idle `streamtimeout` risk
// because filtered keepalive comments never reset the idle timer), and
// two full model loads of pure thrash. Same-model concurrency is fine:
// the router interleaves requests to one resident model natively.
//
// This gate serializes whole TURNS across DISTINCT models on one
// server (keyed by baseurl), while same-model turns share exactly as
// `parallel` intends. The hold spans the ENTIRE turn (every tool-loop
// iteration and the final stream) so llama.cpp keeps the session's KV
// cache warm between iterations — per-iteration prefill collapses to
// just the new tokens — and there is exactly one model swap per
// collision, never per-iteration thrash.
//
// The wake rules make the queue head starvation-free under continuous
// same-model traffic: same-model waiters pass any resident model (they
// consume no capacity and never delay the head); a NEW model admits
// only at the head while capacity allows. Admissions within one wake
// pass can make later same-model waiters eligible in the SAME pass
// (admitting B at the head turns queued B' into a same-model waiter) —
// exactly the desired sharing.
//
// Liveness valve: the session gate's documented wedge applies verbatim
// (waiter = per-channel delivery head producing nothing; holder streams
// into its 200-message output buffer only the waiter's completed
// delivery can drain — holder blocks, gate never releases, channel
// wedges until ^stop). Same fix and calibration as sessionGate.go:
// max(10m floor, 3× the command's turn timeout). A misfire costs one
// visible "try again" notice; a wedge costs a jammed channel.
//
// Registry entries are never deleted (mirrors sessionGate): each is a
// few dozen bytes, bounded by the number of distinct API servers.
var modelGates sync.Map // map[string]*modelGate

type modelGate struct {
	mu       sync.Mutex
	resident map[string]int // model → in-flight turn count
	waiters  []*modelGateWaiter
}

type modelGateWaiter struct {
	model    string
	ready    chan struct{} // closed on admission
	admitted bool          // set under g.mu before ready is closed
}

func loadModelGate(key string) *modelGate {
	v, _ := modelGates.LoadOrStore(key, &modelGate{resident: make(map[string]int)})
	return v.(*modelGate)
}

// modelGateWaitLimit is the FLOOR of the valve limit; the effective
// per-waiter limit is max(floor, 3× turn timeout) via
// modelGateWaitLimitFor. Package var (not const) so tests can shrink it.
var modelGateWaitLimit = 10 * time.Minute

// modelGateWaitLimitFor mirrors sessionGateWaitLimitFor: a holder's
// hold is not one timeout budget (tool loops mint a fresh cfg.Timeout
// per iteration, MCP tool calls run outside it), so a multi-iteration
// hold legitimately exceeds 3× — firing mid-legitimate-hold costs one
// resend-class notice and nothing else, while a wedged holder never
// finishes. Calibration erring small is safe; the floor keeps small
// timeouts sensible.
func modelGateWaitLimitFor(timeout time.Duration) time.Duration {
	if derived := 3 * timeout; derived > modelGateWaitLimit {
		return derived
	}
	return modelGateWaitLimit
}

// normalizeModelGateKey collapses baseurl variants onto one server
// identity: trim trailing whitespace/slashes and a trailing "/v1",
// lowercase scheme+host (the path beyond /v1 is not meaningful for
// llama servers and keeps the rule total).
func normalizeModelGateKey(baseURL string) string {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u = strings.TrimSuffix(u, "/v1")
	if idx := strings.Index(u, "://"); idx >= 0 {
		u = u[:idx+3] + strings.ToLower(u[idx+3:])
	}
	return u
}

// acquireModelSlot blocks until the model may run on the server:
// admitted immediately when the model is already resident or a
// distinct-model slot is free; otherwise FIFO-queued (see wake rules
// in the file's DESIGN NOTE). capacity < 1 or empty model = ungated
// no-op. On success the caller MUST call the returned release (idempotent);
// on !ok the caller must abort the turn — cancelled waiters silently
// (the queue already told the user), valve-aborted ones with the
// model_wait_timeout notice (the caller distinguishes via ctx.Err()).
func acquireModelSlot(ctx context.Context, key, model string, capacity int, waitLimit time.Duration) (func(), bool) {
	if capacity < 1 || model == "" {
		return func() {}, true
	}
	if ctx.Err() != nil {
		return nil, false
	}
	g := loadModelGate(key)
	start := time.Now()

	g.mu.Lock()
	if g.tryAdmitLocked(model, capacity) {
		g.mu.Unlock()
		return g.releaseFunc(model, capacity), true
	}
	w := &modelGateWaiter{model: model, ready: make(chan struct{})}
	g.waiters = append(g.waiters, w)
	g.mu.Unlock()
	loggerMG.Info("model gate wait: model busy on server",
		"server", key, "model", model, "wait_limit", waitLimit)

	timer := time.NewTimer(waitLimit)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		g.giveUpWaiter(w, capacity)
		return nil, false
	case <-timer.C:
		g.giveUpWaiter(w, capacity)
		loggerMG.Warn("model gate wait exceeded limit, aborting waiter",
			"server", key, "model", model, "limit", waitLimit)
		return nil, false
	case <-w.ready:
		if ctx.Err() != nil {
			// Admitted in the same instant the caller was cancelled:
			// roll the admission back so resident counts don't leak.
			g.release(model, capacity)
			return nil, false
		}
		loggerMG.Info("model gate acquired after wait",
			"server", key, "model", model,
			"wait", time.Since(start).Round(time.Millisecond))
		return g.releaseFunc(model, capacity), true
	}
}

// giveUpWaiter cleans up a waiter that stopped waiting (ctx cancel or
// valve). If the wake pass had already admitted it — ready closed in
// the same instant the caller gave up — the admission is rolled back
// and the wake pass re-run, so the gate never leaks a resident count
// for a caller that walked away, and nobody admitted behind us is
// stranded behind a dead holder.
func (g *modelGate) giveUpWaiter(w *modelGateWaiter, capacity int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if w.admitted {
		g.releaseLocked(w.model)
		g.wakeWaitersLocked(capacity)
		return
	}
	for i, other := range g.waiters {
		if other == w {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			return
		}
	}
}

func (g *modelGate) release(model string, capacity int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseLocked(model)
	g.wakeWaitersLocked(capacity)
}

func (g *modelGate) releaseLocked(model string) {
	if g.resident[model] <= 1 {
		delete(g.resident, model)
	} else {
		g.resident[model]--
	}
}

// releaseFunc carries the capacity the holder acquired under, so a
// mid-flight capacity change (probe → pin swap) only affects later
// acquisitions. Idempotent via sync.Once.
func (g *modelGate) releaseFunc(model string, capacity int) func() {
	var once sync.Once
	return func() {
		once.Do(func() { g.release(model, capacity) })
	}
}

func (g *modelGate) tryAdmitLocked(model string, capacity int) bool {
	if g.resident[model] > 0 || len(g.resident) < capacity {
		g.resident[model]++
		return true
	}
	return false
}

// wakeWaitersLocked admits waiters in queue order in ONE pass:
//   - same-model waiters (their model is now resident) pass freely —
//     no capacity consumed, head never delayed;
//   - a new model admits only while capacity allows; since admissions
//     only grow len(resident) during the pass, a capacity-blocked head
//     structurally blocks every later NEW model (FIFO), while later
//     same-model waiters may still pass — the spec's fairness rule.
//
// close(w.ready) under the mutex is safe: it only makes the waiter's
// goroutine runnable; any lock it needs next is released before this
// call returns.
func (g *modelGate) wakeWaitersLocked(capacity int) {
	remaining := g.waiters[:0]
	for _, w := range g.waiters {
		switch {
		case g.resident[w.model] > 0:
			g.admitWaiterLocked(w)
		case len(g.resident) < capacity:
			g.admitWaiterLocked(w)
		default:
			remaining = append(remaining, w)
		}
	}
	g.waiters = remaining
}

func (g *modelGate) admitWaiterLocked(w *modelGateWaiter) {
	g.resident[w.model]++
	w.admitted = true
	close(w.ready)
}

// modelGateBusy reports (advisory, races with acquire/release at worst
// one notice) whether acquiring `model` would block right now, plus
// the comma-joined sorted resident models for the wait notice.
func modelGateBusy(key, model string, capacity int) (blocked bool, held string) {
	v, ok := modelGates.Load(key)
	if !ok {
		return false, ""
	}
	g := v.(*modelGate)
	g.mu.Lock()
	defer g.mu.Unlock()
	models := make([]string, 0, len(g.resident))
	for m := range g.resident {
		models = append(models, m)
	}
	sort.Strings(models)
	held = strings.Join(models, ", ")
	blocked = g.resident[model] == 0 && len(g.resident) >= capacity
	return blocked, held
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestModelGate|TestNormalizeModelGate' . -v`
Expected: all PASS. If FIFO/wake tests are flaky on a loaded machine, bump the 50ms sequencing sleeps — never reorder the channel assertions.

- [ ] **Step 5: fmt, vet, commit**

```bash
go fmt ./... && go vet ./...
git add modelGate.go modelGate_test.go
git commit -m "feat: model gate core — cross-model turn serialization for single-model llama routers"
```

---

### Task 2: Capacity resolution — `/props` probe, cache, `models_max` config

**Files:**
- Modify: `modelGate.go` (append capacity section)
- Modify: `config.go` — `Service` struct (~line 389, next to `Parallel`) + validation loop (~line 869, after the `Parallel` default)
- Modify: `main.go` — reload path (~line 589, next to `queueMgr.UpdateServiceLimits`)
- Modify: `config/services.toml` — reference list + example block
- Test: `modelGate_test.go` (append), `config_test.go` (append)

**Interfaces:**
- Consumes: `loggerMG`, `normalizeModelGateKey` (Task 1); `Service` struct (config.go).
- Produces (Tasks 4/5/6 rely on these):
  - `Service.ModelsMax int` (`toml:"models_max"`)
  - `resolveModelGateCapacity(llamaSvc bool, override int, baseURL, apiKey string, logger logxi.Logger) int` — 0 = ungated
  - `clearModelGateCache()`
  - consts `modelGateProbeTimeout`, `modelGateProbeFailureTTL`

- [ ] **Step 1: Write the failing tests**

Append to `modelGate_test.go`:

```go
func newPropsServer(t *testing.T, propsBody string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			if hits != nil {
				atomic.AddInt32(hits, 1)
			}
			fmt.Fprint(w, propsBody)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(clearModelGateCache)
	return srv
}

func TestProbeModelGateProps(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantCap    int
		wantOK     bool
	}{
		{"router max 1", `{"role":"router","max_instances":1}`, 1, true},
		{"router max 2", `{"role":"router","max_instances":2}`, 2, true},
		{"not a router", `{"role":"server","max_instances":1}`, 0, false},
		{"missing max_instances", `{"role":"router"}`, 0, false},
		{"zero max_instances", `{"role":"router","max_instances":0}`, 0, false},
		{"garbage", `not json`, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newPropsServer(t, tt.body, nil)
			cap, ok := probeModelGateProps(srv.URL+"/v1", "")
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantCap, cap)
		})
	}
}

func TestProbeModelGatePropsFailOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	cap, ok := probeModelGateProps(srv.URL+"/v1", "")
	assert.False(t, ok)
	assert.Equal(t, 0, cap)
}

func TestResolveModelGateCapacityOverrides(t *testing.T) {
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)
	// A props server that would answer 1 — overrides must never probe it.
	var hits int32
	srv := newPropsServer(t, `{"role":"router","max_instances":1}`, &hits)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)

	assert.Equal(t, 0, resolveModelGateCapacity(true, -1, srv.URL+"/v1", "", logger),
		"-1 disables even a llama service")
	assert.Equal(t, 0, resolveModelGateCapacity(false, 0, srv.URL+"/v1", "", logger),
		"auto never engages a non-llama service")
	assert.Equal(t, 2, resolveModelGateCapacity(false, 2, srv.URL+"/v1", "", logger),
		"positive override engages even a non-llama service")
	assert.Equal(t, int32(0), atomic.LoadInt32(&hits),
		"overrides must pin without probing /props")
}

func TestResolveModelGateCapacityCachesSuccess(t *testing.T) {
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)
	var hits int32
	srv := newPropsServer(t, `{"role":"router","max_instances":1}`, &hits)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	url := srv.URL + "/v1"

	for i := 0; i < 3; i++ {
		assert.Equal(t, 1, resolveModelGateCapacity(true, 0, url, "", logger),
			"auto resolution on a router with max_instances 1")
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits),
		"success is cached for the process lifetime — one probe, not one per turn")
}

func TestResolveModelGateCapacityFailureTTLRetries(t *testing.T) {
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	url := srv.URL + "/v1"

	assert.Equal(t, 0, resolveModelGateCapacity(true, 0, url, "", logger), "probe failure fails open")
	assert.Equal(t, 0, resolveModelGateCapacity(true, 0, url, "", logger), "failure still cached")
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "failure entry suppresses retries within its TTL")

	// Expire the failure entry: the next resolution probes again.
	modelGateCapMu.Lock()
	if e, ok := modelGateCapacities[normalizeModelGateKey(url)]; ok {
		e.retryAfter = time.Now().Add(-time.Second)
	}
	modelGateCapMu.Unlock()
	assert.Equal(t, 0, resolveModelGateCapacity(true, 0, url, "", logger))
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits), "expired failure entry retries the probe")
}

func TestPinModelGateCapacityStricterWins(t *testing.T) {
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)

	pinModelGateCapacity("gate://pin", 1, logger)
	pinModelGateCapacity("gate://pin", 2, logger) // conflicting override
	modelGateCapMu.RLock()
	got := modelGateCapacities["gate://pin"].capacity
	modelGateCapMu.RUnlock()
	assert.Equal(t, 1, got, "conflicting overrides keep the stricter capacity")
}

func TestResolveModelGateCapacityPinDuringProbeWins(t *testing.T) {
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/props":
			close(probeStarted)
			<-releaseProbe
			fmt.Fprint(w, `{"role":"router","max_instances":2}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	url := srv.URL + "/v1"

	resolved := make(chan int, 1)
	go func() { resolved <- resolveModelGateCapacity(true, 0, url, "", logger) }()
	<-probeStarted
	// An explicit override pins while the probe is still in flight.
	pinModelGateCapacity(normalizeModelGateKey(url), 1, logger)
	close(releaseProbe)

	select {
	case got := <-resolved:
		assert.Equal(t, 1, got, "a pin that lands mid-probe must win over the probe result")
	case <-time.After(5 * time.Second):
		t.Fatal("resolver never returned")
	}
	modelGateCapMu.RLock()
	e := modelGateCapacities[normalizeModelGateKey(url)]
	modelGateCapMu.RUnlock()
	require.NotNil(t, e)
	assert.True(t, e.pinned, "the probe must not have overwritten the pin")
	assert.Equal(t, 1, e.capacity)
}
```

Append to `config_test.go` (mirrors `TestLoadConfigDirRejectsInvalidInjectionRoleService` at config_test.go:478):

```go
func TestLoadConfigDirRejectsBadModelsMax(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": `
[localsvc]
type = "llama"
baseurl = "http://localhost:8000/v1"
models_max = -2
`,
	})
	defer os.RemoveAll(dir)

	_, err := loadConfigDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "models_max")
	assert.Contains(t, err.Error(), "localsvc")
}

func TestLoadConfigDirAcceptsModelsMax(t *testing.T) {
	for _, v := range []string{"-1", "0", "1", "3"} {
		dir := createTestConfigDir(t, "", map[string]string{
			"services.toml": `
[localsvc]
type = "llama"
baseurl = "http://localhost:8000/v1"
models_max = ` + v + `
`,
		})
		cfg, err := loadConfigDir(dir)
		require.NoError(t, err, "models_max = %s must load", v)
		require.Contains(t, cfg.Services, "localsvc")
		os.RemoveAll(dir)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestProbeModelGateProps|TestResolveModelGateCapacity|TestPinModelGate|TestLoadConfigDir.*ModelsMax' . -v`
Expected: FAIL — `undefined: probeModelGateProps`, `clearModelGateCache`, `pinModelGateCapacity`, `modelGateCapMu`; and `Service.ModelsMax` unknown in the config tests.

- [ ] **Step 3: Implement**

Append the capacity section to `modelGate.go`:

```go
// ---------------------------------------------------------------------------
// Capacity resolution: /props probe + per-server cache + models_max override.
//
// The router TELLS us its model capacity (GET /props →
// {"role":"router","max_instances":N}); nothing in services.toml is
// required in the common case. Anything else — old builds, plain
// single-model llama-server, llama-swap (whose /props needs ?model=),
// error, timeout — means ungated, which is correct for single-model
// servers (no cross-model collision is possible) and fails open like
// load_notice elsewhere. models_max overrides for setups /props can't
// answer: 0/unset = auto, >0 = forced capacity (pins the shared gate),
// -1 = off. Staleness errs safe: a stale LOW capacity over-serializes
// briefly; it never under-serializes.

const (
	modelGateProbeTimeout   = 2 * time.Second
	modelGateProbeFailureTTL = 60 * time.Second
)

var (
	modelGateCapMu        sync.RWMutex
	modelGateCapacities   = map[string]*modelGateCapacityEntry{}
	modelGateProbeClient  = &http.Client{Timeout: modelGateProbeTimeout}
)

type modelGateCapacityEntry struct {
	capacity   int
	pinned     bool      // set by an explicit models_max override
	retryAfter time.Time // zero = valid forever; set only on probe failure
}

// clearModelGateCache drops cached /props results. Called on /reload so
// a server-side max_instances change is picked up without a restart.
// Gate objects (resident/waiters) are NOT touched: in-flight turns must
// keep their accounting even if capacity just changed.
func clearModelGateCache() {
	modelGateCapMu.Lock()
	defer modelGateCapMu.Unlock()
	modelGateCapacities = make(map[string]*modelGateCapacityEntry)
}

// resolveModelGateCapacity returns the gate capacity for a server, or
// 0 when the requesting service's turns are ungated. llamaSvc is the
// service's type = "llama" (auto mode engages only for it); override is
// the service's models_max (0 auto, >0 forced, -1 off — defensive here
// so every call site stays correct without pre-checks).
func resolveModelGateCapacity(llamaSvc bool, override int, baseURL, apiKey string, logger logxi.Logger) int {
	if override < 0 {
		return 0
	}
	if override == 0 && !llamaSvc {
		return 0
	}
	key := normalizeModelGateKey(baseURL)
	if override > 0 {
		pinModelGateCapacity(key, override, logger)
		return override
	}

	modelGateCapMu.RLock()
	e, ok := modelGateCapacities[key]
	valid := ok && (e.pinned || e.retryAfter.IsZero() || time.Now().Before(e.retryAfter))
	var cached int
	if valid {
		cached = e.capacity // copy under RLock: pinModelGateCapacity mutates in place
	}
	modelGateCapMu.RUnlock()
	if valid {
		return cached
	}

	if capacity, probed := probeModelGateProps(baseURL, apiKey); probed {
		modelGateCapMu.Lock()
		// A pin may have landed while the probe was in flight (up to
		// modelGateProbeTimeout). Pins are never overwritten by probes
		// — spec invariant; the pin's capacity wins instead.
		if cur, exists := modelGateCapacities[key]; exists && cur.pinned {
			capacity = cur.capacity
		} else {
			modelGateCapacities[key] = &modelGateCapacityEntry{capacity: capacity}
		}
		modelGateCapMu.Unlock()
		return capacity
	}
	modelGateCapMu.Lock()
	if cur, exists := modelGateCapacities[key]; exists && cur.pinned {
		modelGateCapMu.Unlock()
		return cur.capacity
	}
	modelGateCapacities[key] = &modelGateCapacityEntry{retryAfter: time.Now().Add(modelGateProbeFailureTTL)}
	modelGateCapMu.Unlock()
	return 0
}

// pinModelGateCapacity records an explicit models_max override. Pins
// are never overwritten by probes; two services on one server pinning
// different values keep the stricter (smaller) — over-serialization is
// the safe direction.
func pinModelGateCapacity(key string, capacity int, logger logxi.Logger) {
	modelGateCapMu.Lock()
	defer modelGateCapMu.Unlock()
	e, ok := modelGateCapacities[key]
	if !ok {
		modelGateCapacities[key] = &modelGateCapacityEntry{capacity: capacity, pinned: true}
		return
	}
	if e.pinned {
		if capacity < e.capacity {
			e.capacity = capacity
			if logger != nil {
				logger.Warn("model gate: conflicting models_max overrides on one server, keeping the stricter",
					"server", key, "keeping", e.capacity, "saw", capacity)
			}
		}
		return
	}
	e.capacity = capacity
	e.pinned = true
	e.retryAfter = time.Time{}
}

// propsURL maps an API baseurl to the router's /props endpoint
// ("http://host:8000/v1" → "http://host:8000/props").
func propsURL(baseURL string) string {
	u := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	u = strings.TrimSuffix(u, "/v1")
	return u + "/props"
}

// probeModelGateProps asks the router how many models fit in memory.
// ok=false means "don't gate" (not an error worth surfacing).
func probeModelGateProps(baseURL, apiKey string) (int, bool) {
	req, err := http.NewRequest(http.MethodGet, propsURL(baseURL), nil)
	if err != nil {
		return 0, false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := modelGateProbeClient.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var props struct {
		Role         string `json:"role"`
		MaxInstances int    `json:"max_instances"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&props); err != nil {
		return 0, false
	}
	if props.Role != "router" || props.MaxInstances < 1 {
		return 0, false
	}
	return props.MaxInstances, true
}
```

Add imports `encoding/json`, `io`, `net/http` to `modelGate.go`'s import block.

`config.go` — in the `Service` struct, immediately after the `Parallel` field (~line 389):

```go
	ModelsMax            int            `toml:"models_max"`
```

In the service validation loop, immediately after the `Parallel` default block (~line 871):

```go
		if service.ModelsMax < -1 {
			return fmt.Errorf("services.%s: models_max must be -1 (off), 0 (auto-detect via /props), or a positive model capacity", name)
		}
```

`main.go` — in the reload path, immediately after `queueMgr.UpdateNotices(config.Notices)` (~line 591):

```go
	clearModelGateCache()
```

`config/services.toml` — reference list, after the `parallel` line (line 28):

```toml
#   models_max           (int, default: 0=auto)      Model-capacity gating for type = "llama" services: serialize whole turns across DISTINCT models when the server can only hold a few models in memory (the llama-server router's max_instances), while same-model turns keep running concurrently per `parallel`. 0 = auto-detect via GET /props; >0 = forced capacity (use when /props can't answer, e.g. llama-swap); -1 = disable. Service-level only (server property, no command cascade).
```

Example block, after `# parallel = 1` (line 60):

```toml
# models_max = 0
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestProbeModelGateProps|TestResolveModelGateCapacity|TestPinModelGate|TestLoadConfigDir.*ModelsMax' . -v`
Expected: all PASS.

- [ ] **Step 5: fmt, vet, commit**

```bash
go fmt ./... && go vet ./...
git add modelGate.go modelGate_test.go config.go config_test.go main.go config/services.toml
git commit -m "feat: /props capacity auto-detection + models_max override for the model gate"
```

---

### Task 3: Notices — `model_wait` / `model_wait_timeout`

**Files:**
- Modify: `notices.go` — `QueueNotices` struct (~line 33, after `TurnWaitTimeout`) + `setNoticesDefaults` (~line 236)
- Modify: `config/notices.toml` — reference list + example block
- Test: `notices_test.go` if it exists, else append to `modelGate_test.go` (check with `grep -l setNoticesDefaults *_test.go` first)

**Interfaces:**
- Consumes: `expandNotice`, `getNotices` (notices.go, existing).
- Produces: `QueueNotices.ModelWait`, `QueueNotices.ModelWaitTimeout` (string fields, `toml:"model_wait"` / `toml:"model_wait_timeout"`) — used by Task 4/5.

- [ ] **Step 1: Write the failing test**

```go
func TestNoticesModelWaitDefaults(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)
	assert.Contains(t, n.Queue.ModelWait, "{nick}")
	assert.Contains(t, n.Queue.ModelWait, "{model}")
	assert.Contains(t, n.Queue.ModelWait, "{held}")
	assert.Contains(t, n.Queue.ModelWaitTimeout, "{wait}")

	out := expandNotice(n.Queue.ModelWait, map[string]string{"nick": "shrew", "model": "mB", "held": "mA"})
	assert.Contains(t, out, "shrew")
	assert.Contains(t, out, "mB")
	assert.Contains(t, out, "mA")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestNoticesModelWaitDefaults . -v`
Expected: FAIL — `n.Queue.ModelWait undefined`.

- [ ] **Step 3: Implement**

`notices.go`, in `QueueNotices` after `TurnWaitTimeout`:

```go
	ModelWait              string `toml:"model_wait"`
	ModelWaitTimeout       string `toml:"model_wait_timeout"`
```

In `setNoticesDefaults`, after the `TurnWaitTimeout` block (~line 236):

```go
	if n.Queue.ModelWait == "" {
		n.Queue.ModelWait = "\x0307⏳ {nick}: {model} is waiting for {held} to finish (one model at a time on this server)...\x0f"
	}

	if n.Queue.ModelWaitTimeout == "" {
		n.Queue.ModelWaitTimeout = "\x0304⚠️ {nick}: gave up after {wait} waiting for the model — nothing was sent, please try again."
	}
```

`config/notices.toml` — reference list, after the `turn_wait_timeout` entry (line 12):

```toml
#   model_wait        (string)    Sent when a turn must wait for another model to finish on a single-model server. Placeholders: {nick}, {model}, {held}
#   model_wait_timeout (string)   Sent when that wait exceeded its safety limit (see services.toml models_max). Placeholders: {nick}, {wait}
```

Example block, after the `turn_wait_timeout` line (line 158):

```toml
# model_wait = "\x0307⏳ {nick}: {model} is waiting for {held} to finish (one model at a time on this server)...\x0f"
# model_wait_timeout = "\x0304⚠️ {nick}: gave up after {wait} waiting for the model — nothing was sent, please try again."
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestNoticesModelWaitDefaults . -v`
Expected: PASS.

- [ ] **Step 5: fmt, vet, commit**

```bash
go fmt ./... && go vet ./...
git add notices.go config/notices.toml notices_test.go modelGate_test.go
git commit -m "feat: model_wait / model_wait_timeout notices"
```

(Add whichever test file actually received the test.)

---

### Task 4: `runTurn` hook + `modelGateAcquire` helper + `chatRunner.modelsMax`

**Files:**
- Modify: `modelGate.go` (append the hook helper)
- Modify: `aiCmds.go` — `chatRunner` struct (~line 279, after `llamaService`), `newChatRunner` (~line 311 struct literal), `runTurn` top (~line 1325, before `cr.maybeNotifyModelLoad()`)
- Test: `modelGate_test.go` (append wiring tests)

**Interfaces:**
- Consumes: `acquireModelSlot`, `modelGateBusy`, `resolveModelGateCapacity`, `normalizeModelGateKey`, `modelGateWaitLimitFor` (Tasks 1-2); `Queue.ModelWait`/`ModelWaitTimeout` (Task 3); `getNotices`, `expandNotice`.
- Produces:
  - `chatRunner.modelsMax int` field
  - `modelGateAcquire` struct + `acquire() (func(), bool)` — reused by Task 5
  - `chatRunner.sendWarning` / `sendError` (existing methods, no change)

- [ ] **Step 1: Write the failing tests**

Append to `modelGate_test.go`:

```go
// newModelGateTurnRunner builds a chatRunner against an EXISTING test
// server — contenders MUST share one server so they share one gate key
// (mirrors newModelLoadTurnRunner, modelload_test.go:360, otherwise).
func newModelGateTurnRunner(t *testing.T, srvURL, model string, outputCh chan string, llama bool, modelsMax int) *chatRunner {
	t.Helper()
	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srvURL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	return &chatRunner{
		// LoadNotice must be ON: the probe-hit pin in the serialization
		// test is only real if turns actually probe /v1/models
		// (maybeNotifyModelLoad no-ops on nil LoadNotice).
		cfg:          AIConfig{Model: model, Timeout: 30 * time.Second, LoadNotice: boolPtr(true)},
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      srvURL + "/v1",
		apiKey:       "test-key",
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
		llamaService: llama,
		modelsMax:    modelsMax,
	}
}

// gateTurnHandler serves /v1/models and a chat-completions endpoint where
// the FIRST mA request announces itself on aParked and parks until
// releaseA closes; later mA requests complete immediately; mB arrivals
// are announced on bArrived (once).
func gateTurnHandler(t *testing.T, releaseA, aParked, bArrived chan struct{}, modelsHits *int32) http.Handler {
	var parked int32
	var bOnce sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(modelsHits, 1)
			fmt.Fprint(w, `{"data":[]}`)
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			switch body["model"] {
			case "mA":
				if atomic.CompareAndSwapInt32(&parked, 0, 1) {
					close(aParked)
					<-releaseA
				}
				fmt.Fprint(w, `{"id":"a","object":"chat.completion","created":1,"model":"mA","choices":[{"index":0,"message":{"role":"assistant","content":"a done"},"finish_reason":"stop"}]}`)
			case "mB":
				bOnce.Do(func() { close(bArrived) })
				fmt.Fprint(w, `{"id":"b","object":"chat.completion","created":1,"model":"mB","choices":[{"index":0,"message":{"role":"assistant","content":"b done"},"finish_reason":"stop"}]}`)
			default:
				t.Errorf("unexpected model %v", body["model"])
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func seedGateSession(t *testing.T) (int64, []ChatMessage) {
	t.Helper()
	setupTestDB(t)
	setupNoticesDefaults(t)
	sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "mA")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	return sid, messages
}

func TestRunTurnModelGateSerializesModels(t *testing.T) {
	sidA, messages := seedGateSession(t)
	sidB := createTestSession(t, "testnet", "#101", "plong", "testcmd", "svc", "mB")
	require.NoError(t, sessionMgr.AddMessage(sidB, ChatMessage{Role: RoleUser, Content: "yo"}))
	msgsB, err := sessionMgr.GetMessages(sidB)
	require.NoError(t, err)

	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	releaseA := make(chan struct{})
	aParked := make(chan struct{})
	bArrived := make(chan struct{})
	var modelsHits int32
	server := httptest.NewServer(gateTurnHandler(t, releaseA, aParked, bArrived, &modelsHits))
	t.Cleanup(server.Close)

	outA := make(chan string, 8)
	crA := newModelGateTurnRunner(t, server.URL, "mA", outA, true, 1)
	crA.sessionID = sidA
	outB := make(chan string, 8)
	crB := newModelGateTurnRunner(t, server.URL, "mB", outB, true, 1)
	crB.sessionID = sidB

	aDone := make(chan struct{})
	go func() { crA.runTurn(newTurnContext(sidA, messages)); close(aDone) }()
	select {
	case <-aParked:
	case <-time.After(5 * time.Second):
		t.Fatal("mA turn never reached the server")
	}

	hitsWhileQueued := atomic.LoadInt32(&modelsHits)
	bDone := make(chan struct{})
	go func() { crB.runTurn(newTurnContext(sidB, msgsB)); close(bDone) }()

	select {
	case <-bArrived:
		t.Fatal("mB reached the server while mA held the gate")
	case <-bDone:
		t.Fatal("mB turn finished while mA held the gate")
	case <-time.After(150 * time.Millisecond):
	}
	assert.Equal(t, hitsWhileQueued, atomic.LoadInt32(&modelsHits),
		"queued turn must not probe /v1/models — gate sits BEFORE the load notice")
	lines := drainOutput(t, outB, 8, 200*time.Millisecond)
	waited := false
	for _, l := range lines {
		if strings.Contains(l, "waiting for") {
			waited = true
		}
	}
	assert.True(t, waited, "model_wait notice expected, got %q", lines)

	close(releaseA)
	<-aDone
	select {
	case <-bArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("mB never reached the server after mA finished")
	}
	<-bDone
	drainOutput(t, outA, 8, time.Second)
}

func TestRunTurnModelGateSameModelShares(t *testing.T) {
	sidA, messages := seedGateSession(t)
	sidB := createTestSession(t, "testnet", "#101", "plong", "testcmd", "svc", "mA")
	require.NoError(t, sessionMgr.AddMessage(sidB, ChatMessage{Role: RoleUser, Content: "yo"}))
	msgsB, err := sessionMgr.GetMessages(sidB)
	require.NoError(t, err)

	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	releaseA := make(chan struct{})
	aParked := make(chan struct{})
	bArrived := make(chan struct{})
	var parked int32
	var bOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[]}`)
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			if body["model"] != "mA" {
				t.Errorf("unexpected model %v", body["model"])
			}
			if atomic.CompareAndSwapInt32(&parked, 0, 1) {
				close(aParked)
				<-releaseA // A parks, holding the gate
			} else {
				bOnce.Do(func() { close(bArrived) }) // B shares while A parks
			}
			fmt.Fprint(w, `{"id":"a","object":"chat.completion","created":1,"model":"mA","choices":[{"index":0,"message":{"role":"assistant","content":"a done"},"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	outA := make(chan string, 8)
	crA := newModelGateTurnRunner(t, server.URL, "mA", outA, true, 1)
	crA.sessionID = sidA
	outB := make(chan string, 8)
	crB := newModelGateTurnRunner(t, server.URL, "mA", outB, true, 1)
	crB.sessionID = sidB

	go func() { crA.runTurn(newTurnContext(sidA, messages)) }()
	select {
	case <-aParked:
	case <-time.After(5 * time.Second):
		t.Fatal("mA turn never reached the server")
	}
	go func() { crB.runTurn(newTurnContext(sidB, msgsB)) }()
	select {
	case <-bArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("same-model turn must share the server, not wait")
	}
	close(releaseA)
	drainOutput(t, outA, 8, time.Second)
	drainOutput(t, outB, 8, time.Second)
}

func TestRunTurnModelGateDisabledByOverride(t *testing.T) {
	sidA, messages := seedGateSession(t)
	sidB := createTestSession(t, "testnet", "#101", "plong", "testcmd", "svc", "mB")
	require.NoError(t, sessionMgr.AddMessage(sidB, ChatMessage{Role: RoleUser, Content: "yo"}))
	msgsB, err := sessionMgr.GetMessages(sidB)
	require.NoError(t, err)

	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	releaseA := make(chan struct{})
	aParked := make(chan struct{})
	bArrived := make(chan struct{})
	var modelsHits int32
	server := httptest.NewServer(gateTurnHandler(t, releaseA, aParked, bArrived, &modelsHits))
	t.Cleanup(server.Close)

	outA := make(chan string, 8)
	crA := newModelGateTurnRunner(t, server.URL, "mA", outA, true, 1)
	crA.sessionID = sidA
	outB := make(chan string, 8)
	// models_max = -1: gating off even for a llama service.
	crB := newModelGateTurnRunner(t, server.URL, "mB", outB, true, -1)
	crB.sessionID = sidB

	go func() { crA.runTurn(newTurnContext(sidA, messages)) }()
	select {
	case <-aParked:
	case <-time.After(5 * time.Second):
		t.Fatal("mA turn never reached the server")
	}
	go func() { crB.runTurn(newTurnContext(sidB, msgsB)) }()
	select {
	case <-bArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("models_max = -1 must leave turns ungated")
	}
	close(releaseA)
	drainOutput(t, outA, 8, time.Second)
	drainOutput(t, outB, 8, time.Second)
}

func TestRunTurnModelGateValveNotice(t *testing.T) {
	sidA, messages := seedGateSession(t)
	sidB := createTestSession(t, "testnet", "#101", "plong", "testcmd", "svc", "mB")
	require.NoError(t, sessionMgr.AddMessage(sidB, ChatMessage{Role: RoleUser, Content: "yo"}))
	msgsB, err := sessionMgr.GetMessages(sidB)
	require.NoError(t, err)

	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	origLimit := modelGateWaitLimit
	modelGateWaitLimit = 40 * time.Millisecond
	t.Cleanup(func() { modelGateWaitLimit = origLimit })

	releaseA := make(chan struct{})
	aParked := make(chan struct{})
	bArrived := make(chan struct{})
	var modelsHits int32
	server := httptest.NewServer(gateTurnHandler(t, releaseA, aParked, bArrived, &modelsHits))
	t.Cleanup(server.Close)
	outA := make(chan string, 8)
	crA := newModelGateTurnRunner(t, server.URL, "mA", outA, true, 1)
	crA.sessionID = sidA
	outB := make(chan string, 8)
	crB := newModelGateTurnRunner(t, server.URL, "mB", outB, true, 1)
	crB.sessionID = sidB
	crB.cfg.Timeout = time.Millisecond // waitLimitFor = max(40ms floor, 3ms) = 40ms

	go func() { crA.runTurn(newTurnContext(sidA, messages)) }()
	select {
	case <-aParked:
	case <-time.After(5 * time.Second):
		t.Fatal("mA turn never reached the server")
	}

	bDone := make(chan struct{})
	go func() { crB.runTurn(newTurnContext(sidB, msgsB)); close(bDone) }()
	select {
	case <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("valve-aborted turn never returned")
	}
	select {
	case <-bArrived:
		t.Fatal("valve-aborted turn must never reach the server")
	default:
	}
	lines := drainOutput(t, outB, 8, 200*time.Millisecond)
	found := false
	for _, l := range lines {
		if strings.Contains(l, "try again") {
			found = true
		}
	}
	assert.True(t, found, "model_wait_timeout notice expected, got %q", lines)
	close(releaseA)
	drainOutput(t, outA, 8, time.Second)
}
```

Note: `openai`, `option`, `logxi`, `girc` etc. are already imported by `modelGate_test.go`'s sibling tests — add any the compiler asks for (`github.com/openai/openai-go/v3` as used in `modelload_test.go`; check its import block and copy).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestRunTurnModelGate' . -v`
Expected: FAIL — `modelsMax` unknown field; B reaches the server freely (serialization assertions fail).

- [ ] **Step 3: Implement**

`aiCmds.go` — `chatRunner` struct, after the `llamaService` field block (~line 279):

```go
	// modelsMax mirrors the service's models_max gate override: 0 auto
	// (llama services only), >0 forced capacity, -1 disabled. See
	// modelGate.go.
	modelsMax int
```

`newChatRunner` return literal (~line 311), add after `llamaService: svc.Type == "llama",`:

```go
		modelsMax:    svc.ModelsMax,
```

`modelGate.go` — append the hook helper:

```go
// modelGateAcquire bundles what the user-facing hook sites need: the
// chat path fills it from chatRunner fields, the legacy completions
// path from its locals. warn/fail split mirrors the turn_wait
// precedent (advisory wait → sendWarning, valve abort → sendError).
type modelGateAcquire struct {
	ctx       context.Context
	llamaSvc  bool
	modelsMax int           // service models_max: 0 auto, >0 forced, -1 off
	baseURL   string
	apiKey    string
	model     string
	nick      string
	timeout   time.Duration // the command's turn timeout (valve calibration)
	warn      func(string)  // advisory wait-notice channel (nil = silent)
	fail      func(string)  // valve-abort notice channel (nil = silent)
	logger    logxi.Logger
}

// acquire resolves gating, sends the wait notice when the turn is about
// to block, and takes the whole-turn model slot. ok=false means the
// turn must abort: cancelled waiters silently (the queue already told
// the user), valve-aborted ones with the timeout notice already sent
// through fail. NOTE for the chat path: unlike the session gate this
// sits INSIDE the turn, after chat() persisted the user message — a
// valve abort leaves the user row, exactly like an API-error turn, so
// the notice says "try again", never "resend".
func (h modelGateAcquire) acquire() (func(), bool) {
	if h.modelsMax < 0 || (h.modelsMax == 0 && !h.llamaSvc) {
		return func() {}, true
	}
	capacity := resolveModelGateCapacity(h.llamaSvc, h.modelsMax, h.baseURL, h.apiKey, h.logger)
	if capacity < 1 {
		return func() {}, true
	}
	key := normalizeModelGateKey(h.baseURL)
	if blocked, held := modelGateBusy(key, h.model, capacity); blocked && h.warn != nil {
		n := getNotices()
		h.warn(expandNotice(n.Queue.ModelWait, map[string]string{
			"nick": h.nick, "model": h.model, "held": held,
		}))
	}
	start := time.Now()
	waitLimit := modelGateWaitLimitFor(h.timeout)
	release, ok := acquireModelSlot(h.ctx, key, h.model, capacity, waitLimit)
	if !ok {
		if h.ctx.Err() != nil {
			if h.logger != nil {
				h.logger.Info("turn skipped: cancelled while waiting for the model gate", "model", h.model)
			}
			return nil, false
		}
		if h.logger != nil {
			h.logger.Warn("turn aborted: model gate wait exceeded limit", "model", h.model, "limit", waitLimit)
		}
		if h.fail != nil {
			n := getNotices()
			h.fail(expandNotice(n.Queue.ModelWaitTimeout, map[string]string{
				"wait": time.Since(start).Round(time.Second).String(),
			}))
		}
		return nil, false
	}
	return release, true
}
```

`aiCmds.go` `runTurn` — insert at the very top of the function body, BEFORE `cr.maybeNotifyModelLoad()` (line 1329):

```go
	// Whole-turn model gate (modelGate.go DESIGN NOTE): serialize turns
	// across DISTINCT models on single-model llama routers; same-model
	// turns share. Acquired BEFORE the model-load notice so that notice
	// only fires once the load is actually imminent — a queued turn
	// must not emit a misleading "loading" notice (or even probe).
	releaseModel, modelOK := (modelGateAcquire{
		ctx: cr.ctx, llamaSvc: cr.llamaService, modelsMax: cr.modelsMax,
		baseURL: cr.baseURL, apiKey: cr.apiKey, model: cr.cfg.Model,
		nick: cr.nick, timeout: cr.cfg.Timeout,
		warn: cr.sendWarning, fail: cr.sendError, logger: cr.logger,
	}).acquire()
	if !modelOK {
		return true
	}
	defer releaseModel()

```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestRunTurnModelGate|TestModelGate|TestNoticesModelWait' . -v`
Expected: all PASS. Also run `go test -run 'TestRunTurn' . -count=1` to confirm no existing runTurn test broke.

- [ ] **Step 5: fmt, vet, commit**

```bash
go fmt ./... && go vet ./...
git add modelGate.go modelGate_test.go aiCmds.go
git commit -m "feat: whole-turn model gate on runTurn (before the load-notice probe)"
```

---

### Task 5: `completion()` hook

**Files:**
- Modify: `aiCmds.go` — `completion()` (~line 52: capture the whole Service; insert gate block before the load-notice block at ~line 72)
- Test: `modelGate_test.go` (append)

**Interfaces:**
- Consumes: `modelGateAcquire` + `.acquire()` (Task 4), `Service.ModelsMax` (Task 2).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `modelGate_test.go` (mirrors `TestCompletionModelLoadNoticeDisabled`, modelload_test.go:401):

```go
func TestCompletionModelGateSerializes(t *testing.T) {
	setupNoticesDefaults(t)
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	releaseA := make(chan struct{})
	aParked := make(chan struct{})
	bArrived := make(chan struct{})
	var parked int32
	var bOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			switch body["model"] {
			case "mA":
				if atomic.CompareAndSwapInt32(&parked, 0, 1) {
					close(aParked)
					<-releaseA
				}
				fmt.Fprint(w, `{"id":"a","object":"text_completion","created":1,"model":"mA","choices":[{"text":"a ok","index":0,"finish_reason":"stop"}]}`)
			case "mB":
				bOnce.Do(func() { close(bArrived) })
				fmt.Fprint(w, `{"id":"b","object":"text_completion","created":1,"model":"mB","choices":[{"text":"b ok","index":0,"finish_reason":"stop"}]}`)
			default:
				t.Errorf("unexpected model %v", body["model"])
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"localsvc": {
		Type: "llama", BaseURL: server.URL + "/v1", ModelsMax: 1,
		LoadNotice: boolPtr(false),
	}}
	defer func() { config.Services = prevServices }()

	mkCfg := func(model string) AIConfig {
		cfg := AIConfig{Name: "c", Service: "localsvc", Model: model, Timeout: 30 * time.Second}
		cfg.ApplyDefaults(config.Services["localsvc"])
		return cfg
	}
	e := girc.Event{Source: &girc.Source{Name: "shrew"}, Params: []string{"#101"}}

	outA := make(chan string, 4)
	aDone := make(chan struct{})
	go func() {
		completion(Network{Name: "testnet"}, nil, e, mkCfg("mA"), context.Background(), outA, "hello")
		close(aDone)
	}()
	select {
	case <-aParked:
	case <-time.After(5 * time.Second):
		t.Fatal("mA completion never reached the server")
	}

	outB := make(chan string, 4)
	bDone := make(chan struct{})
	go func() {
		completion(Network{Name: "testnet"}, nil, e, mkCfg("mB"), context.Background(), outB, "world")
		close(bDone)
	}()

	select {
	case <-bArrived:
		t.Fatal("mB completion reached the server while mA held the gate")
	case <-bDone:
		t.Fatal("mB completion finished while mA held the gate")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseA)
	<-aDone
	select {
	case <-bArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("mB completion never ran after mA finished")
	}
	<-bDone
	drainOutput(t, outA, 4, time.Second)
	drainOutput(t, outB, 4, time.Second)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestCompletionModelGateSerializes . -v`
Expected: FAIL — `mB completion reached the server while mA held the gate`.

- [ ] **Step 3: Implement**

`aiCmds.go` `completion()` — replace the config-read block (lines 53-57) with one that captures the whole service (behavior-identical reads):

```go
	var svc Service
	readConfig(func() {
		svc = config.Services[cfg.Service]
	})
	svcKey, svcBaseURL := svc.Key, svc.BaseURL
```

Then, after the `aiClient` construction and `logger :=` line, BEFORE the `apiCtx, cancel := context.WithTimeout(...)` block — and MOVE that apiCtx block to AFTER the gate (the gate wait must not burn the API-call timeout budget: a completion that parks at the gate then gets admitted would hit `context deadline exceeded` immediately; the chat path grants a fresh budget after its gate, this path must too) — insert:

```go
	// Whole-turn model gate (modelGate.go): the same cross-model
	// serialization the chat path applies in runTurn, ahead of the
	// load-notice probe for the same ordering reason.
	releaseModel, modelOK := (modelGateAcquire{
		ctx: ctx, llamaSvc: svc.Type == "llama", modelsMax: svc.ModelsMax,
		baseURL: svcBaseURL, apiKey: svcKey, model: cfg.Model,
		nick: e.Source.Name, timeout: cfg.Timeout,
		warn: func(msg string) { sendToOutput(msg, output, ctx) },
		fail: func(msg string) { sendToOutput(msg, output, ctx) },
		logger: logger,
	}).acquire()
	if !modelOK {
		return
	}
	defer releaseModel()

	apiCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
```

(`logger` already exists at that point — declared at line 64; move nothing, the block sits between `logger := ...` and the LoadNotice `if`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestCompletion' . -v`
Expected: all PASS (new + existing completion tests).

- [ ] **Step 5: fmt, vet, commit**

```bash
go fmt ./... && go vet ./...
git add aiCmds.go modelGate_test.go
git commit -m "feat: model gate on legacy completion()"
```

---

### Task 6: `callSummarizer()` hook

**Files:**
- Modify: `compaction.go` — `callSummarizer` (~line 600, after the `svcOK` check, before `transport := newDaveTransport(...)`)
- Test: `compaction_test.go` (append)

**Interfaces:**
- Consumes: `resolveModelGateCapacity`, `acquireModelSlot`, `normalizeModelGateKey`, `modelGateWaitLimitFor`, `loggerMG` (Tasks 1-2).
- Produces: nothing new.

- [ ] **Step 1: Write the failing test**

Append to `compaction_test.go`:

```go
func TestCallSummarizerModelGateWaits(t *testing.T) {
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	releaseHolder := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			if body["model"] == "mSum" {
				<-releaseHolder // hold the gate via the request itself
				fmt.Fprint(w, `{"id":"s","object":"chat.completion","created":1,"model":"mSum","choices":[{"index":0,"message":{"role":"assistant","content":"summary text"},"finish_reason":"stop"}]}`)
				return
			}
			fmt.Fprint(w, `{"id":"s","object":"chat.completion","created":1,"model":"mOther","choices":[{"index":0,"message":{"role":"assistant","content":"other"},"finish_reason":"stop"}]}`)
		case "/props":
			fmt.Fprint(w, `{"role":"router","max_instances":1}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"sumsvc": {Type: "llama", BaseURL: server.URL + "/v1"}}
	defer func() { config.Services = prevServices }()

	// Holder: a summarizer call for mSum parks inside the handler,
	// holding the gate.
	holderDone := make(chan struct{})
	go func() {
		_, _, _, err := callSummarizer(context.Background(),
			AIConfig{Service: "sumsvc", Model: "mSum", Timeout: 30 * time.Second},
			"sys", []ChatMessage{{Role: RoleUser, Content: "x"}}, 1, 100)
		assert.NoError(t, err)
		close(holderDone)
	}()

	// Wait until the holder's request is parked: its /props probe has
	// resolved capacity by then.
	deadline := time.Now().Add(5 * time.Second)
	capResolved := false
	for time.Now().Before(deadline) {
		modelGateCapMu.RLock()
		_, ok := modelGateCapacities[normalizeModelGateKey(server.URL+"/v1")]
		modelGateCapMu.RUnlock()
		if ok {
			capResolved = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.True(t, capResolved, "holder never resolved gate capacity")
	time.Sleep(100 * time.Millisecond) // let the mSum request park

	// A different-model summarizer call must block until the holder finishes.
	otherDone := make(chan error)
	go func() {
		_, _, _, err := callSummarizer(context.Background(),
			AIConfig{Service: "sumsvc", Model: "mOther", Timeout: 30 * time.Second},
			"sys", []ChatMessage{{Role: RoleUser, Content: "x"}}, 1, 100)
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		t.Fatalf("different-model summarizer ran while gate held (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseHolder)
	<-holderDone
	select {
	case err := <-otherDone:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("waiting summarizer never ran after the holder finished")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestCallSummarizerModelGateWaits . -v`
Expected: FAIL — `different-model summarizer ran while gate held`.

- [ ] **Step 3: Implement**

`compaction.go` — insert after the `if !svcOK { ... }` block (line 600), before `transport := newDaveTransport(nil, nil)`:

```go
	// Whole-turn model gate (modelGate.go): an ungated summarizer call
	// could itself swap the resident model mid-turn on a single-model
	// router. Silent variant — compaction has no IRC channel; a valve
	// abort surfaces as the error below and flows through compaction's
	// existing failure handling (session unchanged).
	if capacity := resolveModelGateCapacity(svc.Type == "llama", svc.ModelsMax, svc.BaseURL, svc.Key, loggerMG); capacity > 0 {
		releaseModel, ok := acquireModelSlot(ctx, normalizeModelGateKey(svc.BaseURL), cfg.Model, capacity, modelGateWaitLimitFor(cfg.Timeout))
		if !ok {
			return "", nil, 0, fmt.Errorf("summarizer service %q model gate wait aborted (model %q busy)", cfg.Service, cfg.Model)
		}
		defer releaseModel()
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestCallSummarizer|TestCompactSession' . -v`
Expected: all PASS.

- [ ] **Step 5: fmt, vet, commit**

```bash
go fmt ./... && go vet ./...
git add compaction.go compaction_test.go
git commit -m "feat: model gate on compaction summarizer calls"
```

---

### Task 7: Docs sweep + full verification

**Files:**
- Modify: `AGENTS.md` — new architecture bullet after the session turn gate bullet
- Modify: `docs/queue-and-sessions.md` — new "Model gating" section after "Parallel Execution"

**Interfaces:**
- Consumes: everything above (documentation only).
- Produces: docs.

- [ ] **Step 1: Add the AGENTS.md bullet**

After the **Session turn gate** bullet, add:

```markdown
- **Model gate** (`modelGate.go`, Nov 2026; spec `docs/superpowers/specs/2026-11-08-llama-model-gate-design.md`): llama-server routers with `max_instances` < distinct requested models preempt in-flight generations (unload A, serve B, reload A, re-prefill A) — a long mid-stream stall, A's KV destroyed, `streamtimeout` kill risk (filtered keepalives never reset the idle timer), and two model loads of thrash. The gate serializes whole TURNS across DISTINCT models per SERVER (registry keyed by normalized baseurl: trailing `/`+`/v1` trimmed, scheme+host lowercased) while same-model turns share exactly as `parallel` intends; the whole-turn hold keeps llama.cpp's KV cache warm across tool-loop iterations (per-iteration prefill = new tokens only). Capacity is AUTO-DETECTED from `GET /props` (`{"role":"router","max_instances":N}` — the router's own answer; probe is lazy, Bearer-authed, 2s timeout, fail-open like `load_notice`; success cached for the process lifetime, failure cached 60s then retried, `/reload` clears the cache via `clearModelGateCache`); service-level `models_max` overrides: 0/unset = auto (llama services only), >0 = forced capacity — PINS the shared gate, probes never overwrite a pin, conflicting pins keep the stricter + WARN (the llama-swap escape hatch whose `/props` can't answer), -1 = off; validation rejects < -1 at load. Wake rules make the FIFO queue head starvation-free: same-model waiters pass any resident model (no capacity consumed, head never delayed); a new model admits only at the head while capacity allows — admissions in one pass can make later same-model waiters eligible in the SAME pass (admitting B turns queued B' same-model: the desired sharing). Race safety: a waiter that gives up (ctx cancel / valve) in the same instant the wake pass admitted it rolls the admission back (`giveUpWaiter` checks `w.admitted` under the gate mutex) — resident counts never leak; release funcs are `sync.Once`-idempotent and carry the capacity they acquired under (a probe→pin swap mid-flight affects only later acquisitions). Liveness valve mirrors `sessionGateWaitLimitFor` (`modelGateWaitLimitFor` = max(10m floor, 3× turn timeout); floor is a package var tests shrink) because the session gate's delivery-FIFO wedge applies VERBATIM (waiter = delivery head producing nothing; holder streams into its 200-message output buffer only the waiter's completed delivery drains). DIFFERENCE from the session valve: this gate sits INSIDE the turn, after `chat()` persisted the user message, so a valve abort leaves the user row exactly like an API-error turn — notice says "try again", never "resend". Hook sites (each BEFORE its model-load notice so a queued turn neither probes nor emits a misleading "loading" notice): `runTurn` top via `modelGateAcquire.acquire()` (covers all four variants + ephemeral generators + async continuations), legacy `completion()` via the same helper, compaction `callSummarizer` via the raw functions (silent; abort returns an error through compaction's existing failure handling). Notices `[queue] model_wait` ({nick},{model},{held}) on wait start (advisory `modelGateBusy` check, `turn_wait` pattern) and `[queue] model_wait_timeout` ({nick},{wait}) on valve fire; cancelled waiters stay silent. Gate key = raw `cfg.Model` string (two commands aliasing one model over-serialize — safe direction; alias canonicalization deliberately deferred). Known residual: non-dave clients (owner webui; future img-mcp enhancement calls) bypass the gate — dave's resident set can drift stale, always toward over-serialization. Guarded by the `TestModelGate*` unit family + `TestNormalizeModelGateKey` + `TestModelGateWaitLimitFor` + `TestProbeModelGateProps*` + `TestResolveModelGateCapacity*` + `TestPinModelGateCapacityStricterWins` + `TestLoadConfigDir*ModelsMax` + `TestNoticesModelWaitDefaults` + `TestRunTurnModelGate*` + `TestCompletionModelGateSerializes` + `TestCallSummarizerModelGateWaits`.
```

- [ ] **Step 2: Add the queue doc section**

In `docs/queue-and-sessions.md`, after the "Parallel Execution" section, add:

```markdown
## Model gating (llama services)

`parallel` is per **service** and knows nothing about which **model** a
command uses. On a local llama-server router that can only hold one
model in memory at a time, two commands requesting different models
would otherwise trigger a mid-generation model swap (the server unloads
the running model, serves the new request, reloads, and re-prefills the
first — a long stall plus wasted work).

The model gate serializes whole turns across **distinct models** on the
same server, while same-model turns keep running concurrently. Capacity
is auto-detected from the server (`GET /props`, `max_instances`); a
service-level `models_max` can force it (see services.toml). A turn
waiting for another model tells the user (`⏳ … waiting for …`), and a
safety valve aborts waits that exceed 10 minutes / 3× the command
timeout.
```

- [ ] **Step 3: Full verification**

```bash
go build -o dave .
go test ./... 2>&1 | tail -20
go fmt ./... && go vet ./...
```

Expected: build OK, ALL tests pass across every package, fmt/vet clean.

- [ ] **Step 4: Commit**

```bash
git add AGENTS.md docs/queue-and-sessions.md
git commit -m "docs: model gate architecture notes"
```

---

## Self-Review (completed during planning)

- **Spec coverage:** gate semantics + wake rules (Task 1), valve + wedge rationale (Tasks 1/4), `/props` detection + cache + pinning (Task 2), `models_max` tri-state + validation + reload clear (Task 2), notices + defaults + docs (Task 3), three hooks ordered before probes (Tasks 4/5/6), services.toml + notices.toml reference docs (Tasks 2/3), AGENTS.md + queue doc (Task 7), test families per spec's Testing section (all). Spec's "no command cascade" honored (service field only).
- **Type consistency:** `acquireModelSlot(ctx, key, model, capacity, waitLimit)` used identically in Tasks 1/4/6; `resolveModelGateCapacity(llamaSvc, override, baseURL, apiKey, logger)` in Tasks 2/4/6; `modelGateAcquire` struct defined Task 4, reused Task 5 verbatim; `Service.ModelsMax` named identically everywhere.
- **Placeholder scan:** every step carries real code; no TBDs.
- **Review Focus:** all five lines carry pinned tests in Tasks 1, 2, and 4.
