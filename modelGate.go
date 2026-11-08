package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	logxi "github.com/mgutz/logxi/v1"
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
	return strings.ToLower(u)
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
	modelGateProbeTimeout    = 2 * time.Second
	modelGateProbeFailureTTL = 60 * time.Second
)

var (
	modelGateCapMu       sync.RWMutex
	modelGateCapacities  = map[string]*modelGateCapacityEntry{}
	modelGateProbeClient = &http.Client{Timeout: modelGateProbeTimeout}
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
