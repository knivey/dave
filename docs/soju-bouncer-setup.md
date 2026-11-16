# soju Bouncer Setup & Migration Runbook

Operational guide for running dave behind a [soju](https://soju.im) IRC bouncer.
The design rationale lives in
`docs/superpowers/specs/2026-11-16-soju-bouncer-support-design.md`; this document
is the deployment + migration checklist. The dave-side feature is described in
AGENTS.md ("soju bouncer support").

## Why this layout

```
IRC networks (Libera, GameSurge, …)      fast machine Y                current server X (today's IP)
        ▲                                       │                              │
        │  TLS, still from X's IP               │  TLS ircs://X:6697           │
        └─────────────────────── soju ◄─────────┴──────────────────────────────┘
```

- **soju runs on the current server (X).** IRC networks keep seeing today's IP —
  cloaks, rate limits, and ban history are untouched.
- **dave runs on the fast machine (Y)** and connects to soju like a client: one
  TLS connection per configured network, plus one unbound "admin" session.
- Upstream connections live inside soju and persist 24/7, independent of dave
  restarts. Deploys stop producing reconnect storms; dave's nick, channels, and
  place in the network survive restarts.
- Messages that arrive while dave is down are stored by soju and replayed on
  reconnect. dave logs them (with their original timestamps — `irc_logs` stops
  losing deploy-window traffic) but never dispatches them: no trigger fires, no
  mention starts a turn, no matter how long the outage was.
- Offline visibility: when dave's sessions drop, soju marks the nick
  `AWAY :Auto away` upstream and clears it on reconnect. That is the only
  notification — IRC has no channel broadcast, and this is a deliberate owner
  decision.

## Server X: install soju

Use a current release (2025 or later — needed for `message-expiry` and the
ISUPPORT behaviors dave's girc version compensates for). Upstream is
[codeberg.org/emersion/soju](https://codeberg.org/emersion/soju) (GitHub is a
mirror); distro packages are fine if recent.

```
git clone https://git.sr.ht/~emersion/soju && cd soju   # or the codeberg mirror
go build -o /usr/local/bin/soju ./cmd/soju
go build -o /usr/local/bin/sojuctl ./cmd/sojuctl
```

## Server X: `/etc/soju/config`

This is the whole file. soju keeps users/networks/channels in its database —
there is deliberately NO static user, network, or channel config here; dave
manages all of that at runtime (see below).

```
# Public name IRC networks see in dave's usermask (and in soju's own messages).
hostname irc.example.com

# What dave connects to.
listen ircs://0.0.0.0:6697

# Admin socket: sojuctl. Used exactly once, to create the dave user.
listen unix+admin:///run/soju/admin

# Optional but recommended: bouncer metrics.
listen http+prometheus://localhost:8461

# Only needed if IRC networks check ident for dave (parity with a direct bot).
# listen ident://

tls /etc/soju/cert.pem /etc/soju/key.pem

# Bouncer state. postgres preferred (shared instance, handles the per-message
# transactional write path well); sqlite3 works too.
db postgres "host=/run/postgresql dbname=soju"

# Message store: default is `db`. Enables the backlog replay + CHATHISTORY
# that gives dave gap-free logs across restarts.
message-store db

# Prune the store so it doesn't grow forever.
message-expiry 60d

auth internal
```

Notes:

- soju's sqlite driver runs WAL + `synchronous=NORMAL`; with postgres, per-
  message transactions are simply normal operation. IRC message rates are not a
  concern on either.
- soju stores everything EXCEPT NickServ traffic (it refuses to log service
  messages that can carry passwords).
- `SIGHUP` reloads the cert/key and MOTD. The `listen`, `db`, and
  `message-store` directives require a restart.
- If you use sqlite, remember deleted messages don't reclaim space until
  `VACUUM` (soju deletes expired messages daily).

## Server X: TLS

dave refuses to talk to the bouncer in plain text by default (`[soju] ssl`
defaults to true), so server X needs a certificate dave trusts:

- **Simplest:** a Let's Encrypt certificate for `irc.example.com` (certbot or
  acme.sh; reload soju on renewal via a deploy hook — soju re-reads the cert on
  SIGHUP).
- **Private-network alternative:** WireGuard/Tailscale between the machines and
  `listen irc+insecure://<tailscale-ip>:6697`, with dave's network configured
  `ssl = true` against a cert it skips verification for
  (`insecure_skip_verify = true`). Acceptable only inside a private tunnel.

## Server X: systemd

```
# /etc/systemd/system/soju.service
[Unit]
Description=soju IRC bouncer
After=network-online.target postgresql.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/soju -config /etc/soju/config
Restart=always
RestartSec=5
User=soju
Group=soju
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
```

soju is now the SPOF for IRC presence — the same blast radius dave's process
had before, but the Prometheus listener (`http://localhost:8461/metrics`) and
`Restart=always` cover it.

## Server X: one-time bootstrap

The ONLY manual step in the entire setup — dave must authenticate before it can
admin anything, and user creation needs the admin socket:

```
sojuctl create-user dave -password '<soju-pass>'
```

Everything else — networks, channels, SASL credentials, connect-commands —
dave creates itself through its own connections on first boot.

## Fast machine Y: dave config

`config/config.toml`:

```toml
[soju]
host = "irc.example.com"
port = 6697                    # default 6697
ssl = true                     # default true
insecure_skip_verify = false
user = "dave"
pass = "<soju-pass>"
# force_reconcile = false      # see "Editing credentials" below
```

Per network that should use the bouncer:

```toml
[networks.libera]
nick = "dave"
bouncer = true                 # dial [soju] instead of [[networks.libera.servers]]
# keep the servers list! its first entry is recorded as the upstream address.

[networks.libera.sasl]
user = "dave"                  # UPSTREAM Libera credentials — dave pushes these
pass = "<libera-pass>"         # into soju; they are never sent to soju itself
```

Rules (validated at config load):

- `bouncer = true` requires `[soju]`, and the network needs ≥1 `[[servers]]`
  entry — the first one becomes the recorded upstream address (`host`, or
  `host:port` when the port isn't 6697). soju connects upstream with TLS.
- Network names used with `bouncer = true` must not contain `/` or `@` (they
  ride inside dave's SASL username to soju). Avoid spaces too — soju command
  quoting round-trips them, but plain names keep every management command
  readable.
- Networks without `bouncer = true` connect directly exactly as before; nothing
  about them changes.

GameSurge-style networks (no SASL; services auth after registration):

```toml
[networks.gamesurge]
nick = "dave"
bouncer = true
connect_commands = [
  "PRIVMSG AuthServ@Services.GameSurge.net :AUTH <user> <pass>",
]
```

`connect_commands` works in BOTH modes: direct mode sends the lines after every
registration; bouncer mode pushes them into soju, which replays them after
every upstream registration. (No `[networks.X.sasl]` block = nothing pushed —
soju connects to GameSurge without SASL, exactly as dave would directly.)

## What dave does automatically

On startup with at least one enabled `bouncer = true` network:

1. Opens the unbound admin session (`dave`) — it talks only to BouncerServ.
2. Queries `network status` and reconciles:
   - Networks missing from soju are **created** with their connect-commands
     folded into the `network create` itself, then their SASL credentials are
     pushed (`sasl set-plain`) — first boot only.
   - Existing networks get **no** pushes: soju already has the values, and
     pushing would force an upstream reconnect (visible to IRC networks).
     Every routine dave boot is invisible upstream.
   - soju networks whose name matches no TOML network are **deleted**
     (WARN-logged first) — removing a network from TOML and restarting removes
     it from soju, credentials included. Names present in TOML but direct-mode
     or disabled are preserved with a WARN.
3. Starts one girc session per bouncer network, SASL
   `dave/<network-name>@bot`.
4. ~15s after each network's registration, diffs soju's joined channels
   against the TOML channel list and **PARTs** anything soju remembers that
   TOML no longer lists. `TUI /join` and `/part` propagate naturally (soju
   stores what its clients join and forgets what they part).

Removing a CHANNEL: delete it from `[networks.X.channels]` and restart dave.
Removing a NETWORK: delete the whole `[networks.X]` block and restart. That's
the whole story — soju converges on startup.

## Editing credentials later

SASL passwords and connect-commands are pushed to soju only at network
creation. To change them for a network that already exists in soju:

1. Set `[soju] force_reconcile = true` in dave's config.
2. Restart dave — it re-pushes SASL + connect-commands for every bouncer
   network (each `network update` bounces that one upstream connection, which
   is why this isn't done every boot).
3. Remove the flag (leave it set and it re-pushes on every restart — harmless
   but pointless upstream churn).

## Migration checklist (do this per network, test network first)

1. Stand up soju on X (above), create the dave user.
2. Set `[soju]` in dave's config; set `bouncer = true` on the test network
   only; restart dave on the fast machine.
3. Verify, in order:
   - Admin session connects; `sojuctl user run dave network status` shows the
     created network with its upstream address, and dave's log shows the
     INFO/WARN command trail (`network create`, `sasl set-plain`).
   - The bouncer session connects; channels join; `irc_logs` keeps recording.
   - Account resolution still works: dave's logs show WHOX 354 replies landing
     and `method=` resolution fields populated (soju relays WHOX; this is the
     one live-verification item the code review flagged).
   - **Backlog is logged but not dispatched:** deploy dave mid-conversation on
     purpose. The restart must produce: no old triggers firing, no replayed
     mentions answered, and the missed window present in `irc_logs` with
     original timestamps.
   - **Second boot is invisible:** restart dave again and confirm the log
     shows no `network create`/`network update` attempts (the reconciliation
     found everything in place).
   - Queue delivery, triggers, and mentions work normally.
4. Flip the remaining networks one at a time (config edit + restart each).
5. Keep the upstream SASL blocks in TOML — dave is the source of truth and
   re-pushes them if a network is ever recreated. Remove only the old
   `[[servers]]` rotation entries' significance conceptually: they stay in the
   file (the first entry records the upstream address).

## Failure modes

| Symptom | Meaning | Action |
|---|---|---|
| `soju: network status query failed — skipping reconciliation` | Admin session couldn't get an authoritative listing (soju down / socket died) | None — dave retries on the next admin reconnect; network sessions unaffected |
| WARN `soju: deleting orphaned network` | A soju network no longer in TOML was removed | Expected when you delete a network block; investigate if you didn't |
| WARN `soju: network present in TOML but not bouncer mode` | Leftover soju state for a direct-mode/disabled network | Informational; preserved by design |
| WARN with BouncerServ reply text after a command | soju rejected or answered a management command (e.g. >20 connect-commands, unknown channel) | Read the reply; fix the TOML; `force_reconcile` if it's a credential push |
| dave connects to soju but SASL fails | Wrong `[soju]` user/pass | Fix TOML, restart |
| Replayed old messages visible in logs but nothing dispatched | The replay guard working as designed | None |
| Nick shows away to WHOIS while dave runs | dave's sessions to soju are down (machine Y offline, dave crashed) | soju holds presence; fix dave |

## Deleting the bouncer setup

Remove `bouncer = true` from all networks (they return to direct connections on
restart), then on server X: `sojuctl user delete dave` (asks for a confirmation
token) — that drops every stored network, channel, and credential. The message
store follows `message-expiry`.
