package main

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMentionTracker_RecordAndCount(t *testing.T) {
	tr := newMentionTracker()

	count := tr.recordMention("testnet", 1)
	assert.Equal(t, 1, count)
	assert.False(t, tr.isMuted("testnet", 1))

	count = tr.recordMention("testnet", 1)
	assert.Equal(t, 2, count)
}

func TestMentionTracker_SetMuted(t *testing.T) {
	tr := newMentionTracker()

	assert.False(t, tr.isMuted("testnet", 1))
	tr.setMuted("testnet", 1)
	assert.True(t, tr.isMuted("testnet", 1))
}

func TestMentionTracker_Reset(t *testing.T) {
	tr := newMentionTracker()

	tr.recordMention("testnet", 1)
	tr.setMuted("testnet", 1)
	assert.True(t, tr.isMuted("testnet", 1))

	tr.reset("testnet", 1)
	assert.False(t, tr.isMuted("testnet", 1))

	count := tr.recordMention("testnet", 1)
	assert.Equal(t, 1, count)
}

func TestMentionTracker_IsMutedUnknown(t *testing.T) {
	tr := newMentionTracker()
	assert.False(t, tr.isMuted("testnet", 999))
}

func TestMentionTracker_Sweep(t *testing.T) {
	tr := newMentionTracker()

	tr.recordMention("testnet", 1)
	tr.setMuted("testnet", 2)
	tr.recordMention("testnet", 3)

	tr.sweep()

	assert.False(t, tr.isMuted("testnet", 1))
	assert.True(t, tr.isMuted("testnet", 2))
	assert.False(t, tr.isMuted("testnet", 3))
}

func TestMentionTracker_Concurrent(t *testing.T) {
	tr := newMentionTracker()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.recordMention("testnet", 1)
			tr.isMuted("testnet", 1)
			tr.reset("testnet", 1)
		}()
	}
	wg.Wait()
}

func TestMentionTracker_DifferentUsers(t *testing.T) {
	tr := newMentionTracker()

	tr.recordMention("testnet", 1)
	count := tr.recordMention("testnet", 2)
	assert.Equal(t, 1, count)
}
