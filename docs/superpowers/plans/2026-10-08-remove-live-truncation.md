# Remove Live-Path Truncation + Per-Command Context Window — Plan

> **ADDENDUM (post-implementation owner decision):** the "keep maxhistory
> as a viewer display cap" middle state described below was superseded the
> same day — the owner chose full removal: `^resume` counts the full live
> history, and the `maxhistory`/`MaxHistory` knob (AIConfig+Service,
> cascade, default-100 backfill, incident field) plus `TruncateHistory`
> itself were deleted. Old configs carrying `maxhistory = N` load fine
> (unknown TOML keys are ignored). The sections below are the original
> plan as first approved.

Owner decisions (2026-10-08): option C — remove TruncateHistory from the
API request path (keep for viewer display); add compaction context_window
per chat command (command > service > [compaction] global), because the
window varies per MODEL more than per service.

## Background

TruncateHistory (Dec 2024, `e09091e`) predates compaction by ~17 months
and was the only context-size mechanism then. Today it (a) risks
splitting tool call/result pairs at the window edge (orphan tool outputs),
(b) STARVES auto-compaction — the trigger reads the truncated prompt's
tokens, so small maxhistory keeps prompts under the threshold forever and
sessions are never summarized, and (c) silently drops history by message
count instead of tokens on non-chained services.

## Changes

1. `SessionManager.GetMessages(sessionID)` — drop the maxHistory param
   and the truncation; returns full live history. Caller updates:
   aiCmds chat() ×3, jobManager delivery loop, tui /tokencount (which
   must count exactly what a turn sends — now full history), tests
   (mechanic). The `^resume` and `^history` paths keep their own local
   TruncateHistory — display/count caps only.
2. `tokensPerMessage` — drop the maxHistory cap (divisor = all live
   rows); `maybeAutoCompact`'s our_token_count counts full live history
   (must equal the next request's payload).
3. `AIConfig.ContextWindow` (`context_window`, chats.toml) — resolved
   command > service > [compaction] global inside `effectiveContextWindow`
   (no ApplyDefaults cascade, so the resolver can attribute the source
   "command"/"service"/"compaction" in logs). No `*int` tri-state needed:
   0 = unset.
4. Docs: chats.toml (new option + maxhistory reworded to viewer cap),
   services.toml (maxhistory reworded), AGENTS.md (fix stale "maxhistory
   defaults to 8" — code default is 100; compaction cascade becomes
   command-first; note the removal).
5. Tests: GetMessages full-history; effectiveContextWindow command case;
   tokensPerMessage signature/cap updates; existing suites.

## Explicitly NOT changed

- maxhistory stays as the viewer/resume display cap (its only remaining
  job) and the session-creation default seed; not removed from config.
- compaction's own selection of archived range (already DB-based,
  unaffected).
