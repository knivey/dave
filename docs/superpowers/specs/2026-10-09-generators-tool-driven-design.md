# Generators: tool-driven retrieval, output, and logging parity

**Date:** 2026-10-09
**Status:** approved design; supersedes the retrieval/output/logging portions of `2026-10-09-generators-design.md` (the v1 spec stays as history — its Log-Query Module, Notices, and transcript-injection executor model are replaced by this document; its config/registration/dispatch/queue model is unchanged and still binding).

## Context

The shipped generators feature pre-builds the channel-log transcript before the
first LLM call (window from `[name.log]`, optionally overridden by a leading
duration token in the user's args) and injects it into the user message. Three
problems, all owner-identified:

1. **Host-side argument parsing is the wrong layer.** A strict leading-token
   grammar (`^(?:\d+[smhd])+$`) can't read "the argument since Tuesday" or
   "7d focus on drama" written any other way — and the window must be known
   before retrieval, so the model can never recover a window we didn't fetch.
2. **Every reply with text lands in IRC** (`handleToolCallResponse` sends
   intermediate tool-loop chatter; streaming sends deltas live), so a summary
   arrives as preamble + answer instead of one curated result.
3. **Ephemeral turns are second-class in logging**: the apiLogger no-ops at
   session 0, so generator API traffic is absent from `api_logs/` and the
   terminal's `api_log` breadcrumb points at nothing.

This design moves retrieval and output into the model's hands (tools), and
gives every generator run a first-class api-log session, while keeping every
host-side guarantee that the model cannot be trusted with (token budgets, row
caps, event filters, queue discipline).

## Goals

- The model decides when and what to retrieve: `query_channel_logs` tool,
  natural-language-friendly, multi-call.
- Quiet channels: intermediate model text never hits IRC on generator runs;
  the final answer is either auto-sent (fallback, streams) or delivered via
  an optional `respond` tool (config-gated per generator).
- Generator API traffic logged per run in `api_logs/` (own file, same
  filename shape as sessions) with real terminal breadcrumbs and incident
  references.
- Host-side caps unchanged: per-call token budget, row cap, event filter,
  `maxToolIterations` loop bound, queue serialization via existing
  per-service `Parallel` semantics (explicitly unchanged — generators stay
  ordinary queue citizens).

## Non-goals

- No persistence changes: still ephemeral turns, SessionID-0 usage rows, no
  compaction, no sessions.
- No changes to chats/completions/tools command behavior. Suppression,
  `query_channel_logs`, and `respond` are ephemeral-turn-only.
- No cross-call total retrieval budget (per-call `max_tokens` + the 20-iteration
  loop cap + the model's own context window bound it; a new knob is YAGNI —
  revisit if production shows runaway multi-call loops).
- No changes to the queue manager.

## 1. Retrieval: `query_channel_logs`

### Offering

- Offered **only** on ephemeral turns (`cr.ephemeral`) whose generator config
  carries a `[name.log]` block. Not offered to regular chats. **Not gated on
  MCP tools existing** (a log-fed generator with zero MCP tools still gets
  it — unlike the regular builtins, which ride the MCP-tools-exist rule).
- Honors `disabled_builtin_tools` (cascade: command > service > root) — a
  disabled call returns an error tool result naming the disable, identical to
  the existing builtin behavior.
- Joins the default `hidden_tools` list (root config default gains
  `query_channel_logs`), so no `[tools] call` IRC notice unless an admin
  unhides it; `toolverbose = false` remains the per-generator silencer.

### Definition

- Name: `query_channel_logs`.
- Parameters: `window` — optional string. Description documents: the duration
  grammar (`s`/`m`/`h`/`d` units, compound like `1d12h`), the **configured
  default** (embedded per-generator at offering time, e.g. "default: 24h"),
  and what the tool returns (a transcript of this channel's activity for the
  window, keep-newest token-budgeted, truncation disclosed in the result).
- The def is **per-run**, not a static `builtinTools` map entry: the
  description embeds the configured default window, and the handler needs the
  runner's log context. The executor assembles the def when the ephemeral
  runner is created.

### Dispatch

- A dedicated branch in `executeToolCalls` (alongside, not inside, the global
  `builtinTools` map) routing to a handler in `generators.go`. Server label for
  any visible call notice: `builtin`.
- The runner carries a generator log context set once by `generator()`:
  the defaulted `LogQuerySpec`, raw and normalized channel names, and the
  generator name for logs. (The runner already has network; raw channel is
  the one piece `setChannel` doesn't keep — the context carries both.)

### Handler behavior

1. `window` omitted → the configured default; present → parsed with the same
   grammar (`parseWindowDuration`). Invalid → error tool result listing the
   grammar with examples (the model self-corrects; no user-facing notice).
2. Calls the existing `fetchChannelLogFn` injection seam with the resolved
   spec (window swapped in, defaults applied — same defense-in-depth as the
   v1 executor).
3. `errLogWindowTooLarge` (row cap) → error tool result naming the cap and
   telling the model to narrow the window.
4. Success → tool result containing:
   - a header: channel, requested window, covered range (`FirstKept`→
     `LastKept`, omitted when zero), kept/total line counts;
   - the rendered transcript (`buildTranscriptLines` pipeline, day
     separators — unchanged);
   - the existing truncation marker line appended when the token budget
     clipped lines, so the model can (and per the shipped system template,
     should) disclose it.
5. Zero lines → a plain "no activity in this window" tool result — the model
   relays it (the v1 user-facing `no_activity` notice dies with injection).
6. INFO log per call: generator name, window, coverage, kept/total (the v1
   start-of-run log moves here).
7. Reusable across calls: the model may query multiple windows (24h, then
   narrow to 2h around an event). Each call gets the full per-call budget;
   the 20-iteration loop cap is the only cross-call bound.

### What dies

- The executor's leading-duration arg grammar and transcript injection
  (the whole `splitFirstWord`/override/fetch/header block).
- `GeneratorNotices{NoActivity, Truncated, WindowTooLarge}` — the struct, its
  `setNoticesDefaults` entries, the `[generators]` section in
  `config/notices.toml`, and their tests. Disclosure now rides the tool
  result (marker) and the model's retelling.
- `LogQuerySpec` config shape is unchanged (`window`/`max_tokens`/`events`);
  its meaning becomes tool configuration.

## 2. Output: quiet intermediates + optional `respond`

### Suppression (always on for ephemeral turns)

- Intermediate tool-loop text — any iteration that also carries tool calls —
  is **never auto-sent to IRC**: `handleToolCallResponse`'s text-send block
  skips on ephemeral, and the streaming tool-call branch (the unsent-tail
  flush) discards instead of sending (text still logged). The text still
  enters the turn history (`turn.Add`) — the model sees its own words — and
  the INFO logs are unchanged.
- System notices (`sendError`, `sendWarning`, queue notices, rate/ban
  messages) are unaffected — only model-authored text is suppressed.
- Non-ephemeral behavior is byte-identical (the existing guards pin it).
- Dispatch truth table unchanged: log-fed generators stay optional-args
  (bare `^summary` valid — the model defaults the window via the tool).

### `respond` tool (config-gated)

- New `GeneratorConfig` field: `respond_tool` (bool, default **false**).
- When true, `respond(text)` is offered on the ephemeral turn (log block not
  required — any generator may opt in; `fakenews` included). Like
  `query_channel_logs`, it is **not gated on MCP tools existing**.
- Parameter: `text` (required, string) — the complete final answer for the
  user.
- Handler: validates non-empty (empty → error tool result asking for text),
  sends through the normal render/pastebin plumbing (`sendRendered`), marks
  the turn complete.
- Turn completion: the loop ends **immediately after** `executeToolCalls`
  returns — no extra API round-trip, no empty-response handling (the respond
  iteration carried a tool call, so it is not an empty response by the
  existing definition). Every `runTurn` variant (chat-completions
  streaming/non-streaming, Responses streaming/non-streaming) checks the
  completion signal after `executeToolCalls` and returns.
- If the model calls `respond` alongside other tools in one iteration: calls
  execute in order; when `respond` fires, its text is sent, the remaining
  calls still execute (results logged, history is ephemeral anyway), and the
  turn ends.
- Honors `disabled_builtin_tools`; joins the default `hidden_tools` list.
- Not streamable: a tool-delivered answer arrives as one message (tool
  arguments are not streamed by our renderer). Documented trade-off — the
  fallback path keeps live streaming.

### Fallback (respond_tool = false, or the model just finishes naturally)

- The final no-tool-call iteration auto-sends exactly as today
  (`sendFinalText`; streaming intact). Small models that never call
  `respond` still get their answer out.

## 3. api-log parity: one file per generator run

- Every ephemeral run allocates a **unique negative api-log id** at runner
  setup:
  - negative → can never collide with DB session ids (≥ 1);
  - seeded from the clock at boot, then atomically decremented → unique
    across concurrent runs AND across restarts (no counter state, no
    cross-boot file reuse);
  - negative ids mean the apiLogger's `sessionID == 0` no-op guards stay
    byte-identical — zero keeps meaning "no session attached".
- `generator()` opens the run's api-log session via the existing
  `RestoreSession(id, network, channel, userID)` (its guard only rejects 0 —
  works as-is), producing `api_logs/<network>_<channel>_user<uid>_<id>.jsonl`
  — same filename shape sessions get, one file per run, O_APPEND within it.
  No shared bucket, no overwrites, no cross-channel bleed (unique ids keep
  the bare-session-id sessions map correct — no composite-key rework).
- The runner carries the id in a field separate from `sessionID` (which stays
  0 — it is load-bearing for usage attribution and the ephemeral gates); the
  transport is pointed at it via the existing `setAPILogger` path, so
  request/response/stream entries flow through the unchanged
  `daveTransport` call sites.
- Terminal parity mirroring `chat()`: `generator()` logs the same
  post-turn Debug breadcrumb (`api_log=<real path>`, e.g. "generator
  finished"), and incident logging resolves the run's file via the runner's
  api-log id instead of returning "" at session 0.
- DB attribution unchanged: `turn_usage` rows keep SessionID 0. The negative
  id is api-log-file-only and never touches the database.

## 4. Config and docs surface

- `config/generators.toml`: reference block rewritten — `[name.log]`
  documents tool semantics (window = tool default, max_tokens = per-call
  budget, events = transcript shape, QUIT/NICK note stays);
  `respond_tool` documented with both output modes; the live `[summary]`
  system template rewritten to instruct the tool loop (map the user's
  duration wording into the `window` argument; call the tool; disclose
  truncation when the result reports it; deliver via `respond` when
  offered).
- `config/notices.toml`: `[generators]` section deleted with the struct.
- `AGENTS.md`: the generators bullet rewritten to the tool-driven model
  (retrieval tool, respond tool, suppression, per-run api-log ids, the dead
  notices), guard-test names updated.
- This spec file records the supersession; the v1 spec is not edited further.

## 5. Queue

Explicitly unchanged (owner-confirmed): generators remain ordinary queue
citizens — per-channel queue + depth cap + `^stop` + position notices,
concurrency governed by the per-service `Parallel` knob exactly as for chats.
The longer wall-clock of tool-driven runs is the admin's lever: a dedicated
service entry for log-fed generators gives summaries their own slot budget
without code.

## 6. Testing

Guard tests (table-driven, testify, existing helpers):

- `query_channel_logs` offered only on ephemeral + log-fed runners; not
  offered on regular runners; not gated on MCP tools.
- Handler: window default/parse/invalid-grammar error; row-cap error
  wording; result header + transcript + truncation marker; zero-lines
  result; INFO log fields.
- `disabled_builtin_tools` disables both new tools; default `hidden_tools`
  lists both.
- Suppression: intermediate tool-loop text not sent (streaming and not);
  still present in turn history; non-ephemeral send behavior byte-identical.
- `respond`: offered only when `respond_tool = true`; sends rendered text;
  turn ends with no second API call (counter the calls); empty-text error
  result; alongside-other-tools ordering; fallback path sends final reply
  when disabled.
- api-log parity: per-run unique negative ids (two runs → two ids, two
  files); LogRequest/LogResponse entries written for ephemeral runs; `0`
  guards still no-op; breadcrumb path non-empty; `RestoreSession(0)` still
  refused; concurrent different-channel runs land in different files.
- Config: `respond_tool` loads (default false); `[generators]` notices
  removal doesn't break notices loading; shipped `config/generators.toml`
  loads through `loadConfigDir`.
- Existing v1 tests that pinned transcript injection/arg-grammar/notices are
  rewritten to the new model, not deleted wholesale — coverage must not
  regress on the surviving pieces (fetch pipeline, budget math, config
  validation).

## Key decisions and rationale

- **Model-driven retrieval over pre-built transcript**: the window is a
  retrieval parameter the model can now express in natural language and
  refine multi-hop; host-side grammar dies. Caps stay host-side — the model
  can't widen its own budget.
- **Suppression always-on (both respond modes)**: generators are one-shot
  "produce a thing" commands; chatter isn't output. The fallback path
  preserves streaming and small-model compatibility.
- **`respond` config-gated, default off**: opt-in per generator; the
  fallback guarantees output even if a model never calls it. One-message
  delivery for tool answers is the accepted cost.
- **Negative clock-seeded api-log ids**: zero apiLogger surgery, per-run
  files, cross-boot-unique, collision-free by construction. The composite
  sessions-map rework considered earlier is unnecessary and dropped.
- **No new budget knobs**: per-call `max_tokens` + 20-iteration cap + model
  context window bound runaway retrieval; observed production behavior is
  the trigger for anything stricter.
