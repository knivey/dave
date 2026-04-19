package main

import "sync"

var runningPrompts map[string]int
var runningMutex sync.Mutex

func init() {
	runningPrompts = make(map[string]int)
}

func startedRunning(netchan string) {
	runningMutex.Lock()
	runningPrompts[netchan]++
	runningMutex.Unlock()
}

func getRunning(netchan string) bool {
	runningMutex.Lock()
	defer runningMutex.Unlock()
	return runningPrompts[netchan] > 0
}

func stoppedRunning(netchan string) {
	runningMutex.Lock()
	if runningPrompts[netchan] > 0 {
		runningPrompts[netchan]--
	}
	runningMutex.Unlock()
}

func forceStopRunning(netchan string) {
	runningMutex.Lock()
	runningPrompts[netchan] = 0
	runningMutex.Unlock()
}
