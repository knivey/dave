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
			ConnectCommands: []string{"PRIVMSG X :Y", "PRIVMSG AuthServ :AUTH"}},
		{Name: "gamesurge", Addr: "irc.gamesurge.net:6697", Nick: "dave", RealName: "dave"},
	}
	names := map[string]bool{"libera": true, "gamesurge": true, "directnet": true}
	return plans, names
}

// replyRunner is a fakeRunner whose every command yields a canned reply —
// BouncerServ answers EVERY command (success confirmations and errors alike)
// with NOTICEs, so reconciliation must proceed through replies.
type replyRunner struct {
	lines   []string
	replies []string
}

func (f *replyRunner) Command(line string) []string {
	f.lines = append(f.lines, line)
	return f.replies
}

func TestReconcileNetworks(t *testing.T) {
	t.Run("missing network created with creds pushed", func(t *testing.T) {
		run := &fakeRunner{}
		plans, toml := sojuTestPlans()
		reconcileNetworks(run, map[string]bool{}, plans, toml, false, testLogger())
		// `network create` accepts -connect-command natively: a non-empty
		// TOML list rides the create itself (atomic, and no second
		// `network update` round-trip whose updateNetwork would
		// stop/reconnect the just-created upstream). ONE flag per line —
		// soju replaces the whole stored list, so repeated flags in one
		// command are the append form.
		assert.Contains(t, run.lines, `network create -addr irc.libera.chat -name "libera" -nick "dave" -realname "dave" -connect-command "PRIVMSG X :Y" -connect-command "PRIVMSG AuthServ :AUTH"`)
		assert.Contains(t, run.lines, `sasl set-plain -network "libera" "dave" "libpw"`)
		assert.Contains(t, run.lines, `network create -addr irc.gamesurge.net:6697 -name "gamesurge" -nick "dave" -realname "dave"`)
		assert.NotContains(t, run.lines, "sasl set-plain -network gamesurge", "no SASL configured → no push")
		for _, l := range run.lines {
			assert.NotContains(t, l, `network update "`, "fresh creation never updates: create carries the commands, and soju's update would reconnect the just-created upstream")
		}
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
		assert.Contains(t, run.lines, `sasl set-plain -network "libera" "dave" "libpw"`)
		assert.Contains(t, run.lines, `network update "libera" -connect-command "PRIVMSG X :Y" -connect-command "PRIVMSG AuthServ :AUTH"`)
	})

	t.Run("force with empty connect_commands clears stored list", func(t *testing.T) {
		run := &fakeRunner{}
		_, toml := sojuTestPlans()
		// gamesurge alone: no SASL, no connect_commands — an existing soju
		// network may still carry stale stored commands, so force sends the
		// explicit clear form (-connect-command "" clears the list in soju)
		// and nothing else.
		plans := []sojuNetworkPlan{{Name: "gamesurge", Addr: "irc.gamesurge.net:6697", Nick: "dave", RealName: "dave"}}
		reconcileNetworks(run, map[string]bool{"gamesurge": true}, plans, toml, true, testLogger())
		assert.Equal(t, []string{`network update "gamesurge" -connect-command ""`}, run.lines)
	})

	t.Run("orphan deleted, toml-name kept", func(t *testing.T) {
		run := &fakeRunner{}
		plans, toml := sojuTestPlans()
		existing := map[string]bool{"libera": true, "ghostnet": true, "directnet": true}
		reconcileNetworks(run, existing, plans, toml, false, testLogger())
		matches := 0
		for _, l := range run.lines {
			if l == `network delete "ghostnet"` {
				matches++
			}
		}
		assert.Equal(t, 1, matches, "orphan deleted exactly once")
		for _, l := range run.lines {
			assert.NotContains(t, l, "delete directnet", "TOML name must never be deleted")
			assert.NotContains(t, l, "delete libera")
		}
	})

	t.Run("replies from BouncerServ never abort reconciliation", func(t *testing.T) {
		run := &replyRunner{replies: []string{"something went wrong upstream"}}
		plans, toml := sojuTestPlans()
		reconcileNetworks(run, map[string]bool{"libera": true, "ghostnet": true}, plans, toml, true, testLogger())
		// Every mutating command still sent despite replies at each step:
		assert.Contains(t, run.lines, `sasl set-plain -network "libera" "dave" "libpw"`)
		assert.Contains(t, run.lines, `network update "libera" -connect-command "PRIVMSG X :Y" -connect-command "PRIVMSG AuthServ :AUTH"`)
		assert.Contains(t, run.lines, `network delete "ghostnet"`)
	})
}

