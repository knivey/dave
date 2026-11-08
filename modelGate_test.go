package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
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

// TestModelGateGiveUpAfterAdmissionRollsBack pins the leak invariant: a
// waiter whose select observed ctx.Done/timer in the same instant the
// wake pass admitted it must roll its resident count back and re-run the
// wake pass, so a later queued model is not stranded behind a dead
// holder. Hand-builds the post-admission state (deterministic — no
// select race to win).
func TestModelGateGiveUpAfterAdmissionRollsBack(t *testing.T) {
	key := "gate://giveup-admitted"
	g := loadModelGate(key)

	g.mu.Lock()
	g.resident["mA"] = 1
	w := &modelGateWaiter{model: "mB", ready: make(chan struct{})}
	g.waiters = append(g.waiters, w)
	// Mirror release(): free mA, then run the wake pass, which admits mB
	// (count 1, removed from the queue, ready closed).
	g.releaseLocked("mA")
	g.wakeWaitersLocked(1)
	g.mu.Unlock()
	require.True(t, w.admitted, "hand-built state: mB must be admitted")

	// A new model queued behind the (about-to-abandon) holder.
	wC := &modelGateWaiter{model: "mC", ready: make(chan struct{})}
	g.mu.Lock()
	g.waiters = append(g.waiters, wC)
	g.mu.Unlock()

	// mB's waiter gives up AFTER admission — the race this test pins.
	g.giveUpWaiter(w, 1)

	require.True(t, wC.admitted, "rollback must re-run the wake pass and admit the queued waiter")
	g.mu.Lock()
	_, mBResident := g.resident["mB"]
	remaining := len(g.waiters)
	g.mu.Unlock()
	assert.False(t, mBResident, "admitted-then-abandoned waiter must roll back its resident count")
	assert.Equal(t, 0, remaining, "no stale queue entries")

	// Clean up via the public API and prove the gate still works. The
	// rollback's own wake pass admitted wC, so free its slot first — at
	// capacity 1 the probe acquire below would otherwise queue behind mC
	// and wedge on the valve.
	g.release("mC", 1)
	release, ok := acquireModelSlot(context.Background(), key, "mA", 1, time.Minute)
	require.True(t, ok, "gate must be usable after the rollback")
	release()
}

// TestEnqueueWithStartModelGateSuffix drives the REAL enqueue path — the
// regression guard for the self-occupancy bug: a post-hoc "slot free"
// consult counts the just-enqueued item's own slot and suppressed the
// bg suffix in exactly the contended cases it exists for. EnqueueWithStart
// reports the authoritative started fact instead.
func TestEnqueueWithStartModelGateSuffix(t *testing.T) {
	setupNoticesDefaults(t)
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	origQM := queueMgr
	queueMgr = newTestQM(t)
	t.Cleanup(func() { queueMgr = origQM })

	// One model resident on the gated server's capacity-1 gate.
	releaseHold, ok := acquireModelSlot(context.Background(), "gate://wiring", "mA", 1, time.Minute)
	require.True(t, ok)
	defer releaseHold()

	origServices := config.Services
	config.Services = map[string]Service{
		// The flagship scenario: parallel = 2, one slot busy with item A.
		"par2": {Type: "llama", BaseURL: "gate://wiring/v1", ModelsMax: 1, Parallel: 2},
		// The suppression scenario: parallel = 1, the only slot busy.
		"par1": {Type: "llama", BaseURL: "gate://wiring/v1", ModelsMax: 1, Parallel: 1},
	}
	t.Cleanup(func() { config.Services = origServices })
	queueMgr.UpdateServiceLimits(config.Services)

	releaseItems := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(releaseItems) }) }
	t.Cleanup(releaseAll)
	blocking := func(cx context.Context, output chan<- string) { <-releaseItems }

	// Flagship: B queues behind A's DELIVERY but starts executing
	// immediately on the free second slot — both suffixes must appear.
	_, startedA := queueMgr.EnqueueWithStart("net", "#w1", 1, "userA", "par2", "", blocking)
	require.True(t, startedA)
	posB, startedB := queueMgr.EnqueueWithStart("net", "#w1", 2, "userB", "par2", "", blocking)
	require.Positive(t, posB, "B queues behind A's delivery")
	require.True(t, startedB, "parallel=2 with one slot busy: B must start")
	out := modelGateQueueSuffix("par2", "mB", posB, startedB)
	assert.Contains(t, out, "background", "the bg suffix must survive its own enqueue (self-occupancy regression)")
	assert.Contains(t, out, "mB")
	assert.Contains(t, out, "mA")

	// Suppression: at parallel=1 the second item waits to start — the bg
	// suffix must drop while the model suffix stays.
	_, startedA1 := queueMgr.EnqueueWithStart("net", "#w2", 1, "userA", "par1", "", blocking)
	require.True(t, startedA1)
	posB1, startedB1 := queueMgr.EnqueueWithStart("net", "#w2", 2, "userB", "par1", "", blocking)
	require.Positive(t, posB1)
	require.False(t, startedB1, "parallel=1 with the slot busy: B must wait to start")
	out1 := modelGateQueueSuffix("par1", "mB", posB1, startedB1)
	assert.NotContains(t, out1, "background")
	assert.Contains(t, out1, "mB")

	releaseAll()
	waitForNotRunning(t, queueMgr, "net", "#w1", 1)
	waitForNotRunning(t, queueMgr, "net", "#w1", 2)
	waitForNotRunning(t, queueMgr, "net", "#w2", 1)
	waitForNotRunning(t, queueMgr, "net", "#w2", 2)
}

