package main

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

var rateMutex sync.Mutex
var rateLimits map[string]*rate.Limiter

func init() {
	rateLimits = make(map[string]*rate.Limiter)
}

func checkRate(network Network, key string) bool {
	rateMutex.Lock()
	defer rateMutex.Unlock()

	if val, ok := rateLimits[network.Name+key]; ok {
		return val.Allow()
	}
	rateLimits[network.Name+key] = rate.NewLimiter(rate.Every(time.Second), 2)
	return rateLimits[network.Name+key].Allow()
}
