# soju Bouncer Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let any configured network route through a soju IRC bouncer (per-network opt-in) while dave itself administers the bouncer (networks, SASL, connect-commands, channel membership) from its TOML — with replayed history logged but never dispatched.

**Architecture:** dave keeps its one-girc-client-per-network model. A root `[soju]` config section carries the bouncer dial parameters; networks with `bouncer = true` dial soju instead of their `[[servers]]` list, authenticating as `<soju.user>/<name>@bot`. One extra unbound girc session (the "admin session") talks to soju's BouncerServ service to create/delete networks, push SASL and connect-commands, and delete channels that vanished from TOML. soju's on-reconnect backlog replay is neutralized by an O(1) per-message guard in the PRIVMSG dispatch path: batch-tagged (or pre-registration) messages are logged by the existing `ALL_EVENTS` handler but never dispatched.

**Tech Stack:** Go 1.25, `github.com/lrstanley/girc` **v1.1.3** (already bumped — prerequisite), testify (`assert`/`require`), TOML config via the existing `loadConfigDir` pipeline.

**Spec:** `docs/superpowers/specs/2026-11-16-soju-bouncer-support-design.md` — read it first; this plan argues from it and only restates what the executor needs per task.

## Global Constraints

- **No rate assumptions.** dave runs on servers that can deliver 10,000 messages in an instant. Every per-message addition must be O(1) with no allocations on the live path; no new buffering anywhere. (Spec §Problem, §Replay guard.)
- **soju is optional.** With `[soju]` absent and no `bouncer = true`, behavior is byte-identical to today: no admin session, no new code paths, guard never fires. (Spec §Direct-connection parity.)
- **Replay policy is ignore-all**: replayed messages are LOGGED (existing `ALL_EVENTS` path) and never dispatched — no triggers, no mentions, no exceptions. (Spec §Replay guard.)
- **dave's TOML is the single source of truth**; soju state is a projection dave reconciles. Removals in TOML (+ restart) remove from soju. Never delete soju state for a name present in TOML in any mode. (Spec §soju admin module.)
- girc is pinned at v1.1.3 — do not set `DisableAutoWHOOnJoin`/`DisableAutoMODEOnJoin` (dave's WHOX account resolution depends on the auto-WHO).
- House rules: every task ends with `go fmt ./...`, `go vet ./...` green and tests passing; commit per task; use `github.com/stretchr/testify` (`assert`/`require`); table-driven tests with `t.Run()`.
- Config TOML files must keep their reference-section convention: new options get a line in the reference list AND the commented example block (spec §Config shape; convention in AGENTS.md).

## Review Focus

Input classes the spec implies but no single task's tests fully exercise — each is pinned by the test named here:

1. **10k-message instant burst at reconnect** — expect: old messages logged, zero dispatched, no deadlock, no new allocation per message beyond today's. Pinned by Task 1's predicate table (`batch tag wins`, `within skew passes`, `live now passes`) + `TestDispatchChanMessageSkipsReplay` (dispatch short-circuits before any queue/DB work).
2. **Batch tag present on a message with a fresh timestamp** — expect: still skipped (tag wins over time). Pinned by Task 1's `batch tag trips regardless of timestamp` case.
3. **Live message arriving within the skew window of registration** (join-burst chatter) — expect: dispatched normally. Pinned by Task 1's `within skew passes` + `live now passes`.
4. **TOML network name containing `/` or `@` with `bouncer = true`** — expect: config load error naming the network. Pinned by Task 3's `TestLoadConfigDirSoju` rejection cases.
5. **Admin session unreachable / soju down at startup** — expect: network sessions unaffected, `Command` returns nil without blocking forever, reconciliation simply doesn't run. Pinned by Task 5's `TestSojuAdminCommandGatesOnConnected` and Task 6's pure-function tests (which never touch a live session).
6. **TOML network/channel removed while dave was down** — expect: deleted from soju at next startup reconcile, never for a name still in TOML. Pinned by Task 6's orphan-deletion scenario + Task 7's `TestExtraChannels`.

---

### Task 1: Replay guard (ignore-all)

**Files:**
- Modify: `irc_handlers.go` (PRIVMSG handler at lines 154-159; add predicate near `accountFromEvent`, ~line 88)
- Create: `replayguard_test.go`

**Interfaces:**
- Consumes: `girc.Event` (v1.1.3; `Event.Tags` is `girc.Tags` = `map[string]string`; `Event.Timestamp` is ALWAYS set — parse-time `now()` when no `time` tag, per girc `event.go:67`).
- Produces: `isReplayedEvent(event girc.Event, connectedAt time.Time) bool`; `dispatchChanMessage(bot *Bot, network Network, client *girc.Client, event girc.Event)`; package var `handleChanMessageFn = handleChanMessage` (injectable, house pattern). Task 7 relies on nothing here; later live-verification relies on the guard being first in the dispatch path.

- [ ] **Step 1: Write the failing predicate tests**

Create `replayguard_test.go`:

```go
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
	dispatchChanMessage(bot, Network{Name: "test"}, nil, girc.Event{
		Command: girc.PRIVMSG, Tags: girc.Tags{"batch": "1"},
		Params: []string{"#chan", "hello"}, Timestamp: connect.Add(-time.Hour),
	})
	assert.False(t, called, "replayed message must not reach handleChanMessage")

	// Live: dispatched.
	bot.connectedAt = connect
	dispatchChanMessage(bot, Network{Name: "test"}, nil, girc.Event{
		Command: girc.PRIVMSG,
		Params:  []string{"#chan", "hello"}, Timestamp: connect.Add(time.Second),
	})
	assert.True(t, called, "live message must reach handleChanMessage")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./... -run 'TestIsReplayedEvent|TestDispatchChanMessageSkipsReplay' 2>&1 | tail -5`
Expected: FAIL — `isReplayedEvent` / `dispatchChanMessage` / `handleChanMessageFn` undefined (compile error).

- [ ] **Step 3: Implement the predicate and rewire the PRIVMSG handler**

In `irc_handlers.go`, add above `registerIRCHandlers` (near `accountFromEvent`):

```go
// replaySkew is how far past a session's connect instant a message timestamp
// may lag and still count as live. Generous on purpose: the batch tag is the
// exact replay signal (soju backlog always carries one); the timestamp
// fallback only matters if tags are ever absent, and there tolerating clock
// skew beats precision. See docs/superpowers/specs/2026-11-16-soju-bouncer-support-design.md.
const replaySkew = 30 * time.Second

// isReplayedEvent reports whether an inbound PRIVMSG is soju backlog replay
// rather than live traffic. soju delivers its backlog on reconnect inside an
// IRCv3 "batch chathistory", tagging every replayed message with the batch
// ref; live traffic never carries one. The timestamp fallback is defense in
// depth: girc always sets Event.Timestamp (parse-time now() when the
// server-time tag is absent), so a message materially older than this
// session's registration cannot be live. On direct connections neither signal
// ever fires — the guard is a structural no-op there.
func isReplayedEvent(event girc.Event, connectedAt time.Time) bool {
	if event.Tags["batch"] != "" {
		return true
	}
	if connectedAt.IsZero() {
		return false
	}
	return event.Timestamp.Before(connectedAt.Add(-replaySkew))
}

// handleChanMessageFn is an injectable seam (the claimNickForFn pattern) so
// tests can observe dispatch without a live IRC environment.
var handleChanMessageFn = handleChanMessage

// dispatchChanMessage is the ONLY entry from the PRIVMSG handler into command
// dispatch. soju backlog replay is logged by the ALL_EVENTS handler (with its
// original @time, closing deploy-window log gaps) and must never dispatch:
// an old mention re-firing an LLM turn is the exact failure this guards.
// Per-message cost is one map lookup + one time comparison — the burst-rate
// constraint in the spec leaves no room for anything heavier.
func dispatchChanMessage(bot *Bot, network Network, client *girc.Client, event girc.Event) {
	if !event.IsFromChannel() {
		return
	}
	bot.mu.Lock()
	connectedAt := bot.connectedAt
	bot.mu.Unlock()
	if isReplayedEvent(event, connectedAt) {
		return
	}
	handleChanMessageFn(network, client, event)
}
```

Replace the existing PRIVMSG handler body in `registerIRCHandlers` (lines 154-159):

```go
	client.Handlers.AddBg(girc.PRIVMSG, func(client *girc.Client, event girc.Event) {
		dispatchChanMessage(bot, network, client, event)
	})
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./... -run 'TestIsReplayedEvent|TestDispatchChanMessageSkipsReplay' -v 2>&1 | tail -15`
Expected: PASS (all subtests).

- [ ] **Step 5: Full suite + fmt + vet**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -5`
Expected: all `ok`.

- [ ] **Step 6: Commit**

```bash
git add irc_handlers.go replayguard_test.go
git commit -m "soju: replay guard — backlog PRIVMSGs are logged but never dispatched"
```

---

### Task 2: `connect_commands` field (direct-mode sender)

**Files:**
- Modify: `config.go` (Network struct ~line 173; ApplyDefaults network loop ~line 836)
- Modify: `irc_handlers.go` (RPL_WELCOME handler, after the join loop ~line 152)
- Modify: `config/config.toml` (reference list `[networks.<name>] options` ~line 105; example block ~line 220)
- Test: `config_test.go`

**Interfaces:**
- Produces: `Network.ConnectCommands []string` (TOML `connect_commands`). Task 3 validates the field; Task 6 pushes the same field into soju in bouncer mode.

- [ ] **Step 1: Write the failing config test**

Append to `config_test.go`:

```go
func TestNetworkConnectCommands(t *testing.T) {
	t.Run("parses and survives ApplyDefaults", func(t *testing.T) {
		dir := createTestConfigDir(t, `
trigger = "."
[[networks.gamesurge.servers]]
host = "irc.gamesurge.net"
ssl = true
[networks.gamesurge]
nick = "dave"
connect_commands = ["PRIVMSG AuthServ@Services.GameSurge.net :AUTH user pass"]
`, nil)
		cfg, err := loadConfigDir(dir)
		require.NoError(t, err)
		net := cfg.Networks["gamesurge"]
		assert.Equal(t, []string{"PRIVMSG AuthServ@Services.GameSurge.net :AUTH user pass"}, net.ConnectCommands)
	})

	t.Run("empty entry rejected", func(t *testing.T) {
		dir := createTestConfigDir(t, `
trigger = "."
[networks.x]
nick = "dave"
connect_commands = ["PRIVMSG NickServ :IDENTIFY a", "  "]
`, nil)
		_, err := loadConfigDir(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "connect_commands")
	})
}
```

(Note: the network needs a `[[networks.X.servers]]` entry or the servers-empty case may fail elsewhere — if `loadConfigDir` tolerates zero servers, drop the servers block from the fixture.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestNetworkConnectCommands 2>&1 | tail -5`
Expected: FAIL — `connect_commands` unknown TOML key (ignored) → first subtest asserts on empty slice and fails, second passes vacuously until validation exists (fix both in Step 3).

- [ ] **Step 3: Implement field + validation + WELCOME sender**

`config.go` — add to the `Network` struct (after `Quitmsg`):

```go
	ConnectCommands []string       `toml:"connect_commands"`
```

In the ApplyDefaults network loop (the `for name, network := range config.Networks` block ~line 836, alongside the `Trigger`/`Quitmsg` inheritance), add after the `Quitmsg` default:

```go
		for _, cmd := range network.ConnectCommands {
			if strings.TrimSpace(cmd) == "" {
				return config, fmt.Errorf("networks.%s: connect_commands entries must be non-empty IRC lines", name)
			}
		}
```

`irc_handlers.go` — in the `RPL_WELCOME` handler, after the join loop's `client.Cmd.Join(noKey...)`:

```go
		// connect_commands: raw IRC lines sent after every registration —
		// AuthServ/NickServ identification on networks without SASL. Same
		// semantics soju uses for its per-network connect-commands.
		bot.mu.Lock()
		connectCmds := bot.Network.ConnectCommands
		bot.mu.Unlock()
		for _, cmd := range connectCmds {
			if err := client.Cmd.SendRaw(cmd); err != nil {
				log.Warn("connect_command send failed", "line", cmd, "error", err.Error())
			}
		}
```

`config/config.toml` — add to the `[networks.<name>] options:` reference list (after `auto_rejoin_delay`):

```
#   connect_commands   (list of strings)    Raw IRC lines sent after registration on every connect (e.g. AuthServ AUTH on networks without SASL)
```

and to the example block (after `auto_rejoin_delay = "3s"`):

```
# connect_commands = ["PRIVMSG AuthServ@Services.GameSurge.net :AUTH user pass"]
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run TestNetworkConnectCommands -v 2>&1 | tail -8`
Expected: PASS both subtests.

- [ ] **Step 5: Full suite + fmt + vet + commit**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -3`

```bash
git add config.go irc_handlers.go config/config.toml config_test.go
git commit -m "soju: per-network connect_commands, sent after registration (direct mode)"
```

---

### Task 3: `[soju]` config section + `bouncer` flag

**Files:**
- Modify: `config.go` (Config struct ~line 21; Network struct; new SojuConfig type near `Server`; validation in the ApplyDefaults network loop region)
- Modify: `config/config.toml` (new `[soju]` reference block near line 100; example block)
- Test: `config_test.go`

**Interfaces:**
- Consumes: `Network.ConnectCommands` (Task 2), `Network.IsEnabled()`.
- Produces (used by Tasks 4-7): 

```go
type SojuConfig struct {
	Host               string `toml:"host"`
	Port               int    `toml:"port"`
	Ssl                *bool  `toml:"ssl"`
	InsecureSkipVerify bool   `toml:"insecure_skip_verify"`
	User               string `toml:"user"`
	Pass               string `toml:"pass"`
	ForceReconcile     bool   `toml:"force_reconcile"`
}
func (s *SojuConfig) DialPort() int  // 0 → 6697
func (s *SojuConfig) UseTLS() bool   // nil → true
// Config gains:  Soju *SojuConfig `toml:"soju"`
// Network gains: Bouncer *bool `toml:"bouncer"`  +  func (n *Network) IsBouncer() bool
```

- [ ] **Step 1: Write the failing config tests**

Append to `config_test.go`:

```go
func TestLoadConfigDirSoju(t *testing.T) {
	base := `
trigger = "."
[soju]
host = "bouncer.example.com"
user = "dave"
pass = "secret"
[networks.libera]
nick = "dave"
bouncer = true
[[networks.libera.servers]]
host = "irc.libera.chat"
ssl = true
[networks.direct]
nick = "dave"
[[networks.direct.servers]]
host = "irc.other.net"
ssl = true
`
	t.Run("valid parse + defaults", func(t *testing.T) {
		dir := createTestConfigDir(t, base, nil)
		cfg, err := loadConfigDir(dir)
		require.NoError(t, err)
		require.NotNil(t, cfg.Soju)
		assert.Equal(t, "bouncer.example.com", cfg.Soju.Host)
		assert.Equal(t, 6697, cfg.Soju.DialPort(), "port defaults to 6697")
		assert.True(t, cfg.Soju.UseTLS(), "ssl defaults to true")
		assert.True(t, cfg.Networks["libera"].IsBouncer())
		assert.False(t, cfg.Networks["direct"].IsBouncer(), "default is direct")
	})

	t.Run("bouncer without soju section rejected", func(t *testing.T) {
		dir := createTestConfigDir(t, `
trigger = "."
[networks.libera]
nick = "dave"
bouncer = true
`, nil)
		_, err := loadConfigDir(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "libera")
	})

	t.Run("missing pass rejected", func(t *testing.T) {
		dir := createTestConfigDir(t, `
trigger = "."
[soju]
host = "bouncer.example.com"
user = "dave"
[networks.libera]
nick = "dave"
bouncer = true
`, nil)
		_, err := loadConfigDir(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "[soju]")
	})

	t.Run("name with slash rejected", func(t *testing.T) {
		dir := createTestConfigDir(t, `
trigger = "."
[soju]
host = "bouncer.example.com"
user = "dave"
pass = "x"
[networks."a/b"]
nick = "dave"
bouncer = true
`, nil)
		_, err := loadConfigDir(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a/b")
	})

	t.Run("disabled bouncer network is inert", func(t *testing.T) {
		dir := createTestConfigDir(t, `
trigger = "."
[networks.libera]
nick = "dave"
bouncer = true
enabled = false
`, nil)
		cfg, err := loadConfigDir(dir)
		require.NoError(t, err)
		require.Nil(t, cfg.Soju)
		assert.True(t, cfg.Networks["libera"].IsBouncer())
	})

	t.Run("soju section without bouncer networks loads", func(t *testing.T) {
		dir := createTestConfigDir(t, `
trigger = "."
[soju]
host = "bouncer.example.com"
user = "dave"
pass = "secret"
[networks.direct]
nick = "dave"
`, nil)
		cfg, err := loadConfigDir(dir)
		require.NoError(t, err)
		require.NotNil(t, cfg.Soju)
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run TestLoadConfigDirSoju 2>&1 | tail -5`
Expected: FAIL — `cfg.Soju` undefined / `IsBouncer` undefined (compile error).

- [ ] **Step 3: Implement**

`config.go`:

1. Add to `Config` struct (line ~21 region, near `Networks`):

```go
	Soju                *SojuConfig `toml:"soju"`
```

2. Add near `type Server struct`:

```go
// SojuConfig is the root-level soju bouncer connection block. Networks opt in
// per-network with bouncer = true; with no opt-ins the section is inert
// (byte-identical direct behavior). Spec:
// docs/superpowers/specs/2026-11-16-soju-bouncer-support-design.md.
type SojuConfig struct {
	Host               string `toml:"host"`
	Port               int    `toml:"port"`
	Ssl                *bool  `toml:"ssl"`
	InsecureSkipVerify bool   `toml:"insecure_skip_verify"`
	User               string `toml:"user"`
	Pass               string `toml:"pass"`
	ForceReconcile     bool   `toml:"force_reconcile"`
}

// DialPort returns the bouncer IRC-over-TLS port, defaulting to 6697.
func (s *SojuConfig) DialPort() int {
	if s.Port == 0 {
		return 6697
	}
	return s.Port
}

// UseTLS defaults to true — plain-text to a remote bouncer would leak the
// SASL credentials dave sends it.
func (s *SojuConfig) UseTLS() bool {
	if s.Ssl == nil {
		return true
	}
	return *s.Ssl
}
```

3. Add to `Network` struct (after `Quitmsg`):

```go
	Bouncer           *bool          `toml:"bouncer"`
```

and the resolver (house style: pointer-field + defensive resolver like `IsEnabled`):

```go
// IsBouncer reports whether this network dials the [soju] bouncer instead of
// its own server list. Nil = false (direct).
func (n *Network) IsBouncer() bool {
	return n.Bouncer != nil && *n.Bouncer
}
```

4. In the ApplyDefaults network loop (~line 836), inside `for name, network := range config.Networks`, before `config.Networks[name] = network`:

```go
		if network.IsBouncer() && network.IsEnabled() {
			if err := validateBouncerNetwork(name, config.Soju); err != nil {
				return config, err
			}
		}
```

and the validator as a free function right after the loop's enclosing function:

```go
// validateBouncerNetwork enforces the soju preconditions for one enabled
// bouncer network. Disabled networks are inert (validated at startup when
// they are actually dialed).
func validateBouncerNetwork(name string, soju *SojuConfig) error {
	if soju == nil {
		return fmt.Errorf("networks.%s: bouncer = true requires a [soju] section", name)
	}
	if strings.TrimSpace(soju.Host) == "" {
		return fmt.Errorf("[soju]: host is required when a network uses bouncer = true")
	}
	if soju.User == "" || soju.Pass == "" {
		return fmt.Errorf("[soju]: user and pass are required when a network uses bouncer = true")
	}
	// The network name rides inside the downstream SASL username
	// ("<user>/<name>@bot") and as the soju network name in BouncerServ
	// commands; '/' and '@' are username-delimiters and would corrupt both.
	if strings.ContainsAny(name, "/@") {
		return fmt.Errorf("networks.%s: network names used with bouncer = true must not contain '/' or '@'", name)
	}
	return nil
}
```

`config/config.toml` — insert a `[soju]` reference block right before the `[networks.<name>] options:` block:

```
# [soju] options (soju bouncer support; optional — networks opt in with bouncer = true):
#   host               (string)                Bouncer address
#   port               (int, default: 6697)    Bouncer port
#   ssl                (bool, default: true)   Use TLS to the bouncer
#   insecure_skip_verify (bool, default: false) Skip bouncer TLS certificate verification
#   user               (string)                soju username
#   pass               (string)                soju password
#   force_reconcile    (bool, default: false)  One-shot: re-push SASL + connect-commands for all bouncer networks at startup (set, restart, unset)
```

and to the example block (before `# [networks.example]`):

```
# [soju]
# host = "bouncer.example.com"
# port = 6697
# ssl = true
# user = "dave"
# pass = "soju-password"
# force_reconcile = false
```

plus in the `[networks.<name>] options:` reference list (after `connect_commands`):

```
#   bouncer            (bool, default: false)  Connect via the [soju] bouncer instead of dialing servers directly (requires [soju])
```

and in the example network block:

```
# bouncer = false
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run TestLoadConfigDirSoju -v 2>&1 | tail -12`
Expected: PASS all subtests.

- [ ] **Step 5: Full suite + fmt + vet + commit**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -3`

```bash
git add config.go config/config.toml config_test.go
git commit -m "soju: [soju] config section + per-network bouncer flag with validation"
```

---

### Task 4: Bouncer-mode connection wiring

**Files:**
- Modify: `config.go` (add `BouncerDial` + `BouncerDial` struct near `SojuConfig`)
- Modify: `main.go` (`ircClient()` lines ~950-1051)
- Test: `config_test.go`

**Interfaces:**
- Consumes: `Network.IsBouncer()`, `SojuConfig` (Task 3), `readConfig(f func())` (main.go:277 — closure reads the global `config`).
- Produces:

```go
type BouncerDial struct {
	Host               string
	Port               int
	Ssl                bool
	InsecureSkipVerify bool
	SASLUser           string // "<soju.user>/<network-name>@bot"
	SASLPass           string
}
func (n *Network) BouncerDial(soju *SojuConfig) *BouncerDial // nil for direct networks
```

- [ ] **Step 1: Write the failing dial tests**

Append to `config_test.go`:

```go
func TestNetworkBouncerDial(t *testing.T) {
	soju := &SojuConfig{Host: "bnc.example.com", User: "dave", Pass: "pw"}

	t.Run("direct network returns nil", func(t *testing.T) {
		n := Network{Name: "libera"}
		assert.Nil(t, n.BouncerDial(soju))
	})

	t.Run("nil soju returns nil", func(t *testing.T) {
		n := Network{Name: "libera", Bouncer: boolPtr(true)}
		assert.Nil(t, n.BouncerDial(nil))
	})

	t.Run("bouncer network resolves with SASL username", func(t *testing.T) {
		n := Network{Name: "libera", Bouncer: boolPtr(true)}
		d := n.BouncerDial(soju)
		require.NotNil(t, d)
		assert.Equal(t, "bnc.example.com", d.Host)
		assert.Equal(t, 6697, d.Port)
		assert.True(t, d.Ssl)
		assert.Equal(t, "dave/libera@bot", d.SASLUser)
		assert.Equal(t, "pw", d.SASLPass)
	})

	t.Run("port and ssl overrides honored", func(t *testing.T) {
		off := false
		s := &SojuConfig{Host: "bnc.example.com", Port: 7000, Ssl: &off, User: "dave", Pass: "pw"}
		n := Network{Name: "libera", Bouncer: boolPtr(true)}
		d := n.BouncerDial(s)
		require.NotNil(t, d)
		assert.Equal(t, 7000, d.Port)
		assert.False(t, d.Ssl)
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run TestNetworkBouncerDial 2>&1 | tail -5`
Expected: FAIL — `BouncerDial`/`BouncerDial` type undefined.

- [ ] **Step 3: Implement dial resolution + ircClient wiring**

`config.go`, after `SojuConfig`'s methods:

```go
// BouncerDial holds the resolved [soju] connection parameters for one
// network. The downstream SASL username encodes the soju network (soju's
// legacy per-network connection mode) and a per-client buffer name so dave's
// backlog position is its own.
type BouncerDial struct {
	Host               string
	Port               int
	Ssl                bool
	InsecureSkipVerify bool
	SASLUser           string
	SASLPass           string
}

// BouncerDial returns nil for direct-mode networks (or when soju is not
// configured) — callers must treat nil as "dial the server list as today".
func (n *Network) BouncerDial(soju *SojuConfig) *BouncerDial {
	if !n.IsBouncer() || soju == nil {
		return nil
	}
	return &BouncerDial{
		Host:               soju.Host,
		Port:               soju.DialPort(),
		Ssl:                soju.UseTLS(),
		InsecureSkipVerify: soju.InsecureSkipVerify,
		SASLUser:           soju.User + "/" + n.Name + "@bot",
		SASLPass:           soju.Pass,
	}
}
```

`main.go` — `ircClient()`. Replace the dial-resolution head of the function:

```go
	var soju *SojuConfig
	readConfig(func() { soju = config.Soju })
	dial := network.BouncerDial(soju)

	var ircServer Server
	if dial != nil {
		ircServer = Server{
			Host:               dial.Host,
			Port:               dial.Port,
			Ssl:                dial.Ssl,
			InsecureSkipVerify: dial.InsecureSkipVerify,
		}
		log.Info("dialing bouncer", "host", dial.Host, "port", dial.Port, "network", network.Name)
	} else {
		var err error
		ircServer, err = network.getNextServer()
		if err != nil {
			log.Error(err.Error())
			return
		}
		log.Info("dialing server", "host", ircServer.Host, "port", ircServer.GetPort())
	}
```

The `girc.Config` literal stays as-is (it consumes `ircServer`). Replace the SASL block:

```go
	if dial != nil {
		// Downstream SASL authenticates to soju itself; the network's own
		// [sasl] block (if any) is UPSTREAM credentials pushed into soju by
		// the admin session, never sent here. ServerPass stays unset — the
		// bouncer is not a server-password service.
		client.Config.SASL = &girc.SASLPlain{
			User: dial.SASLUser,
			Pass: dial.SASLPass,
		}
		client.Config.ServerPass = ""
		log.Info("SASL PLAIN configured", "user", dial.SASLUser)
	} else if network.SASL != nil && network.SASL.User != "" {
		client.Config.SASL = &girc.SASLPlain{
			User: network.SASL.User,
			Pass: network.SASL.Pass,
		}
		log.Info("SASL PLAIN configured", "user", network.SASL.User)
	}
```

In the reconnect loop, replace the server-rotation tail (the block after `if !reconnect { ... break }`):

```go
		if dial != nil {
			// Bouncer mode has exactly one address; rotation is meaningless.
			log.Info("reconnecting", "host", dial.Host, "port", dial.Port)
			continue
		}
		ircServer, err := bot.Network.getNextServer()
		if err != nil {
			log.Error(err.Error())
			break
		}
		log.Info("reconnecting", "host", ircServer.Host, "port", ircServer.GetPort())
		client.Config.Server = ircServer.Host
		client.Config.Port = ircServer.GetPort()
		client.Config.ServerPass = ircServer.Pass
		client.Config.SSL = ircServer.Ssl
		sslConfig.InsecureSkipVerify = ircServer.InsecureSkipVerify
```

(Keep the existing `bot.mu.Lock(); reconnect := bot.Reconnect; ...` check above this block unchanged. Note the `continue` skips the rotation block entirely for bouncer mode — the Config still holds the single bouncer address from initial construction.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run TestNetworkBouncerDial -v 2>&1 | tail -8`
Expected: PASS.

- [ ] **Step 5: Full suite + fmt + vet + commit**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -3`

```bash
git add config.go main.go config_test.go
git commit -m "soju: bouncer-mode dialing — SASL <user>/<net>@bot, no server rotation"
```

---

### Task 5: Admin session + BouncerServ client

**Files:**
- Create: `sojuadmin.go`
- Create: `sojuadmin_test.go`
- Modify: `main.go` (global var; `shutdown()` — admin Stop goes in Task 7 with startup wiring)

**Interfaces:**
- Consumes: `SojuConfig` (Task 3), house patterns: `readConfig`, logger via `newLogger("soju-admin")`, `wg`, injectable seam vars.
- Produces (Tasks 6-7 consume):

```go
type SojuAdmin struct{ ... }
func startSojuAdmin(soju *SojuConfig) *SojuAdmin // builds client, starts reconnect goroutine (wg-tracked)
func (a *SojuAdmin) Stop()
func (a *SojuAdmin) Connected() bool
func (a *SojuAdmin) Command(line string) []string // serialized; nil when session down
type commandRunner interface{ Command(line string) []string } // *SojuAdmin satisfies it
func collectReplies(sink <-chan string, quietPeriod, maxWait time.Duration) []string // pure, testable
var anyEnabledBouncerNetwork = func(networks map[string]Network) bool // seam
```

- [ ] **Step 1: Write the failing tests**

Create `sojuadmin_test.go`:

```go
package main

import (
	"testing"
	"time"

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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestCollectReplies|TestSojuAdmin|TestAnyEnabledBouncer' 2>&1 | tail -5`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement `sojuadmin.go`**

Create `sojuadmin.go`:

```go
package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/lrstanley/girc"
	"github.com/kitlog-fork/logxi" // CHECK: use the same logxi import path as main.go's newLogger
)

// DESIGN NOTE (docs/superpowers/specs/2026-11-16-soju-bouncer-support-design.md):
// SojuAdmin is dave's unbound session against the configured soju bouncer —
// an ordinary girc client bound to no network, so it sees only soju's
// BouncerServ service. dave's TOML is the single source of truth; this
// session is how the projection gets maintained: networks are created and
// deleted, SASL credentials and connect-commands are pushed, orphaned soju
// state is removed (Task 6). Its death never affects network sessions.
//
// Command/reply protocol constraint: BouncerServ replies are plain NOTICEs
// from the service with no per-command correlation, so commands are
// serialized under a mutex and replies are attributed by quiet-period timing.

const bouncerServiceNick = "BouncerServ"

// SojuAdmin is one process-wide instance (global `sojuAdmin`), started iff
// any enabled network has bouncer = true.
type SojuAdmin struct {
	mu        sync.Mutex
	client    *girc.Client
	connected bool
	sink      chan string

	Reconnect bool
	quitCh    chan struct{}
	stopOnce  sync.Once
	soju      *SojuConfig
}

func startSojuAdmin(soju *SojuConfig) *SojuAdmin {
	log := newLogger("soju-admin")
	a := &SojuAdmin{
		Reconnect: true,
		quitCh:    make(chan struct{}),
		soju:      soju,
	}
	client := girc.New(girc.Config{
		Server:     soju.Host,
		Port:       soju.DialPort(),
		Nick:       soju.User,
		SSL:        soju.UseTLS(),
		TLSConfig:  &tls.Config{ServerName: soju.Host, InsecureSkipVerify: soju.InsecureSkipVerify},
		AllowFlood: true,
	})
	client.Config.SASL = &girc.SASLPlain{User: soju.User, Pass: soju.Pass}
	a.client = client

	client.Handlers.Add(girc.RPL_WELCOME, func(c *girc.Client, e girc.Event) {
		a.mu.Lock()
		a.connected = true
		a.mu.Unlock()
		log.Info("admin session connected")
	})

	client.Handlers.AddBg(girc.NOTICE, func(c *girc.Client, e girc.Event) {
		if e.Source == nil || !strings.EqualFold(e.Source.Name, bouncerServiceNick) {
			return
		}
		a.deliver(e.Last())
	})

	client.Handlers.Add(girc.DISCONNECTED, func(c *girc.Client, e girc.Event) {
		a.mu.Lock()
		a.connected = false
		a.mu.Unlock()
	})

	wg.Add(1)
	go a.loop(log)
	return a
}

// loop mirrors ircClient's reconnect loop (first retry immediate, then the
// default delay; quit via Stop).
func (a *SojuAdmin) loop(log logxi.Logger) {
	defer wg.Done()
	for {
		if err := a.client.Connect(); err != nil {
			delay := 60 * time.Second
			select {
			case <-time.After(delay):
			case <-a.quitCh:
			}
		}
		a.mu.Lock()
		reconnect := a.Reconnect
		a.mu.Unlock()
		if !reconnect {
			log.Info("admin session finished quitting")
			return
		}
	}
}

// Stop is idempotent (shutdown path + tests call it freely).
func (a *SojuAdmin) Stop() {
	a.stopOnce.Do(func() {
		a.mu.Lock()
		a.Reconnect = false
		client := a.client
		a.mu.Unlock()
		close(a.quitCh)
		if client != nil {
			client.Close()
		}
	})
}

func (a *SojuAdmin) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connected
}

// deliver fans a BouncerServ NOTICE line into the active command's sink, if
// any. Called from the NOTICE handler goroutine.
func (a *SojuAdmin) deliver(line string) {
	a.mu.Lock()
	if a.sink != nil {
		select {
		case a.sink <- line:
		default: // sink full: drop the line, Command's max-wait still bounds the call
		}
	}
	a.mu.Unlock()
}

// Command sends one BouncerServ command and collects its NOTICE replies,
// returning when the service goes quiet or maxWait elapses. Serialized:
// replies carry no per-command correlation. Returns nil when the session is
// down — callers must treat that as "management unavailable", never fatal.
func (a *SojuAdmin) Command(line string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil || !a.connected {
		return nil
	}
	sink := make(chan string, 64)
	a.sink = sink
	defer func() { a.sink = nil }()
	if err := a.client.Cmd.Message(bouncerServiceNick, line); err != nil {
		return nil
	}
	return collectReplies(sink, 750*time.Millisecond, 5*time.Second)
}

// collectReplies drains sink until quietPeriod passes with no new line, or
// maxWait elapses, whichever first. Pure so tests can drive it directly.
func collectReplies(sink <-chan string, quietPeriod, maxWait time.Duration) []string {
	var replies []string
	quiet := time.NewTimer(quietPeriod)
	max := time.NewTimer(maxWait)
	defer quiet.Stop()
	defer max.Stop()
	for {
		select {
		case r := <-sink:
			replies = append(replies, r)
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(quietPeriod)
		case <-quiet.C:
			return replies
		case <-max.C:
			return replies
		}
	}
}

// commandRunner is the seam reconciliation is tested through (Task 6): the
// real admin session or a recording fake.
type commandRunner interface {
	Command(line string) []string
}

// anyEnabledBouncerNetwork reports whether any network in the map is enabled
// AND in bouncer mode — the admin session only exists then.
var anyEnabledBouncerNetwork = func(networks map[string]Network) bool {
	for _, n := range networks {
		if n.IsEnabled() && n.IsBouncer() {
			return true
		}
	}
	return false
}
```

Add `"crypto/tls"` and `"strings"` to the import block (the logxi import must match `main.go`'s — check with `grep -n logxi main.go | head -2`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestCollectReplies|TestSojuAdmin|TestAnyEnabledBouncer' -v 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 5: Full suite + fmt + vet + commit**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -3`

```bash
git add sojuadmin.go sojuadmin_test.go
git commit -m "soju: admin session + serialized BouncerServ command client"
```

---

### Task 6: Network reconciliation (parser + logic)

**Files:**
- Modify: `sojuadmin.go` (append: plan builder, parser, reconcile logic, real wiring function)
- Test: `sojuadmin_test.go` (append)

**Interfaces:**
- Consumes: `commandRunner` (Task 5), `SojuConfig.ForceReconcile`, `Network.ConnectCommands` (Task 2), `Network.SASL`, `Network.Servers` + `Server.GetPort()` (config.go:741).
- Produces:

```go
type sojuNetworkPlan struct {
	Name            string
	Addr            string // first server host, with :port when != 6697
	Nick            string
	RealName        string
	SASL            *SASLConfig
	ConnectCommands []string
}
func buildSojuPlans(networks map[string]Network) (plans []sojuNetworkPlan, tomlNames map[string]bool)
func parseNetworkStatus(lines []string) map[string]bool
func reconcileNetworks(run commandRunner, existing map[string]bool, plans []sojuNetworkPlan, tomlNames map[string]bool, force bool, log logxi.Logger)
func reconcileSojuNetworks(a *SojuAdmin, log logxi.Logger) // real wiring: reads global config
```

- [ ] **Step 1: Write the failing tests**

Append to `sojuadmin_test.go`:

```go
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
```

(If `newLogger` is unsuitable in tests, substitute the test logger pattern used elsewhere in the repo — check how existing tests obtain a logger; a plain `logxi.New` with `SetLevel(logxi.LevelAll)` per the AGENTS.md logger rule works.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run 'TestParseNetworkStatus|TestReconcileNetworks' 2>&1 | tail -5`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

Append to `sojuadmin.go`:

```go
// sojuNetworkPlan is one TOML bouncer network resolved against the soju
// projection. Addr is the recorded upstream address for network create:
// the first server's host, with :port appended when it is not the TLS
// default (soju connects upstream with TLS by default).
type sojuNetworkPlan struct {
	Name            string
	Addr            string
	Nick            string
	RealName        string
	SASL            *SASLConfig
	ConnectCommands []string
}

// buildSojuPlans snapshots the enabled bouncer networks (plan set) and every
// TOML network name (the never-delete set, regardless of mode or enabled).
func buildSojuPlans(networks map[string]Network) ([]sojuNetworkPlan, map[string]bool) {
	var plans []sojuNetworkPlan
	tomlNames := make(map[string]bool, len(networks))
	for name, n := range networks {
		tomlNames[name] = true
		if !n.IsEnabled() || !n.IsBouncer() {
			continue
		}
		p := sojuNetworkPlan{
			Name:            name,
			Nick:            n.Nick,
			RealName:        n.RealName,
			SASL:            n.SASL,
			ConnectCommands: n.ConnectCommands,
		}
		if len(n.Servers) > 0 {
			s := n.Servers[0]
			if s.GetPort() != 6697 {
				p.Addr = fmt.Sprintf("%s:%d", s.Host, s.Port)
			} else {
				p.Addr = s.Host
			}
		}
		plans = append(plans, p)
	}
	return plans, tomlNames
}

// parseNetworkStatus extracts network names from BouncerServ `network status`
// NOTICE lines. soju's service.go prints "<name> [<status>, ...][: details]"
// and "<name> (<addr>) [<status>, ...]" — the name is everything before the
// first " [", minus a trailing " (<addr>)". Anything unparseable is skipped;
// this is the ONLY BouncerServ output dave parses (spec constraint), so if a
// soju upgrade changes the shape, this function is the one file to fix.
func parseNetworkStatus(lines []string) map[string]bool {
	names := map[string]bool{}
	for _, line := range lines {
		idx := strings.Index(line, " [")
		if idx <= 0 {
			continue
		}
		name := line[:idx]
		if i := strings.Index(name, " ("); i > 0 {
			name = name[:i]
		}
		name = strings.TrimSpace(name)
		if name != "" {
			names[name] = true
		}
	}
	return names
}

// reconcileNetworks aligns soju's network set with dave's TOML (the spec's
// network-lifecycle steps): create missing bouncer networks (pushing their
// SASL + connect-commands, since a fresh soju network has none), leave
// existing ones untouched unless force, and delete orphans — soju networks
// whose name matches no TOML network in any mode. Names present in TOML but
// direct-mode/disabled are WARNed and deliberately preserved: a temporary
// bouncer = false flip must never destroy stored credentials.
func reconcileNetworks(run commandRunner, existing map[string]bool, plans []sojuNetworkPlan, tomlNames map[string]bool, force bool, log logxi.Logger) {
	inPlans := make(map[string]bool, len(plans))
	for _, p := range plans {
		inPlans[p.Name] = true
		if existing[p.Name] {
			if force {
				pushSojuCreds(run, p, log)
			}
			continue
		}
		log.Info("soju: creating network", "network", p.Name, "addr", p.Addr)
		run.Command(fmt.Sprintf("network create -addr %s -name %s -nick %q -realname %q", p.Addr, p.Name, p.Nick, p.RealName))
		pushSojuCreds(run, p, log)
	}
	for name := range existing {
		switch {
		case tomlNames[name] && !inPlans[name]:
			log.Warn("soju: network present in TOML but not bouncer mode (or disabled) — leaving soju state as-is", "network", name)
		case tomlNames[name]:
			// bouncer-mode plan, already handled above
		default:
			log.Warn("soju: deleting orphaned network (absent from config.toml)", "network", name)
			run.Command("network delete " + name)
		}
	}
}

// pushSojuCreds writes a plan's upstream credentials into soju. Only ever
// called for freshly created networks or under force_reconcile — network
// update forces an upstream reconnect, so unconditional pushes would make
// every dave boot visible to the IRC networks.
func pushSojuCreds(run commandRunner, p sojuNetworkPlan, log logxi.Logger) {
	if p.SASL != nil && p.SASL.User != "" {
		log.Info("soju: pushing SASL credentials", "network", p.Name)
		run.Command(fmt.Sprintf("sasl set-plain -network %s %q %q", p.Name, p.SASL.User, p.SASL.Pass))
	}
	for _, cmd := range p.ConnectCommands {
		log.Info("soju: pushing connect-command", "network", p.Name)
		run.Command(fmt.Sprintf("network update -connect-command %q", cmd))
	}
}

// reconcileSojuNetworks is the real wiring: snapshot config, query soju's
// network list, reconcile. Runs on the admin session's RPL_WELCOME.
func reconcileSojuNetworks(a *SojuAdmin, log logxi.Logger) {
	var soju *SojuConfig
	var networks map[string]Network
	readConfig(func() {
		soju = config.Soju
		networks = make(map[string]Network, len(config.Networks))
		for name, n := range config.Networks {
			networks[name] = n
		}
	})
	if soju == nil {
		return
	}
	plans, tomlNames := buildSojuPlans(networks)
	existing := parseNetworkStatus(a.Command("network status"))
	reconcileNetworks(a, existing, plans, tomlNames, soju.ForceReconcile, log)
}
```

Add `"fmt"` to the import block.

Finally, wire reconciliation into the admin session's `RPL_WELCOME` handler (the one from Task 5) — reconciliation re-runs on every (re)connect and is idempotent:

```go
		// Reconciliation re-runs on every (re)connect; it is idempotent.
		go reconcileSojuNetworks(a, log)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run 'TestParseNetworkStatus|TestReconcileNetworks' -v 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 5: Full suite + fmt + vet + commit**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -3`

```bash
git add sojuadmin.go sojuadmin_test.go
git commit -m "soju: network reconciliation — create+push creds, orphan deletion, force_reconcile"
```

---

### Task 7: Channel reconciliation + main() wiring + shutdown

**Files:**
- Modify: `sojuadmin.go` (append `extraChannels`, grace const)
- Modify: `irc_handlers.go` (RPL_WELCOME handler — channel diff scheduling)
- Modify: `main.go` (global `sojuAdmin`, startup wiring after client start loop, `shutdown()` stop)
- Test: `sojuadmin_test.go` (append)

**Interfaces:**
- Consumes: `Network.IsBouncer()` (Task 3), `SojuAdmin` (Task 5), `sojuAdmin` global (this task), `bot.casemapping`/`bot.Network.Channels` (existing), `client.ChannelList()` (girc v1.1.3, client.go:600), `client.Cmd.Part` (soju forgets stored channels on downstream PART — its documented removal mechanism; plain PART only, never the "detach" reason).
- Produces: `extraChannels(joined []string, channels map[string]ChannelConfig, casemapping string) []string`; global `sojuAdmin *SojuAdmin`; `const channelDiffGrace = 15 * time.Second`.

- [ ] **Step 1: Write the failing tests**

Append to `sojuadmin_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run TestExtraChannels 2>&1 | tail -5`
Expected: FAIL — `extraChannels` undefined.

- [ ] **Step 3: Implement**

Append to `sojuadmin.go`:

```go
// channelDiffGrace is how long after RPL_WELCOME a bouncer network waits
// before diffing girc's channel state against the TOML channel list. soju
// sends its stored-channel join burst right after registration; the diff
// must observe the settled state. Re-armed naturally per reconnect (each
// WELCOME schedules one diff; repeats are idempotent).
const channelDiffGrace = 15 * time.Second

// extraChannels returns channels girc shows joined that dave's TOML does not
// configure — the soju-side leftovers a channel removal leaves behind.
// Normalized compare, same rule as Network.GetChannelConfig (config keys stay
// exactly as typed; normalization happens at lookup time).
func extraChannels(joined []string, channels map[string]ChannelConfig, casemapping string) []string {
	cm := casemapping
	if cm == "" {
		cm = "rfc1459"
	}
	var out []string
	for _, ch := range joined {
		norm := normalizeIRC(ch, cm)
		found := false
		for k := range channels {
			if normalizeIRC(k, cm) == norm {
				found = true
				break
			}
		}
		if !found {
			out = append(out, ch)
		}
	}
	return out
}
```

`irc_handlers.go` — in the `RPL_WELCOME` handler, after the connect-commands loop (Task 2), inside the same closure (`log` is in scope):

```go
		// soju channel reconciliation: after the join burst settles, PART the
		// soju-side channels that no longer exist in TOML — the owner's
		// removal must propagate, and a downstream PART makes soju leave the
		// channel upstream AND forget it from its stored list (plain PART
		// only; "detach" would keep it). Spec channel-lifecycle section.
		if network.IsBouncer() {
			time.AfterFunc(channelDiffGrace, func() {
				bot.mu.Lock()
				cm := bot.casemapping
				chs := bot.Network.Channels
				bot.mu.Unlock()
				for _, extra := range extraChannels(client.ChannelList(), chs, cm) {
					log.Warn("soju: parting channel absent from config", "channel", extra, "network", network.Name)
					client.Cmd.Part(extra)
				}
			})
		}
```

`main.go`:

1. Global var near the other globals (e.g. next to `bots`):

```go
var sojuAdmin *SojuAdmin
```

2. In `main()`, right after the loop that calls `startClient(...)` for each enabled network (find it via `startClient(network)`):

```go
	// soju admin session: exists only when at least one enabled network
	// dials the bouncer (spec: soju support is optional per network).
	var sojuCfg *SojuConfig
	readConfig(func() { sojuCfg = config.Soju })
	if sojuCfg != nil && anyEnabledBouncerNetwork(config.Networks) {
		sojuAdmin = startSojuAdmin(sojuCfg)
	}
```

3. In `shutdown()` — immediately after the `for _, bot := range snapshotBots() { bot.Quit() }` loop:

```go
	if sojuAdmin != nil {
		sojuAdmin.Stop()
	}
```

(Shutdown-path rule: this is a stop call routed through shutdown(), not a parallel cleanup path — the admin session's own goroutine exits via its loop.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test . -run TestExtraChannels -v 2>&1 | tail -8`
Expected: PASS.

- [ ] **Step 5: Full suite + fmt + vet + commit**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -3`

```bash
git add sojuadmin.go sojuadmin_test.go irc_handlers.go main.go
git commit -m "soju: channel reconciliation via girc state diff + admin session lifecycle"
```

---

### Task 8: Config docs finalization + AGENTS.md + verification

**Files:**
- Modify: `config/config.toml` (verify Task 2/3 doc additions; add nothing new if already present)
- Modify: `AGENTS.md` (one bullet documenting the subsystem, house style)
- Test: none new — this task is the verification gate.

**Interfaces:** Consumes everything; produces docs only.

- [ ] **Step 1: Verify config.toml reference completeness**

Run: `grep -n "soju\|bouncer\|connect_commands" config/config.toml`
Expected: `[soju]` reference block (host/port/ssl/insecure_skip_verify/user/pass/force_reconcile), `bouncer` + `connect_commands` lines in `[networks.<name>] options`, and matching lines in the commented example block. Add anything missing following the exact format of neighboring lines.

- [ ] **Step 2: Add the AGENTS.md bullet**

Add one bullet to AGENTS.md's architecture bullet list (after the guidance-injection or model-gate bullet — placement with the other connection-adjacent subsystems), in the established dense style. Keep it to the load-bearing facts:

```markdown
- **soju bouncer support** (sojuadmin.go + config/main/irc_handlers, Nov 2026; spec `docs/superpowers/specs/2026-11-16-soju-bouncer-support-design.md`): soju runs on the current IRC-facing server (IP preservation) and dave moves machines; per-network opt-in (`bouncer = true` requires root `[soju]`; absent `[soju]` = byte-identical direct behavior). Bouncer networks dial soju with SASL `<soju.user>/<name>@bot` (server rotation skipped; the network's own `[sasl]` block becomes UPSTREAM creds pushed into soju, never sent downstream), and soju's on-reconnect backlog replay is neutralized by `isReplayedEvent` (first statement of the PRIVMSG dispatch path: `batch` tag, or `Event.Timestamp` older than connect−30s — girc always sets Timestamp; ignore-all policy: replayed rows are logged by ALL_EVENTS with original @time — closing deploy-window `irc_logs` gaps — and never dispatched; direct connections never trip it; per-message cost one map lookup + one compare per the burst-rate constraint). One unbound admin session (`SojuAdmin`, exists iff any enabled bouncer network) drives BouncerServ: `network status` is the ONLY parsed reply (parseNetworkStatus — one-file fix if soju format drifts); reconciliation on admin connect creates missing networks (`network create` + `sasl set-plain` + `connect_commands` push — only on FRESH creation or `[soju] force_reconcile`, since `network update` forces an upstream reconnect and every dave boot must stay invisible), deletes orphans (soju names absent from TOML in ANY mode — WARN first; TOML names are never deleted, direct-mode/disabled leftovers are preserved with a WARN), and diffs girc channel state vs TOML 15s after each bouncer WELCOME (PARTs extras downstream — soju forgets parted channels; TOML channel removals must propagate; TUI /part propagates naturally). `connect_commands` (new Network field) is also sent directly after registration in direct mode — AuthServ AUTH on GameSurge-class networks works in both modes. Offline visibility is soju auto-away only (`AWAY :Auto away` upstream when dave's sessions drop; owner decision, no custom shutdown away). Guarded by `TestIsReplayedEvent` + `TestDispatchChanMessageSkipsReplay` + `TestNetworkConnectCommands` + `TestLoadConfigDirSoju` + `TestNetworkBouncerDial` + `TestCollectReplies` + `TestSojuAdmin*` + `TestAnyEnabledBouncerNetwork` + `TestParseNetworkStatus` + `TestReconcileNetworks` + `TestExtraChannels`.
```

(Adjust the wording minimally to match final implementation names if they drifted during Tasks 1-7 — the list of test names must match reality.)

- [ ] **Step 3: Full verification**

Run: `go fmt ./... && go vet ./... && go test ./... 2>&1 | tail -5`
Expected: all `ok` (root package suite included).

- [ ] **Step 4: Commit**

```bash
git add config/config.toml AGENTS.md
git commit -m "soju: config docs + AGENTS.md subsystem bullet"
```

---

## Post-implementation (manual, ops side — from spec appendix)

Not part of any task; the owner runs this when migrating:

1. Deploy soju on the current server (postgres db, `message-store db`, `message-expiry`, systemd `Restart=always`), `sojuctl create-user dave -password '<pass>'`.
2. Set `[soju]` in dave's config; set `bouncer = true` on the TEST network only; restart dave.
3. Verify live: admin session connects; `network create` + `sasl set-plain` landed (check via `sojuctl user run dave network status`); bouncer session joins channels; WHOX 354s still resolving accounts (grep resolution-method log fields); deploy dave mid-conversation deliberately and confirm backlog is logged but NOT dispatched (no old triggers fire); queue delivery works.
4. Flip remaining networks one at a time; keep upstream SASL blocks in TOML (dave is the source of truth and re-pushes only on recreate/force_reconcile).
