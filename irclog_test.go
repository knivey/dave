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

func initTestLogWriter(t *testing.T, cfg LoggingConfig) *LogWriter {
	t.Helper()
	cfg.Enabled = true
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	cfg.SetDefaults()
	err := initLogWriter(cfg)
	require.NoError(t, err)
	require.NotNil(t, logWriter)
	t.Cleanup(func() {
		stopLogWriter()
		wg.Wait()
	})
	return logWriter
}

func TestPeriodKey(t *testing.T) {
	w := &LogWriter{cfg: LoggingConfig{Rotation: "monthly"}}
	assert.Equal(t, "2026-05", w.periodKey(time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)))

	w = &LogWriter{cfg: LoggingConfig{Rotation: "yearly"}}
	assert.Equal(t, "2026", w.periodKey(time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)))
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

func newLogWriterNoRun(t *testing.T, cfg LoggingConfig) *LogWriter {
	t.Helper()
	cfg.SetDefaults()
	cfg.Dir = t.TempDir()
	lw := &LogWriter{
		cfg:     cfg,
		entries: make(chan LogEntry, cfg.BufferSize),
		done:    make(chan struct{}),
	}
	logWriter = lw
	t.Cleanup(func() {
		logWriter = nil
	})
	return lw
}

func TestEventFieldMapping(t *testing.T) {
	ts := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

	lw := newLogWriterNoRun(t, LoggingConfig{
		BufferSize:    100000,
		BatchSize:     100000,
		FlushInterval: time.Hour,
	})

	tests := []struct {
		name   string
		event  girc.Event
		want   LogEntry
		wantOK bool
	}{
		{
			name: "PRIVMSG",
			event: girc.Event{
				Command:   girc.PRIVMSG,
				Source:    &girc.Source{Name: "alice", Ident: "aident", Host: "ahost"},
				Params:    []string{"#chan", "hello world"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.PRIVMSG,
				Nick:      "alice",
				Ident:     "aident",
				Host:      "ahost",
				Target:    "#chan",
				Message:   "hello world",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "NOTICE",
			event: girc.Event{
				Command:   girc.NOTICE,
				Source:    &girc.Source{Name: "bob", Ident: "bident", Host: "bhost"},
				Params:    []string{"#chan", "notice msg"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.NOTICE,
				Nick:      "bob",
				Ident:     "bident",
				Host:      "bhost",
				Target:    "#chan",
				Message:   "notice msg",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "JOIN",
			event: girc.Event{
				Command:   girc.JOIN,
				Source:    &girc.Source{Name: "carol", Ident: "cident", Host: "chost"},
				Params:    []string{"#chan"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.JOIN,
				Nick:      "carol",
				Ident:     "cident",
				Host:      "chost",
				Target:    "#chan",
				Message:   "",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "PART",
			event: girc.Event{
				Command:   girc.PART,
				Source:    &girc.Source{Name: "dave", Ident: "dident", Host: "dhost"},
				Params:    []string{"#chan", "bye"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.PART,
				Nick:      "dave",
				Ident:     "dident",
				Host:      "dhost",
				Target:    "#chan",
				Message:   "bye",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "QUIT",
			event: girc.Event{
				Command:   girc.QUIT,
				Source:    &girc.Source{Name: "eve", Ident: "eident", Host: "ehost"},
				Params:    []string{"quit msg"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "",
				Command:   girc.QUIT,
				Nick:      "eve",
				Ident:     "eident",
				Host:      "ehost",
				Target:    "",
				Message:   "quit msg",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "KICK",
			event: girc.Event{
				Command:   girc.KICK,
				Source:    &girc.Source{Name: "op", Ident: "oident", Host: "ohost"},
				Params:    []string{"#chan", "baduser", "spamming"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.KICK,
				Nick:      "op",
				Ident:     "oident",
				Host:      "ohost",
				Target:    "baduser",
				Message:   "spamming",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "NICK",
			event: girc.Event{
				Command:   girc.NICK,
				Source:    &girc.Source{Name: "oldname", Ident: "nident", Host: "nhost"},
				Params:    []string{"newname"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "",
				Command:   girc.NICK,
				Nick:      "oldname",
				Ident:     "nident",
				Host:      "nhost",
				Target:    "newname",
				Message:   "",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "TOPIC",
			event: girc.Event{
				Command:   girc.TOPIC,
				Source:    &girc.Source{Name: "frank", Ident: "fident", Host: "fhost"},
				Params:    []string{"#chan", "new topic"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.TOPIC,
				Nick:      "frank",
				Ident:     "fident",
				Host:      "fhost",
				Target:    "#chan",
				Message:   "new topic",
				CreatedAt: ts,
			},
			wantOK: true,
		},
		{
			name: "MODE",
			event: girc.Event{
				Command:   girc.MODE,
				Source:    &girc.Source{Name: "grace", Ident: "gident", Host: "ghost"},
				Params:    []string{"#chan", "+o", "nick"},
				Timestamp: ts,
			},
			want: LogEntry{
				Network:   "testnet",
				Channel:   "#chan",
				Command:   girc.MODE,
				Nick:      "grace",
				Ident:     "gident",
				Host:      "ghost",
				Target:    "#chan",
				Message:   "+o nick",
				CreatedAt: ts,
			},
			wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enqueueFromEvent("testnet", tt.event)
			select {
			case entry := <-lw.entries:
				assert.Equal(t, tt.want, entry)
			case <-time.After(time.Second):
				if tt.wantOK {
					t.Fatal("timed out waiting for entry")
				}
			}
		})
	}
}

func TestEnqueueFromEventFiltering(t *testing.T) {
	lw := newLogWriterNoRun(t, LoggingConfig{
		BufferSize:    100000,
		BatchSize:     100000,
		FlushInterval: time.Hour,
	})

	filtered := []string{"PING", "PONG", "001", "CAP", "WHO", "332"}
	for _, cmd := range filtered {
		t.Run(cmd, func(t *testing.T) {
			enqueueFromEvent("testnet", girc.Event{
				Command: cmd,
				Source:  &girc.Source{Name: "someone", Ident: "u", Host: "h"},
				Params:  []string{"#chan", "text"},
			})
			select {
			case <-lw.entries:
				t.Fatal("filtered command should not produce an entry")
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

func TestEnqueueFromEventPrivmsgPrivateFilter(t *testing.T) {
	_ = newLogWriterNoRun(t, LoggingConfig{
		BufferSize:    100000,
		BatchSize:     100000,
		FlushInterval: time.Hour,
	})

	enqueueFromEvent("testnet", girc.Event{
		Command: girc.PRIVMSG,
		Source:  &girc.Source{Name: "alice", Ident: "a", Host: "h"},
		Params:  []string{"bob", "private msg"},
	})

	select {
	case <-logWriter.entries:
		t.Fatal("private PRIVMSG should not be logged")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLogWriterBatchFlush(t *testing.T) {
	cfg := LoggingConfig{
		BufferSize:    10000,
		BatchSize:     3,
		FlushInterval: time.Hour,
	}

	lw := initTestLogWriter(t, cfg)

	ts := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		enqueue(LogEntry{
			Network:   "testnet",
			Channel:   "#chan",
			Command:   girc.PRIVMSG,
			Nick:      "user",
			Target:    "#chan",
			Message:   "msg",
			CreatedAt: ts,
		})
	}

	assert.Eventually(t, func() bool {
		pattern := filepath.Join(lw.cfg.Dir, "*.db")
		matches, _ := filepath.Glob(pattern)
		return len(matches) > 0
	}, 5*time.Second, 50*time.Millisecond, "expected db file to be created after batch flush")
}

func TestLogWriterOverflow(t *testing.T) {
	cfg := LoggingConfig{
		BufferSize:    5,
		BatchSize:     100000,
		FlushInterval: time.Hour,
	}

	lw := initTestLogWriter(t, cfg)

	for i := 0; i < 10; i++ {
		enqueue(LogEntry{
			Network:   "testnet",
			Channel:   "#chan",
			Command:   girc.PRIVMSG,
			Nick:      "user",
			Target:    "#chan",
			Message:   "msg",
			CreatedAt: time.Now(),
		})
	}

	assert.Eventually(t, func() bool {
		return lw.dropped.Load() > 0
	}, 2*time.Second, 10*time.Millisecond, "expected dropped counter to be > 0")
}

func TestLogWriterShutdown(t *testing.T) {
	cfg := LoggingConfig{
		BufferSize:    10000,
		BatchSize:     100000,
		FlushInterval: time.Hour,
	}

	lw := initTestLogWriter(t, cfg)

	ts := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		lw.entries <- LogEntry{
			Network:   "testnet",
			Channel:   "#chan",
			Command:   girc.PRIVMSG,
			Nick:      "user",
			Target:    "#chan",
			Message:   "msg",
			CreatedAt: ts,
		}
	}

	stopLogWriter()
	wg.Wait()

	pattern := filepath.Join(lw.cfg.Dir, "*.db")
	matches, err := filepath.Glob(pattern)
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected db file after shutdown flush")

	for _, f := range matches {
		info, err := os.Stat(f)
		require.NoError(t, err)
		assert.True(t, info.Size() > 0, "db file should not be empty")
	}
}
