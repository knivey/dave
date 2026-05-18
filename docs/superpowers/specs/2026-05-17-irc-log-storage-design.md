# IRC Log Storage Design

## Summary

Store all IRC channel traffic in rotating SQLite files (configurable period — monthly by default) with a buffered batch writer. Captures PRIVMSG, NOTICE, JOIN, PART, QUIT, KICK, NICK, TOPIC, and MODE events from all joined channels, including the bot's own messages.

## Requirements

- Log all channel traffic from all joined channels (no per-channel opt-in/out)
- Store full event spectrum: PRIVMSG, NOTICE, JOIN, PART, QUIT, KICK, NICK, TOPIC, MODE
- Include the bot's own outgoing messages
- Store raw nick/ident/host (no user_id FK)
- Separate SQLite files from the main bot DB
- Batch writes to handle flood volumes without blocking IRC event processing
- Configurable period rotating SQLite files (monthly by default: `YYYY-MM.db`; yearly: `YYYY.db`)
- No automatic deletion — admin manages retention manually

## Data Model

Each rotating SQLite file (name depends on `rotation` config) contains:

### `irc_logs` table

| Column      | Type      | Description                                              |
|-------------|-----------|----------------------------------------------------------|
| id          | INTEGER   | PRIMARY KEY AUTOINCREMENT                                |
| network     | TEXT      | NOT NULL — network name from config                      |
| channel     | TEXT      | NOT NULL — channel name (as-is, not normalized)          |
| command     | TEXT      | NOT NULL — IRC command (PRIVMSG, NOTICE, JOIN, etc.)     |
| nick        | TEXT      | source nick (NULL for some events)                       |
| ident       | TEXT      | source ident                                             |
| host        | TEXT      | source hostname                                          |
| target      | TEXT      | target param (channel, nick, or empty)                   |
| message     | TEXT      | message text (last param for PRIVMSG/NOTICE, etc.)       |
| created_at  | DATETIME  | NOT NULL — event timestamp from girc                     |

### Event field mapping

| Event   | nick       | target              | message                    |
|---------|------------|---------------------|----------------------------|
| PRIVMSG | sender     | channel             | message text               |
| NOTICE  | sender     | channel             | notice text                |
| JOIN    | joiner     | channel             | empty                      |
| PART    | parter     | channel             | part reason (or empty)     |
| QUIT    | quitter    | empty               | quit message               |
| KICK    | kicker     | kicked user         | kick reason                |
| NICK    | old nick   | new nick            | empty                      |
| TOPIC   | setter     | channel             | new topic text             |
| MODE    | setter     | channel or target   | mode string (e.g. "+o nick") |

### Indexes

- `idx_irc_logs_channel_time (network, channel, created_at)` — time-range queries per channel
- `idx_irc_logs_nick_time (network, nick, created_at)` — per-user lookups
- `idx_irc_logs_created_at (created_at)` — general time queries

### `schema_meta` table

Single row tracking the schema version for future migrations:

| Column   | Type    |
|----------|---------|
| key      | TEXT PRIMARY KEY |
| value    | TEXT    |

## Writer Architecture

### LogWriter

Singleton responsible for all log writes. Owns a channel-backed buffer and a single goroutine that drains it.

**Write path**: IRC event → `LogWriter.Enqueue()` → buffered channel → goroutine collects batch → INSERT transaction → done

**Components**:

1. **Buffer**: `chan LogEntry` sized by `buffer_size` config (default 10000). Non-blocking send — if buffer is full, the event is dropped and a `dropped` counter increments. A periodic log warning reports drops.

2. **Batch goroutine**: Single goroutine drains the channel, collects rows into a batch, and writes when either:
   - Batch reaches `batch_size` rows (default 500), OR
   - `flush_interval` elapses (default 2s) since last flush, OR
   - Shutdown signal received
   
   Batch writes execute inside a single SQLite transaction (`BEGIN` / `INSERT ... VALUES (...), (...), ...` / `COMMIT`). This is critical for performance — without a transaction, each INSERT is its own transaction with a full fsync, which reduces throughput by 10-100x on SQLite. The entire batch is committed atomically.

3. **File rotation manager**: Tracks the currently open SQLite file. When a new period is detected (event timestamp crosses the rotation boundary), closes the current file and opens/creates the new one. Each file gets its schema (table + indexes) created on first use. Rotation period is configurable via `rotation` config — `"monthly"` (default) produces `YYYY-MM.db`, `"yearly"` produces `YYYY.db`.

