package main

import (
	"sync"
	"testing"
)

func TestRunningStateFunctions(t *testing.T) {
	runningPrompts = make(map[string]int)
	runningMutex = sync.Mutex{}

	t.Run("initially not running", func(t *testing.T) {
		if getRunning("net", "#chan", "user") {
			t.Error("expected getRunning to return false for unknown key")
		}
	})

	t.Run("startedRunning sets state", func(t *testing.T) {
		startedRunning("net", "#chan", "user")
		if !getRunning("net", "#chan", "user") {
			t.Error("expected getRunning to return true after startedRunning")
		}
	})

	t.Run("stoppedRunning decrements state", func(t *testing.T) {
		startedRunning("net", "#chan2", "user2")
		stoppedRunning("net", "#chan2", "user2")
		if getRunning("net", "#chan2", "user2") {
			t.Error("expected getRunning to return false after stoppedRunning")
		}
	})

	t.Run("different users independent", func(t *testing.T) {
		startedRunning("net", "#chan", "userA")
		startedRunning("net", "#chan", "userB")
		stoppedRunning("net", "#chan", "userA")

		if !getRunning("net", "#chan", "userB") {
			t.Error("userB should still be running")
		}
		if getRunning("net", "#chan", "userA") {
			t.Error("userA should not be running")
		}
	})

	t.Run("stoppedRunning on non-existent key is safe", func(t *testing.T) {
		stoppedRunning("net", "#nonexistent", "user")
		if getRunning("net", "#nonexistent", "user") {
			t.Error("nonexistent key should not be running")
		}
	})

	t.Run("double stoppedRunning is safe", func(t *testing.T) {
		startedRunning("net", "#chan3", "user3")
		stoppedRunning("net", "#chan3", "user3")
		stoppedRunning("net", "#chan3", "user3")
		if getRunning("net", "#chan3", "user3") {
			t.Error("should not be running after double stoppedRunning")
		}
	})

	t.Run("double startedRunning keeps running", func(t *testing.T) {
		startedRunning("net", "#chan4", "user4")
		startedRunning("net", "#chan4", "user4")
		if !getRunning("net", "#chan4", "user4") {
			t.Error("should be running")
		}
	})

	t.Run("reference counting: two starts need two stops", func(t *testing.T) {
		runningPrompts = make(map[string]int)
		startedRunning("net", "#refchan", "user")
		startedRunning("net", "#refchan", "user")
		if !getRunning("net", "#refchan", "user") {
			t.Error("should be running after two starts")
		}
		stoppedRunning("net", "#refchan", "user")
		if !getRunning("net", "#refchan", "user") {
			t.Error("should still be running after one stop (count=1)")
		}
		stoppedRunning("net", "#refchan", "user")
		if getRunning("net", "#refchan", "user") {
			t.Error("should not be running after two stops (count=0)")
		}
	})

	t.Run("forceStopRunning resets to zero", func(t *testing.T) {
		runningPrompts = make(map[string]int)
		startedRunning("net", "#forcechan", "user")
		startedRunning("net", "#forcechan", "user")
		startedRunning("net", "#forcechan", "user")
		if !getRunning("net", "#forcechan", "user") {
			t.Error("should be running")
		}
		forceStopRunning("net", "#forcechan", "user")
		if getRunning("net", "#forcechan", "user") {
			t.Error("should not be running after forceStopRunning")
		}
	})

	t.Run("stoppedRunning after forceStopRunning stays at zero", func(t *testing.T) {
		runningPrompts = make(map[string]int)
		startedRunning("net", "#postforce", "user")
		forceStopRunning("net", "#postforce", "user")
		stoppedRunning("net", "#postforce", "user")
		if getRunning("net", "#postforce", "user") {
			t.Error("should not be running (count clamped at 0)")
		}
	})
}

func TestRunningStateConcurrency(t *testing.T) {
	runningPrompts = make(map[string]int)
	runningMutex = sync.Mutex{}

	const goroutines = 50
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				startedRunning("net", "#concurrent", "user")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				stoppedRunning("net", "#concurrent", "user")
			}
		}()
	}

	wg.Wait()
}
