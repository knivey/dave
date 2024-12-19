package main

import "sync"

var runningPrompts map[string]bool
var runningMutex sync.Mutex

func init() {
	runningPrompts = make(map[string]bool)
}

func startedRunning(netchan string) {
	runningMutex.Lock()
	runningPrompts[netchan] = true
	runningMutex.Unlock()
}

func getRunning(netchan string) bool {
	runningMutex.Lock()
	defer runningMutex.Unlock()
	if val, ok := runningPrompts[netchan]; ok && val {
		return true
	}
	return false
}

func stoppedRunning(netchan string) {
	runningMutex.Lock()
	runningPrompts[netchan] = false
	runningMutex.Unlock()
}
