package main

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type rateEntry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

var rateMutex sync.Mutex
var rateLimits map[string]*rateEntry

func init() {
	rateLimits = make(map[string]*rateEntry)
}

func checkRate(network Network, key string) bool {
	return rateLimiter.Allow(network.Name, key)
}

func startRateLimitGC() {
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			sweepRateLimits()
		}
	}()
}

func sweepRateLimits() {
	rateMutex.Lock()
	defer rateMutex.Unlock()
	now := time.Now()
	for k, v := range rateLimits {
		if now.Sub(v.lastUsed) > time.Hour {
			delete(rateLimits, k)
		}
	}
}
