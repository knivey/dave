# GFM Pastebin Help

## Problem

The help text uploaded to pastebin is currently plain IRC-formatted text wrapped in a code block. The pastebin service supports GFM markdown, so we can produce much more readable output using headings, tables, and styled sections. Additionally, chat commands (the most commonly used) are buried below history and completions, and there's no example showing how to actually use the bot.

## Requirements

1. Pastebin help uses GFM markdown (headings, tables, bold, code blocks)
2. Chat commands appear first (highest visibility)
3. A realistic example chatlog section appears directly after chat commands, showing session start and nick-addressed continuation
4. IRC in-channel help output (when pastebin is off or output is short) remains unchanged
5. For help specifically, no preview lines before the pastebin link — just post the link
6. The `no_context` notice in `irc_handlers.go` also uses the new GFM pastebin help

## New Function

`buildPastebinHelpText(botnick, trigger string, network Network) string` in `help.go`

Generates a standalone GFM markdown document. Does not share code with `buildHelpText` (different formatting language — IRC codes vs markdown).

## Document Structure (in order)

```
# <botnick> Help

Intro paragraph (who I am, how sessions work, regex note).

---

## Chat Commands

GFM table: | Command | Service/Model | Description |
Chats only (AIConfig entries from config.Commands.Chats).

## Example Session

Fenced code block (no syntax highlighting) showing a realistic IRC chatlog:
- User triggers a chat command
- Bot responds
- User continues by addressing bot nick
- Bot replies
- User runs !stop

## Completions

GFM table: | Command | Service/Model | Description |
Completions only (AIConfig entries from config.Commands.Completions).

## Tool Commands

GFM table: | Command | MCP/Tool | Description |
MCP tool commands from config.Commands.Tools.

## History & Sessions

GFM table: | Command | Description |
All history builtins (sessions, history, resume, delete, mystats, jobs, compact, clone).
Only shown when theDB != nil.

## Built-in Commands

GFM table: | Command | Description |
stop, support (filtered by isNetworkCommandDisabled).

## MCP Servers

Bullet list of MCP server info (same data as getAllMCPServerInfo, reformatted).
```

## Example Session Content

The example uses a fictional user "alice" with trigger "!":

```
<alice> !chat hey dave, what's a good recipe for pasta?
<dave> Hey Alice! Here's a classic and simple one — Cacio e Pepe...
<alice> dave, can you make it vegetarian?
<dave> Sure! Swap the pecorino for a good vegetarian alternative...
<alice> !stop
<dave> Session paused.
```

The example uses the actual botnick and trigger from the function arguments so it always matches the live config.

## Changes to Existing Code

### `help.go` — `help()` function

- When uploading to pastebin, call `buildPastebinHelpText` instead of wrapping `buildHelpText` in code fences
- Remove the 3-line preview before the pastebin link — just post the link
- Non-pastebin path (IRC in-channel) continues to use `buildHelpText` unchanged

### `irc_handlers.go` — no_context notice path

- `uploadToPastebin("```\n"+helpText+"\n```", "Dave Help")` changes to `uploadToPastebin(buildPastebinHelpText(...), "Dave Help")`
- Same title, different content

### Tests

- New test `TestBuildPastebinHelpText` covering:
  - Basic output contains expected headings and tables
  - Chat commands appear before other sections
  - Example session uses correct botnick and trigger
  - Disabled commands are excluded
  - Empty sections are omitted
  - `no_context` pastebin upload uses new builder
- Existing `TestBuildHelpText*` tests unchanged (IRC path unchanged)

## What Does NOT Change

- `buildHelpText` — IRC plain text builder stays as-is
- `buildAIConfigEntry`, `formatTable`, `formatCmd`, etc. — used by IRC path only
- Pastebin API/config — no changes to pastebin.go or PastebinConfig
- `findCommandHelp` — per-command help (IRC only) unchanged
- Other pastebin uses (AI responses, etc.) — only help command affected
