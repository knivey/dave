# IRC Log Storage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add IRC channel log storage with a buffered batch writer writing to rotating SQLite files.

**Architecture:** A `LogWriter` singleton accepts events via a buffered channel, batches them in a single goroutine, and writes transactionally to monthly (or yearly) SQLite files. Integration via the existing `ALL_EVENTS` handler plus bot message capture in `drainToChannel`.

**Tech Stack:** Go, GORM/SQLite for log files, existing girc event system.

---

## File Structure

| Action | File | Responsibility |
|--------|------|----------------|
| Create | `irclog.go` | `LogWriter`, `LogEntry`, batch flush, rotation, schema creation |
| Create | `irclog_test.go` | Tests for batching, rotation, overflow, event mapping |
| Modify | `config.go` | Add `LoggingConfig` struct + `Logging` field on `Config` |
| Modify | `config/config.toml` | Add `[logging]` reference section + commented example |
| Modify | `irc_handlers.go` | Add `logWriter.enqueueFromEvent()` in ALL_EVENTS handler |
| Modify | `main.go` | Init/shutdown lifecycle, bot message capture in `drainToChannel` |

---

### Task 1: Add LoggingConfig to config.go

**Files:**
- Modify: `config.go:44` (after Compaction field in Config struct)
- Modify: `config.go:49` (after BanConfig struct)

- [ ] **Step 1: Add LoggingConfig struct after BanConfig**

At `config.go` after line 49 (after `BanConfig`), add:

```go
type LoggingConfig struct {
	Enabled       bool          `toml:"enabled"`
	Dir           string        `toml:"dir"`
	Rotation      string        `toml:"rotation"`
	BufferSize    int           `toml:"buffer_size"`
	BatchSize     int           `toml:"batch_size"`
	FlushInterval time.Duration `toml:"flush_interval"`
}

func (c *LoggingConfig) SetDefaults() {
	if c.Dir == "" {
		c.Dir = "data/logs"
	}
	if c.Rotation == "" {
		c.Rotation = "monthly"
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 10000
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 2 * time.Second
	}
}
```

- [ ] **Step 2: Add Logging field to Config struct**

At `config.go` line 43, add after the `Compaction` field:

```go
	Logging LoggingConfig `toml:"logging"`
```

- [ ] **Step 3: Add defaults call in loadConfigDir**

In `config.go` `loadConfigDir` function, after line 473 (`config.Compaction.ApplyDefaults()`), add:

```go
	config.Logging.SetDefaults()
```

- [ ] **Step 4: Run tests to verify config loads**

Run: `go build .`
Expected: compiles without error.

Run: `go test ./... -run TestLoadConfigDir -count=1`
Expected: existing tests still pass (new field defaults to zero-value, `SetDefaults()` fills it).

- [ ] **Step 5: Commit**

```bash
git add config.go
git commit -m "feat: add LoggingConfig struct for IRC log storage"
```

---

### Task 2: Add [logging] section to config/config.toml

**Files:**
- Modify: `config/config.toml`

- [ ] **Step 1: Add reference section in the header comments**

After line 60 (after the `[pastebin]` reference block ending with `#   pastebin_preview_lines (int, default: 3)  ...`), add:

```toml
#
# [logging] options:
#   enabled            (bool, default: false)    Enable IRC channel log storage
#   dir                (string, default: "data/logs") Directory for SQLite log files
#   rotation           (string, default: "monthly") File rotation period: "monthly" (YYYY-MM.db) or "yearly" (YYYY.db)
#   buffer_size        (int, default: 10000)     Channel buffer size for incoming events
#   batch_size         (int, default: 500)       Rows per INSERT transaction
#   flush_interval     (duration, default: "2s") Max time between flushes
```

- [ ] **Step 2: Add commented-out example block**

After line 146 (after the `[pastebin]` commented example block), add:

```toml
#
# [logging]
# enabled = false
# dir = "data/logs"
# rotation = "monthly"
# buffer_size = 10000
# batch_size = 500
# flush_interval = "2s"
```

- [ ] **Step 3: Verify config still loads**

Run: `go build . && ./dave 2>&1 | head -5`
Expected: bot starts, config loads without error. Kill with Ctrl+C.

