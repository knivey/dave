package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTurnMailboxPushDrainClose(t *testing.T) {
	mb := openTurnMailbox(42)
	t.Cleanup(func() { mb.closeAndDrain() })

	assert.True(t, mb.push("one"))
	assert.True(t, mb.push("two"))
	assert.Equal(t, []string{"one", "two"}, mb.drain())
	assert.Empty(t, mb.drain(), "drain clears pending")

	assert.Empty(t, mb.closeAndDrain())
	assert.False(t, mb.push("three"), "closed mailbox rejects pushes")
}

func TestTurnMailboxForUnknownSession(t *testing.T) {
	assert.Nil(t, turnMailboxFor(999999))
}

func TestTurnMailboxForReturnsOpened(t *testing.T) {
	mb := openTurnMailbox(7)
	t.Cleanup(func() { mb.closeAndDrain() })
	require.Same(t, mb, turnMailboxFor(7))
}

func TestTurnMailboxOpenReplacesStale(t *testing.T) {
	first := openTurnMailbox(7)
	assert.True(t, first.push("old"))
	second := openTurnMailbox(7)
	t.Cleanup(func() { second.closeAndDrain() })
	require.NotSame(t, first, second)
	assert.False(t, first.push("late"), "stale mailbox must reject after replacement")
	assert.Empty(t, second.drain(), "replacement starts fresh")
}

// Review Focus #1: every successful push appears in exactly one drain —
// the absorption obligation. Pushers race a single closeAndDrain; the
// count of accepted pushes must equal the returned stragglers (no drain
// happens before the close in this test), and no push may succeed after
// the close completed.
func TestTurnMailboxPushVsCloseExactlyOnce(t *testing.T) {
	mb := openTurnMailbox(11)
	const pushers = 32
	var accepted atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	closed := make(chan []string, 1)

	for i := 0; i < pushers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if mb.push(fmt.Sprintf("m%d", i)) {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		closed <- mb.closeAndDrain()
	}()
	close(start)
	wg.Wait()

	drained := <-closed
	assert.Equal(t, accepted.Load(), int64(len(drained)),
		"accepted pushes must equal drained messages — exactly once")
	assert.False(t, mb.push("late"), "push after close must fail")
}

func TestTryAbsorbMention(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	t.Run("open mailbox absorbs and returns receipt", func(t *testing.T) {
		mb := openTurnMailbox(50)
		t.Cleanup(func() { mb.closeAndDrain() })
		txt, ok := tryAbsorbMention(50, "make it look like this", "hastur")
		assert.True(t, ok)
		assert.Contains(t, txt, "hastur")
		assert.Equal(t, []string{"make it look like this"}, mb.drain())
	})

	t.Run("no mailbox falls back", func(t *testing.T) {
		_, ok := tryAbsorbMention(999999, "x", "hastur")
		assert.False(t, ok)
	})

	t.Run("closed mailbox falls back", func(t *testing.T) {
		mb := openTurnMailbox(51)
		mb.closeAndDrain()
		_, ok := tryAbsorbMention(51, "x", "hastur")
		assert.False(t, ok)
	})
}

func TestHandleMentionAbsorbsIntoLiveTurn(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "absorbuser")
	sid := createTestSession(t, "testnet", "#absorb", "absorbuser", "testchat", "", "")
	insertTestMessage(t, sid, "system", "you are helpful")

	// handleMention -> getSessionConfig needs the chat command registered.
	configMu.Lock()
	old := config
	c := config
	c.Commands.Chats = map[string]AIConfig{"testchat": makeTestAIConfig()}
	config = c
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		config = old
		configMu.Unlock()
	})

	// Fresh queue: the absorbed path must never touch it.
	origQM := queueMgr
	qm := NewQueueManager(NoticesConfig{}, 5)
	qm.Start()
	queueMgr = qm
	t.Cleanup(func() { queueMgr = origQM; qm.Stop() })

	// Fresh mention tracker so earlier tests can't mute us.
	origTracker := theMentionTracker
	theMentionTracker = newMentionTracker()
	t.Cleanup(func() { theMentionTracker = origTracker })

	// The package logger is only initialized in main(); handleMention logs
	// on this path. Swap per repo convention (see main_test.go).
	origLogger := logger
	logger = newTestLogger()
	t.Cleanup(func() { logger = origLogger })

	mb := openTurnMailbox(sid)
	t.Cleanup(func() { mb.closeAndDrain() })

	network := Network{Name: "testnet"}
	client := chatGateClient()
	event := chatGateEvent("absorbuser", "#absorb")

	handleMention(network, client, event, "#absorb", "just make it look like this")

	got := mb.drain()
	require.Len(t, got, 1, "message was not absorbed")
	assert.Equal(t, "just make it look like this", got[0])
	cur, pending := qm.QueueStatus("testnet", "#absorb", userID)
	assert.Nil(t, cur, "absorbed mention must not enqueue a queue item")
	assert.Empty(t, pending)
}

func TestHandleMentionFallsBackWhenNoMailbox(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	userID := ensureTestUser(t, "testnet", "absorbuser2")
	sid := createTestSession(t, "testnet", "#absorb2", "absorbuser2", "testchat", "", "")
	insertTestMessage(t, sid, "system", "you are helpful")

	configMu.Lock()
	old := config
	c := config
	c.Commands.Chats = map[string]AIConfig{"testchat": makeTestAIConfig()}
	config = c
	configMu.Unlock()
	t.Cleanup(func() {
		configMu.Lock()
		config = old
		configMu.Unlock()
	})

	origQM := queueMgr
	qm := NewQueueManager(NoticesConfig{}, 5)
	qm.Start()
	queueMgr = qm
	t.Cleanup(func() { queueMgr = origQM; qm.Stop() })

	origTracker := theMentionTracker
	theMentionTracker = newMentionTracker()
	t.Cleanup(func() { theMentionTracker = origTracker })

	origLogger := logger
	logger = newTestLogger()
	t.Cleanup(func() { logger = origLogger })

	network := Network{Name: "testnet"}
	client := chatGateClient()
	event := chatGateEvent("absorbuser2", "#absorb2")

	handleMention(network, client, event, "#absorb2", "queue me the old way")

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		cur, _ := qm.QueueStatus("testnet", "#absorb2", userID)
		if cur != nil {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(t, found, "closed-mailbox mention must fall back to the queue path")
}
