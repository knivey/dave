package main

import (
	"sync"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
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

func TestParseNetworkStatus(t *testing.T) {
	lines := []string{
		"libera [connected]: 5 channels",
		"gamesurge (irc.gamesurge.net) [connected as dave|gs]: 1 channels",
		"oldnet [disconnected]: EOF",
		"freenode (irc.freenode.net) [disabled]",
		"no brackets here",
		"",
	}
	names := parseNetworkStatus(lines)
	assert.Equal(t, map[string]bool{"libera": true, "gamesurge": true, "oldnet": true, "freenode": true}, names)
}

type fakeRunner struct{ lines []string }

func (f *fakeRunner) Command(line string) []string {
	f.lines = append(f.lines, line)
	return nil
}

func sojuTestPlans() ([]sojuNetworkPlan, map[string]bool) {
	plans := []sojuNetworkPlan{
		{Name: "libera", Addr: "irc.libera.chat", Nick: "dave", RealName: "dave",
			SASL:            &SASLConfig{User: "dave", Pass: "libpw"},
			ConnectCommands: []string{"PRIVMSG X :Y"}},
		{Name: "gamesurge", Addr: "irc.gamesurge.net:6697", Nick: "dave", RealName: "dave"},
	}
	names := map[string]bool{"libera": true, "gamesurge": true, "directnet": true}
	return plans, names
}

func TestReconcileNetworks(t *testing.T) {
	t.Run("missing network created with creds pushed", func(t *testing.T) {
		run := &fakeRunner{}
		plans, toml := sojuTestPlans()
		reconcileNetworks(run, map[string]bool{}, plans, toml, false, testLogger())
		assert.Contains(t, run.lines, `network create -addr irc.libera.chat -name libera -nick "dave" -realname "dave"`)
		assert.Contains(t, run.lines, `sasl set-plain -network libera "dave" "libpw"`)
		assert.Contains(t, run.lines, `network update -connect-command "PRIVMSG X :Y"`)
		assert.Contains(t, run.lines, `network create -addr irc.gamesurge.net:6697 -name gamesurge -nick "dave" -realname "dave"`)
		assert.NotContains(t, run.lines, "sasl set-plain -network gamesurge", "no SASL configured → no push")
	})

	t.Run("existing network untouched without force", func(t *testing.T) {
		run := &fakeRunner{}
		plans, toml := sojuTestPlans()
		reconcileNetworks(run, map[string]bool{"libera": true, "gamesurge": true}, plans, toml, false, testLogger())
		assert.NotContains(t, run.lines, "network create")
		assert.NotContains(t, run.lines, "sasl set-plain")
		assert.NotContains(t, run.lines, "network update")
	})

	t.Run("force pushes creds for existing networks", func(t *testing.T) {
		run := &fakeRunner{}
		plans, toml := sojuTestPlans()
		reconcileNetworks(run, map[string]bool{"libera": true, "gamesurge": true}, plans, toml, true, testLogger())
		assert.NotContains(t, run.lines, "network create")
		assert.Contains(t, run.lines, `sasl set-plain -network libera "dave" "libpw"`)
		assert.Contains(t, run.lines, `network update -connect-command "PRIVMSG X :Y"`)
	})

	t.Run("orphan deleted, toml-name kept", func(t *testing.T) {
		run := &fakeRunner{}
		plans, toml := sojuTestPlans()
		existing := map[string]bool{"libera": true, "ghostnet": true, "directnet": true}
		reconcileNetworks(run, existing, plans, toml, false, testLogger())
		matches := 0
		for _, l := range run.lines {
			if l == "network delete ghostnet" {
				matches++
			}
		}
		assert.Equal(t, 1, matches, "orphan deleted exactly once")
		for _, l := range run.lines {
			assert.NotContains(t, l, "delete directnet", "TOML name must never be deleted")
			assert.NotContains(t, l, "delete libera")
		}
	})
}

func testLogger() logxi.Logger {
	l := newLogger("soju-admin-test")
	return l
}

func TestBuildSojuPlans(t *testing.T) {
	on, off := true, false
	networks := map[string]Network{
		"explicit": {
			Nick: "dave", RealName: "dave", Bouncer: &on,
			Servers:         []Server{{Host: "irc.explicit.net", Port: 7000}},
			SASL:            &SASLConfig{User: "dave", Pass: "pw"},
			ConnectCommands: []string{"PRIVMSG X :Y"},
		},
		"tlsdefault": {
			Nick: "dave", RealName: "dave", Bouncer: &on,
			Servers: []Server{{Host: "irc.tls.net", Ssl: true}}, // GetPort() = 6697 → bare host
		},
		"plaindefault": {
			Nick: "dave", RealName: "dave", Bouncer: &on,
			Servers: []Server{{Host: "irc.plain.net"}}, // GetPort() = 6667 → explicit :6667, never :0
		},
		"direct": {
			Nick: "dave", RealName: "dave",
			Servers: []Server{{Host: "irc.direct.net"}},
		},
		"disabled": {
			Nick: "dave", RealName: "dave", Bouncer: &on, Enabled: &off,
			Servers: []Server{{Host: "irc.disabled.net"}},
		},
	}
	plans, tomlNames := buildSojuPlans(networks)
	byName := map[string]sojuNetworkPlan{}
	for _, p := range plans {
		byName[p.Name] = p
	}
	// Plan set = enabled bouncer networks only.
	assert.Len(t, plans, 3)
	assert.Contains(t, byName, "explicit")
	assert.Contains(t, byName, "tlsdefault")
	assert.Contains(t, byName, "plaindefault")
	assert.Equal(t, "irc.explicit.net:7000", byName["explicit"].Addr)
	assert.Equal(t, "irc.tls.net", byName["tlsdefault"].Addr)
	assert.Equal(t, "irc.plain.net:6667", byName["plaindefault"].Addr, "implicit plaintext port must resolve to 6667, never :0")
	assert.Equal(t, &SASLConfig{User: "dave", Pass: "pw"}, byName["explicit"].SASL)
	assert.Equal(t, []string{"PRIVMSG X :Y"}, byName["explicit"].ConnectCommands)
	// Never-delete set = every TOML name regardless of mode or enabled.
	assert.Equal(t, map[string]bool{
		"explicit": true, "tlsdefault": true, "plaindefault": true, "direct": true, "disabled": true,
	}, tomlNames)
}

// TestSojuAdminCommandSerializesConcurrent pins cmdMu serialization (Task 5's
// deferred review finding): replies carry no per-command correlation, so while
// one Command collects, every NOTICE lands in ITS sink — the second Command
// installs a sink only after the first fully returns (and has cleared it).
// Both commands must complete. The quiet period (750ms) dwarfs the 200ms
// delivery point, so the first command's collection window is provably open.
func TestSojuAdminCommandSerializesConcurrent(t *testing.T) {
	a := &SojuAdmin{
		client:    girc.New(girc.Config{}), // unconnected: Message's write is dropped by girc
		connected: true,
	}
	var wg sync.WaitGroup
	var first, second []string
	wg.Add(2)
	go func() {
		defer wg.Done()
		first = a.Command("network status")
	}()
	time.Sleep(100 * time.Millisecond) // first is inside its collection window
	go func() {
		defer wg.Done()
		second = a.Command("network status") // parks on cmdMu until first returns (~950ms)
	}()
	time.Sleep(100 * time.Millisecond)
	a.deliver("a1 [connected]") // both land in the FIRST command's sink
	a.deliver("b1 [connected]")
	wg.Wait() // both commands complete — no deadlock, no lost wakeup
	assert.Equal(t, []string{"a1 [connected]", "b1 [connected]"}, first)
	assert.Empty(t, second)
}
