package main

import "sync"

var runningPrompts map[string]int
var runningMutex sync.Mutex

func init() {
	runningPrompts = make(map[string]int)
}

func runningKey(network, channel, nick string) string {
	return network + channel + nick
}

func startedRunning(network, channel, nick string) {
	key := runningKey(network, channel, nick)
	runningMutex.Lock()
	runningPrompts[key]++
	runningMutex.Unlock()
}

func getRunning(network, channel, nick string) bool {
	key := runningKey(network, channel, nick)
	runningMutex.Lock()
	defer runningMutex.Unlock()
	return runningPrompts[key] > 0
}

func stoppedRunning(network, channel, nick string) {
	key := runningKey(network, channel, nick)
	runningMutex.Lock()
	if runningPrompts[key] > 0 {
		runningPrompts[key]--
	}
	runningMutex.Unlock()
}

func forceStopRunning(network, channel, nick string) {
	key := runningKey(network, channel, nick)
	runningMutex.Lock()
	runningPrompts[key] = 0
	runningMutex.Unlock()
}
