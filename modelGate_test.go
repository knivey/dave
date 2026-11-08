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