4. **Shutdown**: `stopLogWriter()` signals the goroutine to flush remaining buffer, close the current file, and exit. Registered with the main `wg` WaitGroup.

### LogEntry struct

```go
type LogEntry struct {
    Network   string
    Channel   string
    Command   string
    Nick      string
    Ident     string
    Host      string
    Target    string
    Message   string
    CreatedAt time.Time
}
```

### Integration points

1. **ALL_EVENTS handler** (`irc_handlers.go` line 14): Add `logWriter.enqueueFromEvent(network, event)` call. This single hook captures all IRC events. The writer filters to only the events we care about (PRIVMSG, NOTICE, JOIN, PART, QUIT, KICK, NICK, TOPIC, MODE).

2. **Bot's own messages** (`drainToChannel` / send path): After each `client.Cmd.Message()` or `client.Cmd.Notice()`, call `logWriter.enqueueBotMessage(network, channel, text)`. The bot's nick is set as the source.

3. **PMs**: Not logged in this phase. PM handling doesn't exist yet (Phase 5). Can be added later.

### Overflow handling

Non-blocking send to the channel. On overflow:
- Event is silently dropped
- `dropped` counter increments (atomic)
- Every 60s (or on shutdown), if `dropped > 0`, log a warning: `"irclog: dropped N events due to buffer overflow"`

## Configuration

New `[logging]` section in `config/config.toml`:

```toml
[logging]
# IRC log storage settings

# enabled = false          # Enable/disable IRC log storage (default: false)
# dir = "data/logs"        # Directory for SQLite log files
# rotation = "monthly"     # Rotation period: "monthly" (YYYY-MM.db) or "yearly" (YYYY.db)
# buffer_size = 10000      # Channel buffer size for incoming events
# batch_size = 500         # Rows per INSERT transaction
# flush_interval = "2s"    # Max time between flushes
```

### LoggingConfig struct (config.go)

```go
type LoggingConfig struct {
    Enabled       bool          `toml:"enabled"`
    Dir           string        `toml:"dir"`
    Rotation      string        `toml:"rotation"`
    BufferSize    int           `toml:"buffer_size"`
    BatchSize     int           `toml:"batch_size"`
    FlushInterval time.Duration `toml:"flush_interval"`
}
```

### Defaults (applied in ApplyDefaults):

- `Enabled`: false (opt-in)
- `Dir`: "data/logs"
- `Rotation`: "monthly" (valid: "monthly", "yearly")
- `BufferSize`: 10000
- `BatchSize`: 500
- `FlushInterval`: 2s

### Reload behavior

The `[logging]` section is NOT reloadable at runtime. Changing logging settings requires a restart. On `/reload`, if the section changed, log a warning (same pattern as img-mcp's non-reloadable fields). Rationale: the writer goroutine, buffer, and open file handle are long-lived state that can't be safely hot-swapped.

## File Structure

### New files

- `irclog.go` — `LogWriter`, `LogEntry`, `initLogWriter`, `stopLogWriter`, batch flush logic, rotation file management, schema creation
- `irclog_test.go` — tests for batching, rotation boundary, overflow handling, schema creation

### Modified files

- `config.go` — add `LoggingConfig` struct, `Logging` field on `Config`, defaults, validation
- `config/config.toml` — add `[logging]` section with reference docs per convention
- `irc_handlers.go` — add `logWriter.enqueueFromEvent()` call in ALL_EVENTS handler
- `main.go` — call `initLogWriter` at startup, `stopLogWriter` on shutdown, register with `wg`

## Testing

- Unit tests in `irclog_test.go`:
  - Batch accumulation and flush on size threshold
  - Flush on timer
  - Rotation boundary crossing (mock clock or time-based)
  - Buffer overflow drop counting
  - Schema creation for new files
  - Event field mapping (each event type produces correct column values)
  - Shutdown drains remaining buffer
- Integration: verify that `go test ./...` passes with the new config fields
- Config tests: `LoggingConfig` defaults and validation

## Out of Scope (Future)

- Query/search API for logged messages (Phase 4+)
- Spam/flood detection or marking in logs
- PM logging (blocked on PM handler, Phase 5)
- Cross-file query utilities (future query layer handles spanning multiple period files)
- Vector DB or embedding-based search
- Log export/download for admins
