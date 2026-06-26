# Auto-Rejoin on Kick + `/join` Fix

## Problem

Two related issues around channel membership after a KICK:

1. **`/join` bug (original report):** When the bot is KICKed from a channel, a subsequent TUI `/join <network> <channel>` reports "Already in <channel>". Root cause: `tuiCmdJoin` checks the **config map** (`bot.Network.Channels[channel]`) to decide "already in", but a KICK updates girc's internal state, not the config map. The user had to `/part` then `/join` as a workaround. Worse, `/part` then `/join` would clobber any configured channel key, because `tuiCmdJoin` overwrites the entry with an empty `ChannelConfig{}`.

2. **No auto-rejoin:** There is no option to automatically rejoin a channel after being kicked. Operators want this, enabled by default, with the ability to opt out per-network or per-channel.

A prior attempted fix *removed* the channel from the config map on KICK. That was rejected: it discards the channel's configured settings (notably the +k `key`), and suppresses auto-rejoin on reconnect. This design deliberately **never mutates config in response to a KICK**.

## Design

### Part A — Fix `/join` (`tui_commands.go`)

`/join` must send the IRC JOIN based on intent, not on cached/config state. girc's joined-state can lag or be wrong; sending JOIN when already joined is harmless (the server no-ops or just re-sends topic/names). So `/join` no longer checks "already in" at all.

New `tuiCmdJoin` flow:

1. Resolve the bot for the network (unchanged) — unknown network → error.
2. **Send the IRC JOIN unconditionally:**
   - If a configured `key` exists for the channel (`GetChannelConfig(channel).Key != ""`) → `bot.Client.Cmd.JoinKey(channel, key)`.
   - Else → `bot.Client.Cmd.Join(channel)`.
3. **After** joining, ensure a config entry exists **only if missing** (never clobber an existing entry, so configured keys/settings survive):
   ```go
   bot.mu.Lock()
   if bot.Network.Channels == nil {
       bot.Network.Channels = make(map[string]ChannelConfig)
   }
   if _, ok := bot.Network.Channels[channel]; !ok {
       bot.Network.Channels[channel] = ChannelConfig{}
   }
   bot.mu.Unlock()
   ```
4. Print "Joined %s on %s" (existing message). The "Already in %s" path is removed entirely.

Channel-config lookup uses `GetChannelConfig` (casemapping-aware) rather than a raw map index, consistent with the "never normalize config keys at load time" rule — though the raw channel string is used for the actual JOIN and for storing a brand-new entry (matching how the bot currently stores/joins channels).

### Part B — Auto-rejoin on kick (`irc_handlers.go`)

#### Config (3 new fields)

Inheritance uses the existing `*bool` / `*time.Duration` pointer-field pattern (like `Network.Enabled`, `Network.ReconnectDelay`): `nil` means "inherit/default".

- `Network.AutoRejoin` — `*bool`, TOML `auto_rejoin`, default **true** (network-level default).
- `Network.AutoRejoinDelay` — `*time.Duration`, TOML `auto_rejoin_delay`, default **3s** (per-network).
- `ChannelConfig.AutoRejoin` — `*bool`, TOML `auto_rejoin`, default **nil = inherit network/default** (per-channel opt-out).

```go
// Network (config.go)
AutoRejoin      *bool          `toml:"auto_rejoin"`
AutoRejoinDelay *time.Duration `toml:"auto_rejoin_delay"`

// ChannelConfig (config.go)
AutoRejoin *bool `toml:"auto_rejoin"`
```

#### Resolution helpers (pure, testable)

- `shouldAutoRejoin(net *Network, chCfg ChannelConfig) bool`:
  - If `chCfg.AutoRejoin != nil` → return `*chCfg.AutoRejoin`.
  - Else if `net.AutoRejoin != nil` → return `*net.AutoRejoin`.
  - Else → `true` (default).
- `autoRejoinDelay(net *Network) time.Duration`:
  - If `net.AutoRejoinDelay != nil` → return `*net.AutoRejoinDelay`.
  - Else → `3 * time.Second`.

#### KICK handler change

In `registerIRCHandlers`, the `girc.KICK` handler's `kickedNick == client.GetNick()` branch (currently a bare `return`) becomes:

