# Auto-Rejoin on Kick + `/join` Fix — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `/join` work after a KICK (send JOIN regardless of cached state, preserve channel config) and add configurable auto-rejoin-on-kick (default on, overridable per-network/per-channel, with a per-network delay).

**Architecture:** Three config fields (`auto_rejoin` on `Network` and `ChannelConfig`; `auto_rejoin_delay` on `Network`) resolved by two pure helpers. `/join` stops checking the config map for "already in" and just sends the IRC JOIN. The KICK handler, on self-kick, schedules a non-blocking delayed rejoin via an injectable timer+join seam; it never mutates channel config.

**Tech Stack:** Go 1.25, `lrstanley/girc` v1.1.1, `BurntSushi/toml`, `testify`. Single `package main` at repo root.

**Spec:** `docs/superpowers/specs/2026-06-26-auto-rejoin-on-kick-design.md`

---

## File Map

- `config.go` — add 3 struct fields + `shouldAutoRejoin` / `autoRejoinDelay` helpers.
- `config_test.go` — helper tests + config-load tests.
- `tui_commands.go` — rewrite `tuiCmdJoin`.
- `tui_commands_test.go` — `/join` behavior tests.
- `irc_handlers.go` — `rejoinChannel` + `scheduleRejoin` seams, `handleSelfKick`, KICK handler wiring.
- `irc_handlers_test.go` (new) — `TestHandleSelfKick`.
- `config/config.toml` — reference + example doc blocks.

Conventions: `go fmt ./... && go vet ./...` after each task; tests via `go test ./...`. Pointer helper `boolPtr` already exists in `config_test.go:707`. The TUI test helper `setupTUITest` (tui_commands_test.go:14) registers a `testnet` bot with a disconnected `girc.Client` — girc's `Send` safely drops events when `c.conn == nil` (conn.go:450), so `Cmd.Join`/`JoinKey` are safe to call in tests without a server.

---

## Task 1: Config fields + resolution helpers

**Files:**
- Modify: `config.go` (`Network` struct ~line 149-165, `ChannelConfig` struct ~line 104-109, add helpers after `IsEnabled` ~line 192)
- Test: `config_test.go`

- [ ] **Step 1: Write the failing tests (append to `config_test.go`)**

```go
func TestShouldAutoRejoin(t *testing.T) {
	tests := []struct {
		name string
		net  *bool
		ch   *bool
		want bool
	}{
		{"default true when both unset", nil, nil, true},
		{"network true", boolPtr(true), nil, true},
		{"network false", boolPtr(false), nil, false},
		{"channel overrides network to true", boolPtr(false), boolPtr(true), true},
		{"channel overrides network to false", boolPtr(true), boolPtr(false), false},
		{"channel set when network nil", nil, boolPtr(false), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			net := &Network{AutoRejoin: tt.net}
			ch := ChannelConfig{AutoRejoin: tt.ch}
			assert.Equal(t, tt.want, shouldAutoRejoin(net, ch))
		})
	}
}

func TestAutoRejoinDelay(t *testing.T) {
	t.Run("defaults to 3s when unset", func(t *testing.T) {
		assert.Equal(t, 3*time.Second, autoRejoinDelay(&Network{}))
	})
	t.Run("uses configured delay", func(t *testing.T) {
		d := 10 * time.Second
		assert.Equal(t, 10*time.Second, autoRejoinDelay(&Network{AutoRejoinDelay: &d}))
	})
}

func TestLoadConfigDirNetworkAutoRejoin(t *testing.T) {
	t.Run("nil when unset (default-on via helper)", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)
		net := loadConfigDirOrDie(dir).Networks["testnet"]
		assert.Nil(t, net.AutoRejoin)
		assert.Nil(t, net.AutoRejoinDelay)
	})
	t.Run("parses network + channel settings", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
auto_rejoin = false
auto_rejoin_delay = "10s"
[[networks.testnet.servers]]
host = "irc.example.com"
[networks.testnet.channels."#x"]
auto_rejoin = true
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)
		net := loadConfigDirOrDie(dir).Networks["testnet"]
		require.NotNil(t, net.AutoRejoin)
		assert.False(t, *net.AutoRejoin)
		require.NotNil(t, net.AutoRejoinDelay)
		assert.Equal(t, 10*time.Second, *net.AutoRejoinDelay)
		require.NotNil(t, net.Channels["#x"].AutoRejoin)
		assert.True(t, *net.Channels["#x"].AutoRejoin)
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestShouldAutoRejoin|TestAutoRejoinDelay|TestLoadConfigDirNetworkAutoRejoin' ./...`
Expected: build failure — `Network.AutoRejoin undefined`, `Network.AutoRejoinDelay undefined`, `shouldAutoRejoin undefined`, `autoRejoinDelay undefined`.

