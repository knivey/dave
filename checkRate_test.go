package main

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

type mockRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	calls    map[string]int
}

func newMockRateLimiter() *mockRateLimiter {
	return &mockRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		calls:    make(map[string]int),
	}
}

func (m *mockRateLimiter) Allow(networkName, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls[networkName+key]++
	rateKey := networkName + key
	if val, ok := m.limiters[rateKey]; ok {
		return val.Allow()
	}
	m.limiters[rateKey] = rate.NewLimiter(rate.Every(time.Second), 2)
	return m.limiters[rateKey].Allow()
}

func TestCheckRateKeyFormat(t *testing.T) {
	t.Run("combines network and key", func(t *testing.T) {
		limiter := newMockRateLimiter()
		limiter.Allow("network1", "channel1")

		if _, ok := limiter.calls["network1channel1"]; !ok {
			t.Error("expected limiter to be called with combined key")
		}
	})

	t.Run("different keys are tracked separately", func(t *testing.T) {
		limiter := newMockRateLimiter()
		limiter.Allow("network1", "key1")
		limiter.Allow("network1", "key2")
		limiter.Allow("network2", "key1")

		if limiter.calls["network1key1"] != 1 {
			t.Errorf("network1key1 calls = %d, want 1", limiter.calls["network1key1"])
		}
		if limiter.calls["network1key2"] != 1 {
			t.Errorf("network1key2 calls = %d, want 1", limiter.calls["network1key2"])
		}
		if limiter.calls["network2key1"] != 1 {
			t.Errorf("network2key1 calls = %d, want 1", limiter.calls["network2key1"])
		}
	})
}

func TestRateLimiterInterface(t *testing.T) {
	t.Run("interface allows mock implementation", func(t *testing.T) {
		var _ RateLimiter = (*mockRateLimiter)(nil)
	})

	t.Run("global limiter implements interface", func(t *testing.T) {
		var _ RateLimiter = rateLimiter
	})
}