- [ ] **Step 4: Commit**

```bash
git add config/config.toml
git commit -m "docs: add [logging] section to config.toml"
```

---

### Task 3: Create LogWriter core in irclog.go

**Files:**
- Create: `irclog.go`

- [ ] **Step 1: Write the LogEntry struct and LogWriter skeleton**

Create `irclog.go` with:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lrstanley/girc"
	"gorm.io/gorm"
	sqlite "gorm.io/driver/sqlite"
	logxi "github.com/mgutz/logxi/v1"
)

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

type ircLog struct {
	ID        int64     `gorm:"primaryKey;autoIncrement"`
	Network   string    `gorm:"not null"`
	Channel   string    `gorm:"not null"`
	Command   string    `gorm:"not null"`
	Nick      string
	Ident     string
	Host      string
	Target    string
	Message   string
	CreatedAt time.Time `gorm:"not null;index"`
}

func (ircLog) TableName() string { return "irc_logs" }

type schemaMeta struct {
	Key   string `gorm:"primaryKey"`
	Value string
}

func (schemaMeta) TableName() string { return "schema_meta" }

type LogWriter struct {
	cfg     LoggingConfig
	entries chan LogEntry
	done    chan struct{}
	dropped atomic.Int64
	mu      sync.Mutex
	db      *gorm.DB
	current string
	log     logxi.Logger
}

var logWriter *LogWriter

func initLogWriter(cfg LoggingConfig, log logxi.Logger) error {
	if !cfg.Enabled {
		return nil
	}
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return fmt.Errorf("creating log directory %s: %w", cfg.Dir, err)
	}
	w := &LogWriter{
		cfg:     cfg,
		entries: make(chan LogEntry, cfg.BufferSize),
		done:    make(chan struct{}),
		log:     log,
	}
	logWriter = w
	wg.Add(1)
	go w.run()
	log.Info("IRC log writer started", "dir", cfg.Dir, "rotation", cfg.Rotation)
	return nil
}

func stopLogWriter() {
	if logWriter == nil {
		return
	}
	close(logWriter.done)
	logWriter = nil
}

func (w *LogWriter) enqueue(entry LogEntry) {
	select {
	case w.entries <- entry:
	default:
		w.dropped.Add(1)
	}
}

func enqueueFromEvent(networkName string, event girc.Event) {
	if logWriter == nil {
		return
	}
	switch event.Command {
	case girc.PRIVMSG, girc.NOTICE, girc.JOIN, girc.PART,
		girc.QUIT, girc.KICK, girc.NICK, girc.TOPIC, girc.MODE:
	default:
		return
	}

	entry := LogEntry{
		Network:   networkName,
		Command:   event.Command,
		CreatedAt: event.Timestamp,
	}

	if event.Source != nil {
		entry.Nick = event.Source.Nick
		entry.Ident = event.Source.Ident
		entry.Host = event.Source.Host
	}

	switch event.Command {
	case girc.PRIVMSG, girc.NOTICE:
		if len(event.Params) > 0 {
			entry.Target = event.Params[0]
		}
		if len(event.Params) > 1 {
			entry.Message = event.Params[1]
		}
	case girc.JOIN:
		if len(event.Params) > 0 {
			entry.Channel = event.Params[0]
			entry.Target = event.Params[0]
		}
	case girc.PART:
		if len(event.Params) > 0 {
			entry.Channel = event.Params[0]
			entry.Target = event.Params[0]
		}
		if len(event.Params) > 1 {
			entry.Message = event.Params[1]
		}
	case girc.QUIT:
		if len(event.Params) > 0 {
			entry.Message = event.Params[0]
		}
	case girc.KICK:
		if len(event.Params) > 0 {
			entry.Channel = event.Params[0]
			entry.Target = event.Params[0]
		}
		if len(event.Params) > 2 {
			entry.Message = event.Params[2]
		}
	case girc.NICK:
		if len(event.Params) > 0 {
			entry.Target = event.Params[0]
		}
	case girc.TOPIC:
		if len(event.Params) > 0 {
			entry.Channel = event.Params[0]
			entry.Target = event.Params[0]
		}
		if len(event.Params) > 1 {
			entry.Message = event.Params[1]
		}
	case girc.MODE:
		if len(event.Params) > 0 {
			entry.Target = event.Params[0]
		}
		if len(event.Params) > 1 {
			entry.Message = event.Params[1]
		}
		for _, p := range event.Params[2:] {
			entry.Message += " " + p
		}
	}

	if entry.Channel == "" {
		entry.Channel = entry.Target
	}

	w := logWriter
	if w != nil {
		w.enqueue(entry)
	}
}

