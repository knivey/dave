# soju Bouncer Support — Design

Date: 2026-11-16
Status: approved approach, spec stage
Prerequisite (already done): girc bumped v1.1.1 → v1.1.3 (go.mod only; build/vet/tests
green; reviewed). The upgrade is load-bearing: girc ≤ v1.1.1 requires 005 ISUPPORT
lines to end in `"this server"`, and soju sends `"are supported"` — the old parser
silently dropped every ISUPPORT token soju sends (CASEMAPPING, WHOX, PREFIX, NETWORK),
breaking dave's casemapping capture behind the bouncer. v1.1.3 parses both forms.

## Problem

dave needs to move to a faster machine, but IRC networks must keep seeing the
current server's IP (cloaks, rate limits, ban history are tied to it). Running a
soju bouncer on the current server solves the IP problem and, as a side effect,
gives dave things a direct connection cannot:

- **Always-on presence.** soju holds the upstream connections 24/7, independent
  of dave restarts. Deploys stop producing reconnect storms, rejoin bursts, and
  nick-regain races on the networks.
- **Gap-free IRC logs.** Messages that arrive while dave is down are stored by
  soju and replayed on reconnect with original `@time=` timestamps. dave's
  `irc_logs` (the source for `logquery`/generators) stops losing everything
  written during a deploy.
- **Persistence for free.** Channels dave joins are stored by soju and
  auto-rejoined forever; SASL credentials live server-side; networks can be
  managed at runtime through BouncerServ instead of config files.

Constraint from the owner: **soju support must be optional.** Direct connections
remain fully supported and are the default; the bouncer is a per-network opt-in.
The owner also set two performance constraints that shape this design:

- **No rate assumptions.** dave is on servers where 10,000 messages can arrive
  in an instant. Every per-message addition must be O(1) and allocation-light,
  and the design must rely on dave's existing backpressure behavior rather than
  inventing new buffering.
- soju's own write path (one DB transaction per message, verified in its source;
  there is no batch layer and the design is accepted as-is) is soju's problem,
  not dave's — dave just must not add a second per-message cost on top of it.

## Goals

- dave can route any configured network through a soju bouncer with one TOML
  flag; all other networks keep connecting directly with zero behavior change.
- dave admins soju as much as possible: networks, SASL credentials, and
  connect-commands are created/updated/removed by dave itself, driven by dave's
  TOML as the single source of truth. soju's config file contains only
  listeners/TLS/db — zero users, networks, or channels ever live in it.
- Replayed history is **logged but never dispatched** (ignore-all policy): no
  trigger fires, no mention starts a turn, regardless of how long dave was down.
- Removing a channel or network from dave's TOML (and restarting) removes it
  from soju too — no orphaned upstream connections or stale credentials.
- GameSurge-style networks (no SASL; AuthServ auth after registration) work in
  both modes: direct mode sends connect-commands itself; bouncer mode pushes
  them into soju's per-network connect-commands.
- Offline visibility is soju's auto-away and nothing more (owner decision):
  when dave's sessions drop, soju marks the nick `AWAY :Auto away` upstream and
  clears it on reconnect. dave adds no custom shutdown away message.

## Non-goals

- No `draft/chathistory` client support: dave keeps soju's auto-backlog on
  reconnect (pulling history on demand is future work if ever wanted).
