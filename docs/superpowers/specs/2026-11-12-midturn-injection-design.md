# Mid-Turn Message Injection — Design

Date: 2026-11-12
Status: approved approach, spec stage
Incident that motivated this: 2026-11-12 09:27–09:36 local (`gay`/#gay, session 3161,
api log `gay__gay_user429_3161.jsonl`)

## Problem

A user message arriving while dave's agentic tool loop is still running is queued
as a whole second turn. Three failure modes follow, all visible in the incident:

1. **The running turn is blind to the new input.** The model kept executing a plan
   the user had already superseded (generated another image after being told
   "just make it look like this <url>"), burning minutes of GPU and queue time.
2. **Confusing queue choreography.** The second message produced
   `⏳ Queued (position 1) — already processing in the background`, then 24s later
   `▶ Processing your request (waited 24s)...` immediately followed by
   `⚠️ ⏳ still working on your previous message...` (the session-gate wait notice,
   buffered and displayed after the started line). To a user this reads as the bot
   hallucinating.
3. **Phantom "Processing" announcements.** Async image jobs that completed during
   the second turn each enqueued a `deliverAsyncResult` queue item; the turn's own
   inline post-turn loop delivered the results first (first-wins by design), and
   the stale queue items then each emitted `▶ Processing your request (waited
   2m9s/1m42s)...` before discovering they had nothing to do — followed by
   silence. The user asked "is he alright".

Root causes:

- (a) No mechanism to hand a new message to a live turn.
- (b) `onAsyncJobCompleted` enqueues delivery items with plain `Enqueue`, so they
  inherit the generic user-facing started notice
  (`▶ {nick}: Processing your request (waited {wait})...`) which the queue sends
  unconditionally for any item that waited >1s — before the item's `Execute` can
  know it will no-op. See "Bounded side-fix".

## Goals

- A same-session continuation (mention) arriving while its session's turn is
  running is absorbed into that turn: persisted and sent as a plain `RoleUser`
  row after the pending tool results of the current round, so the next API
  request carries it and the model can pivot immediately.
- The user gets one immediate, quiet receipt notice at absorption time.
- Nothing absorbed is ever lost, including messages absorbed during the model's
  final response or racing turn shutdown.
- Eliminate the phantom started notices from async-delivery queue items.

## Non-goals

- Absorbing fresh trigger commands (`=grk ...`): they complete the old session
  and start a new one; they keep today's queue-a-new-turn behavior.
- Absorbing into async-delivery continuation turns (`deliverAsyncResult`) or
  ephemeral/generator turns: only `chat()` (the user-turn path) opens a mailbox.
- Interrupting an in-flight API call: absorption happens at round boundaries
  only. A message arriving while the final response streams is handled by a
  post-turn continuation turn, not by cancelling the stream.
- The `fetch → view_image → view_image` re-viewing churn (separate follow-up
  task, owner-acknowledged).

## Owner decisions (locked)

| Question | Decision |
|---|---|
| Eligibility | Same-session continuations (mentions) only |
| User feedback | Subtle receipt notice, e.g. `📎 {nick}: added to the running turn` |
| What the model sees | Plain user row — no marker, no prefix; position after tool results is the signal |
| Configurability | None — always on; no knob |

## Feasibility evidence

The exact wire shape this feature needs — `[..., assistant(tool_calls), tool,
..., user]` as one request — is already in production via the `view_image`
injection path (`executeToolCalls` flushes `pendingInjections` after all tool
results of the round). In the incident session's api log, ~30 requests on
`orcarouter/Qwen3.8-27B-Uncensored-GGUF:IQ4_XS` contain user-after-tool
sequences, with zero error events. All historical Jinja template rejections in
prod logs are `System message must be at the beginning` (the already-fixed
consecutive-system merge); no provider in the fleet has ever rejected tool/user
ordering. The Responses-path delta builder already carries injected user rows
(`TestToolLoopMsgsToInputItemsCarriesInjectedUser`). Consecutive user rows are
also a shipped shape (multiple `view_image` calls in one round).

## Design

### 1. Turn mailbox (`midturn.go`, new file)

A package-level registry of at most one open mailbox per session:

```go
type turnMailbox struct {
    mu      sync.Mutex
    open    bool
    pending []string   // raw message texts, arrival order
}
var turnMailboxes sync.Map // sessionID (int64) -> *turnMailbox
```

Operations (all race-freedom lives in one mutex):

- `openTurnMailbox(sessionID) *turnMailbox` — creates/opens; called by `chat()`.
- `(*turnMailbox).push(msg string) bool` — locks; if `!open` returns false;
  otherwise appends and returns true. True means the caller (the turn) has taken
  on the obligation to drain-or-persist that message.
