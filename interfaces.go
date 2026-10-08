package main

import (
	"crypto/rand"
	"encoding/hex"
)

func generateConvID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var rateLimiter RateLimiter = &globalRateLimiter{}