- [ ] **Step 3: Add the struct fields (in `config.go`)**

`ChannelConfig` becomes:

```go
type ChannelConfig struct {
	Key                  string `toml:"key"`
	Pastebin             bool   `toml:"pastebin"`
	MaxLines             int    `toml:"max_lines"`
	PastebinPreviewLines *int   `toml:"pastebin_preview_lines"`
	AutoRejoin           *bool  `toml:"auto_rejoin"`
}
```

`Network` — add two fields before `Casemapping`:

```go
	DisabledCommands []string       `toml:"disabled_commands"`
	AutoRejoin       *bool          `toml:"auto_rejoin"`
	AutoRejoinDelay  *time.Duration `toml:"auto_rejoin_delay"`
	Casemapping      string         `toml:"-"`
```

- [ ] **Step 4: Add the resolution helpers (in `config.go`, after `IsEnabled`)**

```go
// shouldAutoRejoin reports whether the bot should automatically rejoin a
// channel after being kicked. A per-channel setting (ChannelConfig.AutoRejoin)
// takes precedence, then the network-level setting, then the default (true).
func shouldAutoRejoin(net *Network, chCfg ChannelConfig) bool {
	if chCfg.AutoRejoin != nil {
		return *chCfg.AutoRejoin
	}
	if net.AutoRejoin != nil {
		return *net.AutoRejoin
	}
	return true
}

// autoRejoinDelay returns the delay before auto-rejoining after a kick, using
// the network-level setting and defaulting to 3s when unset.
func autoRejoinDelay(net *Network) time.Duration {
	if net.AutoRejoinDelay != nil {
		return *net.AutoRejoinDelay
	}
	return 3 * time.Second
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -run 'TestShouldAutoRejoin|TestAutoRejoinDelay|TestLoadConfigDirNetworkAutoRejoin' ./...`
Expected: PASS. Then `go fmt ./... && go vet ./...` (clean).

