# Aliases Replace Regex — Design

**Date:** 2026-06-02
**Status:** Draft

## Summary

Replace the regex-based command trigger system with a simpler alias system. Commands (chats, completions, and tools) no longer use regex patterns for matching. Chats and completions gain an `aliases` field — a list of additional trigger names that invoke the same command. Tools use only their section name as the trigger (no aliases, no regex).

The user experience: aliases work transparently. `!gpt hello` and `!chat hello` invoke the same command if `gpt` is an alias of `chat`. Help listings show all triggers. There is no distinction visible to the end user.

## Motivation

Regex triggers are overpowered for the common case. Almost all commands use their section name as the literal trigger. The few commands that use actual regex patterns (e.g. `dave?`) are rare and can be expressed as explicit aliases. Removing regex eliminates:

- Accidental trigger collisions from overlapping patterns
- Confusion about regex syntax in config
- Regex compilation overhead in dispatch
- Complexity in help display (the `(regex)` marker, the Regex column)

Aliases are simpler to configure, validate, and reason about.

## Scope

**In scope:**
- Remove `Regex` field from `AIConfig` and `MCPCommandConfig`.
- Add `Aliases []string` field to `AIConfig` (chats + completions only).
- Update dispatch from regex iteration to `map[string]CmdFunc` lookup.
- Update help system (IRC table, per-command lookup, pastebin GFM).
- Update disabled command validation.
- Update config file documentation blocks.
- Update tests.

**Out of scope:**
- Built-in commands (stop, help, sessions, etc.) — keep their hardcoded regex patterns.
- TUI commands — separate system, unaffected.
- Automatic config migration.

## Config Changes

### AIConfig (chats.toml + completions.toml)

- **Remove:** `Regex string` field and its TOML `regex` tag.
- **Add:** `Aliases []string` with TOML tag `aliases`.
- The section name (TOML key) remains the **canonical name** and is always a trigger.
- `Aliases` is optional (defaults to nil/empty).

Example:

```toml
[chat]
description = "General IRC-aware chat"
aliases = ["gpt", "ask", "ai"]
service = "openai"
model = "gpt-4o"
```

Triggers for this command: `chat`, `gpt`, `ask`, `ai`.

### MCPCommandConfig (tools.toml)

- **Remove:** `Regex string` field.
- The section name is the only trigger. No aliases.
- The `arg` field still controls whether the command takes arguments.

### Validation

In `validateAIConfig` / `validateCommands`:

- Remove the `if cfg.Regex == "" { cfg.Regex = name }` defaulting.
- `cfg.Name = name` stays as-is.
- New: collect all triggers (canonical name + all aliases for AI configs; canonical name only for tools) across chats, completions, and tools. Error on first collision. Aborts reload (previous `configCmds` preserved).

## Dispatch Internals

### Replacing CmdMap

The current `map[*regexp.Regexp]CmdFunc` is replaced by:

```go
type CmdFunc func(Network, *girc.Client, girc.Event, context.Context, chan<- string, ...string)

// Config command dispatch tables (atomically replaced on /reload)
var configCmds     map[string]CmdFunc   // trigger -> handler
var configCmdNames map[string]string    // trigger -> canonical name
var rateExemptCmds map[string]bool      // trigger -> skipbusy
var chatCmds       map[string]bool      // trigger -> is chat command
```

Builtins keep their current `map[*regexp.Regexp]CmdFunc` since they have patterns like `^help(?:\s+(.+))?$` that aren't simple name matches. No change to builtin dispatch.

### Trigger Matching

For config commands, dispatch becomes a plain map lookup:

1. Strip trigger prefix from message -> get `stripped` (e.g. `"gpt hello there"`).
2. Split on first whitespace: `triggerWord, rest, hasArgs = splitFirstWord(stripped)`.
3. Lookup `configCmds[triggerWord]`. If not found, fall through.
4. Check arg compatibility:
   - **Completions and chats**: always require args. If `!hasArgs`, no match (fall through).
   - **Tools with `arg` set**: require args. If `!hasArgs`, no match.
   - **Tools without `arg`**: reject args. If `hasArgs`, no match.
5. If compatible, `args = [rest]` (or `[]` for no-arg tools).

A fifth map `var configCmdTakesArgs map[string]bool` tracks whether each trigger expects args, set during registration based on command type and `arg` field.

This replaces regex compilation/matching with a simple split + map lookup. O(1) instead of O(n) regex matches.

### registerCommandsLocked Changes

For each command (completion/chat/tool), insert one map entry per trigger:
- Canonical name -> handler closure
- Each alias -> same handler closure

All four maps (`configCmds`, `configCmdNames`, `rateExemptCmds`, `chatCmds`) get the same set of keys. Build into local maps first, then atomic swap (same pattern as today).

### Dispatch Order in handleTrigger

