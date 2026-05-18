# db-mcp: LLM Notebook MCP Server

## Overview

A standalone MCP server at `mcps/db-mcp/` that gives the LLM a persistent notebook — a simple key-value store scoped per IRC channel. The LLM can store, retrieve, search, and organize freeform text notes. Uses its own SQLite database, completely separate from dave's main DB.

**Transport:** Stdio only (no HTTP mode).

## Data Model

### `notes` table

| Column | Type | Description |
|--------|------|-------------|
| id | INTEGER PK AUTOINCREMENT | Note ID |
| network | TEXT NOT NULL | IRC network name |
| channel | TEXT NOT NULL | IRC channel |
| user_id | INTEGER NOT NULL | Dave's resolved user_id |
| nick | TEXT NOT NULL | User's nick at time of storage (snapshot) |
| key | TEXT NOT NULL | Tag/category for the note |
| value | TEXT NOT NULL | The note content |
| created_at | DATETIME NOT NULL | When it was stored |
| updated_at | DATETIME NOT NULL | Last modification |

- Keys are **not unique** per scope. Multiple notes can share the same key — each `put_note` creates a new row.
- `nick` is a snapshot at storage time. If the user changes nick, old notes keep the old nick. The stable `user_id` handles cross-referencing.

### Indexes

- `idx_notes_scope_key (network, channel, key)` — key lookups
- `idx_notes_scope_user (network, channel, user_id)` — user filtering
- `idx_notes_scope_nick (network, channel, nick)` — nick lookups
- `idx_notes_scope_time (network, channel, created_at)` — time-range queries

### FTS5 virtual table

`notes_fts` on the `value` column for full-text search with BM25 ranking. Content synced via triggers or application-level writes.

## Scope Injection Convention

Dave passes context (network, channel, user_id, nick) to MCP tools automatically. The LLM never sees or fills these fields.

**Mechanism:**
- MCP tool input structs include fields prefixed with `_dave_inject_`:
  - `_dave_inject_network`
  - `_dave_inject_channel`
  - `_dave_inject_user_id`
  - `_dave_inject_nick`
- Dave inspects MCP tool schemas at connection time. Any field prefixed `_dave_inject_` is auto-filled from the current chat context and hidden from the LLM's view of the schema.
- Any future MCP can opt into scope injection by using these field names — no extra config needed.

## Tools

All tools have `_dave_inject_network`, `_dave_inject_channel`, `_dave_inject_user_id`, `_dave_inject_nick` as hidden required fields (injected by dave). The LLM only sees the parameters below.

### `put_note`
Store a new note. Multiple notes can share the same key — each call creates a new entry.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| key | string | yes | Tag/category for the note |
| value | string | yes | The note content |

Returns: `{ id: int, key: string, created_at: string }`

### `get_notes`
Get all notes matching a key in the current channel scope.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| key | string | yes | Key to look up |
| filter_nick | string | no | Only return notes from this nick |

Returns: `{ notes: [{ id, key, value, nick, created_at, updated_at }] }`

### `search_notes`
Full-text search on note values within the current channel scope.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| query | string | yes | FTS5 search query |
| filter_key | string | no | Only search notes with this key |
| filter_nick | string | no | Only search notes from this nick |
| within | string | no | Relative time offset (e.g. "1h", "7d", "30d") |
| limit | int | no | Max results (default 20) |

Returns: `{ notes: [{ id, key, value, nick, created_at, updated_at }], total: int }`

### `recent_notes`
Get recent notes by time range within the current channel scope. No text search — timeline browsing.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| within | string | yes | Relative time offset (e.g. "1h", "24h", "7d") |
| filter_key | string | no | Only notes with this key |
| filter_nick | string | no | Only notes from this nick |
| limit | int | no | Max results (default 20) |

Returns: `{ notes: [{ id, key, value, nick, created_at, updated_at }], total: int }`

### `delete_note`
Delete a single note by ID. Only the note owner can delete (verified by user_id).

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| id | int | yes | Note ID to delete |

Returns: `{ deleted: bool }`

### `delete_notes`
Delete all notes with a given key belonging to the current user.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| key | string | yes | Key to delete all notes for |

Returns: `{ deleted_count: int }`