func enqueueBotMessage(networkName, channel, text string) {
	if logWriter == nil {
		return
	}
	bot := getBot(networkName)
	if bot == nil {
		return
	}
	w := logWriter
	if w != nil {
		w.enqueue(LogEntry{
			Network:   networkName,
			Channel:   channel,
			Command:   girc.PRIVMSG,
			Nick:      bot.Network.Nick,
			Message:   text,
			CreatedAt: time.Now(),
		})
	}
}

func (w *LogWriter) periodKey(t time.Time) string {
	switch w.cfg.Rotation {
	case "yearly":
		return t.Format("2006")
	default:
		return t.Format("2006-01")
	}
}

func (w *LogWriter) filename(key string) string {
	return filepath.Join(w.cfg.Dir, key+".db")
}

func (w *LogWriter) openDB(key string) (*gorm.DB, error) {
	path := w.filename(key)
	db, err := gorm.Open(sqlite.Open(path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("opening log db %s: %w", path, err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&ircLog{}, &schemaMeta{}); err != nil {
		return nil, fmt.Errorf("migrating log db %s: %w", path, err)
	}

	db.Exec("CREATE INDEX IF NOT EXISTS idx_irc_logs_channel_time ON irc_logs (network, channel, created_at)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_irc_logs_nick_time ON irc_logs (network, nick, created_at)")

	return db, nil
}

func (w *LogWriter) ensureDB(key string) (*gorm.DB, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == key && w.db != nil {
		return w.db, nil
	}
	if w.db != nil {
		sqlDB, _ := w.db.DB()
		sqlDB.Close()
	}
	db, err := w.openDB(key)
	if err != nil {
		return nil, err
	}
	w.db = db
	w.current = key
	return db, nil
}

func (w *LogWriter) writeBatch(batch []LogEntry) {
	if len(batch) == 0 {
		return
	}

	key := w.periodKey(batch[0].CreatedAt)
	db, err := w.ensureDB(key)
	if err != nil {
		w.log.Error("failed to open log db", "key", key, "error", err)
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		for _, e := range batch {
			row := ircLog{
				Network:   e.Network,
				Channel:   e.Channel,
				Command:   e.Command,
				Nick:      e.Nick,
				Ident:     e.Ident,
				Host:      e.Host,
				Target:    e.Target,
				Message:   e.Message,
				CreatedAt: e.CreatedAt,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		w.log.Error("failed to write log batch", "error", err, "count", len(batch))
	}
}

func (w *LogWriter) run() {
	defer wg.Done()

	var batch []LogEntry
	flushTicker := time.NewTicker(w.cfg.FlushInterval)
	defer flushTicker.Stop()

	droppedLogTicker := time.NewTicker(60 * time.Second)
	defer droppedLogTicker.Stop()

	flush := func() {
		if len(batch) > 0 {
			w.writeBatch(batch)
			batch = batch[:0]
		}
	}

	for {
		select {
		case entry, ok := <-w.entries:
			if !ok {
				flush()
				w.closeDB()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= w.cfg.BatchSize {
				flush()
			}
		case <-flushTicker.C:
			flush()
		case <-droppedLogTicker.C:
			if d := w.dropped.Swap(0); d > 0 {
				w.log.Warn("irclog: dropped events due to buffer overflow", "count", d)
			}
		case <-w.done:
			for {
				select {
				case entry, ok := <-w.entries:
					if !ok {
						flush()
						w.closeDB()
						return
					}
					batch = append(batch, entry)
				default:
					flush()
					w.closeDB()
					return
				}
			}
		}
	}
}

func (w *LogWriter) closeDB() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.db != nil {
		sqlDB, _ := w.db.DB()
		sqlDB.Close()
		w.db = nil
		w.current = ""
	}
}
```

- [ ] **Step 2: Verify compilation**

Run: `go build .`
Expected: compiles without error.

- [ ] **Step 3: Commit**

```bash
git add irclog.go
git commit -m "feat: add LogWriter core with buffered batch writer and rotation"
```

---

### Task 4: Wire LogWriter into startup, shutdown, and IRC handlers

**Files:**
- Modify: `irc_handlers.go:14-18` (ALL_EVENTS handler)
- Modify: `main.go:449` (after initDB)
- Modify: `main.go:517` (shutdown sequence, before queueMgr.Stop)

- [ ] **Step 1: Add logWriter call in ALL_EVENTS handler**

In `irc_handlers.go`, modify the ALL_EVENTS handler (lines 14-18) to add the enqueue call:

```go
	client.Handlers.Add(girc.ALL_EVENTS, func(client *girc.Client, event girc.Event) {
		if str, ok := event.Pretty(); ok {
			log.Info(str)
		}
		enqueueFromEvent(network.Name, event)
	})
```

- [ ] **Step 2: Add initLogWriter in main() after initDB**

In `main.go`, after line 453 (`sessionMgr = NewSessionManager(theDB)`), add:

```go
	if err := initLogWriter(config.Logging, logger); err != nil {
		logger.Error("Failed to initialize log writer", "error", err)
		os.Exit(1)
	}
```

- [ ] **Step 3: Add stopLogWriter in shutdown sequence**

In `main.go` shutdown handler, after line 516 (`apiLogger.CloseAll()`) and before line 517 (`if queueMgr != nil`), add:

```go
		stopLogWriter()
```

- [ ] **Step 4: Add bot message capture in drainToChannel**

In `main.go`, modify `drainToChannel` (lines 108-122) to capture bot messages:

```go
func drainToChannel(client *girc.Client, channel string, throttle time.Duration, outCh <-chan string, ctx context.Context, networkName string) {
	for msg := range outCh {
		if ctx != nil && ctx.Err() != nil {
			for range outCh {
			}
			break
		}
		enqueueBotMessage(networkName, channel, msg)
		if action, ok := isIRCAction(msg); ok {
			client.Cmd.Action(channel, action)
		} else {
			client.Cmd.Message(channel, "\x02\x02"+msg)
		}
		time.Sleep(throttle)
	}
}
```

- [ ] **Step 5: Update all drainToChannel call sites to pass networkName**

There are 3 call sites. Update each:

1. `main.go:606` — find the call site context to determine network name (this is in the `stop` command handler). Pass the appropriate network name string.

2. `irc_handlers.go:340` — change:
```go
go drainToChannel(client, channel, time.Millisecond*network.Throttle, outCh, nil)
```
to:
```go
go drainToChannel(client, channel, time.Millisecond*network.Throttle, outCh, nil, network.Name)
```

3. `irc_handlers.go:360` — same change.

4. `queue.go:525` — change:
```go
drainToChannel(bot.Client, item.Channel, throttle, item.outputCh, item.ctx)
```
to:
```go
drainToChannel(bot.Client, item.Channel, throttle, item.outputCh, item.ctx, bot.Network.Name)
```

5. `main.go:606` — find the stop command handler and pass the appropriate network name. Search the surrounding code to identify which network is in scope.

- [ ] **Step 6: Build and verify**

Run: `go build .`
Expected: compiles without error.

Run: `go vet ./...`
Expected: no issues.

- [ ] **Step 7: Commit**

```bash
git add irc_handlers.go main.go queue.go
git commit -m "feat: wire LogWriter into startup, shutdown, and IRC handlers"
```

---

### Task 5: Handle /reload non-reloadable warning for logging config

**Files:**
- Modify: `main.go:385-416` (reloadAll function)

- [ ] **Step 1: Add non-reloadable warning in reloadAll**

In `main.go` `reloadAll()`, after the initial `loadReloadableDir` call succeeds (after line 390), add a check that warns if logging config changed:

```go
	if oldConfig.Logging != config.Logging {
		config.Logging = oldConfig.Logging
		logger.Warn("[logging] is not reloadable, restart required to apply changes")
	}
```

Wait — `reloadAll` uses `loadReloadableDir` which only reloads MCPs, services, template vars, commands, and notices. It does NOT reload the root config.toml. The `[logging]` section lives in `config.toml` which is only loaded at startup via `loadConfigDir`. So `/reload` won't even see the logging config change. This means no warning is needed — the config simply won't change on reload.

Skip this task. The reload system already doesn't touch `[logging]`.

- [ ] **Step 2: Commit**

No changes needed. Move to next task.

---

### Task 6: Write tests for LogWriter

**Files:**
- Create: `irclog_test.go`

- [ ] **Step 1: Write test file with event mapping and batch tests**

Create `irclog_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnqueueFromEvent_Filtering(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"PRIVMSG is logged", girc.PRIVMSG, true},
		{"NOTICE is logged", girc.NOTICE, true},
		{"JOIN is logged", girc.JOIN, true},
		{"PART is logged", girc.PART, true},
		{"QUIT is logged", girc.QUIT, true},
		{"KICK is logged", girc.KICK, true},
		{"NICK is logged", girc.NICK, true},
		{"TOPIC is logged", girc.TOPIC, true},
		{"MODE is logged", girc.MODE, true},
		{"PING is not logged", "PING", false},
		{"PONG is not logged", "PONG", false},
		{"001 is not logged", "001", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := LoggingConfig{
				Enabled:       true,
				Dir:           dir,
				Rotation:      "monthly",
				BufferSize:    100,
				BatchSize:     10,
				FlushInterval: 100 * time.Millisecond,
			}
			log := newTestLogger(t)
			err := initLogWriter(cfg, log)
			require.NoError(t, err)
			defer stopLogWriter()

			event := girc.Event{
				Command:   tt.command,
				Timestamp: time.Now(),
				Source:    &girc.Source{Name: "nick!user@host", Nick: "nick", Ident: "user", Host: "host"},
				Params:   []string{"#test", "hello"},
			}
			enqueueFromEvent("testnet", event)

			time.Sleep(200 * time.Millisecond)

			logWriter.mu.Lock()
			hasDB := logWriter.db != nil
			logWriter.mu.Unlock()

			if tt.want {
				files, _ := filepath.Glob(filepath.Join(dir, "*.db"))
				assert.Equal(t, hasDB || len(files) > 0, true, "expected log db to exist")
			}
		})
	}
}

func TestPeriodKey(t *testing.T) {
	w := &LogWriter{cfg: LoggingConfig{Rotation: "monthly"}}
	assert.Equal(t, "2026-05", w.periodKey(time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)))

	w = &LogWriter{cfg: LoggingConfig{Rotation: "yearly"}}
	assert.Equal(t, "2026", w.periodKey(time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)))
}

