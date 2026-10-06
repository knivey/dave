# Live Config Precedence & Turn Usage Attribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Config changes apply to all sessions on `/reload` (the stored-settings pin is retired to creation-time provenance), Responses-API chains never cross a model switch, and `turn_usage` rows carry the model/service/reasoning-effort that produced them.

**Architecture:** Behavior change in `getSessionConfig` plus column additions (`sessions.response_model`, `turn_usage.model/service/reasoning_effort`) — all via GORM AutoMigrate, no hand-written migrations, no destructive changes. The dead `messages.settings_id` writes stop; the column stays in the DB. Existing error-recovery paths (`isResponseIDError` full-history retry) are preserved and only extended.

**Tech Stack:** Go, GORM (sqlite + postgres via existing `DatabaseConfig`), existing session/usage infrastructure.

**Spec:** `docs/superpowers/specs/2026-10-06-live-config-and-usage-attribution-design.md`

---

### Task 1: Live config precedence — retire the settings overlay

**Files:**
- Modify: `main.go` (`getSessionConfig`, ~line 123)
- Modify: `sessionManager.go` (delete `GetSessionSettings` and `ApplySettings`, ~lines 213-249; update `CreateSessionSettings` comment, ~line 192)
- Modify: `contextStore_test.go` (delete `TestDBApplySettings`, ~line 81; adjust `TestDBCreateSessionSettings` if it references deleted helpers)
- Modify: `clone_test.go` (~lines 342-344 — calls the deleted `GetSessionSettings` to verify the clone copied the settings row; re-author as a direct `theDB.Where("id = ?").First(&SessionSetting{})` read or a small test helper so the copy-coverage assertion survives)
- Modify: `main_test.go` or a new `main_settings_test.go` (new regression test)

- [x] **Step 1: Simplify `getSessionConfig`**

Remove the `session.SettingsID` block entirely. The function becomes:

```go
func getSessionConfig(session *Session) (AIConfig, bool) {
	var cfg AIConfig
	var ok bool
	readConfig(func() {
		cfg, ok = config.Commands.Chats[session.ChatCommand]
	})
	return cfg, ok
}
```

Note the semantic change: `ok` is now false whenever the chat command is gone
from config, uniformly. (Today a settings row forces `ok=true` with a gutted
config.) All callers already handle `ok=false` — verify each: `irc_handlers.go:441`
(warn + ignore), `jobManager.go:333,376`, `historyCmds.go:509,686,788`,
`tui_commands.go:722,796`.

- [x] **Step 2: Delete `GetSessionSettings` and `ApplySettings` from `sessionManager.go`**

`CreateSessionSettings` stays. Update its doc comment to the new semantics:

```go
// CreateSessionSettings records the creation-time config snapshot for the
// session (what it started with) and updates the session's settings_id
// foreign key. Provenance only — stored settings are never applied to live
// turns; the live config always wins (see
// docs/superpowers/specs/2026-10-06-live-config-and-usage-attribution-design.md).
// Returns the settings row ID.
```

Delete the `{{.Vars.*}}` DESIGN NOTE — it described the abandoned change-tracking idea. `cloneDBSession` (`db.go`) copies the settings row directly and needs no change.

- [x] **Step 3: Update tests**

Delete `TestDBApplySettings`. Keep `TestDBCreateSessionSettings` (creation roundtrip is still valid). Remove any other references to the deleted functions (`grep -rn "ApplySettings\|GetSessionSettings"`).

- [x] **Step 4: Add regression test for the incident class**

New test `TestGetSessionConfigLiveConfigWins`: session with a settings row pinning `Model: "old-model"`, `ReasoningEffort: ""`; live `config.Commands.Chats` with `Model: "new-model"`, `ReasoningEffort: "low"`. Assert `getSessionConfig` returns exactly the live values — the old-model-plus-new-effort mismatch must be impossible.

- [x] **Step 5: Verify**

`go build ./... && go test ./... && go vet ./... && go fmt ./...`

---

### Task 2: Responses-chain guard across model switches

**Files:**
- Modify: `db.go` (`Session` struct ~line 53; `updateDBSessionResponseID` ~line 306)
- Modify: `sessionManager.go` (`UpdateResponseID` ~line 184; delete dead `SetResponseIDForActive` ~line 305)
- Modify: `contextLifecycle.go` (delete dead `SetContextResponseID` ~line 28)
- Modify: `aiCmds.go` (`handleResponseIDSave` ~line 438; `shouldRetryWithoutResponseID` clear-site ~line 462; `runTurnResponses` chain decision ~line 1142)
- Modify: `tui_commands.go` (clear-sites ~lines 817, 893)
- Modify: `compaction.go` (raw `Update("response_id", nil)` ~line 595)
- Modify: `responses.go` (`isResponseIDError` ~line 249)
- Modify: `tools/migrate-sqlite-to-pgsql/main.go` (Session struct copy ~line 30)
- Tests: `contextStore_test.go`, `aiCmds_test.go`, `responses_test.go`, `compaction_test.go`

