package main

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestSendConnectCommands pins the WARN-and-continue contract of the
// RPL_WELCOME connect_commands sender: one failing line must never abort the
// remaining lines. girc's SendRaw errors on unparseable input (the "X" line
// below); on a never-connected client the valid lines are silently dropped by
// girc's write path — either way every line is attempted and nothing panics.
func TestSendConnectCommands(t *testing.T) {
	client := girc.New(girc.Config{}) // disconnected
	assert.NotPanics(t, func() {
		sendConnectCommands(client, []string{
			"PRIVMSG AuthServ@Services.GameSurge.net :AUTH user pass",
			"X", // unparseable → SendRaw error → WARN, continue
			"MODE dave +i",
		}, testLogger())
	})
	// Empty list is a clean no-op.
	assert.NotPanics(t, func() {
		sendConnectCommands(client, nil, testLogger())
	})
}

func TestShouldSendConnectCommands(t *testing.T) {
	on := true
	tests := []struct {
		name    string
		network Network
		want    bool
	}{
		{"direct (bouncer unset)", Network{}, true},
		{"direct (explicit false)", Network{Bouncer: new(bool)}, true},
		{"bouncer", Network{Bouncer: &on}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldSendConnectCommands(tc.network))
		})
	}
}

// TestWelcomeConnectCommandsBouncerGate drives the REAL RPL_WELCOME closure
// (registerIRCHandlers) through a live girc connection against a one-shot
// fake IRC server and asserts on the wire: direct-mode networks send their
// connect_commands downstream after registration; bouncer networks send
// NOTHING — soju relays downstream PRIVMSGs upstream AND fires its own stored
// copy on upstream registration, so a downstream send would double-execute
// every non-idempotent command (AuthServ AUTH et al.) on coincident
// registrations.
func TestWelcomeConnectCommandsBouncerGate(t *testing.T) {
	const authserv = "PRIVMSG AuthServ@Services.GameSurge.net :AUTH user pass"

	t.Run("direct mode sends connect_commands downstream", func(t *testing.T) {
		lines := welcomeWireLines(t, Network{Name: "direct", Nick: "dave", ConnectCommands: []string{authserv}})
		assert.Contains(t, lines, authserv)
	})

	t.Run("bouncer mode sends nothing downstream", func(t *testing.T) {
		on := true
		lines := welcomeWireLines(t, Network{Name: "bounced", Nick: "dave", Bouncer: &on, ConnectCommands: []string{authserv}})
		assert.NotContains(t, lines, authserv)
	})
}

// welcomeWireLines registers dave's handlers on a fresh girc client, connects
// it to a local one-shot IRC server that completes registration immediately
// (001), and returns every line the client put on the wire until the read
// deadline. girc dispatches each event's handlers concurrently, so the
// RPL_WELCOME closure fires without waiting on the builtin handler's 2s
// sleep; the writes land well inside the 1.2s collection window.
func welcomeWireLines(t *testing.T, network Network) []string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	done := make(chan []string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- nil
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(1200 * time.Millisecond))
		_, _ = fmt.Fprintf(conn, ":srv 001 dave :Welcome test\r\n")
		var lines []string
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		done <- lines
	}()

	client := girc.New(girc.Config{
		Server: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		Nick: "dave", User: "dave", Name: "dave",
		SSL: false, AllowFlood: true,
	})
	bot := &Bot{Network: network, quitCh: make(chan struct{})}
	registerIRCHandlers(bot, client, network, testLogger())
	go func() { _ = client.Connect() }()
	t.Cleanup(func() { client.Close() })

	select {
	case lines := <-done:
		return lines
	case <-time.After(4 * time.Second):
		t.Fatal("fake IRC server never finished collecting client lines")
		return nil
	}
}
