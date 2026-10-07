# Live Config Precedence & Turn Usage Attribution — Design

Date: 2026-10-06

## Problem

`session_settings` (commit `369e0ce`, May 2026) was built as a per-session config
snapshot with two intents that never fully landed:

1. **Token attribution** — know which model / reasoning effort produced each
   turn's tokens, to compare cost across models and measure how
   `reasoningeffort` affects actual reasoning tokens. The per-message piece
   (`messages.settings_id`, "reserved for future change-tracking phase") never
   landed: regular messages never get it set, only compaction propagates it
   onto rows it re-inserts, and nothing reads it.
2. **System-prompt change tracking** — detect when the live config's system
   prompt diverged from what the session started with. Never built, and we are
   deliberately **not** building it: forcing a new system prompt mid-session is
   undesirable anyway. The new prompt applies naturally at the next compaction.

What DID land is the overlay: `ApplySettings` pins `Model` (and `System`) from
the snapshot onto the live config for the session's whole lifetime
(`getSessionConfig`, `main.go`). Because its zero-value semantics make empty
snapshot fields fall through to the live config, an old session whose config
later gained `reasoningeffort` ran **old model + new effort** — the old model
didn't support the parameter and the API errored. Meanwhile the explicit
trigger path (`config.Commands.Chats` at dispatch) already used the live
config, so pinned vs. live behavior was inconsistent between paths.

`turn_usage` records per-turn `PromptTokens`/`CompletionTokens`/
`CachedTokens`/`ReasoningTokens`/`FinishReason`/`APIPath`/`DurationMs` but has
**no model, service, or reasoning-effort attribution**, so the original stats
goals are unbuildable from stored data.

## Solution

Four parts, one coherent change to what `session_settings` means:

### A. Live config precedence (the fix for the incident class)

Config changes take effect for **all** sessions on `/reload`. The stored
settings row becomes pure creation-time provenance — "what this session
started with" — and is never applied to a live turn again.

- `getSessionConfig` (`main.go`) drops the `ApplySettings` overlay; it becomes
  a plain live-config lookup. `ok=false` when the chat command is gone from
  config, uniformly (today a settings row could force `ok=true` with a
  gutted config — callers already handle the missing-command case).
- `ApplySettings` and `GetSessionSettings` are deleted (no production callers
  remain). `CreateSessionSettings` stays: sessions still record their
  creation snapshot, and `cloneDBSession` still copies it.
- New system prompt lands at the next compaction. `renderFreshSystemPrompt`
  already prefers the live `SystemTmpl` (the overlay only ever replaced the
  rendered `System` string, which the template path ignores), so template
  prompts already worked this way; after this change it is consistent for
  non-template prompts too.
- `sessions.model`/`sessions.service` stay creation-time display columns.
  They are provenance, not runtime state.

### B. Responses-chain guard across model switches

When a chain error is *recognized*, recovery already exists and works:
`isResponseIDError` → `shouldRetryWithoutResponseID` retries the turn with
the full history from our side and gets a fresh server-side response
(`aiCmds.go`). Expired / invalid `previous_response_id` chains are a solved
problem and this change does not touch that path's semantics.

Cross-model chaining is different in one decisive way: it is not guaranteed
to error at all. Observed shapes when chaining a stored response from model
A onto a request for model B:

- recognized chain errors — already recovered today;
- *unrecognized* errors — e.g. the 400 `Reasoning input items can only be
  provided to a reasoning or computer use model` (reasoning → non-reasoning)
  or a generic 500 — which fail the turn visibly instead of recovering;
- **silent success with the prior assistant history dropped** — no error, so
  no recovery path can exist; the conversation continues with the bot
  forgetting its own replies, and keeps chaining from the new response.

Same two-layer idiom `isResponseIDError` itself already documents
("Layer 1 prevention / Layer 2 safety net"):

1. **Layer 1, prevention**: new `sessions.response_model` column. Written
   whenever `response_id` is written (and cleared with it — including
   compaction's raw reset). In `runTurnResponses` (`aiCmds.go`), if
   `session.ResponseModel != cfg.Model`, skip chaining for that turn (send
   full history, same as an expired chain) and log why. This is the only
   defense against the silent-loss shape.
2. **Layer 2, net**: extend `isResponseIDError` to treat the reasoning-item
   mismatch wording as a chain error, so any cross-model attempt that slips
   past layer 1 (provider wording variance, e.g. xAI) still lands in the
   existing full-history retry instead of a visible failure.

Scope boundary: the guard compares `cfg.Model` only. A provider switch that
keeps the same model name passes layer 1 and falls to the net — one wasted
call and a WARN, then clean recovery. Storing and comparing the service too
is cheap optional hardening if that ever bites in practice.

### C. Turn usage attribution (the original stats intent)

`turn_usage` gains `model`, `service`, and `reasoning_effort` columns,
populated from the chatRunner's effective `AIConfig` at call time via
`storeUsage` → `insertDBTurnUsage`. This is deliberately **denormalized**:
per-turn capture is accurate across any number of config reloads with zero
change-tracking machinery. Pre-existing rows keep `''` (unknown).

Rejected alternative: `turn_usage.settings_id` FK + new snapshot rows on every
config change. That is the original change-tracking design — it needs
reload-time divergence detection, snapshot churn, and joins for every stat,
and it still can't answer "what effort was in effect" for sessions whose
settings predate a field. Denormalized columns answer every original question
directly (`SELECT model, reasoning_effort, SUM(reasoning_tokens) … GROUP BY`).

### D. Cleanup of the dead per-message column

`messages.settings_id` is removed from the `Message` struct and compaction
stops stamping it (it was only ever incidental propagation — compaction
copied it the same way it copies `Role`/`Content`; nothing reads it). The DB
column is left in place (no destructive migration); it stays NULL forever.
The migrate-sqlite-to-pgsql struct copy is kept in sync.

## Consequences

- Existing sessions with pinned old models pick up the current config on
  their next turn — intended; that is the incident being fixed.
- A mid-session model switch continues the same conversation history on the
  new model. For Responses-API sessions the chain restarts cleanly (full
  history resend) instead of erroring or silently losing context.
- Compaction's summarizer call is unaffected (it already uses the effective
  cfg with effort cleared) and its usage is still not persisted — non-goal.
- No TOML changes, no user-visible commands change, no destructive migrations.

## History (why this doc exists)

Recorded so the archaeology doesn't repeat: `session_settings` was "Phase 1
infrastructure" (`369e0ce`) whose Phase 2 — per-message settings attribution
via `messages.settings_id` — never shipped. The only other trace of the plan
was a DESIGN NOTE in `ApplySettings` about comparing `{{.Vars.*}}`
references, also never implemented, and both are removed by this change.
Token attribution now lives where it belonged all along: on the usage rows
themselves.