- No grace-window dispatch of recent replayed mentions (owner decision:
  ignore-all; someone who messaged dave during an outage gets the same silence
  as today, except the message is now in dave's logs for a human).
- No hot-reload of network configuration: networks are loaded at startup only
  (unchanged dave behavior). TOML network edits — including soju-related ones —
  apply on restart.
- No custom away messages, no channel-detachment features, no soju.im
  extensions beyond what girc already negotiates.
- The soju deployment itself (server X) is ops work; this spec includes a
  runbook appendix but no dave code depends on it.

## Architecture

```
IRC networks (Libera, GameSurge, …)      fast machine Y                current server X (today's IP)
        ▲                                       │                              │
        │  TLS, still from X's IP               │  TLS ircs://X:6697           │
        └─────────────────────── soju ◄─────────┴──────────────────────────────┘
```

- soju runs on the current server. Upstream connections are per-network
  goroutines inside soju with exponential backoff, running independent of any
  downstream client — dave can be down for hours with its nick, channels, and
  place in the network intact.
- dave (machine Y) opens **one downstream connection per bouncer network** plus
  **one unbound admin session**. Legacy per-network connections are the right
  integration mode: soju's `bouncer-networks` single-connection multiplexing
  needs client support girc does not have, and dave's per-network
  `Bot`/casemapping/channels model maps 1:1 onto per-network sessions anyway.
- Identity: SASL PLAIN to soju. Admin session username is the bare soju
  username; network sessions use `<user>/<network-name>@bot`. The `@bot` client
  suffix gives dave its own per-client backlog buffer, independent of any other
  client attached later. dave's TOML network names are used verbatim as soju
  network names, so every later management command (`network delete`,
  `channel delete -network`, `sasl set-plain -network`) references the name the
  owner already knows.

### Config shape

Root `config.toml` gains:

```toml
[soju]
host = "irc.example.com"       # bouncer address (required when used)
port = 6697                    # default 6697
ssl = true                     # default true
insecure_skip_verify = false
user = "dave"                  # soju username (base)
pass = "secret"                # soju password
force_reconcile = false        # one-shot: push SASL + connect-commands for ALL bouncer networks at startup
```

Per network (`[networks.<name>]`):

```toml
bouncer = true                 # default false — route through [soju] instead of dialing servers directly
connect_commands = [           # new field, both modes (see GameSurge section)
  "PRIVMSG AuthServ@Services.GameSurge.net :AUTH user pass",
]
```

Rules:

- `[soju]` absent and no `bouncer = true` anywhere → byte-identical behavior to
  today; not even the admin session starts.
- A network with `bouncer = true` ignores its `[[networks.<name>.servers]]`
  list entirely (rotation is meaningless behind one bouncer address; the
  latent TLS-ServerName-not-rotated-on-rotation quirk becomes unreachable).
  Its `[networks.<name>.sasl]` block, if set, is **upstream** credentials to be
  pushed into soju — dave's own downstream SASL to soju is always
  `<soju.user>/<name>@bot` + `<soju.pass>`.
- Config-load validation: `bouncer = true` requires `[soju]`; network names used
  with `bouncer = true` must not contain `/` or `@` (they ride inside the SASL
  username); `bouncer = true` with an empty `[soju].pass` is rejected (soju
  auth would fail every connect).
- Upstream address recorded on the soju network at creation time = the network's
  first configured server host (plus `:port` when the port is not the TLS
  default). Nick/realname from the network block are passed at creation.
  Documented limitation: soju defaults the upstream to TLS; a bouncer network
  whose direct-mode server list is plain-text needs a manual
  `network update -addr irc+insecure://...` (rare; GameSurge supports TLS).

### Connection wiring (`ircClient()` / `startClient()`)

For a bouncer network, the `girc.Config` is built exactly as today except:

- `Server`/`Port`/`SSL`/`TLSConfig` come from `[soju]` instead of
  `ircServer.Host/GetPort()`. `ServerPass` is not set (SASL covers auth).
- `Config.SASL = &girc.SASLPlain{User: "<soju.user>/<name>@bot", Pass: soju.pass}`.
- The reconnect loop skips `getNextServer()` rotation (single address) and
  otherwise behaves identically: `RPL_WELCOME` still fires the casemapping read
  and the configured-channel join loop (idempotent behind soju — a JOIN for a
  channel soju already has is a local attach; a JOIN for a channel soju lacks
  joins upstream **and persists it**, which keeps dave's channel list the
  effective source of truth for membership).
- girc's auto-WHO on join stays enabled (the `DisableAutoWHOOnJoin` /
  `DisableAutoMODEOnJoin` knobs from v1.1.3 are NOT set): dave's WHOX-based
  account resolution depends on those 354 replies, and soju relays WHO/WHOX
  correctly (it is WHOX-listed in both directions; it refuses to serve cached
  account data for upstreams without `account-notify`, forwarding upstream
  instead — Libera supports it).

### Admin session

One extra girc client per process, started iff at least one enabled network has
`bouncer = true`:

