package main

import (
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
)

func TestIsReplayedEvent(t *testing.T) {
	connect := time.Now()
	tests := []struct {
		name        string
		event       girc.Event
		connectedAt time.Time
		want        bool
	}{
		{
			name:        "batch tag trips regardless of timestamp",
			event:       girc.Event{Command: girc.PRIVMSG, Tags: girc.Tags{"batch": "7"}, Timestamp: connect},
			connectedAt: connect,
			want:        true,
		},
		{
			name:        "empty batch tag does not trip",
			event:       girc.Event{Command: girc.PRIVMSG, Tags: girc.Tags{"batch": ""}, Timestamp: connect},
			connectedAt: connect,
			want:        false,
		},
		{
			name:        "live now passes",
			event:       girc.Event{Command: girc.PRIVMSG, Timestamp: connect.Add(time.Second)},
			connectedAt: connect,
			want:        false,
		},
		{
			name:        "within skew passes",
			event:       girc.Event{Command: girc.PRIVMSG, Timestamp: connect.Add(-replaySkew)},
			connectedAt: connect,
			want:        false,
		},
		{
			name:        "older than skew trips",
			event:       girc.Event{Command: girc.PRIVMSG, Timestamp: connect.Add(-replaySkew - time.Second)},
			connectedAt: connect,
			want:        true,
		},
		{
			name:        "no connect instant yet passes",
			event:       girc.Event{Command: girc.PRIVMSG, Timestamp: connect.Add(-time.Hour)},
			connectedAt: time.Time{},
			want:        false,
		},
		{
			name:        "batch tag beats fresh timestamp",
			event:       girc.Event{Command: girc.PRIVMSG, Tags: girc.Tags{"batch": "x"}, Timestamp: connect.Add(time.Minute)},
			connectedAt: connect,
			want:        true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isReplayedEvent(tc.event, tc.connectedAt))
		})
	}
}

func TestDispatchChanMessageSkipsReplay(t *testing.T) {
	orig := handleChanMessageFn
	t.Cleanup(func() { handleChanMessageFn = orig })

	called := false
	handleChanMessageFn = func(network Network, client *girc.Client, event girc.Event) {
		called = true
	}

	connect := time.Now()
	bot := &Bot{}
	// Replayed: batch tag set → logged elsewhere, never dispatched.
	// (Source is required: girc's IsFromChannel returns false when
	// Source is nil, and the channel gate runs before the replay guard.)
	dispatchChanMessage(bot, Network{Name: "test"}, nil, girc.Event{
		Command: girc.PRIVMSG, Tags: girc.Tags{"batch": "1"},
		Source: &girc.Source{Name: "replayer"},
		Params: []string{"#chan", "hello"}, Timestamp: connect.Add(-time.Hour),
	})
	assert.False(t, called, "replayed message must not reach handleChanMessage")

	// Live: dispatched.
	bot.connectedAt = connect
	dispatchChanMessage(bot, Network{Name: "test"}, nil, girc.Event{
		Command: girc.PRIVMSG,
		Source:  &girc.Source{Name: "speaker"},
		Params:  []string{"#chan", "hello"}, Timestamp: connect.Add(time.Second),
	})
	assert.True(t, called, "live message must reach handleChanMessage")
}