// TestGetModelForConfigCmd pins the three-map mirror: every command kind
// resolves its model, unknown triggers resolve empty (the suffix helper
// treats empty model as ungated).
func TestGetModelForConfigCmd(t *testing.T) {
	origNames := configCmdNames
	origCommands := config.Commands
	configCmdNames = map[string]string{
		"=c": "c", "=h": "h", "=g": "g", "=x": "x",
	}
	config.Commands = Commands{
		Completions: map[string]AIConfig{"c": {Model: "mC"}},
		Chats:       map[string]AIConfig{"h": {Model: "mH"}},
		Generators:  map[string]GeneratorConfig{"g": {AIConfig: AIConfig{Model: "mG"}}},
	}
	t.Cleanup(func() {
		configCmdNames = origNames
		config.Commands = origCommands
	})

	assert.Equal(t, "mC", getModelForConfigCmd("=c"))
	assert.Equal(t, "mH", getModelForConfigCmd("=h"))
	assert.Equal(t, "mG", getModelForConfigCmd("=g"))
	assert.Empty(t, getModelForConfigCmd("=x"), "unknown trigger resolves empty")
}

// TestExecutionWaitNotice pins the position-0 standalone wait line: the
// requests that get NO queue notice (their channel's delivery is idle)
// but which will not actually start generating now — a pre-existing
// silence older than the model gate. Exactly one reason — the primary
// blocker in time order (the slot wait precedes the gate: the turn
// cannot even start until a slot frees).
func TestExecutionWaitNotice(t *testing.T) {
	setupNoticesDefaults(t)
	clearModelGateCache()
	t.Cleanup(clearModelGateCache)

	// One model resident on the gated server's capacity-1 gate.
	releaseHold, ok := acquireModelSlot(context.Background(), "gate://execwait", "mA", 1, time.Minute)
	require.True(t, ok)
	defer releaseHold()

	origServices := config.Services
	config.Services = map[string]Service{
		"gatedsvc": {Type: "llama", BaseURL: "gate://execwait/v1", ModelsMax: 1},
		"offsvc":   {Type: "llama", BaseURL: "gate://execwait/v1", ModelsMax: -1},
		"plainsvc": {BaseURL: "https://api.example.com/v1"},
	}
	t.Cleanup(func() { config.Services = origServices })

	t.Run("queued items never get the standalone line", func(t *testing.T) {
		assert.Empty(t, executionWaitNotice("shrew", "gatedsvc", "mB", 1, false))
	})

	t.Run("slot wait when not started", func(t *testing.T) {
		out := executionWaitNotice("shrew", "gatedsvc", "mB", 0, false)
		assert.Contains(t, out, "shrew")
		assert.Contains(t, out, "free slot")
		assert.NotContains(t, out, "mB", "slot wait is the primary blocker — no model line")
	})

	t.Run("model wait when started and gate blocked", func(t *testing.T) {
		out := executionWaitNotice("shrew", "gatedsvc", "mB", 0, true)
		assert.Contains(t, out, "shrew")
		assert.Contains(t, out, "mB")
		assert.Contains(t, out, "mA")
		assert.NotContains(t, out, "free slot")
	})

	t.Run("silent when started and unblocked", func(t *testing.T) {
		assert.Empty(t, executionWaitNotice("shrew", "gatedsvc", "mA", 0, true))
	})

	t.Run("silent for off and ungated services", func(t *testing.T) {
		assert.Empty(t, executionWaitNotice("shrew", "offsvc", "mB", 0, true))
		assert.Empty(t, executionWaitNotice("shrew", "plainsvc", "mB", 0, true))
	})

	t.Run("silent with empty model when started", func(t *testing.T) {
		assert.Empty(t, executionWaitNotice("shrew", "gatedsvc", "", 0, true))
	})
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
		release, ok := acquireModelSlot(context.Background(), key, "mC", 2, time.Minute)
		cDone <- ok
		release() // registry entries are never deleted — don't leak the admitted slot across -count reruns
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
		name    string
		body    string
		wantCap int
		wantOK  bool
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
	// The turn-side wait notice is GONE by design: turn output sits in the
	// delivery buffer until the wait is over, so it would display stale
	// (Nov 8 production lesson). The queue-time suffix on the queued
	// notice is the wait explanation now.
	lines := drainOutput(t, outB, 8, 200*time.Millisecond)
	for _, l := range lines {
		assert.NotContains(t, l, "waiting for", "no turn-side wait notice expected, got %q", lines)
		assert.NotContains(t, l, "will load after", "no turn-side wait notice expected, got %q", lines)
	}

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
			assert.NotContains(t, l, "{wait}", "valve notice must expand {wait}")
			assert.NotContains(t, l, "{nick}", "valve notice must expand {nick}")
		}
	}
	assert.True(t, found, "model_wait_timeout notice expected, got %q", lines)
	// The default template addresses {nick} — an unfilled placeholder
	// would leak the literal "{nick}" to IRC.
	assert.NotContains(t, strings.Join(lines, "\n"), "{nick}")
	close(releaseA)
	drainOutput(t, outA, 8, time.Second)
}

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
	// Failure-mode safety: if an assertion fatals while mA's handler is
	// parked on <-releaseA, release it so the deferred server.Close()
	// (which blocks until outstanding requests complete) doesn't hang
	// the test until the go test timeout. This must be a FUNCTION-LEVEL
	// defer registered AFTER server.Close: defers run LIFO, so this
	// releases the parked handler BEFORE Close runs — a t.Cleanup would
	// run only after the test function returns, i.e. after the Close
	// hang it is meant to prevent. The select-guard is idempotent with
	// the green path's explicit close(releaseA).
	defer func() {
		select {
		case <-releaseA:
		default:
			close(releaseA)
		}
	}()

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