- Connects to `[soju]` with SASL username `<soju.user>` (no suffix), nick =
  `<soju.user>`. Bound to no network: it sees only BouncerServ.
- Own reconnect loop, same delay policy as network clients. Its death does not
  affect network sessions — they keep working; reconciliation simply waits for
  the admin session to return, since every management path runs through it.
- All soju management goes through it as `PRIVMSG BouncerServ :...` lines.

### soju admin module (reconciliation)

**Owner's requirement:** when dave's TOML removes a channel or network, soju
must converge. dave's TOML is the single source of truth; soju is a projection.

Triggers: process startup (after the admin session connects) and, for channel
membership, each network session's `RPL_WELCOME`. Networks are not hot-reloaded,
so there is no `/reload` trigger (restart applies TOML changes — unchanged dave
behavior).

Network lifecycle — blind, idempotent, parse-free:

1. For every enabled bouncer network: send `network create -addr <host[:port]>
   -name <name> -nick <nick> -realname <real>`. If soju answers that the
   network already exists, ignore the error and **do not** push SASL or
   connect-commands (they persist in soju's DB from the first creation; this
   keeps every dave boot invisible to the upstream — no forced reconnects).
2. If the network was freshly created: push `sasl set-plain -network <name>
   <user> <pass>` when the TOML has SASL credentials, and push
   connect-commands (below) when set.
3. Orphan deletion: once per admin-session connect, query `network status` and
   parse the reply (the ONLY BouncerServ output dave parses — isolated in one
   function so format drift is a one-file fix). A soju network whose name does
   not correspond to ANY network in TOML (any mode, any enabled state) is an
   orphan and is deleted with `network delete <name>` — WARN-logged before the
   command is sent. Everything else is deliberately left alone so a temporary
   `bouncer = false` flip or a disabled network never destroys stored
   credentials; a network present in TOML but not in bouncer mode, or present
   but disabled, gets a WARN ("soju side left as-is") instead.
4. TOML edits to SASL/connect-commands for an *existing* soju network are not
   detected (soju already has the values; dave has no journal). Escape hatch:
   `[soju] force_reconcile = true` — on the next startup dave pushes SASL and
   connect-commands for every bouncer network regardless of creation state.
   One restart with the flag set, then remove it.

Channel lifecycle — no parsing at all:

- Added channels: dave's existing `RPL_WELCOME` join loop JOINs them; soju
  stores what its client joins. Nothing new.
- Removed channels: after each bouncer session's `RPL_WELCOME` (join burst
  settled — a fixed grace timer, default 15s, re-armed per reconnect), dave
  diffs girc's channel state (which mirrors soju's stored channel list) against
  `bot.Network.Channels` and sends `channel delete <chan>` for extras via the
  admin session, WARN-logged. Note `channel delete` leaves-and-forgets upstream,
  which is exactly the TOML-removal semantic. TUI `/part` already propagates
  naturally (a downstream PART makes soju forget the channel).
- Channel keys: dave's keyed joins carry the key downstream; soju stores it
  with the channel. Covered by the same paths.

Command plumbing: a small `BouncerServ` client (send command, collect NOTICE
replies until the service goes quiet or a timeout elapses, return them). Error
tolerance: creation/existence conflicts are expected and ignored selectively;
unexpected failures are WARN-logged with the reply text and never fatal to the
sessions.

### Replay guard (ignore-all)

soju's backlog behavior (all verified in source): messages that arrived while
no downstream client was connected are replayed on reconnect — PRIVMSG/NOTICE
only, capped at **4000 per target** (`backlogLimit`), wrapped in an IRCv3
`batch chathistory`, each message carrying its original `@time=` tag. girc does
not request `draft/chathistory`, so auto-backlog is ON.

- Placement: first statement of the PRIVMSG `AddBg` handler
  (`irc_handlers.go:154`) before `handleChanMessage`. Replayed → return
  immediately. The `ALL_EVENTS` logger has already recorded the row (it fires
  for every event), so replayed messages still land in `irc_logs` with their
  original timestamps — this is the gap-filling win.