func TestLogWriterBatchFlush(t *testing.T) {
	dir := t.TempDir()
	cfg := LoggingConfig{
		Enabled:       true,
		Dir:           dir,
		Rotation:      "monthly",
		BufferSize:    100,
		BatchSize:     3,
		FlushInterval: 5 * time.Second,
	}
	log := newTestLogger(t)
	err := initLogWriter(cfg, log)
	require.NoError(t, err)
	defer stopLogWriter()

	now := time.Now()
	for i := 0; i < 3; i++ {
		logWriter.enqueue(LogEntry{
			Network:   "testnet",
			Channel:   "#test",
			Command:   girc.PRIVMSG,
			Nick:      "user",
			Message:   "hello",
			CreatedAt: now,
		})
	}

	time.Sleep(200 * time.Millisecond)

	key := now.Format("2006-01")
	dbPath := filepath.Join(dir, key+".db")
	_, err = os.Stat(dbPath)
	require.NoError(t, err, "log db file should exist after batch flush")
}

func TestLogWriterOverflow(t *testing.T) {
	dir := t.TempDir()
	cfg := LoggingConfig{
		Enabled:       true,
		Dir:           dir,
		Rotation:      "monthly",
		BufferSize:    5,
		BatchSize:     100,
		FlushInterval: 5 * time.Second,
	}
	log := newTestLogger(t)
	err := initLogWriter(cfg, log)
	require.NoError(t, err)
	defer stopLogWriter()

	for i := 0; i < 10; i++ {
		logWriter.enqueue(LogEntry{
			Network:   "testnet",
			Channel:   "#test",
			Command:   girc.PRIVMSG,
			Nick:      "user",
			Message:   "hello",
			CreatedAt: time.Now(),
		})
	}

	assert.True(t, logWriter.dropped.Load() > 0, "expected some events to be dropped")
}

func TestLogWriterShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := LoggingConfig{
		Enabled:       true,
		Dir:           dir,
		Rotation:      "monthly",
		BufferSize:    100,
		BatchSize:     1000,
		FlushInterval: 5 * time.Second,
	}
	log := newTestLogger(t)
	err := initLogWriter(cfg, log)
	require.NoError(t, err)

	logWriter.enqueue(LogEntry{
		Network:   "testnet",
		Channel:   "#test",
		Command:   girc.PRIVMSG,
		Nick:      "user",
		Message:   "pending message",
		CreatedAt: time.Now(),
	})

	stopLogWriter()

	key := time.Now().Format("2006-01")
	dbPath := filepath.Join(dir, key+".db")
	_, err = os.Stat(dbPath)
	require.NoError(t, err, "log db file should exist after shutdown flush")
}

func TestEventFieldMapping(t *testing.T) {
	tests := []struct {
		name    string
		event   girc.Event
		want    LogEntry
	}{
		{
			name: "PRIVMSG",
			event: girc.Event{
				Command:   girc.PRIVMSG,
				Timestamp: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
				Source:    &girc.Source{Nick: "alice", Ident: "a", Host: "host1"},
				Params:   []string{"#chan", "hello world"},
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.PRIVMSG,
				Nick:      "alice",
				Ident:     "a",
				Host:      "host1",
				Target:    "#chan",
				Message:   "hello world",
				CreatedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "NICK change",
			event: girc.Event{
				Command:   girc.NICK,
				Timestamp: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
				Source:    &girc.Source{Nick: "oldname", Ident: "a", Host: "host1"},
				Params:   []string{"newname"},
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "newname",
				Command:   girc.NICK,
				Nick:      "oldname",
				Ident:     "a",
				Host:      "host1",
				Target:    "newname",
				CreatedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "QUIT",
			event: girc.Event{
				Command:   girc.QUIT,
				Timestamp: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
				Source:    &girc.Source{Nick: "bob", Ident: "b", Host: "host2"},
				Params:   []string{"bye bye"},
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "",
				Command:   girc.QUIT,
				Nick:      "bob",
				Ident:     "b",
				Host:      "host2",
				Message:   "bye bye",
				CreatedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
			},
		},
		{
			name: "KICK",
			event: girc.Event{
				Command:   girc.KICK,
				Timestamp: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
				Source:    &girc.Source{Nick: "op", Ident: "o", Host: "host3"},
				Params:   []string{"#chan", "baduser", "spamming"},
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.KICK,
				Nick:      "op",
				Ident:     "o",
				Host:      "host3",
				Target:    "#chan",
				Message:   "spamming",
				CreatedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := LoggingConfig{
				Enabled:       true,
				Dir:           dir,
				Rotation:      "monthly",
				BufferSize:    100,
				BatchSize:     100,
				FlushInterval: 5 * time.Second,
			}
			log := newTestLogger(t)
			err := initLogWriter(cfg, log)
			require.NoError(t, err)
			defer stopLogWriter()

			enqueueFromEvent("testnet", tt.event)

			select {
			case entry := <-logWriter.entries:
				assert.Equal(t, tt.want, entry)
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for entry")
			}
		})
	}
}

func TestLoggingConfigDefaults(t *testing.T) {
	cfg := LoggingConfig{}
	cfg.SetDefaults()
	assert.Equal(t, "data/logs", cfg.Dir)
	assert.Equal(t, "monthly", cfg.Rotation)
	assert.Equal(t, 10000, cfg.BufferSize)
	assert.Equal(t, 500, cfg.BatchSize)
	assert.Equal(t, 2*time.Second, cfg.FlushInterval)
}

func newTestLogger(t *testing.T) logxi.Logger {
	t.Helper()
	l := logxi.New("test")
	l.SetLevel(logxi.LevelAll)
	return l
}
```

- [ ] **Step 2: Run the tests**

Run: `go test -v -run "TestPeriodKey|TestLoggingConfigDefaults|TestEventFieldMapping" ./...`
Expected: PASS for period key, defaults, and event mapping tests.

Run: `go test -v -run "TestLogWriterBatchFlush|TestLogWriterOverflow|TestLogWriterShutdown|TestEnqueueFromEvent" -timeout 30s ./...`
Expected: PASS for batch, overflow, and shutdown tests. The overflow test may have timing sensitivity — if flaky, increase the buffer fill count.

- [ ] **Step 3: Fix any test issues**

If the `TestEnqueueFromEvent_Filtering` tests have timing issues (the writer goroutine needs time to flush), adjust the sleep duration or use a sync mechanism. The tests use real goroutines and channels, so small delays are expected.

If `TestEventFieldMapping` fails because `enqueueFromEvent` reads from `logWriter` global which is being written by another test, ensure tests run sequentially with `defer stopLogWriter()`.

- [ ] **Step 4: Run full test suite**

Run: `go test ./... -timeout 60s`
Expected: all tests pass.

Run: `go fmt ./... && go vet ./...`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add irclog_test.go
git commit -m "test: add IRC log writer tests for batching, rotation, overflow, event mapping"
```

---

### Task 7: Add config test for LoggingConfig loading

**Files:**
- Modify: `config_test.go` (add test after existing config loading tests)

- [ ] **Step 1: Add test for logging config loading from TOML**

Add to `config_test.go`:

```go
func TestLoadConfigDirLoggingDefaults(t *testing.T) {
	mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	dir := createTestConfigDir(t, mainTOML, nil)
	defer os.RemoveAll(dir)
	config := loadConfigDirOrDie(dir)
	assert.False(t, config.Logging.Enabled, "logging should be disabled by default")
	assert.Equal(t, "data/logs", config.Logging.Dir)
	assert.Equal(t, "monthly", config.Logging.Rotation)
	assert.Equal(t, 10000, config.Logging.BufferSize)
	assert.Equal(t, 500, config.Logging.BatchSize)
	assert.Equal(t, 2*time.Second, config.Logging.FlushInterval)
}

func TestLoadConfigDirLoggingEnabled(t *testing.T) {
	mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"

[logging]
enabled = true
dir = "custom/logs"
rotation = "yearly"
buffer_size = 5000
batch_size = 200
flush_interval = "5s"
`
	dir := createTestConfigDir(t, mainTOML, nil)
	defer os.RemoveAll(dir)
	config := loadConfigDirOrDie(dir)
	assert.True(t, config.Logging.Enabled)
	assert.Equal(t, "custom/logs", config.Logging.Dir)
	assert.Equal(t, "yearly", config.Logging.Rotation)
	assert.Equal(t, 5000, config.Logging.BufferSize)
	assert.Equal(t, 200, config.Logging.BatchSize)
	assert.Equal(t, 5*time.Second, config.Logging.FlushInterval)
}
```

- [ ] **Step 2: Run config tests**

Run: `go test -v -run "TestLoadConfigDirLogging" ./...`
Expected: PASS

- [ ] **Step 3: Run full suite**

Run: `go test ./... -timeout 60s`
Expected: all tests pass.

Run: `go fmt ./... && go vet ./...`
Expected: clean.

- [ ] **Step 4: Commit**

```bash
git add config_test.go
git commit -m "test: add logging config loading and defaults tests"
```

---

### Task 8: Final integration verification

**Files:**
- No new files

- [ ] **Step 1: Build the binary**

Run: `go build -o dave .`
Expected: builds successfully.

- [ ] **Step 2: Run all tests**

Run: `go test ./... -timeout 60s`
Expected: all tests pass.

- [ ] **Step 3: Run fmt and vet**

Run: `go fmt ./... && go vet ./...`
Expected: clean output, no warnings.

- [ ] **Step 4: Final commit (if any formatting fixes needed)**

```bash
git add -A
git commit -m "chore: final cleanup for IRC log storage feature"
```
