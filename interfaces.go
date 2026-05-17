package main

import (
	"crypto/rand"
	"encoding/hex"
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

var rateLimiter RateLimiter = &globalRateLimiter{}