- Predicate `isReplayedEvent(event, connectedAt)`:
  1. Primary: `event.Tags["batch"] != ""` — exact, spec-clean; only soju-style
     backlog sets it.
  2. Fallback (defense in depth, LOGGED when it trips): `event.Timestamp` is
     set (server-time cap, negotiated by default) and predates the session's
     connect instant by more than a small skew allowance (2s). Live messages
     carry ~now timestamps; anything materially older than the session cannot
     be live.
  On direct connections neither condition ever fires (ircds don't batch; live
  @time ≈ now) — the guard is a structural no-op there.
- Cost: one map lookup + one time comparison per PRIVMSG, inside the
  already-spawned per-event goroutine. A 4000-per-channel × N-channel replay
  burst costs the same per message as today's live traffic — no new buffering,
  no allocation on the hot path, no rate assumptions.
- Burst behavior is today's behavior, documented: `ALL_EVENTS` is an inline
  handler doing a **non-blocking** enqueue (`select/default` with a drop
  counter, irclog.go) — under an extreme burst the 10k log buffer can overflow
  and rows drop (counted as before). Backlog replay narrows deploy-window log
  gaps but cannot guarantee zero gaps at burst rates. Replayed PRIVMSGs still
  spawn their (short-lived) `AddBg` goroutine — unchanged goroutine churn.
- Queries are out of scope: dave dispatches channel PRIVMSGs only and does not
  log non-`#` targets today — unchanged.

### GameSurge / non-SASL networks (`connect_commands`)

New per-network TOML field `connect_commands` (array of raw IRC lines, validated
as non-empty strings, quoted as one line each):

- **Direct mode** (new dave capability, parity with bouncer mode): sent once
  after registration completes — hooked where `RPL_WELCOME` fires, sent via the
  client's raw send with the existing throttle semantics, re-sent on every
  reconnect (matching soju's semantics: connect-commands fire on each upstream
  registration).
- **Bouncer mode**: pushed to soju at network creation (and under
  `force_reconcile`) as `network update -connect-command "<line>"` — one flag
  instance per line. TOML removals are only applied via the
  `force_reconcile` path, where a fully-removed list sends the empty-string
  clear form before pushing the (now empty) set.
- GameSurge specifics: no SASL upstream, so `sasl set-plain` is simply not
  pushed when the TOML block is absent; AuthServ is addressed with its hostmask
  (`PRIVMSG AuthServ@Services.GameSurge.net :AUTH <user> <pass>`). soju replays
  connect-commands on every upstream registration (verified: stored on the
  network, max 20). WHOX/extended-join degradation on GameSurge is identical to
  today's direct behavior (deferral TTL expiry → first-interaction resolution).

### Offline visibility

soju auto-away only (owner decision, no dave code): with the network's
`auto-away` at its default (enabled), soju sends `AWAY :Auto away` upstream
when dave's sessions for that network are all gone and clears it on reconnect.
Visible via WHO/WHOIS/away-notices; nobody in-channel is proactively notified —
accepted; IRC has no broadcast mechanism.

### Direct-connection parity checklist

- `[soju]` absent: no admin session, no new code paths, guard predicate never
  fires, config validation inert. Diff surface is limited to: the new guard
  (inert), the `connect_commands` direct-mode sender (inert unless set), and
  config plumbing.
- `bouncer = true` on one network does not affect other direct networks.

## Testing