1. `channel := event.Params[0]` (safe: the existing `len(event.Params) < 2` guard covers Params[0] and Params[1]).
2. **Never touch `bot.Network.Channels`** (config preserved).
3. Resolve channel cfg via `bot.Network.GetChannelConfig(channel)` (casemapping-aware).
4. If `!shouldAutoRejoin(&bot.Network, chCfg)` → log DEBUG ("auto_rejoin disabled"), return.
5. Else schedule a delayed rejoin:
   ```go
   delay := autoRejoinDelay(&bot.Network)
   key := chCfg.Key
   time.AfterFunc(delay, func() {
       if bot.Client == nil || !bot.Client.IsConnected() {
           return // disconnected/shutdown: skip; channel stays in config for RPL_WELCOME rejoin
       }
       if key != "" {
           bot.Client.Cmd.JoinKey(channel, key)
       } else {
           bot.Client.Cmd.Join(channel)
       }
   })
   log.Info("bot kicked; scheduling auto-rejoin", "channel", channel, "network", network.Name, "delay", delay)
   ```

Rationale for `time.AfterFunc` (non-blocking): the handler runs in girc's event goroutine and must not sleep inline. The `IsConnected()` guard makes the callback safe if it fires during shutdown or a netsplit; no new shutdown-cleanup path is needed (shutdown stays single-source in `shutdown()`).

#### Edge cases

- **Disconnected / netsplit at fire time** → guarded, rejoin skipped. The channel remains in `bot.Network.Channels`, so the `RPL_WELCOME` rejoin loop (irc_handlers.go) rejoins it on reconnect. No loss.
- **Kicked from a channel not in config** (e.g. joined via raw) → `GetChannelConfig` returns empty `ChannelConfig` → `AutoRejoin` nil → inherits default(true) → plain rejoin (no key).
- **Repeated kicks** → each KICK schedules its own `AfterFunc`. The fixed delay mitigates kick-rejoin flooding; exponential backoff is intentionally out of scope (user chose fixed delay). A future enhancement could add per-channel backoff.
- **Shutdown** → `IsConnected()` false → skip. No new cleanup path.

### Part C — Config docs (`config/config.toml`)

Per the config-documentation convention (AGENTS.md), update:

1. The `[networks.<name>]` reference list — add:
   - `auto_rejoin (bool, default: true)   Automatically rejoin after being kicked`
   - `auto_rejoin_delay (duration, default: "3s") Delay before auto-rejoining after a kick`
2. The `[networks.<name>.channels."<#channel>"]` reference list — add:
   - `auto_rejoin (bool, inherits network) Override auto-rejoin-on-kick for this channel`
3. The commented example block — add `auto_rejoin` / `auto_rejoin_delay` under the example network, and `auto_rejoin` under the example channel.

Existing live config sections are left untouched.

## Testing

- **`/join`** (TUI harness in `tui_commands_test.go`, mocks `botIsInChannel` where relevant):
  - Not currently joined → JOIN sent, config entry created when missing.
  - Existing configured key is **preserved** (not clobbered) when re-joining a configured channel.
  - Already-joined → JOIN still sent (no "Already in" message; verify the message is gone).
- **`shouldAutoRejoin`** (table-driven, pure): channel `true` overrides network `false`; channel `false` overrides network `true`; nil-channel inherits network; both nil → default true.
- **`autoRejoinDelay`**: explicit value used; nil → 3s default.
- The KICK→`AfterFunc` wiring is a thin caller over the tested helpers. girc event dispatch is not unit-testable without a live socket, so the handler glue relies on the helper tests + manual/integration verification (consistent with the rest of `irc_handlers.go`, which has no dispatch tests).

All tests via `go test ./...`; `go fmt ./...` and `go vet ./...` run after.

## Non-goals

- Per-channel rejoin delay (delay is per-network only, per user decision).
- Exponential backoff / per-channel kick-counters.
- Persisting runtime channel state back to the TOML file (config remains file-driven; in-memory map is rebuilt from file on restart, matching existing `/part` semantics).
- Changing the PART handler (same early-return shape, but only reachable via TUI `/part`, which already maintains the map correctly).
