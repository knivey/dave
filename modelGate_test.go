package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
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