- `(*turnMailbox).drain() []string` — locks; returns and clears pending;
  mailbox stays open.
- `(*turnMailbox).closeAndDrain() []string` — locks; sets `open = false`;
  returns pending. After this, `push` fails (dispatch falls back to enqueue).
- `turnMailboxFor(sessionID) *turnMailbox` — lookup only (dispatch side).

A `push` that succeeds is guaranteed to be seen by a subsequent `drain` or by the
`closeAndDrain` that flipped `open` — there is no interleaving where a message is
accepted and then vanishes, except turn cancellation (handled in §4).

Opening a mailbox for an already-open session cannot happen: `chat()` holds the
session turn gate before opening, and only `chat()` opens. If a stale entry is
found (defensive), opening replaces it after closing the old one.

### 2. Dispatch absorption (`handleMention`, irc_handlers.go)

After the existing session + config resolution (post line ~445, before
`EnqueueWithStart`):

```go
if mb := turnMailboxFor(session.ID); mb != nil && mb.push(msg) {
    n := getNotices()
    client.Cmd.Reply(event, expandNotice(n.Mentions.Absorbed,
        map[string]string{"nick": event.Source.Name}))
    return
}
```

- The reply is sent directly to IRC (unbuffered, immediate — same pattern as
  `executionWaitNotice`), NOT through a queue item.
- All eligibility machinery has already run at that point: user resolution, ban
  check, `ContextExists`, `checkRate`, mention-tracker reset, active-session and
  config lookup. Absorbed messages are therefore subject to the same rate limit
  and ban gates as queued ones.
- On push failure the function falls through to today's enqueue path verbatim.
- Absorbed messages create no queue item: no Queued notice, no started notice,
  no delivery-slot occupancy, no model-gate wait (the running turn already holds
  the gate for its model).
- The trigger path (`handleTrigger` → fresh sessions) is untouched.

### 3. Turn-side drain (round boundary)

Inside `executeToolCalls`, immediately after the existing `pendingInjections`
flush (`aiCmds.go` ~1629):

- Drain the session's mailbox (no-op when none is open — ephemeral turns and
  sessions without a running `chat()` never have one).
- For each drained text, run the same user-message preprocessing `chat()` runs
  today (`cfg.DetectImages` → `detectImageURLs` → `buildImageMessage` with
  live-turn budget via `countContextImages(turn.Messages())`, sequential across
  the drained batch), then `turn.Add(...)`.
- Row order within the round: tool results → `view_image` injections → absorbed
  user messages. The user message last = newest input closest to the generation
  prompt.
- Preprocessing is extracted from `chat()` into a shared helper so both call
  sites use one implementation.

Deviation from today's preprocessing semantics, deliberate: today a limit
failure (`Images.LimitReached` / `Images.PartialLimit`) or a
`buildImageMessage` error makes `chat()` warn and **drop** the message
unpersisted. An absorbed message cannot be dropped — the user already received
the receipt. On those failures the drain sends the same warning notice through
the turn's output channel and persists the message as **text only**
(`cleanText`), matching pre-`detectimages` wire behavior. The model still sees
the message; images are just not attached.

All four turn-loop variants (chat completions × streaming, Responses ×
streaming) funnel through `executeToolCalls` (two directly, two via
`handleToolCallResponse`), so this single seam covers them all.

### 4. Lifecycle in `chat()`

