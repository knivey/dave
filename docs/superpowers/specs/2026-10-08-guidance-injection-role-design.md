# Unified Guidance Injection — Role Selection Design

Status: RESOLVED — all decisions settled (see "Decisions resolved during
review" and "Open Decisions"); implemented per
docs/superpowers/plans/2026-10-08-guidance-injection-role.md.
Date: 2026-10-08
Follows findings from the empty-response handling work (Oct 2026, aiCmds.go).

## Addendum (2026-10-08, post-implementation): async result delivery mode

A third delivery path for the async site, selected by a NEW per-command
knob (kept separate from Knob 1/2 — it replaces the whole delivery shape,
not just the role):

```toml
# async_result_delivery = "message"   # default; "tool"
```

- `"message"` (default): the two-knob guidance mechanism exactly as
  designed above — no behavior change.
- `"tool"`: the notification is a SYNTHETIC TOOL ROUND-TRIP instead of a
  guidance row: an assistant row carrying a tool call
  (`job_status`, arguments `{"job_id": "…"}`, id `call_<hex>`) plus the
  matching RoleTool result row with the `[System: Background task
  completed…]` payload. Rationale: the assistant→tool-call→result cycle
  is the one conversation shape every OpenAI-compat provider handles
  natively, and a trailing tool result inherently demands a response —
  Knob 2 has no analog here. Target use: smaller models (OpenRouter,
  local llama) that handle mid-chain system rows poorly. The empty-
  response correction ALWAYS stays on the guidance mechanism.

Resolved decisions:
- **Chain clear on injection**: tool mode MUST clear `session.ResponseID`
  (chained LastN(1) would send an orphan tool output — the synthetic call
  predates the chain head). Accepted cost: one full-history resend per
  notification; the models this targets do not use previous_response_id,
  so the token cost is moot in practice.
- **Wording trial-first**: description drops meta-instruction ("respond
  to it as you would any tool result" invites tool-call reasoning);
  it states the delivery fact passively: "The result will be delivered
  as a tool response when it completes. Do not poll or wait for
  results. Continue the conversation normally."
- **Tool name**: fixed synthetic `job_status` (providers do not require
  history tool calls to have been advertised; reusing `wait_for_job`
  would contradict the "do not wait" instruction).
- **Atomicity**: the pair is written in ONE transaction (a crash between
  rows would leave an incomplete tool call, making the session
  un-clonable).
- `.AsyncResultRole` template variable gains the value `"tool"` in this
  mode (resolver `asyncResultRole`: tool → "tool", else `guidanceRole`).

## Problem

dave injects system-role guidance into conversations from several unrelated
code paths, each inventing its own approach:

| Site | File | Persisted? | Position at next request | Current mechanism |
|------|------|-----------|--------------------------|-------------------|
| Async background-job result | `injectAsyncResultFromDB` (jobManager.go:408) | yes (DB) | **TRAILING** — a turn fires immediately after injection (aiCmds.go pending-jobs loop) | always `RoleSystem`; appends a `RoleUser` "Respond…" suffix only when `needsusersuffix = true` or model matches `anthropic/` |
| Empty-response retry correction | `addEmptyResponseCorrection` (aiCmds.go) | no (ephemeral) — owner proposes persisting (D7) | **TRAILING** — it is the retry request's last message | hardcoded `RoleUser`, content marked `[automated notice, not from the user]` |
| TUI `/systemmsg` | tui_commands.go:1050 | yes (DB) | followed (user speaks next) | always `RoleSystem` |
| TUI `/reinject` | tui_commands.go:974 | yes (DB) | followed (user speaks next) | always `RoleSystem` |
| Session-creation system prompt | chat() (aiCmds.go) | yes | leading (index 0) | `RoleSystem` — out of scope |
| Compaction summary rows | compaction.go | yes | leading (indexes 0-1) | `RoleSystem` — out of scope |

Two problems:

1. **No shared config.** Whether an injection can be a trailing `system`
   row depends on the provider, but only the async path has any knob
   (`needsusersuffix`, a plain bool, no service cascade, default false —
   and a name that reads two ways: "append a user turn after system" vs
   "use user instead of system").
2. **The trailing-system shape is provider-fragile**, which the codebase
   already knew in two places (see Findings) — yet the async path ships
   that exact shape to every non-Anthropic model, and the new empty-
   response correction swung to the opposite extreme (always user).

## Findings (verified sources)

1. **Trailing system rows break on providers dave demonstrably serves** —
   both attested in-repo:
   - Anthropic via OpenRouter requires a user turn to answer; that is why
     `injectAsyncResultFromDB`'s `NeedsUserSuffix` mechanism exists.
   - xAI/Grok rejects `system → assistant` adjacency; that is why
     `pickCompactionCutTurn` enforces user-starting compaction tails.
2. **llama.cpp local services**: Jinja templates that render mid-chat
   system in place (ChatML / Llama-3 / Qwen — the common `--jinja` case)
   are fine with trailing system. Templates whose Jinja source lacks the
   system role entirely (e.g. the Gemma family) get no help: llama.cpp's
   `common/chat.cpp` `system_message_not_supported()` merges-or-drops
   ONLY `messages.front()`, so a non-leading system row cannot render.
   (Verified against llama.cpp master, Oct 2026. Counter-point: Gemma is
   the notable exception, not the rule; owner reports all local models
   use system-capable Jinja templates.)
3. **llama.cpp accepts `developer` and maps it to `system` server-side**
   (`workaround::map_developer_role_to_system`, common/chat.cpp — applied
   for all models except GPT-OSS). So `developer` is a no-op alias for
   system on llama-server.
4. **OpenAI natively supports `developer`** (the documented successor of
   system instructions for o-series/gpt-5 era models).
5. Strict self-hosted OpenAI-compat gateways may reject unknown roles
   (same class of problem as unknown fields — cf. apiIdentity's design
   note). Auto-sending `developer` everywhere is therefore unsafe;
   it must be opt-in per config.

## Design

### Two orthogonal concerns, two knobs

The role of the guidance payload and the trailing-user-turn requirement are
SEPARATE problems and stay separate (explicitly not collapsed into one
knob — the payload must keep its correct semantic role even when the
provider cannot end a request on it):

**Knob 1 — payload role.** Per chat command in `chats.toml`, cascading
from `services.toml` (same shape as `load_notice`):

```toml
# injection_role = "system"   # default; "developer"; "user"
```

- `"system"` — default. Correct semantics; works on OpenAI, xAI,
  OpenRouter non-Anthropic, and system-capable Jinja templates
  (the owner's whole local fleet).
- `"developer"` — for OpenAI services that treat developer instructions
  specially; harmless alias of system on llama.cpp (Finding 3).
- `"user"` — only for models that cannot render a mid-chain system row
  at all (e.g. Gemma-family templates, Finding 2); content marker keeps
  it auditable.

**Knob 2 — trailing user turn.** Whether a short `RoleUser` turn is
appended after the guidance row when that row would otherwise be the
LAST message of the request. This is the existing `needsusersuffix`
mechanism, kept as-is semantically (system payload + user suffix = two
rows) and upgraded from a plain bool to the standard tri-state cascade:

```toml
# needsusersuffix = true|false   # unset = inherit service; still unset = auto (anthropic/ models)
```

Auto-detection (`modelNeedsUserSuffix`, `anthropic/` regex) remains the
default-resolution step. Explicit values override both ways, per command.

### Shared helpers used by every injection site

```go
// guidanceRole resolves Knob 1 for the payload row.
// Cascade: command > service > default "system" (materialized in
// ApplyDefaults); unknown values fall back to system defensively.
func guidanceRole(cfg AIConfig) string

// needsUserSuffix resolves Knob 2: whether a user turn must follow the
// guidance row. Cascade: command > service > anthropic auto-detect.
// Explicit values override in BOTH directions (false suppresses even
// the auto-detect — a behavior change vs the old plain-bool knob,
// where explicit false on an anthropic model was still auto-true).
func needsUserSuffix(cfg AIConfig) bool

// guidanceMessages builds the rows for one guidance injection: the
// payload row under the resolved role, plus — only when trailing, Knob 2
// resolves true, and the payload is not already a user row — the
// site-supplied user suffix (wording is per-site).
func guidanceMessages(cfg AIConfig, content, suffixText string, trailing bool) []ChatMessage
```

Sites persist the returned rows themselves (`sessionMgr.AddMessage` for
the async result, `turn.Add` for the correction — persisted per D7).

- `trailing` is a property of the CALL SITE, not config: async result and
  empty-response correction are trailing; `/systemmsg` and `/reinject`
  are followed (never get a suffix — a real user message follows them).
- All current sites persist their rows (`turn.Add` /
  `sessionMgr.AddMessage`); the correction's persistence is design D7.
- Content provenance markers (`[System: Background task completed…]`,
  `[automated notice, not from the user]`) are kept regardless of wire
  role, so history/audit display never depends on the role choice.

### Persisting the empty-response correction (D7 rationale)

Orthogonal to the Responses-API chain handling (clearing
`previous_response_id` on empty output is about request construction —
the retry must resend full history, and chaining the old head on top of
that would duplicate context server-side; it says nothing about storing
the correction row). Persisting the correction instead of keeping it
ephemeral:

- **History/model parity**: the stored session becomes exactly what the
  API saw (`[user, correction, assistant]`), eliminating the current
  drift where DB history omits a message the model was actually told.
- **Standing guidance**: for models that chronically strand answers in
  reasoning, the row keeps nudging on subsequent turns — the owner's
  stated goal ("keep the model on track in the future").
- **Cost**: one short row per empty-retry EVENT (in-turn back-to-back
  duplicates still collapse); compaction summarizes/archives them like
  any other row; on chained Responses turns the row is not re-sent (the
  chain head already includes it), so there is no token duplication.
- **History viewer**: rows appear under their wire role with the
  `[automated notice…]` content marker — auditable, clearly not user
  input.

### Site-by-site behavior after the change

| Site | Payload role (Knob 1) | Trailing | Suffix (Knob 2) |
|------|----------------------|----------|-----------------|
| Async bg result | resolved, default `system` | yes | auto: appended for `anthropic/` (existing behavior preserved) or when `needsusersuffix = true`; never when a user turn already follows |
| Empty-response correction | resolved, default `system` | yes | same auto rule — payload stays system, suffix appended when the provider needs a user turn |
| `/systemmsg`, `/reinject` | stay `RoleSystem` | no | never (the user's next message is the following turn); admin-explicit, no knobs (D6) |

### Interactions analyzed

- **Compaction**: guidance rows (any role) are ordinary rows — archived
  and summarized in range, re-inserted verbatim in tails;
  `pickCompactionCutTurn` already walks to the next `RoleUser` boundary,
  so extra system/mid rows never block a safe cut.
- **Responses API chains**: persisted injections already clear
  `session.ResponseID` (both TUI commands do; async injection precedes a
  full turn). The correction drops the chain on retry (existing behavior
  from the empty-response work — request-construction concern, unrelated
  to D7 persistence).
- **History viewer**: rows display under their wire role with the content
  marker; no schema change.
- **Clone**: `[bg job result]` rows already clone verbatim regardless of
  role (clone_test.go pins system-role rows; a role change alters the
  pinned fixtures only).

## Decisions resolved during review

- **Two rows, not role escalation (was D4)**: when a trailing guidance row
  meets a provider that needs a user turn, the payload KEEPS its role and
  the short user suffix row is appended after it — the existing
  `injectAsyncResultFromDB` shape, extended to all sites. Role and suffix
  are deliberately separate knobs; collapsing them was rejected (the
  payload's semantic role should not change because of a trailing-turn
  constraint).
- **`needsusersuffix` is NOT deprecated (was D3)**: it is Knob 2, upgraded
  to the standard tri-state cascade (command > service > anthropic
  auto-detect). Existing `true` values keep their meaning; `false` gains
  force (it now suppresses the anthropic auto-detect, which the old
  plain-bool knob could not express).

## Open Decisions

| # | Question | Options / recommendation |
|---|----------|--------------------------|
| D1 | Knob 1 name | `injection_role` (recommended — says what it selects) vs `guidance_role` |
| D2 | Empty-response correction role | Follow Knob 1 (default **system** + auto suffix for anthropic) — recommended, per "system is the better role" — vs keep today's hardcoded user payload |
| D5 | `developer` auto-use | Config-only opt-in (recommended — strict gateways) vs auto for `isOpenAIService` |
| D6 | TUI inject commands | Leave hardcoded RoleSystem (recommended — explicit admin intent, followed position) vs honor the knobs |
| D7 | Persist the empty-response correction | Persist (owner's lean, recommended — history/model parity + standing guidance; see rationale above) vs keep ephemeral |

## Testing plan

- Resolver table tests, Knob 1: cascade (command > service > default
  "system"); invalid values rejected at load or logged + defaulted.
- Resolver table tests, Knob 2: cascade (command > service > anthropic
  auto-detect); explicit true/false beats auto-detect.
- Site tests: async injection payload role + suffix presence under each
  Knob 1/Knob 2 combination (request-body assertions like the existing
  `TestRunTurnEmptyResponseRetryInjectsCorrection`); suffix absent when a
  user turn already follows (followed sites) or payload role is "user".
- Correction persistence (D7): correction row present in DB after a
  successful retry; still exactly one per empty-retry event (in-turn
  dedupe); compaction handles the row like any system/user row.
- Back-compat: config with `needsusersuffix = true` behaves identically
  to today (system payload + suffix), now overridable per service.
- Regression: full `go test ./...`, `-race` on touched tests.

## Rollout

1. Land both resolvers + `injectGuidance` + config plumbing
   (`injection_role` new; `needsusersuffix` → `*bool` cascade) with the
   async site switched over — behavior-identical for every existing
   config (anthropic keeps its two-row shape exactly).
2. Switch the empty-response correction to the shared mechanism per D2/D7
   (payload role from Knob 1, persisted, suffix per Knob 2).
3. AGENTS.md + chats.toml/services.toml reference updates.