func testLogger() logxi.Logger {
	l := newLogger("soju-admin-test")
	return l
}

// statusRunner fakes the admin session for the query→reconcile wiring: the
// `network status` query returns `status` (nil = Command's failure return —
// the session-down gate refused, or zero replies arrived in the collection
// window on a dying socket), everything else is recorded as a mutating
// command.
type statusRunner struct {
	status []string
	lines  []string
}

func (f *statusRunner) Command(line string) []string {
	f.lines = append(f.lines, line)
	if line == "network status" {
		return f.status
	}
	return nil
}

// TestReconcileFromStatusNilQuerySkips pins the invisible-boot invariant: a
// FAILED status query (nil) must be read as "management unavailable", never
// as "soju has zero networks". Proceeding on nil would create-duplicate every
// network and — far worse — run pushSojuCreds against the EXISTING soju
// networks, whose `network update` stops and reconnects every upstream
// connection at boot, exactly what the spec forbids.
func TestReconcileFromStatusNilQuerySkips(t *testing.T) {
	on := true
	run := &statusRunner{status: nil}
	networks := map[string]Network{
		"libera": {
			Nick: "dave", RealName: "dave", Bouncer: &on,
			Servers:         []Server{{Host: "irc.libera.chat", Ssl: true}},
			SASL:            &SASLConfig{User: "dave", Pass: "pw"},
			ConnectCommands: []string{"PRIVMSG X :Y"},
		},
	}
	// force_reconcile = true is the adversarial setting: even force must not
	// push when the projection could not be read.
	reconcileFromStatus(run, &SojuConfig{ForceReconcile: true}, networks, testLogger())
	assert.Equal(t, []string{"network status"}, run.lines,
		"only the failed query went out — no creates, no pushes, no deletes")
}

// TestReconcileFromStatusEmptyReplyReconciles pins the other side of the
// nil-vs-empty discrimination: a genuinely EMPTY soju answers "No network
// configured" (a NON-nil slice), and that DOES reconcile — the missing
// network is created.
func TestReconcileFromStatusEmptyReplyReconciles(t *testing.T) {
	on := true
	run := &statusRunner{status: []string{"No network configured"}}
	networks := map[string]Network{
		"libera": {
			Nick: "dave", RealName: "dave", Bouncer: &on,
			Servers: []Server{{Host: "irc.libera.chat", Ssl: true}},
		},
	}
	reconcileFromStatus(run, &SojuConfig{}, networks, testLogger())
	assert.Contains(t, run.lines, `network create -addr irc.libera.chat -name "libera" -nick "dave" -realname "dave"`)
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

func TestExtraChannels(t *testing.T) {
	chans := map[string]ChannelConfig{
		"#libera": {},
		"#off-by": {},
	}
	t.Run("extras reported, configured kept, casemapping honored", func(t *testing.T) {
		got := extraChannels(
			[]string{"#libera", "#OFF-BY", "#ghostchan"},
			chans,
			"rfc1459",
		)
		assert.Equal(t, []string{"#ghostchan"}, got)
	})

	t.Run("empty config means everything is extra", func(t *testing.T) {
		got := extraChannels([]string{"#a", "#b"}, map[string]ChannelConfig{}, "")
		assert.Equal(t, []string{"#a", "#b"}, got)
	})

	t.Run("empty joined list", func(t *testing.T) {
		assert.Nil(t, extraChannels(nil, chans, "ascii"))
	})
}
