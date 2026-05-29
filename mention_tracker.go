package main

import (
	"fmt"
	"sync"
)

type mentionState struct {
	count int
	muted bool
}

type mentionTracker struct {
	mu    sync.Mutex
	users map[string]*mentionState
}

func newMentionTracker() *mentionTracker {
	return &mentionTracker{
		users: make(map[string]*mentionState),
	}
}

func (t *mentionTracker) key(network string, userID int64) string {
	return fmt.Sprintf("%s:%d", network, userID)
}

func (t *mentionTracker) recordMention(network string, userID int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.key(network, userID)
	s, ok := t.users[k]
	if !ok {
		s = &mentionState{}
		t.users[k] = s
	}
	s.count++
	return s.count
}

func (t *mentionTracker) isMuted(network string, userID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.users[t.key(network, userID)]
	if !ok {
		return false
	}
	return s.muted
}

func (t *mentionTracker) setMuted(network string, userID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.key(network, userID)
	s, ok := t.users[k]
	if !ok {
		s = &mentionState{}
		t.users[k] = s
	}
	s.muted = true
}

func (t *mentionTracker) reset(network string, userID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.users, t.key(network, userID))
}

func (t *mentionTracker) sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, s := range t.users {
		if !s.muted {
			delete(t.users, k)
		}
	}
}
