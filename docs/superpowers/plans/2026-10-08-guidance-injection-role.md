# Guidance Injection Role — Implementation Plan

Spec: `docs/superpowers/specs/2026-10-08-guidance-injection-role-design.md`
(all decisions resolved: D1 `injection_role`, D2 correction follows Knob 1,
D5 developer config-only, D6 TUI commands untouched, D7 correction persists).

## Tasks

### 1. Types + config (`types.go`, `config.go`)
- Add `RoleDeveloper = "developer"`.
- `AIConfig`: add `InjectionRole string` (`toml:"injection_role"`);
  change `NeedsUserSuffix bool` → `*bool` (`toml:"needsusersuffix"`).
- `Service`: add both fields (service-level cascade sources).
- `ApplyDefaults`: `InjectionRole` cascade command > service, then
  validate ∈ {system, developer, user} — invalid values FAIL command load
  (error-returning validation in the chats/services load path), empty →
  `"system"`. `NeedsUserSuffix` cascade command > service only (stays nil
  when unset — resolution is model-dependent, done at call time).

### 2. Shared mechanism (`guidance.go`, new)
- `guidanceRole(cfg AIConfig) string` — Knob 1 resolution; empty →
  `system`; unknown (hand-built cfgs) → `system` defensively.
- `needsUserSuffix(cfg AIConfig) bool` — Knob 2 resolution: explicit
  `*bool` wins (false even for anthropic/), nil → `modelNeedsUserSuffix`.
- `guidanceMessages(cfg AIConfig, content, suffixText string, trailing bool) []ChatMessage`
  — payload row under resolved role; appends the user suffix row only
  when `trailing && role != user && needsUserSuffix(cfg)`.
- `messagesEndWith(msgs, seq []ChatMessage) bool` — tail equality on
  role+content, for in-turn dedupe.

### 3. Wire `developer` through serializers
- `chatCompletion.go` `messagesToChatCompletionParams`: `RoleDeveloper`
  → `openai.DeveloperMessage(content)` (SDK v3 constructor exists).
- `responses.go` `messagesToResponseInputItems`: `RoleDeveloper` →
  `EasyInputMessageRoleDeveloper`.
- `compaction.go` summarizer transcript: explicit `[developer] ` tag
  (default branch already handles unknown roles; explicit for clarity).

### 4. Async site (`jobManager.go`)
- Rewrite `injectAsyncResultFromDB` to emit `guidanceMessages(cfg,
  content, asyncUserSuffix, trailing=true)` rows; async suffix text kept
  EXACTLY as today ("Respond to the user based on the above background
  task result."). Behavior identical for all existing configs; payload
  role now honors Knob 1; `needsusersuffix = false` can now suppress the
  anthropic auto-suffix.

### 5. Empty-response correction (`aiCmds.go`)
- `addEmptyResponseCorrection(turn, cfg, reasoning)`: payload role via
  Knob 1 (default system), PERSISTS via `turn.Add` (D7), dedupe via
  `messagesEndWith` (payload+suffix tail), suffix row per Knob 2 with
  correction-specific text. Drop the hardcoded RoleUser + marker design;
  keep the `[automated notice, not from the user]` content marker.
- Remove `turnContext.AddEphemeral` (no remaining caller) and its test.

### 6. Incident log (`incident.go`)
- `sanitizedAIConfig`: `NeedsUserSuffix` stays JSON bool (deref, nil →
  false); add `InjectionRole`.

### 7. Config docs (`config/chats.toml`, `config/services.toml`)
- Reference lists + commented examples for `injection_role` (and
  `needsusersuffix` cascade semantics) in both files.

### 8. Tests
- `guidance_test.go`: role table, suffix table (explicit override,
  auto-detect, user-payload suppression, followed sites), dedupe helper.
- `config_test.go`: ApplyDefaults cascade cases for both knobs;
  invalid `injection_role` rejected at load.
- `jobManager_test.go`: update NeedsUserSuffix cases to `*bool`; add
  developer/user payload role cases.
- `aiCmds_test.go`: correction now system-role by default (update the
  `"role":"user"` request-body assertions to `"role":"system"` +
  marker); correction persists in DB after retry (D7); remove
  AddEphemeral test; TestAddEmptyResponseCorrection updated (role,
  suffix, dedupe-with-suffix).

### 9. AGENTS.md
- Update the empty-response bullet (persisted correction, Knob 1 role,
  Knob 2 suffix; drop AddEphemeral references); add the guidance
  injection bullet pointing at the spec.

### 10. Verify
- `go build`, `go vet`, `go fmt`, full `go test ./...`, `-race` on
  touched tests; mandatory code review afterwards.
