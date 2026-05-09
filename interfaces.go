package main

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"golang.org/x/time/rate"
)

func TruncateHistory(msgs []ChatMessage, maxHistory int) []ChatMessage {
	if len(msgs) > maxHistory+1 {
		newMsgs := []ChatMessage{msgs[0]}
		return append(newMsgs, msgs[len(msgs)-maxHistory:]...)
	}
	return msgs
}

func generateConvID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type RateLimiter interface {
	Allow(networkName, key string) bool
}

var rateLimiter RateLimiter = &globalRateLimiter{}

type globalRateLimiter struct{}

func (r *globalRateLimiter) Allow(networkName, key string) bool {
	rateMutex.Lock()
	defer rateMutex.Unlock()

	rateKey := networkName + key
	if entry, ok := rateLimits[rateKey]; ok {
		entry.lastUsed = time.Now()
		return entry.limiter.Allow()
	}
	rateLimits[rateKey] = &rateEntry{
		limiter:  rate.NewLimiter(rate.Every(time.Second), 2),
		lastUsed: time.Now(),
	}
	return rateLimits[rateKey].limiter.Allow()
}