### `list_keys`
List distinct keys in the current channel with note counts.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| filter_nick | string | no | Only count notes from this nick |
| limit | int | no | Max keys to return (default 50) |

Returns: `{ keys: [{ key: string, count: int }] }`

### `count_notes`
Count notes matching filters in the current channel scope.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| filter_key | string | no | Only count notes with this key |
| filter_nick | string | no | Only count notes from this nick |
| within | string | no | Only count notes within this time offset |

Returns: `{ count: int }`

## Config

### `config.toml` (optional — runs with defaults if absent)

```toml
[database]
# Path to SQLite database (relative to binary directory)
# Default: "data/notes.db"
path = "data/notes.db"
# Max characters per note value (truncated with [truncated] suffix)
# Default: 10000
max_value_size = 10000
# Max notes per user across all channels (oldest auto-pruned)
# Default: 500
max_notes_per_user = 500

[server]
# Default: "db-mcp"
name = "db-mcp"
# Default: "0.1.0"
version = "0.1.0"
```

### Dave integration (`mcps.toml`)

```toml
[db-mcp]
transport = "stdio"
command = "mcps/db-mcp/db-mcp"
args = []
timeout = "30s"
```

## Directory Structure

```
mcps/db-mcp/
  main.go            # entry point, CLI flags, signal handling, server wiring
  config.go          # TOML config structs + loadConfig() + defaults
  tools.go           # ToolHandlers struct, Input/Output types, handler methods
  db.go              # SQLite init, goose migrations, query functions
  logging.go         # logxi logger init (same pattern as yt-mcp)
  config.toml        # live config (gitignored)
  example.toml       # reference documentation with all options
  data/              # SQLite database at runtime
  migrations/        # goose SQL migrations (001_init.sql)
```

Build: `go build -o mcps/db-mcp/db-mcp ./mcps/db-mcp`

## Error Handling

### Delete safety
- `delete_note(id)` verifies the note belongs to the current `(network, channel, user_id)` before deleting. Returns error if not owned.
- `delete_notes(key)` scoped to current user only. No cross-user deletion.

### Auto-pruning
- On `put_note`, after insert, MCP checks total note count for that `(network, user_id)`. If over `max_notes_per_user`, deletes oldest notes (by `created_at`) until under limit.
- Pruning is per-user across all channels — a user's notes in every channel count toward their limit.

### Value limits
- Values exceeding `max_value_size` characters are truncated with `[truncated]` appended.

### Search behavior
- FTS5 MATCH on `value` column. Results ranked by `bm25()`.
- `within` parsed as Go duration (`"1h"`, `"7d"`, `"30d"`). Parsed by custom function that supports `d` suffix (converted to hours). Applied as `created_at > now - offset`.
- Empty/missing `within` = no time filter (all time).

### Empty results
- Queries with no matches return empty lists, not errors.
- `search_notes` with no matches includes `"no notes found matching query"` in the response.

### Invalid input
- Empty `key` or `value` on `put_note` → error.
- Empty `query` on `search_notes` → error.
- Invalid `within` format → error with expected format hint (e.g., `"expected duration like 1h, 24h, 7d, 30d"`).

## Architecture Decisions

### Why SQLite
Single-file database, no server process, follows img-mcp pattern. Good enough for per-channel notes. The MCP is long-lived (spawned by dave), so connection pooling isn't a concern.

### Why goose for migrations
Consistent with img-mcp. Simple SQL-based migrations for schema evolution.

### Why per-channel scoping
Notes are tied to the conversation context. The LLM in `#chan1` has no reason to see notes from `#chan2`. User_id is metadata for filtering, not an isolation boundary.

### Why relative time offsets
The LLM doesn't know the current time precisely. Relative offsets (`"1h"`, `"7d"`) let it query "recent" notes without needing absolute timestamps. The MCP resolves them against `time.Now()` at query time.

### Why graceful missing config
If `config.toml` is absent, the MCP runs with built-in defaults. This matches yt-mcp's pattern and makes first-run simpler — just build and connect. A config file is only needed to override defaults.

### Why `_dave_inject_` prefix
Schema-based injection is more elegant than config flags. The prefix is dave-specific and self-documenting — any developer reading an MCP's tool struct immediately knows these fields are auto-filled. Any MCP can opt in by convention without dave config changes.