- [ ] **Step 6: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add auto_rejoin config fields and resolution helpers"
```

---

## Task 2: Fix `/join`

**Files:**
- Modify: `tui_commands.go` (`tuiCmdJoin` ~line 106-131)
- Test: `tui_commands_test.go`

- [ ] **Step 1: Write the failing tests (append to `tui_commands_test.go`)**

```go
func TestTuiCmdJoin(t *testing.T) {
	t.Run("creates config entry when channel missing", func(t *testing.T) {
		setupTUITest(t)
		tuiCmdJoin([]string{"/join", "testnet", "#newchan"}, "/join testnet #newchan")
		assert.Contains(t, getLogViewText(), "Joined #newchan on testnet")
		bot := bots["testnet"]
		_, ok := bot.Network.Channels["#newchan"]
		assert.True(t, ok, "config entry should be created for new channel")
	})

	t.Run("preserves existing channel key (no clobber)", func(t *testing.T) {
		setupTUITest(t)
		bots["testnet"].Network.Channels = map[string]ChannelConfig{"#secret": {Key: "passw0rd"}}
		tuiCmdJoin([]string{"/join", "testnet", "#secret"}, "/join testnet #secret")
		assert.Contains(t, getLogViewText(), "Joined #secret on testnet")
		assert.Equal(t, "passw0rd", bots["testnet"].Network.Channels["#secret"].Key,
			"existing key must not be clobbered")
	})

	t.Run("does not report already in (joins regardless)", func(t *testing.T) {
		setupTUITest(t)
		bots["testnet"].Network.Channels = map[string]ChannelConfig{"#x": {}}
		tuiCmdJoin([]string{"/join", "testnet", "#x"}, "/join testnet #x")
		assert.NotContains(t, getLogViewText(), "Already in")
		assert.Contains(t, getLogViewText(), "Joined #x on testnet")
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run TestTuiCmdJoin ./...`
Expected: FAIL — "preserves existing channel key" fails because current code overwrites with `ChannelConfig{}`; "does not report already in" fails because current code prints "Already in".

- [ ] **Step 3: Rewrite `tuiCmdJoin` (in `tui_commands.go`)**

Replace the body from `bot.mu.Lock()` through the end of the function:

```go
	// Send the IRC JOIN regardless of cached/config state — girc's joined-state
	// can lag or be wrong, and joining while already joined is harmless. Use the
	// configured key (if any) so +k channels work.
	bot.mu.Lock()
	key := bot.Network.GetChannelConfig(channel).Key
	if bot.Network.Channels == nil {
		bot.Network.Channels = make(map[string]ChannelConfig)
	}
	// Create a config entry only if one doesn't exist, so we never clobber a
	// configured key or other channel settings.
	if _, exists := bot.Network.Channels[channel]; !exists {
		bot.Network.Channels[channel] = ChannelConfig{}
	}
	bot.mu.Unlock()

	if key != "" {
		bot.Client.Cmd.JoinKey(channel, key)
	} else {
		bot.Client.Cmd.Join(channel)
	}
	fmt.Fprintf(logView, "[green]Joined %s on %s[white]\n", channel, network)
```

(The `Usage`/`Unknown network` guards at the top stay unchanged.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestTuiCmdJoin ./...`
Expected: PASS. Then `go fmt ./... && go vet ./... && go test ./...` (all green).

- [ ] **Step 5: Commit**

```bash
git add tui_commands.go tui_commands_test.go
git commit -m "fix: /join sends JOIN regardless of config state, preserves channel key"
```

---

## Task 3: KICK handler auto-rejoin

**Files:**
- Modify: `irc_handlers.go` (add seams + `handleSelfKick`; wire into KICK handler ~line 106-125)
- Test: `irc_handlers_test.go` (new)

- [ ] **Step 1: Write the failing test (create `irc_handlers_test.go`)**

```go
package main

import (
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/assert"
)

func TestHandleSelfKick(t *testing.T) {
	origRejoin := rejoinChannel
	origSchedule := scheduleRejoin
	t.Cleanup(func() { rejoinChannel = origRejoin; scheduleRejoin = origSchedule })

	t.Run("enabled schedules rejoin with configured key and default delay", func(t *testing.T) {
		bot := &Bot{Network: Network{
			Name:     "testnet",
			Channels: map[string]ChannelConfig{"#Foo": {Key: "sekret"}},
		}}
		var gotDelay time.Duration
		var joinCh, joinKey string
		var scheduled bool
		scheduleRejoin = func(d time.Duration, f func()) { scheduled = true; gotDelay = d; f() }
		rejoinChannel = func(_ *Bot, ch, key string) { joinCh, joinKey = ch, key }

		handleSelfKick(bot, logxi.New("test"), "testnet", "#foo")

		assert.True(t, scheduled, "should schedule a rejoin")
		assert.Equal(t, 3*time.Second, gotDelay, "default delay")
		assert.Equal(t, "#foo", joinCh)
		assert.Equal(t, "sekret", joinKey, "should use configured key")
		assert.Equal(t, "sekret", bot.Network.Channels["#Foo"].Key, "config must be untouched")
	})

	t.Run("disabled by network does not schedule", func(t *testing.T) {
		bot := &Bot{Network: Network{
			Name:       "testnet",
			AutoRejoin: boolPtr(false),
			Channels:   map[string]ChannelConfig{"#foo": {}},
		}}
		scheduled := false
		scheduleRejoin = func(time.Duration, func()) { scheduled = true }
		rejoinChannel = func(*Bot, string, string) {}

		handleSelfKick(bot, logxi.New("test"), "testnet", "#foo")

		assert.False(t, scheduled, "disabled network should not schedule")
		_, ok := bot.Network.Channels["#foo"]
		assert.True(t, ok, "config entry must not be removed")
	})

	t.Run("channel override disables when network enabled", func(t *testing.T) {
		bot := &Bot{Network: Network{
			Name:       "testnet",
			AutoRejoin: boolPtr(true),
			Channels:   map[string]ChannelConfig{"#foo": {AutoRejoin: boolPtr(false)}},
		}}
		scheduled := false
		scheduleRejoin = func(time.Duration, func()) { scheduled = true }
		rejoinChannel = func(*Bot, string, string) {}

		handleSelfKick(bot, logxi.New("test"), "testnet", "#foo")

		assert.False(t, scheduled, "channel opt-out should win")
	})

	t.Run("unconfigured channel rejoins plain (no key)", func(t *testing.T) {
		bot := &Bot{Network: Network{Name: "testnet", Channels: map[string]ChannelConfig{}}}
		var joinCh, joinKey string
		scheduleRejoin = func(_ time.Duration, f func()) { f() }
		rejoinChannel = func(_ *Bot, ch, key string) { joinCh, joinKey = ch, key }

		handleSelfKick(bot, logxi.New("test"), "testnet", "#newchan")

		assert.Equal(t, "#newchan", joinCh)
		assert.Equal(t, "", joinKey, "unconfigured channel has no key")
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestHandleSelfKick ./...`
Expected: build failure — `rejoinChannel`, `scheduleRejoin`, `handleSelfKick` undefined.

- [ ] **Step 3: Add the seams + `handleSelfKick` (in `irc_handlers.go`, above `registerIRCHandlers`)**

```go
// rejoinChannel sends the IRC JOIN for a channel, guarded against a nil or
// disconnected client so a late timer callback (after shutdown or during a
// netsplit) is a safe no-op. Injectable for tests.
var rejoinChannel = func(bot *Bot, channel, key string) {
	if bot.Client == nil || !bot.Client.IsConnected() {
		return
	}
	if key != "" {
		bot.Client.Cmd.JoinKey(channel, key)
	} else {
		bot.Client.Cmd.Join(channel)
	}
}

// scheduleRejoin defers f by delay. Defaults to time.AfterFunc; injected in
// tests so the callback runs synchronously.
var scheduleRejoin = func(delay time.Duration, f func()) {
	time.AfterFunc(delay, f)
}

// handleSelfKick is invoked when the bot itself is kicked from a channel. It
// NEVER mutates bot.Network.Channels: the channel config (key, settings) must
// survive so a later /join or a reconnect (RPL_WELCOME rejoin loop) still has
// it. The config read happens under bot.mu because /join and /part mutate the
// same Channels map from other goroutines (a concurrent map read+write would
// panic). When auto-rejoin is enabled for the channel, it schedules a delayed
// rejoin using the configured key if any.
func handleSelfKick(bot *Bot, log logxi.Logger, networkName, channel string) {
	bot.mu.Lock()
	chCfg := bot.Network.GetChannelConfig(channel)
	enabled := shouldAutoRejoin(&bot.Network, chCfg)
	delay := autoRejoinDelay(&bot.Network)
	key := chCfg.Key
	bot.mu.Unlock()

	if !enabled {
		log.Debug("auto_rejoin disabled for kicked channel", "channel", channel, "network", networkName)
		return
	}
	log.Info("bot kicked; scheduling auto-rejoin", "channel", channel, "network", networkName, "delay", delay)
	scheduleRejoin(delay, func() {
		rejoinChannel(bot, channel, key)
	})
}
```

(`irc_handlers.go` already imports `time` and `logxi`.)

- [ ] **Step 4: Wire into the KICK handler**

In the `girc.KICK` handler, replace the bare self-kick branch:

```go
		kickedNick := event.Params[1]
		if kickedNick == client.GetNick() {
			return
		}
```

with:

```go
		kickedNick := event.Params[1]
		if kickedNick == client.GetNick() {
			handleSelfKick(bot, log, network.Name, event.Params[0])
			return
		}
```

(`event.Params[0]` is safe: the handler already guards `len(event.Params) < 2` above this branch.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -run TestHandleSelfKick ./...`
Expected: PASS. Then `go fmt ./... && go vet ./... && go test ./...` (all green).

- [ ] **Step 6: Commit**

```bash
git add irc_handlers.go irc_handlers_test.go
git commit -m "feat: auto-rejoin channel after being kicked (default on, configurable)"
```

---

## Task 4: Config documentation

**Files:**
- Modify: `config/config.toml` (reference lists + commented example block)

- [ ] **Step 1: Add to the network options reference list**

In `config/config.toml`, after the line:
```
#   disabled_commands  (list of strings)       Command names to block on this network (e.g. ["qwen", "zimage"])
```
add:
```
#   auto_rejoin        (bool, default: true)   Automatically rejoin a channel after being kicked
#   auto_rejoin_delay  (duration, default: "3s") Delay before auto-rejoining after a kick
```

- [ ] **Step 2: Add to the channel options reference list**

After the line:
```
#   pastebin_preview_lines (int, inherits [pastebin])  Preview lines sent before pastebin URL (0 = no preview)
```
add:
```
#   auto_rejoin        (bool, inherits network) Override auto-rejoin-on-kick for this channel
```

- [ ] **Step 3: Add to the commented example network block**

After:
```
# disabled_commands = []
```
add:
```
# auto_rejoin = true
# auto_rejoin_delay = "3s"
```

- [ ] **Step 4: Add to the commented example channel block**

After:
```
# [networks.example.channels."#mychannel"]
# key = "channelkey"
```
add:
```
# auto_rejoin = true
```

- [ ] **Step 5: Verify nothing broke**

Run: `go build -o /tmp/dave-build . && go fmt ./... && go vet ./... && go test ./...`
Expected: build OK, all tests pass (config TOML is only documentation here; no live sections changed).

- [ ] **Step 6: Commit**

```bash
git add config/config.toml
git commit -m "docs: document auto_rejoin / auto_rejoin_delay config options"
```

---

## Done Criteria

- `go test ./...`, `go fmt ./...`, `go vet ./...` all clean.
- `/join` after a KICK no longer says "Already in" and preserves any configured channel key.
- Bot auto-rejoins a kicked channel by default (3s delay), rejoining with the key if set; opt-out via network or channel `auto_rejoin = false`.
- Channel config is never removed on kick.
- `config/config.toml` documents the new options.
