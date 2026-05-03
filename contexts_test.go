package main

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

type mockContextStore struct {
	mu      sync.Mutex
	context map[string]ChatContext
}

func newMockContextStore() *mockContextStore {
	return &mockContextStore{
		context: make(map[string]ChatContext),
	}
}

func (m *mockContextStore) Add(key string, config AIConfig, message ChatMessage) []ChatMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx := m.context[key]
	ctx.Config = config
	ctx.Messages = append(ctx.Messages, message)
	if len(ctx.Messages) > config.MaxHistory+1 {
		newMsgs := []ChatMessage{ctx.Messages[0]}
		ctx.Messages = append(newMsgs, ctx.Messages[len(ctx.Messages)-config.MaxHistory:]...)
	}
	m.context[key] = ctx
	return m.context[key].Messages
}

func (m *mockContextStore) Get(key string) ChatContext {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.context[key]
}

func (m *mockContextStore) Clear(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.context[key] = ChatContext{}
}

func (m *mockContextStore) Exists(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, ok := m.context[key]
	return ok && len(ctx.Messages) > 0
}

func TestTruncateHistory(t *testing.T) {
	tests := []struct {
		name        string
		messages    []ChatMessage
		maxHistory  int
		wantLen     int
		wantFirstIs []ChatMessage
	}{
		{
			name:       "empty messages",
			messages:   []ChatMessage{},
			maxHistory: 10,
			wantLen:    0,
		},
		{
			name: "under limit",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi"},
			},
			maxHistory: 10,
			wantLen:    3,
		},
		{
			name: "exactly at limit",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "1"},
				{Role: RoleUser, Content: "2"},
				{Role: RoleUser, Content: "3"},
			},
			maxHistory: 3,
			wantLen:    4,
		},
		{
			name: "over limit keeps system prompt and last messages",
			messages: []ChatMessage{
				{Role: RoleSystem, Content: "sys"},
				{Role: RoleUser, Content: "1"},
				{Role: RoleAssistant, Content: "a1"},
				{Role: RoleUser, Content: "2"},
				{Role: RoleAssistant, Content: "a2"},
				{Role: RoleUser, Content: "3"},
				{Role: RoleAssistant, Content: "a3"},
			},
			maxHistory: 3,
			wantLen:    4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateHistory(tt.messages, tt.maxHistory)
			assert.Len(t, got, tt.wantLen, "TruncateHistory() len")
			if len(got) > 0 {
				assert.Equal(t, RoleSystem, got[0].Role, "TruncateHistory()[0].Role")
			}
		})
	}
}

func TestChatContextStore(t *testing.T) {
	store := newMockContextStore()

	t.Run("initially empty", func(t *testing.T) {
		assert.False(t, store.Exists("key1"), "expected Exists() to return false for new key")
	})

	t.Run("Add creates context", func(t *testing.T) {
		config := AIConfig{MaxHistory: 5}
		msg := ChatMessage{Role: RoleUser, Content: "hello"}
		store.Add("key1", config, msg)

		assert.True(t, store.Exists("key1"), "expected Exists() to return true after Add")
		ctx := store.Get("key1")
		assert.Len(t, ctx.Messages, 1, "Messages")
	})

	t.Run("Clear removes context", func(t *testing.T) {
		store.Clear("key1")
		assert.False(t, store.Exists("key1"), "expected Exists() to return false after Clear")
	})

	t.Run("Add truncates history", func(t *testing.T) {
		config := AIConfig{MaxHistory: 2}
		msg := ChatMessage{Role: RoleSystem, Content: "sys"}
		store.Add("key2", config, msg)
		for i := 0; i < 10; i++ {
			msg := ChatMessage{Role: RoleUser, Content: string(rune('0' + i))}
			store.Add("key2", config, msg)
		}

		ctx := store.Get("key2")
		assert.Len(t, ctx.Messages, 3, "Messages (maxHistory+1)")
		assert.Equal(t, RoleSystem, ctx.Messages[0].Role, "First message should be system")
	})

	t.Run("preserves config", func(t *testing.T) {
		config := AIConfig{MaxHistory: 3, Temperature: 0.7}
		msg := ChatMessage{Role: RoleUser, Content: "test"}
		store.Add("key3", config, msg)

		ctx := store.Get("key3")
		assert.Equal(t, 3, ctx.Config.MaxHistory, "Config.MaxHistory")
		assert.Equal(t, float32(0.7), ctx.Config.Temperature, "Config.Temperature")
	})

	t.Run("Get on non-existent returns empty", func(t *testing.T) {
		ctx := store.Get("nonexistent")
		assert.Len(t, ctx.Messages, 0, "Get() on nonexistent key should return empty context")
	})
}

func TestChatContextStoreConcurrency(t *testing.T) {
	store := newMockContextStore()
	config := AIConfig{MaxHistory: 10}

	var wg sync.WaitGroup
	const goroutines = 50
	const messagesPerGoroutine = 20

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < messagesPerGoroutine; j++ {
				msg := ChatMessage{
					Role:    RoleUser,
					Content: string(rune('a' + id%26)),
				}
				store.Add("shared_key", config, msg)
			}
		}(i)
	}

	wg.Wait()

	ctx := store.Get("shared_key")
	assert.NotEmpty(t, ctx.Messages, "expected messages after concurrent adds")
}
