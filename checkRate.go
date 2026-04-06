package main

import (
	"sync"

	"golang.org/x/time/rate"
)

var rateMutex sync.Mutex
var rateLimits map[string]*rate.Limiter

func init() {
	rateLimits = make(map[string]*rate.Limiter)
}

func checkRate(network Network, key string) bool {
	return rateLimiter.Allow(network.Name, key)
}