- **Replay guard**: table-driven unit tests on the predicate (batch tag set /
  empty / absent; timestamp fallback trips, skew boundary, live-now passes,
  zero-timestamp passes; direct-mode shape produces no trips). Handler-level
  test: synthetic batch-tagged PRIVMSG through the registered handler → no
  `handleChanMessage` dispatch (no trigger/mention effects), log enqueue still
  recorded. Also: guard ordering vs `Echo` events (echo-message is never
  negotiated by girc — pin that assumption with a comment, don't test girc).
- **Config**: `createTestConfigDir` table tests — `[soju]` parse + defaults;
  `bouncer = true` without `[soju]` rejected; name containing `/`/`@` rejected;
  missing pass rejected; `connect_commands` validation; ApplyDefaults for new
  fields; disabled network with `bouncer = true` inert.
- **Connection wiring**: bouncer network builds girc.Config with soju
  host/port/SASL username `<user>/<name>@bot`; direct network byte-identical
  config to today; rotation skipped in bouncer mode.
- **Admin module**: command builders (create/sasl/connect-command/delete/
  channel delete) render exact BouncerServ lines; reply parser against fixture
  `network status` NOTICE texts (incl. multi-network and weird names);
  reconciliation decisions table (fresh create pushes creds; existing network
  doesn't; orphan deleted with WARN; direct-mode leftover left with WARN;
  force_reconcile pushes everything); admin-session-down queueing flushes in
  order on reconnect.
- **Channel diff**: fake girc state (channels in soju, subset in TOML) →
  exactly the extra channels deleted, keyed channels handled, nothing sent when
  equal.
- **connect_commands direct mode**: after RPL_WELCOME the lines are sent in
  order, once per registration, through the throttle path.
- Full suite `go test ./...`, `go vet ./...`, `go fmt ./...` per house rules.

## Phases (details live in the implementation plan)

1. **Replay guard** — predicate + handler wiring + tests. Land first; inert on
   direct connections; prerequisite for any bouncer use.
2. **Config plumbing** — `[soju]`, `bouncer`, `connect_commands`, validation,
   ApplyDefaults, config.toml reference docs (house convention).
3. **Bouncer-mode connection wiring** — `ircClient()`/`startClient()` changes.
4. **soju admin module** — admin session, BouncerServ client, reconciliation,
   orphan deletion, channel diff.
5. **connect_commands direct-mode sender** — also standalone-useful.
6. **Live migration checklist** — one network at a time, verify per checklist
   below, then flip the rest.

## Risks / open items

- **BouncerServ output format drift**: only `network status` is parsed; parser
  is fixture-tested and isolated. A soju upgrade may require a parser touch.
- **WHOX through soju**: verified compatible on paper (canonical WHOX field
  order, token echo, cache-safe account handling); must be confirmed live
  during migration — dave logs `method=` resolution fields to check against.
- **soju is a SPOF for IRC presence**: same blast radius as dave's process
  today; mitigated by systemd `Restart=always` + the Prometheus listener.
- **Deletion safety**: every delete is WARN-logged with the network name before
  the command; orphan deletion only fires for names matching no enabled bouncer
  network in TOML; direct-mode leftovers are preserved by design.
- **Channel-diff grace timer**: if soju's join burst takes longer than the
  grace period, extras could be deleted before girc sees them (then re-added by
  nothing — they're not in TOML, which is correct) or missed until the next
  reconnect (also correct eventual state). The timer trades latency for
  simplicity; 15s default, configurable only in code.
- **Rate reality**: no dave-side change may assume message rates; the guard and
  admin module add per-message O(1) work or per-command (rare) work only.

## Appendix: soju deployment runbook (server X, ops side)

```
# /etc/soju/config — the whole file
hostname irc.example.com
listen ircs://0.0.0.0:6697
listen unix+admin:///run/soju/admin        # sojuctl, used once for user creation
listen http+prometheus://localhost:8461
tls /etc/soju/cert.pem /etc/soju/key.pem
db postgres "host=/run/postgresql dbname=soju"   # preferred: shared instance
message-store db                                  # default; backlog + chathistory
message-expiry 60d
auth internal
```

One-time bootstrap (unavoidable — dave must authenticate before it can admin):
`sojuctl create-user dave -password '<pass>'`. Everything else dave creates
itself through its own connections on first boot. systemd `Restart=always`.
Current soju release required (Codeberg is upstream; GitHub is a mirror) for
`message-expiry` and the ISUPPORT behaviors girc v1.1.3 compensates for.

Migration order: deploy soju → start dave with `bouncer = true` on the test
network only → verify live (WHOX 354s in resolution logs, extended-join
accounts, backlog logged-but-not-dispatched by deploying mid-conversation on
purpose, queue delivery, trigger dispatch, `network status` shows the created
network with SASL) → flip remaining networks → delete absorbed SASL blocks
from TOML comments or leave them (they are upstream creds dave pushes; keep
them in TOML — dave is the source of truth).
