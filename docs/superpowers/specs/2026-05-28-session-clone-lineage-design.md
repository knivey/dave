# Session Clone Lineage

## Problem

When a user clones a session via `^clone$`, the new session has no persistent link back to the source session. The clone success notice shows `{source_id}` ephemerally, but after that there is no way to tell which session a clone came from or who owned it.

## Design

Add two nullable columns to the `sessions` table to track clone lineage:

- `cloned_from_id *int64` — FK to the source `sessions.id`
- `cloned_from_nick string` — snapshot of the source owner's nick at clone time

`cloned_from_nick` serves as a fallback when the source session has been soft-deleted and its user row is no longer reachable. For sessions that were not created by cloning, both fields are null/empty and nothing is displayed.

### Schema changes

Migration #8 (`add_sessions_cloned_from`):

```sql
ALTER TABLE sessions ADD COLUMN cloned_from_id INTEGER REFERENCES sessions(id);
ALTER TABLE sessions ADD COLUMN cloned_from_nick TEXT NOT NULL DEFAULT '';
```

Both SQLite and PostgreSQL variants. Idempotent via `HasColumn` guard. No index needed — the field is only read when displaying a specific session, never used as a query filter.

### Data flow

**Write path** (`cloneDBSession` in `db.go`):

1. After loading the source session, resolve the source owner's current nick.
2. Set `ClonedFromID = &sourceSession.ID` and `ClonedFromNick = resolvedNick` on the new `Session` struct before `tx.Create`.
3. If nick resolution fails (e.g. user row deleted), set `ClonedFromNick = ""` — the display fallback chain handles this.

The caller `historyClone` in `historyCmds.go` already resolves the source nick (lines 739-748). Pass it into `cloneDBSession` via a new parameter.

**Read path — session list** (`sendSessionsLines` and `sendSessionsLinesWithNick` in `historyCmds.go`, `tuiCmdSessions` in `tui_commands.go`):

After rendering the normal session line (icon, id, msg count, time, trigger+command, preview), if `ClonedFromID != nil`, append a cloned-from suffix:

```
  ● #55  12 msgs  2m ago  !chat  hello... [cloned from #42 by alice]
```

The suffix is rendered by a helper function `formatClonedFrom(session Session) string` that implements the three-tier fallback:

1. **Source session exists** — query the source session (with `Unscoped` to include soft-deleted) and its user to get the current nick. Display: `[cloned from #{id} by {nick}]`
2. **Source session soft-deleted or user gone, nick snapshot available** — use `ClonedFromNick`. Display: `[cloned from #{id} by {nick}]`
3. **Both missing** — display: `[cloned from #{id} (deleted session)]`

**N+1 avoidance**: `sendSessionsLines` and `tuiCmdSessions` iterate sessions in a loop. Rather than calling `formatClonedFrom` per-session (which would issue a query per cloned session), the caller collects all non-nil `ClonedFromID` values, issues a single batch query (`SELECT ... WHERE id IN (...)`) for source sessions joined with users, and builds a `map[int64]string` of source ID → nick. `formatClonedFrom` accepts this map as a parameter.

**Read path — session detail header** (`historyShow` in `historyCmds.go`):

Add `{cloned_from}` placeholder to the `detail_header` notice template. If the session has a `ClonedFromID`, populate it with the same `formatClonedFrom` output. If not, set it to `""`. The default template appends it after the existing content.

### Notice template changes

Add one new placeholder to `sessions.detail_header`:

- `{cloned_from}` — pre-rendered string like `[cloned from #42 by alice]` or empty

No new notice template keys needed. The cloned-from suffix in session list lines is rendered inline (not template-driven) since session list lines are already assembled via `fmt.Sprintf`.

### TUI display

`tuiCmdSessions` in `tui_commands.go` applies the same `formatClonedFrom` logic, appending the suffix after the preview. TUI uses tview color tags (`[yellow]...[white]`) instead of IRC color codes for the suffix.

### What is NOT changing

- No backfill of existing cloned sessions.
- No new IRC commands or TUI commands.
- No changes to how cloning works — only tracking and display.
- No index on `cloned_from_id` (not a query path).

### Files touched

| File | Change |
|------|--------|
| `db.go` | Add `ClonedFromID`/`ClonedFromNick` to `Session` struct; update `cloneDBSession` signature to accept source nick and set the fields |
| `migrations.go` | Add migration #8 `add_sessions_cloned_from` |
| `historyCmds.go` | Pass source nick into `cloneDBSession`; add `formatClonedFrom` helper; update `sendSessionsLines`/`sendSessionsLinesWithNick` to append suffix; update `historyShow` to populate `{cloned_from}` placeholder |
| `tui_commands.go` | Update `tuiCmdSessions` to append cloned-from suffix |
| `config/notices.toml` | Add `{cloned_from}` to `detail_header` default (empty when not a clone) |

### Tests

- Unit test for `formatClonedFrom` with the three fallback tiers (mock the source session query).
- Integration test for `cloneDBSession` verifying `ClonedFromID` and `ClonedFromNick` are set correctly.
- Config test that `detail_header` template renders with `{cloned_from}` placeholder.
