package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/gorm"
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
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	Network   string `gorm:"not null"`
	Channel   string `gorm:"not null"`
	Command   string `gorm:"not null"`
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

func initLogWriter(cfg LoggingConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return fmt.Errorf("creating log directory %s: %w", cfg.Dir, err)
	}
	lw := &LogWriter{
		cfg:     cfg,
		entries: make(chan LogEntry, cfg.BufferSize),
		done:    make(chan struct{}),
	}
	lw.log = logxi.New("irclog")
	lw.log.SetLevel(logxi.LevelAll)
	logWriter = lw
	wg.Add(1)
	go lw.run()
	return nil
}

func stopLogWriter() {
	if logWriter == nil {
		return
	}
	close(logWriter.done)
	logWriter = nil
}

func enqueue(entry LogEntry) {
	if logWriter == nil {
		return
	}
	select {
	case logWriter.entries <- entry:
	default:
		logWriter.dropped.Add(1)
	}
}

func enqueueFromEvent(networkName string, event girc.Event) {
	if logWriter == nil {
		return
	}
	cmd := event.Command
	switch cmd {
	case girc.PRIVMSG, girc.NOTICE, girc.JOIN, girc.PART,
		girc.QUIT, girc.KICK, girc.NICK, girc.TOPIC, girc.MODE:
	default:
		return
	}

	entry := LogEntry{
		Network:   networkName,
		Command:   cmd,
		CreatedAt: event.Timestamp,
	}

	if event.Source != nil {
		entry.Nick = event.Source.Name
		entry.Ident = event.Source.Ident
		entry.Host = event.Source.Host
	}

	switch cmd {
	case girc.PRIVMSG, girc.NOTICE:
		if len(event.Params) >= 1 {
			entry.Target = event.Params[0]
		}
		if len(event.Params) >= 2 {
			entry.Message = event.Params[1]
		}
		if entry.Target != "" && !strings.HasPrefix(entry.Target, "#") {
			return
		}
		entry.Channel = entry.Target
	case girc.JOIN:
		if len(event.Params) >= 1 {
			entry.Channel = event.Params[0]
			entry.Target = event.Params[0]
		}
	case girc.PART:
		if len(event.Params) >= 1 {
			entry.Channel = event.Params[0]
			entry.Target = event.Params[0]
		}
		if len(event.Params) >= 2 {
			entry.Message = event.Params[1]
		}
	case girc.QUIT:
		if len(event.Params) >= 1 {
			entry.Message = event.Params[0]
		}
	case girc.KICK:
		if len(event.Params) >= 1 {
			entry.Channel = event.Params[0]
		}
		if len(event.Params) >= 2 {
			entry.Target = event.Params[1]
		}
		if len(event.Params) >= 3 {
			entry.Message = event.Params[2]
		}
	case girc.NICK:
		if len(event.Params) >= 1 {
			entry.Target = event.Params[0]
		}
	case girc.TOPIC:
		if len(event.Params) >= 1 {
			entry.Channel = event.Params[0]
			entry.Target = event.Params[0]
		}
		if len(event.Params) >= 2 {
			entry.Message = event.Params[1]
		}
	case girc.MODE:
		if len(event.Params) >= 1 {
			entry.Target = event.Params[0]
		}
		if len(event.Params) >= 2 {
			entry.Message = strings.Join(event.Params[1:], " ")
		}
		if entry.Channel == "" {
			entry.Channel = entry.Target
		}
	}

	enqueue(entry)
}

func enqueueBotMessage(networkName, channel, text string) {
	if logWriter == nil {
		return
	}
	nick := ""
	if bot, ok := getBot(networkName); ok {
		bot.mu.Lock()
		nick = bot.Network.Nick
		bot.mu.Unlock()
	}
	enqueue(LogEntry{
		Network:   networkName,
		Channel:   channel,
		Command:   girc.PRIVMSG,
		Nick:      nick,
		Target:    channel,
		Message:   text,
		CreatedAt: time.Now(),
	})
}

func (lw *LogWriter) periodKey(t time.Time) string {
	switch lw.cfg.Rotation {
	case "yearly":
		return t.Format("2006")
	default:
		return t.Format("2006-01")
	}
}

func (lw *LogWriter) filename(key string) string {
	return filepath.Join(lw.cfg.Dir, key+".db")
}

func (lw *LogWriter) openDB(key string) (*gorm.DB, error) {
	path := lw.filename(key)
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating log db directory %s: %w", dir, err)
		}
	}
	dialector := sqlite.Open(path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: newGormLogger(lw.log),
	})
	if err != nil {
		return nil, fmt.Errorf("opening log database %s: %w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("getting underlying db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&ircLog{}, &schemaMeta{}); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("running log migrations: %w", err)
	}

	db.Exec("CREATE INDEX IF NOT EXISTS idx_irc_logs_channel_time ON irc_logs (network, channel, created_at)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_irc_logs_nick_time ON irc_logs (network, nick, created_at)")

	return db, nil
}

func (lw *LogWriter) ensureDB(key string) (*gorm.DB, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if key == lw.current && lw.db != nil {
		return lw.db, nil
	}
	if lw.db != nil {
		if sqlDB, err := lw.db.DB(); err == nil {
			sqlDB.Close()
		}
	}
	db, err := lw.openDB(key)
	if err != nil {
		return nil, err
	}
	lw.db = db
	lw.current = key
	return db, nil
}

func (lw *LogWriter) writeBatch(batch []LogEntry) {
	if len(batch) == 0 {
		return
	}
	key := lw.periodKey(batch[0].CreatedAt)
	db, err := lw.ensureDB(key)
	if err != nil {
		lw.log.Error("failed to open log database", "key", key, "error", err)
		return
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		for _, entry := range batch {
			row := ircLog{
				Network:   entry.Network,
				Channel:   entry.Channel,
				Command:   entry.Command,
				Nick:      entry.Nick,
				Ident:     entry.Ident,
				Host:      entry.Host,
				Target:    entry.Target,
				Message:   entry.Message,
				CreatedAt: entry.CreatedAt,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		lw.log.Error("failed to write log batch", "key", key, "count", len(batch), "error", err)
	}
}

func (lw *LogWriter) run() {
	defer wg.Done()

	flushTicker := time.NewTicker(lw.cfg.FlushInterval)
	defer flushTicker.Stop()

	droppedLogTicker := time.NewTicker(60 * time.Second)
	defer droppedLogTicker.Stop()

	var batch []LogEntry

	flush := func() {
		if len(batch) == 0 {
			return
		}
		lw.writeBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case entry, ok := <-lw.entries:
			if !ok {
				flush()
				lw.closeDB()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= lw.cfg.BatchSize {
				flush()
			}
		case <-flushTicker.C:
			flush()
		case <-droppedLogTicker.C:
			d := lw.dropped.Swap(0)
			if d > 0 {
				lw.log.Warn("irc log entries dropped", "count", d)
			}
		case <-lw.done:
			for {
				select {
				case entry := <-lw.entries:
					batch = append(batch, entry)
				default:
					flush()
					lw.closeDB()
					return
				}
			}
		}
	}
}

func (lw *LogWriter) closeDB() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.db != nil {
		if sqlDB, err := lw.db.DB(); err == nil {
			sqlDB.Close()
		}
		lw.db = nil
		lw.current = ""
	}
}
