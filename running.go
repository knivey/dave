package main

import (
	"context"
	"sync"
)

var runningPrompts map[string]int
var runningMutex sync.Mutex
var runningChanged chan struct{}

func init() {
	runningPrompts = make(map[string]int)
	runningChanged = make(chan struct{}, 1)
}

func notifyRunningChanged() {
	select {
	case runningChanged <- struct{}{}:
	default:
	}
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
	notifyRunningChanged()
}

func forceStopRunning(network, channel, nick string) {
	key := runningKey(network, channel, nick)
	runningMutex.Lock()
	runningPrompts[key] = 0
	runningMutex.Unlock()
	notifyRunningChanged()
}

func waitForIdleAndClaim(network, channel, nick string, ctx context.Context) bool {
	key := runningKey(network, channel, nick)
	for {
		runningMutex.Lock()
		if runningPrompts[key] == 0 {
			runningPrompts[key] = 1
			runningMutex.Unlock()
			return true
		}
		runningMutex.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-runningChanged:
		}
	}
}
