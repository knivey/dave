package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

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
