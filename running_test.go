package main

import (
	"sync"
	"testing"
)

func TestRunningStateFunctions(t *testing.T) {
	runningPrompts = make(map[string]bool)
	runningMutex = sync.Mutex{}

	t.Run("initially not running", func(t *testing.T) {
		if getRunning("testchan") {
			t.Error("expected getRunning to return false for unknown channel")
		}
	})

	t.Run("startedRunning sets state", func(t *testing.T) {
		startedRunning("testchan")
		if !getRunning("testchan") {
			t.Error("expected getRunning to return true after startedRunning")
		}
	})

	t.Run("stoppedRunning clears state", func(t *testing.T) {
		startedRunning("testchan2")
		stoppedRunning("testchan2")
		if getRunning("testchan2") {
			t.Error("expected getRunning to return false after stoppedRunning")
		}
	})

	t.Run("multiple channels independent", func(t *testing.T) {
		startedRunning("chan1")
		startedRunning("chan2")
		stoppedRunning("chan1")

		if !getRunning("chan2") {
			t.Error("chan2 should still be running")
		}
		if getRunning("chan1") {
			t.Error("chan1 should not be running")
		}
	})

	t.Run("stoppedRunning on non-existent channel is safe", func(t *testing.T) {
		stoppedRunning("nonexistent")
		if getRunning("nonexistent") {
			t.Error("nonexistent channel should not be running")
		}
	})

	t.Run("double stoppedRunning is safe", func(t *testing.T) {
		startedRunning("chan3")
		stoppedRunning("chan3")
		stoppedRunning("chan3")
		if getRunning("chan3") {
			t.Error("chan3 should not be running after double stoppedRunning")
		}
	})

	t.Run("double startedRunning keeps true", func(t *testing.T) {
		startedRunning("chan4")
		startedRunning("chan4")
		if !getRunning("chan4") {
			t.Error("chan4 should be running")
		}
	})
}

func TestRunningStateConcurrency(t *testing.T) {
	runningPrompts = make(map[string]bool)
	runningMutex = sync.Mutex{}

	const goroutines = 50
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		chanName := "concurrent"
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				startedRunning(chanName)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				stoppedRunning(chanName)
			}
		}()
	}

	wg.Wait()
}
