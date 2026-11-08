# llama-server Model Gate Design

Status: DESIGN APPROVED (in conversation, 2026-11-08) — ready for
implementation planning.
Date: 2026-11-08

## Problem

The production llama-server runs as a multi-model router with
`max_instances = 1` (the GPU holds one model at a time):

```
$ curl localhost:8000/props
{"role":"router","max_instances":1,"models_autoload":true,...}
```

dave's `parallel` is a per-service execution knob, and the model is a
per-command cascade — dave has no notion of "which model is currently
resident on the server". With `parallel = 2`, a command for model B is
dispatched while model A is mid-generation; the router unloads A, loads
B, finishes B, reloads A, re-prefills A, and finishes A. Costs observed
and derived:

1. **A's user eats a long mid-stream stall** — B's whole generation plus
   two model loads plus A's re-prefill, with A's stream silent
   throughout.
2. **A's KV cache is destroyed** — A's full context (tens of k tokens)
   is re-prefilled from scratch, which is exactly the slow-prefill
   regime behind the historical stream-death incidents.
3. **Lurking turn-kill risk** — `streamtimeout` only resets on real
   chunks, and llama.cpp's SSE keepalive comments are deliberately
   filtered out (ssecomments.go), so they do NOT reset the idle timer.
   A swap-stall longer than `streamtimeout` kills A's turn outright.
   It has survived so far only because stalls happened to be shorter.
4. **GPU thrash** — two full model loads per collision.

Same-model concurrency (the intended use of `parallel = 2`) works fine:
the router interleaves requests to one resident model natively.

### Why not llama-swap

llama-swap's swap policy is the right one (FIFO — "not swap in the
middle of a request", per the maintainer, who also admits
high-concurrency paths are under-tested; issue #588 exists regardless).
But it does not remove the wait — one model in memory is physics — it
relocates the wait **inside B's HTTP request, silently**, where dave
cannot distinguish "queued" from "loading" from "slow prefill", and
where it collides with dave's per-request `timeout`/`streamtimeout`
with zero IRC feedback mid-wait. A dave-side gate gives the same
no-preemption policy plus visible wait notices and control. Deferred
as a possible future front-door policy for non-dave clients.

## Scope

- **In scope:** every dave-originated turn on `type = "llama"` services
  (chat turns in all four variants, streaming and not; generators;
  async continuations; legacy completions; compaction summarizer calls).
- **Accepted residual:** the owner's rare direct webui requests can
  still swap a model mid-turn (owner: "not been a real issue").
- **Non-goal (future):** img-mcp enhancement/vet LLM calls hitting the
  same server — planned for another day; when that lands, img-mcp will
  need its own admission or a shared front door.
- **Non-goal:** llama-swap migration.

## Design

### Gate: `modelGate.go` (mirrors `sessionGate.go`)

Package-level registry `sync.Map` keyed by **normalized baseurl** →
`*modelGate` (normalization: lowercase scheme+host, port kept, trailing
slashes and a trailing `/v1` trimmed — so `host:8000/v1/` and
`host:8000/v1` are one gate). Gate state under one mutex: `capacity`
(max distinct resident models), `resident map[string]int` (model →
in-flight turn count), and a FIFO waiter queue.

`acquireModelSlot(ctx, baseurl, model) (release func(), ok bool)`:

- **Admit** if `model` is already resident (same-model sharing —
  `parallel = 2` semantics preserved) or `len(resident) < capacity`.
- Otherwise enqueue as a waiter.
- **Wake rules on release** (walk the queue in FIFO order):
  same-model waiters may pass any model already resident — they consume
  no capacity and never delay the queue head; a **new** model admits
  only at the head of the queue and only while capacity allows. This
  keeps the head starvation-free even under continuous same-model
  traffic (the theoretical starvation of a queued new-model waiter is
  accepted at IRC rates; strict head-first admission bounds it).
- Release is idempotent; the whole turn holds the slot via `defer`
  (**whole-turn hold** — one swap per collision, and llama.cpp keeps
  the session's KV cache warm across the turn's tool-loop iterations:
  per-iteration prefill collapses to just the new tokens).
- **Ctx-cancelled waiters abort silently** (the queue already told the
  user; mirrors the session gate).

Entry counts are never deleted from the registry map (bounded by the
number of distinct servers, i.e. effectively fixed); the gate is keyed
by baseurl, so multiple services pointing at the same server share one
gate.

### Liveness valve (deliberate reversal of an earlier "skip it")