- [x] **Step 1: Add `ResponseModel` to `Session`**

```go
ResponseID      *string `gorm:"column:response_id;index:idx_sessions_response_id"`
ResponseModel   *string `gorm:"column:response_model"`
```

AutoMigrate adds the column. Existing rows keep NULL — meaning "unknown", which the Step 5 check treats as chainable (preserves today's behavior for old sessions).

- [x] **Step 2: Change `UpdateResponseID` to carry the model**

```go
func (sm *SessionManager) UpdateResponseID(sessionID int64, responseID *string, model string) error
```

`updateDBSessionResponseID` writes both columns in one UPDATE: when `responseID != nil`, set `response_id` and `response_model = model`; when nil, set both to NULL. Update every call site:

- `aiCmds.go` `handleResponseIDSave`: save passes `cr.cfg.Model`; the empty-output clear passes `""` (model ignored on clear).
- `aiCmds.go` `shouldRetryWithoutResponseID`: clear passes `""`.
- `tui_commands.go` `/reinject` and `/systemmsg`: clear passes `""`.
- `compaction.go` raw reset: extend to `Updates(map[string]interface{}{"response_id": nil, "response_model": nil})`.
- Update all test call sites, including `jobManager_test.go:889` which calls the inner `updateDBSessionResponseID` directly (it gains the `model` parameter too).

The single-UPDATE form is load-bearing: it makes save-vs-clear atomic (e.g. `/reinject` clearing while a turn saves), so the row can never end up `(response_id set, response_model NULL)`.

- [x] **Step 3: Delete dead code**

`SetContextResponseID` (`contextLifecycle.go`) has no callers and its only callee is `SetResponseIDForActive` (`sessionManager.go`), whose only caller was `SetContextResponseID`. Delete both. `grep -rn "SetResponseIDForActive\|SetContextResponseID"` must come back empty.

- [x] **Step 4: Extend `isResponseIDError`**

Add to the typed-error switch (400 class, alongside the content-element check) and the string fallback:

```go
case apiErr.StatusCode == http.StatusBadRequest &&
    strings.Contains(apiErr.Message, "Reasoning input items can only be provided to a reasoning or computer use model"):
    return true
```

```go
strings.Contains(s, "Reasoning input items can only be provided to a reasoning or computer use model")
```

Extend the function's DESIGN NOTE: this wording is the observed cross-model chain failure (reasoning → non-reasoning); retrying without `previous_response_id` sends our full history, which never contains reasoning input items (encrypted reasoning content is intentionally dropped), so the retry succeeds. Layer 1 prevention is the `response_model` guard in `runTurnResponses`; this check is the Layer 2 net.

- [x] **Step 5: Guard the chain decision in `runTurnResponses`**

After loading the session (~line 1142):

```go
currentResponseID := ""
if session != nil && session.ResponseID != nil {
    currentResponseID = *session.ResponseID
}
usePrevID := cr.cfg.PreviousResponseID && currentResponseID != ""
if usePrevID && session != nil && session.ResponseModel != nil && *session.ResponseModel != cr.cfg.Model {
    cr.logger.Info("response chain model change, sending full history",
        "chain_model", *session.ResponseModel, "cfg_model", cr.cfg.Model)
    currentResponseID = ""
    usePrevID = false
}
```

NULL `ResponseModel` (legacy rows) chains as before — the Layer 2 net covers those. The streaming variant shares this decision point (it is made before the `cr.cfg.Streaming` branch), so one guard covers both.

Known scope boundaries, both intentional: the guard compares `cfg.Model` only, not `cfg.Service` — a provider switch keeping the same model name falls through to the Layer 2 net (one wasted call + WARN, then clean recovery via the 404 rule). And a pre-existing streaming oddity (empty-output retry at `aiCmds.go` ~508/522 can carry `previous_response_id` *and* full history together) is orthogonal to this change and left untouched — do not "fix" it here.

`cloneDBSession` needs no change: it builds the new `Session` as a struct literal with `ResponseID: nil`, so `ResponseModel` is implicitly nil too (and `usePrevID` requires a non-empty response id regardless). If that literal is ever converted to a struct copy, set `ResponseModel: nil` explicitly alongside `ResponseID`.

- [x] **Step 6: Tests**

- `TestUpdateResponseIDWritesModel` — save writes both columns; clear nulls both; roundtrip via `GetSession`.
- `TestRunTurnResponsesSkipsChainOnModelChange` — session with `ResponseID` + `ResponseModel: "a"`, runner cfg `Model: "b"` → request body contains full history and no `previous_response_id`; matching model chains as before.
- `TestIsResponseIDReasoningItemsMismatch` — both the typed `*openai.Error` and raw-string shapes return true.
- `TestCompactionClearsResponseModel` — extend the existing post-compaction assertion (`compaction_test.go` ~line 423) to check `ResponseModel` is nil too.
- Legacy NULL `ResponseModel` rows still chain (assert in the model-change test's counter-case).

- [x] **Step 7: Verify**

`go build ./... && go test ./... && go vet ./... && go fmt ./...`

---

### Task 3: Turn usage attribution

**Files:**
- Modify: `db.go` (`TurnUsage` struct ~line 149; `insertDBTurnUsage` ~line 328)
- Modify: `aiCmds.go` (`storeUsage` ~line 134)
- Modify: `tools/migrate-sqlite-to-pgsql/main.go` (`TurnUsage` struct copy ~line 73)
- Tests: `contextStore_test.go` (or new `usage_test.go`)

- [x] **Step 1: Add attribution columns to `TurnUsage`**

```go
Model           string `gorm:"not null;default:''"`
Service         string `gorm:"not null;default:''"`
ReasoningEffort string `gorm:"not null;default:''"`
```

Pre-existing rows backfill to `''` (unknown) — acceptable and documented in the spec. No backfill tool.

- [x] **Step 2: Plumb cfg through the insert**

Change `insertDBTurnUsage(sessionID int64, cfg AIConfig, usage *Usage, finishReason, apiPath string, durationMs int)` and populate the three columns from `cfg`. `storeUsage` (the only caller) passes `cr.cfg` — the effective config for the turn, which after Task 1 is always the live config. Compaction's summarizer usage is not persisted (unchanged, non-goal).

- [x] **Step 3: Sync the migrate tool struct**

Add the same three fields to the `TurnUsage` copy in `tools/migrate-sqlite-to-pgsql/main.go` so sqlite→pgsql copies carry them.

- [x] **Step 4: Tests**

`TestInsertTurnUsageAttribution` — insert with `AIConfig{Model: "grok-4-1-fast", Service: "xai", ReasoningEffort: "low"}`, read back, assert columns. Extend one existing `storeUsage`-path or `insertDBTurnUsage` test for the new signature. Sanity-query shape (documents the stats goal):

```sql
SELECT model, reasoning_effort, COUNT(*), SUM(reasoning_tokens)
FROM turn_usage GROUP BY model, reasoning_effort;
```

- [x] **Step 5: Verify**

`go build ./... && go test ./... && go vet ./... && go fmt ./...`

---

### Task 4: Stop writing `messages.settings_id`

**Files:**
- Modify: `db.go` (`Message` struct ~line 87 — remove `SettingsID` field)
- Modify: `compaction.go` (remove the three `SettingsID:` assignments ~lines 537, 548, 575)
- Modify: `tools/migrate-sqlite-to-pgsql/main.go` (`Message` struct copy ~line 54)
- Tests: compile-driven fixes only

- [x] **Step 1: Remove the field and writes**

Delete `SettingsID *int64` from `Message` and the three assignments in compaction (fresh system row, summary row, tail-copy loop). The DB column stays (nullable, forever NULL going forward) — no destructive migration. `Session.SettingsID` is untouched (still written at creation, still copied by clone).

Mechanism note for the migrate tool: `migrateTable` copies struct-declared fields only, so removing the field from the struct silently stops copying an all-NULL column — no error, nothing to migrate. (The tool's `Message` copy is already missing other columns — `Archived`, `CompactionID`, etc. — pre-existing, out of scope.)

- [x] **Step 2: Sync the migrate tool struct and fix tests**

Remove the field from the migrate tool's `Message` copy. Fix any test that constructs `Message` with `SettingsID` — compile errors are the checklist.

- [x] **Step 3: Verify**

`go build ./... && go test ./... && go vet ./... && go fmt ./...`

---

### Task 5: Documentation

**Files:**
- Modify: `AGENTS.md` (High-Signal Gotchas)
- Modify: `docs/cloning.md` (~line 173)

- [x] **Step 1: AGENTS.md — update the Responses API bullet**

Append to the existing `previous_response_id` sentence: chains are never continued across a model change — `sessions.response_model` is written alongside `response_id`, `runTurnResponses` falls back to full history on mismatch, and `isResponseIDError` additionally treats the reasoning-item mismatch wording as a chain error (Layer 2 net; NULL `response_model` legacy rows still chain and rely on the net). While editing that bullet, fix its stale `ChatContext.ResponseID` wording (no `ChatContext` type exists anymore) — response ids live on the `Session` row.

- [x] **Step 2: AGENTS.md — new session-settings bullet**

Under High-Signal Gotchas: `session_settings` is creation-time provenance only ("what the session started with"); the live config always wins on every path (`getSessionConfig` has no overlay); the new system prompt lands at the next compaction via `renderFreshSystemPrompt`; `turn_usage` carries `model`/`service`/`reasoning_effort` per turn for cost and reasoning stats; `messages.settings_id` is dead (column remains, never written). Reference the spec doc.

- [x] **Step 3: `docs/cloning.md` line 173**

Replace "If session has `settings_id`, load stored settings and overlay via `ApplySettings`" with a note that clone uses the live config for the chat command (the copied settings row is provenance only). Add a one-line "Superseded 2026-10-06" pointer to the spec where the clone design mentions settings overlay.

- [x] **Step 4: Verify**

`grep -rn "ApplySettings" --include="*.go" --include="*.md"` → only the spec/plan/history docs. `go build ./... && go test ./...`