1. Builtins first (regex match, unchanged).
2. Config commands (map lookup by trigger word).
3. Disabled check uses `configCmdNames[trigger]` to get the canonical name, then `isNetworkCommandDisabled(network, canonical)`.

## Help System

### !help (no args) — IRC Table

`buildHelpText` changes:

- **Remove** the `(regex)` marker from `formatCmd`. It no longer exists.
- **Remove** the intro line referencing regex.
- `formatCmd(trigger, name, aliases)` returns multi-line text: canonical trigger first, then each alias, all prefixed with the trigger character. Example for `[chat]` with `aliases = ["gpt", "ask"]`:
  ```
  !chat
  !gpt
  !ask
  ```
- `formatTable` handles multi-line cells in the first column. The widest alias set determines the row height. Other columns (info, desc) stay single-line and align to the first line of the cmd column.

### !help (name) — Per-Command Lookup

`findCommandHelp` changes:

- `matchesCommand(cmdName, name, aliases)` replaces `matchesCommand(cmdName, name, regex)`. Match if `cmdName == name` or `cmdName` is in `aliases`.
- No more `regexp.MustCompile` in the help path.

### Pastebin Help (GFM Markdown)

`buildPastebinHelpText` changes:

- **Remove** the `Regex` column from `writeGFMCmdTable` and `writeGFMToolTable`.
- `pastebinEntry.regex bool` field removed.
- Command column shows all triggers. For multi-alias commands, join with `<br>` (valid in GFM table cells for line breaks within a cell):

  ```
  | Command | Service | Model | Media | Description |
  |---------|---------|-------|-------|-------------|
  | chat<br>gpt<br>ask | openai | gpt-4o |  | General IRC-aware chat |
  ```

### helpEntry Struct

- Remove the `regex` parameter from builders. `helpEntry.cmd` becomes `[]string` (slice of trigger strings: canonical first, then aliases) instead of a single string with `(regex)` suffix.

### Sorting

- `sortedAIConfigEntries` / `sortedPastebinEntries` — remove regex references. No change to sort logic otherwise (still sorts by info then name).

## Disabled Commands

### `disabled_commands` (per-network)

- Only canonical command names are valid entries.
- Specifying a canonical name disables all aliases for that command too. E.g. `disabled_commands = ["chat"]` disables `!chat`, `!gpt`, and `!ask`.
- `validateNetworkDisabledCommands` validates against canonical names only (builtins + completions + chats + tools).
- If a network lists an alias in `disabled_commands`, validation errors with: `"<alias>" is an alias of "<canonical>", use the canonical name "<canonical>" instead`.

### `disabled_builtins` (root)

Unchanged. Same semantics for builtins.

## Config File Documentation Updates

Per the project convention (reference section + commented example block at top of each TOML file):

- **chats.toml**: Remove `regex` from reference list, add `aliases` (type: string list, default: `[]`). Update commented example block.
- **completions.toml**: Same changes as chats.toml.
- **tools.toml**: Remove `regex` from reference list. Update commented example block.

Live config sections: remove any `regex =` lines. Convert multi-trigger regex patterns to `aliases` where applicable.

## Migration

This is a **breaking config change**. The `regex` field is removed from all three command config types. Existing configs using `regex = "..."` will fail to load.

### Migration Steps for Admins

1. Commands where `regex` equals the section name: remove the `regex` line.
2. Commands where `regex` differs but is a literal word (e.g. `[dave]` with `regex = "bot"`): remove `regex`, add `aliases = ["bot"]`.
3. Commands using actual regex patterns (e.g. `regex = "dave?"`): decide on explicit aliases or rename the section.
4. Tools using non-name regex: rename the section or drop the regex.

No automatic migration. Documented in release notes.

## Testing

### New Tests

- **Alias dispatch**: verify each alias triggers the correct command handler with correct args.
- **Alias collision detection**: verify reload fails when two commands share a trigger (alias-alias, canonical-alias, canonical-canonical, tool-vs-alias).
- **Help table rendering**: verify multi-line cmd column format with 0, 1, and many aliases.
- **Help lookup by alias**: verify `!help gpt` finds the `chat` command.
- **Disabled by canonical disables aliases**: verify `disabled_commands = ["chat"]` blocks `!gpt` and `!ask`.
- **Disabled validation rejects alias**: verify `disabled_commands = ["gpt"]` fails validation with helpful message.
- **Pastebin format**: verify GFM table has no Regex column and uses `<br>` for multi-trigger commands.

### Updated Tests

- All tests that set `Regex` on `AIConfig` or `MCPCommandConfig` — remove the field or switch to `Aliases`.
- `createTestConfigDir` helper — update any embedded TOML that uses `regex =`.
- `TestLoadConfigDir*` tests that validate regex defaulting — remove or adapt.
- `matchesCommand` tests — update signature.
- `registerCommands` tests — update for map-based dispatch.

### Test Commands

```
go test ./...
go fmt ./...
go vet ./...
```
