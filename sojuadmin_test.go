package main

import (
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectReplies(t *testing.T) {
	t.Run("replies until quiet period", func(t *testing.T) {
		sink := make(chan string, 8)
		sink <- "libera [connected]: 2 channels"
		sink <- "oldnet [disconnected]: EOF"
		got := collectReplies(sink, 100*time.Millisecond, 2*time.Second)
		assert.Equal(t, []string{"libera [connected]: 2 channels", "oldnet [disconnected]: EOF"}, got)
	})

	t.Run("max wait caps a slow trickle", func(t *testing.T) {
		sink := make(chan string, 8)
		sink <- "a [connected]"
		go func() {
			time.Sleep(80 * time.Millisecond)
			sink <- "b [connected]"
			time.Sleep(80 * time.Millisecond)
			sink <- "c [connected]"
		}()
		got := collectReplies(sink, 50*time.Millisecond, 150*time.Millisecond)
		assert.LessOrEqual(t, len(got), 3)
		assert.Contains(t, got, "a [connected]")
	})
}

func TestSojuAdminCommandGatesOnConnected(t *testing.T) {
	a := &SojuAdmin{} // never connected
	assert.Nil(t, a.Command("network status"))
	assert.False(t, a.Connected())
}

func TestSojuAdminDeliverWhenSinkNil(t *testing.T) {
	a := &SojuAdmin{}
	a.deliver("anything [connected]") // must not panic
}

// TestSojuAdminDeliversRepliesDuringCollection pins the core protocol
// property: a BouncerServ NOTICE arriving while Command is collecting MUST
// reach Command's return value. deliver runs on the NOTICE handler goroutine
// and takes the state mutex — Command must NOT hold that mutex across
// collectReplies, or delivery deadlocks against collection until the quiet
// timer fires and replies are lost. Serialization of commands is a separate
// mutex (cmdMu) held across the whole call.
func TestSojuAdminDeliversRepliesDuringCollection(t *testing.T) {
	a := &SojuAdmin{
		client:    girc.New(girc.Config{}), // unconnected: Message's write is dropped by girc, harmless
		connected: true,
	}
	go func() {
		time.Sleep(150 * time.Millisecond) // inside Command's collection window
		a.deliver("libera [connected]: 2 channels")
	}()
	got := a.Command("network status")
	assert.Contains(t, got, "libera [connected]: 2 channels")
}

func TestAnyEnabledBouncerNetwork(t *testing.T) {
	on := true
	off := false
	tests := []struct {
		name     string
		networks map[string]Network
		want     bool
	}{
		{"empty", map[string]Network{}, false},
		{"direct only", map[string]Network{"a": {}}, false},
		{"bouncer enabled", map[string]Network{"a": {Bouncer: &on}}, true},
		{"bouncer disabled", map[string]Network{"a": {Bouncer: &off}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, anyEnabledBouncerNetwork(tc.networks))
		})
	}
}

func TestSojuAdminStopIdempotent(t *testing.T) {
	a := &SojuAdmin{quitCh: make(chan struct{}), Reconnect: true}
	a.Stop()
	a.Stop() // second call must not panic on double-close
	require.NotNil(t, a.quitCh)
}