The session gate's documented wedge applies here with the same shape:
waiter W (model B) is the per-channel **delivery head** producing
nothing while blocked at this gate; holder H (model A) runs behind it,
streams into its 200-message output buffer, fills it, blocks
mid-turn — and only W's completed delivery can drain it. H never
releases; the channel wedges until `^stop`. Same fix, same calibration:
per-waiter limit = `max(10m floor, 3× the command's turn timeout)`
(reuse/extract the session-gate helper). A misfire costs one visible
"try again" notice; a wedge costs a jammed channel.

**Difference from the session valve:** the model gate is acquired
inside `runTurn()`, i.e. AFTER `chat()` persisted the user message —
unlike the session gate, which waits before persistence. So a
valve-aborted turn leaves the user row in history, exactly like a turn
that failed on an API error today; the notice says "please try again",
not "resend". Retrying is the normal post-error flow; sessions tolerate
repeated user rows.

### Capacity detection: `/props`, cached per baseurl

Lazy probe `GET {baseurl minus "/v1"}/props` (Bearer key when set, 2s
timeout, fail open — same philosophy as `load_notice`). Predicate:
`role == "router"` && integer `max_instances >= 1` → capacity N
(generalized: at most N distinct models in flight concurrently).
Anything else — old builds, plain single-model llama-server, llama-swap
(whose `/props` needs `?model=`), error, timeout — means ungated,
exactly today's behavior, which is correct for single-model servers
(no cross-model collision is possible there).

- Probe **success is cached for the process lifetime**; `/reload`
  clears the cache (a server-side `max_instances` change is picked up
  on the next `/reload` or restart). Probe **failure is cached
  briefly** and retried later — a temporarily-down server must not
  permanently disable the gate.
- **Engagement is per requesting turn's service**: a turn participates
  iff its service is `type = "llama"` (or carries a `models_max`
  override — below). Once engaged, it uses the shared per-baseurl gate;
  a non-llama service pointing at the same URL simply doesn't
  participate (documented edge; under-serialization in that direction).
- **Staleness errs safe:** if dave's cached capacity is higher than the
  server's real one (external swap sources, owner webui), dave
  under-serializes to the status quo; if lower, it over-serializes
  briefly. Never worse than today.

### Config surface

Service-level `models_max` (int, snake_case TOML as usual):

| value | meaning |
|---|---|
| 0 / unset | **auto-detect** via `/props` (default) |
| > 0 | forced capacity — the escape hatch for servers where `/props` can't answer (llama-swap-style proxies, exotic gateways) |
| -1 | gating disabled for this service |

- Service-level only, no command cascade — it is a property of the
  **server**, not the command.
- Explicit overrides **pin** the shared gate's capacity; a probe never
  overwrites a pinned gate. If two services on the same baseurl pin
  different values, the stricter (smaller) wins with a WARN.
- Model identity for the gate key is the **raw `cfg.Model` string**.
  Known limitation: two commands naming the same physical model
  differently (router aliases) over-serialize — safe direction,
  documented; canonicalization via a cached `/v1/models` alias map is a
  possible refinement, not v1.

### Hook points (three, each ordered before its load-notice probe)

1. **`runTurn()` top (aiCmds.go)** — acquire BEFORE
   `cr.maybeNotifyModelLoad()`, so the "model loading" notice only
   fires once admitted, when the load is actually imminent (probing
   while still queued behind another model would emit a misleading
   notice). Covers all four variants, ephemeral generator turns, async
   continuations. No interaction with `llamastream.go`: the conv-id
   header is attached per-request inside `runTurnStream`, after
   admission — a queued turn has sent nothing.
2. **`completion()`** (legacy completions path) before its
   `sendModelLoadNotice` block.
3. **`callSummarizer()`** (compaction) after it resolves the Service —
   an ungated summarizer call could itself swap a model mid-turn.
   Acquired per call (one request), not per compaction; valve/abort
   flows into compaction's existing error handling (session unchanged).

**Ungated:** `/v1/models` and `/props` probes (GETs, load nothing),
MCP tool calls, `/tokencount`.

Composition with the queue: the per-service execSlot (`parallel`) is
untouched; the model gate nests inside it. The waiting turn has already
been delivered ("▶ Processing…"), so the gate's own notice explains the
silence.

### Notices + logging

- ~~`[queue] model_wait` — sent when a turn is about to block on the gate~~
  (RETIRED post-implementation — see the addendum: turn-side notices
  display stale because they sit in the delivery output buffer until the
  wait is over; replaced by enqueue-time suffix composition).
- `[queue] model_wait_timeout` — on valve fire, `{wait}` = actual
  elapsed wait. Default: `⚠️ {nick}: gave up after {wait} waiting for
  the model — nothing was sent, please try again.`
- Cancelled waiters stay silent.
- Both are notices.toml entries with `setNoticesDefaults()` defaults,
  reloadable, documented in the reference block.