- Open the mailbox right after acquiring the session turn gate (before
  persisting the turn's own user message). The gate guarantees single-writer;
  opening under the gate means the mailbox and the DB writes it feeds are
  serialized with everything else on the session.
- The primary turn and every continuation turn drain at their round boundaries
  (§3).
- Replace the existing post-turn inline loop's exit condition with a
  **quiescent-close loop**:

```go
for {
    absorbed := mb.drain()
    completedJobs, _ := getCompletedPendingJobs(runner.sessionID)
    if len(absorbed) == 0 && len(completedJobs) == 0 {
        if stragglers := mb.closeAndDrain(); len(stragglers) == 0 {
            break                       // quiescent and closed
        }
        absorbed = stragglers           // raced a push; process once more
        // mailbox stays closed; anything further enqueues normally
    }
    // persist absorbed (preprocessing per §3), inject job results as today,
    // reload history, newTurnContext, runTurn
}
```

  A push that races the close either lands in the returned stragglers (processed
  by one more cycle, after which the mailbox is closed) or fails (dispatch
  enqueues). Bounded: at most one extra cycle per race; sustained traffic keeps
  the loop alive the way a conversation does, one batched turn per drain cycle.
- `maybeAutoCompact` still runs after the loop, as today.

**Cancellation path (`^stop`, shutdown):** today a cancelled queued message is
dropped silently. An accepted push carries a drain-or-persist obligation, so
`chat()` defers a final `closeAndDrain`: if the turn's ctx is done and messages
remain, persist them as plain text-only `RoleUser` rows (no turn runs; they sit
in history for the user's next message) and send nothing (the queue already told
the user the turn was stopped). DB writes do not depend on the queue ctx, so
this is safe during shutdown racing.

### 5. Receipt notice

- New template `absorbed` in the `[mentions]` notices section.
- Default: `"\x0314📎 {nick}: added to the running turn\x0f"` (grey, quiet —
  visually subordinate to the ▶/⏳ family).
- `{nick}` is the only variable. `setNoticesDefaults` entry + reference-list and
  commented-block documentation in `config/notices.toml` per the config
  documentation convention.

### 6. Interactions with existing machinery

- **Session turn gate**: absorbed messages never wait on the gate (the holder
  takes them). The `turn_wait` notice remains only for the enqueue fallback
  (async deliveries, closed-mailbox races). The incident's
  `▶ Processing` → `⚠️ still working` double-notice disappears for
  same-session mentions.
- **Model gate**: the running turn holds its model gate for the whole
  (now longer) turn — consistent with the whole-turn-hold design; absorbed
  messages ride the same model, no new acquisition.
- **`^stop` / `StopCurrent`**: cancels the running turn; absorbed-but-undrained
  messages are persisted per §4. Absorbed messages have no queue items, so
  `CancelPending` has nothing of theirs to remove.
- **Async jobs**: unchanged. The turn staying alive longer means more results
  arrive while it can still consume them (inline window / inline post-turn
  loop), which shrinks the stale-delivery window that produced the phantom
  notices.
- **Compaction / clone / sesssite / website link**: absorbed rows are ordinary
  `RoleUser` rows; no special handling anywhere downstream.
- **`rateExemptCmds`**: mentions are not rate-exempt; `checkRate` gates
  absorption like any mention.
- **Ephemeral/generator and legacy `completion()` turns**: never open a
  mailbox; behavior unchanged.

## Bounded side-fix: silent async-delivery queue items

Separate from (and shippable before) the injection work:

- Add `Silent bool` to `QueueItem`; the async-delivery enqueue in
  `onAsyncJobCompletedWithMgr` marks its item silent; `runJob` skips the
  started notice when set.
- Rationale: the started notice fires from the delivery machinery before
  `Execute` can know the delivery will no-op (first-wins inline delivery,
  another delivery, boot recovery already took the jobs). "Processing your
  request" is also semantically wrong for a background-result delivery — the
  user made no request. Everything user-facing that a real delivery emits
  (session-switch notice, the model's commentary, deferral notices) flows
  through `outputCh`, ordered after delivery-turn acquisition, and needs no
  started line. The no-op path becomes fully silent.
- Boot recovery (`recoverPendingJobs`) enqueues completed-undelivered jobs the
  same way and gets the same silence.
- The image-generation path using `EnqueueAtWithPrompt` with its
  "▶ Processed your image request..." override is NOT touched.

## Testing

- `midturn_test.go`: push/drain/closeAndDrain semantics incl. concurrent
  push-vs-close (exactly-once: every successful push appears in exactly one
  drain), open-replaces-stale, no mailbox for unknown session.
- Dispatch: `handleMention` with an open mailbox sends the receipt, enqueues
  nothing, message lands in the mailbox; closed mailbox → today's enqueue path
  unchanged; banned/rate-limited/no-context users never reach the mailbox.
- Round-boundary drain: rows land after tool results and after `view_image`
  injections (order pinned); detectimages preprocessing at drain (image URL →
  image parts + ref registration); limit failure → warning notice + text-only
  row.
- Wire-shape e2e: absorbed message appears in the next API request as a user
  row after tool results (chat completions and Responses-chain delta shapes,
  mirroring the view_image wire tests).
- Lifecycle: absorbed during final response → post-turn continuation turn
  includes it and answers; quiescent-close race (push vs close) processed
  exactly once; cancellation persists undrained messages as text rows and sends
  nothing.
- Notices: `absorbed` default + expansion.
- Side-fix: no-op async delivery emits no started notice (regression for the
  incident); real delivery still emits its switch notice and commentary.
- Full `go test ./...`, `go fmt ./...`, `go vet ./...`.

## Documentation

- `docs/queue-and-sessions.md`: new "Mid-turn absorption" section.
- `config/notices.toml`: reference-list + commented-block entries for
  `[mentions] absorbed`.
- `AGENTS.md`: update after implementation with verified facts.

## Out of scope

- `fetch`/`view_image` re-viewing churn (follow-up task).
- Absorption into async-delivery or ephemeral turns.
- Any config knob.