- Logging mirrors sessionGate: INFO on wait start (server, model,
  resident set), INFO on acquired-after-wait (duration), WARN on valve
  fire.

## Testing

- Gate unit family (`modelGate_test.go`): same-model shares; different
  models serialize; FIFO order; same-model pass does not starve the
  head; new-model admission blocked at head; ctx-cancel aborts silently
  and later release doesn't wake it; valve fires at limit; release
  idempotent; capacity N=2 admits two distinct models and queues the
  third; registry keyed by baseurl shared across services.
- `/props` parse table: router shape with max_instances 1/2;
  non-router; missing fields; malformed JSON; error/timeout fail-open;
  override values 0/-1/positive; pin-vs-probe precedence; conflicting
  overrides → stricter + WARN.
- Wiring: `runTurn` holds across tool iterations (two-model contention
  test); gate acquired before `maybeNotifyModelLoad` (order pinned);
  `completion()` gated; `callSummarizer()` gated; the ~~`model_wait`
  notice~~ (retired — see addendum) queue-suffix composition on wait
  and not when free; valve notice content; cancelled
  waiter silent; non-llama service ungated.
- Config: `models_max` parsing/defaults/cascade absence;
  services.toml reference updated (config doc convention).
- Full `go test ./...`, `go fmt ./...`, `go vet ./...`.

## Documentation

- Architecture block comment atop `modelGate.go` (design notes: whole-
  turn hold rationale, wake rules, valve, wedge analysis) + AGENTS.md
  bullet.
- `config/services.toml` reference list + commented example entry.
- `docs/queue-and-sessions.md`: a "Model gating" section (parallel is
  per-service; the model gate serializes distinct models on
  single-model servers).

## Decisions resolved (2026-11-08 conversation)

1. **dave-side gate over llama-swap** — wait stays visible and
   controllable; server stays as-is.
2. **Whole-turn hold over per-request** — one swap per collision, KV
   warm across tool iterations; B may wait minutes (accepted, with
   notice). Per-request would thrash swaps and re-prefill per
   iteration (the slow-prefill incident regime).
3. **Auto-detect via `/props` primary**, `models_max` override as
   escape hatch.
4. **Valve included** — the session-gate wedge analysis applies
   verbatim; misfire is cheap, wedge is expensive.
5. **FIFO with same-model pass** — fairness for the head, sharing for
   same-model turns.
6. **Raw model string as gate key** — alias canonicalization deferred
   (over-serialization is the safe direction).

## Limitations

- External swap sources (owner webui, future img-mcp enhancement)
  bypass the gate; dave's resident set can drift stale — always in the
  safe (over-serializing) direction for gate decisions.
- Alias-named same-model commands over-serialize.
- Capacity > 1 is best-effort if the server's own eviction picks a
  victim dave still counts as resident (again errs safe).
- The gate cannot help with a genuinely wedged holder faster than the
  valve; holders terminate via their own timeout chain
  (per-request timeout × ≤20 tool iterations).

## Addendum (2026-11-08, post-implementation): queue-time notice composition

The turn-side `model_wait` notice was REMOVED after the first production
run surfaced a structural problem: with `parallel = 2`, a queued command
starts executing immediately and its turn-side notices go into the
delivery OUTPUT BUFFER, where they sit until the channel's delivery slot
opens — i.e. until the wait is already over. The notice was sent
truthfully at turn start and displayed stale ("waiting for X…" arrived
after X had finished). Any turn-side wait notice has this defect by
construction; a grace window cannot fix display-time staleness.

Replaced by enqueue-time composition on the SAME LINE as the queue
position notice (sent from the dispatch site via `client.Cmd.Reply` —
unbuffered, displays immediately):

- `bg_suffix` — appended when the item STARTED at enqueue
  (`EnqueueWithStart`'s authoritative flag: `enqueueItem` synchronously
  acquires the service slot and launches before returning). NOT a
  post-hoc "is the slot free" consult — that counts the just-enqueued
  item's own occupancy and suppresses the suffix in exactly the
  contended cases it exists for (parallel = 2 with one slot busy is the
  flagship scenario; caught in review).
- `model_suffix` ({model},{held}) — appended when the gate predicts a
  model swap for the command's model (`modelGateQueueSuffix`, advisory
  gate consult incl. capacity resolution).

Both are suffix templates in notices.toml; `model_wait` is retired.
`model_wait_timeout` (valve abort) is unchanged — "gave up after {wait}"
is true whenever it displays. Both the trigger dispatch and the mention
path (a queued continuation behind a predicted swap) compose the suffix;
the remaining no-suffix paths are rate-exempt direct-dispatch triggers
(never enqueue, so no queue notice at all) and position-0 turns (not
queued — they get the started message).
