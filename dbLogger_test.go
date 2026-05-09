package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gormlogger "gorm.io/gorm/logger"
)

type logEntry struct {
	level string
	msg   string
	args  []interface{}
}

type spyLogger struct {
	entries []logEntry
	level   int
}

func (s *spyLogger) Trace(msg string, args ...interface{}) {
	s.entries = append(s.entries, logEntry{"trace", msg, args})
}
func (s *spyLogger) Debug(msg string, args ...interface{}) {
	s.entries = append(s.entries, logEntry{"debug", msg, args})
}
func (s *spyLogger) Info(msg string, args ...interface{}) {
	s.entries = append(s.entries, logEntry{"info", msg, args})
}
func (s *spyLogger) Warn(msg string, args ...interface{}) error {
	s.entries = append(s.entries, logEntry{"warn", msg, args})
	return nil
}
func (s *spyLogger) Error(msg string, args ...interface{}) error {
	s.entries = append(s.entries, logEntry{"error", msg, args})
	return nil
}
func (s *spyLogger) Fatal(msg string, args ...interface{}) {
	s.entries = append(s.entries, logEntry{"fatal", msg, args})
}
func (s *spyLogger) Log(level int, msg string, args []interface{}) {
	s.entries = append(s.entries, logEntry{fmt.Sprintf("%d", level), msg, args})
}
func (s *spyLogger) SetLevel(level int) { s.level = level }
func (s *spyLogger) IsTrace() bool      { return s.level >= 0 }
func (s *spyLogger) IsDebug() bool      { return s.level >= 1 }
func (s *spyLogger) IsInfo() bool       { return s.level >= 2 }
func (s *spyLogger) IsWarn() bool       { return s.level >= 3 }

func newTestGormLogger(spy *spyLogger) *gormLogxiLogger {
	return &gormLogxiLogger{
		logger:        spy,
		level:         gormlogger.Info,
		slowThreshold: 200 * time.Millisecond,
	}
}

func TestGormLogxiLogger_LogMode(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	silent := g.LogMode(gormlogger.Silent)
	assert.Equal(t, gormlogger.Silent, silent.(*gormLogxiLogger).level)

	warn := g.LogMode(gormlogger.Warn)
	assert.Equal(t, gormlogger.Warn, warn.(*gormLogxiLogger).level)

	assert.Equal(t, gormlogger.Info, g.level, "original should not be mutated")
}

func TestGormLogxiLogger_Info(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Info(context.Background(), "test %s", "value")
	require.Len(t, spy.entries, 1)
	assert.Equal(t, "info", spy.entries[0].level)
	assert.Equal(t, "test value", spy.entries[0].msg)
}

func TestGormLogxiLogger_Warn(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Warn(context.Background(), "warning %d", 42)
	require.Len(t, spy.entries, 1)
	assert.Equal(t, "warn", spy.entries[0].level)
	assert.Equal(t, "warning 42", spy.entries[0].msg)
}

func TestGormLogxiLogger_Error(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Error(context.Background(), "error %s", "bad")
	require.Len(t, spy.entries, 1)
	assert.Equal(t, "error", spy.entries[0].level)
	assert.Equal(t, "error bad", spy.entries[0].msg)
}

func TestGormLogxiLogger_Trace_Success(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Trace(context.Background(), time.Now().Add(-10*time.Millisecond),
		func() (string, int64) { return "SELECT 1", 1 }, nil)

	assert.Empty(t, spy.entries, "successful queries should not log by default")
}

func TestGormLogxiLogger_Trace_SuccessWithQueryLogging(t *testing.T) {
	orig := logDBQueries
	logDBQueries = true
	defer func() { logDBQueries = orig }()

	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Trace(context.Background(), time.Now().Add(-10*time.Millisecond),
		func() (string, int64) { return "SELECT 1", 1 }, nil)

	require.Len(t, spy.entries, 1)
	assert.Equal(t, "debug", spy.entries[0].level)
	assert.Equal(t, "query", spy.entries[0].msg)
}

func TestGormLogxiLogger_Trace_RecordNotFound(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Trace(context.Background(), time.Now().Add(-5*time.Millisecond),
		func() (string, int64) { return "SELECT * FROM sessions", 0 },
		gormlogger.ErrRecordNotFound)

	require.Len(t, spy.entries, 1)
	assert.Equal(t, "debug", spy.entries[0].level)
	assert.Equal(t, "record not found", spy.entries[0].msg)
}

func TestGormLogxiLogger_Trace_RealError(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Trace(context.Background(), time.Now().Add(-5*time.Millisecond),
		func() (string, int64) { return "SELECT * FROM sessions", 0 },
		errors.New("connection refused"))

	require.Len(t, spy.entries, 1)
	assert.Equal(t, "error", spy.entries[0].level)
	assert.Equal(t, "database error", spy.entries[0].msg)
}

func TestGormLogxiLogger_Trace_SlowQuery(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)

	g.Trace(context.Background(), time.Now().Add(-500*time.Millisecond),
		func() (string, int64) { return "SELECT * FROM sessions", 100 }, nil)

	require.Len(t, spy.entries, 1)
	assert.Equal(t, "warn", spy.entries[0].level)
	assert.Equal(t, "slow query", spy.entries[0].msg)
}

func TestGormLogxiLogger_Trace_SilentSuppressesAll(t *testing.T) {
	spy := &spyLogger{}
	g := newTestGormLogger(spy)
	silent := g.LogMode(gormlogger.Silent)

	silent.Info(context.Background(), "info msg")
	silent.Warn(context.Background(), "warn msg")
	silent.Error(context.Background(), "error msg")
	silent.Trace(context.Background(), time.Now(),
		func() (string, int64) { return "SELECT 1", 1 }, errors.New("err"))

	assert.Empty(t, spy.entries, "silent mode should suppress all logs")
}

func TestGormLogxiLogger_Trace_WarnLevelSuppressesQueries(t *testing.T) {
	orig := logDBQueries
	logDBQueries = true
	defer func() { logDBQueries = orig }()

	spy := &spyLogger{}
	g := newTestGormLogger(spy)
	warnLevel := g.LogMode(gormlogger.Warn)

	warnLevel.Trace(context.Background(), time.Now().Add(-10*time.Millisecond),
		func() (string, int64) { return "SELECT 1", 1 }, nil)

	assert.Empty(t, spy.entries, "warn level should suppress query logs even with DAVE_DB_QUERIES=1")
}

func TestGormLogxiLogger_Integration(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()
	_ = dbPath

	spy := &spyLogger{}
	theDB.Logger = newTestGormLogger(spy)

	var session Session
	result := theDB.Where("network = ?", "nonexistent").First(&session)
	require.True(t, errors.Is(result.Error, gormlogger.ErrRecordNotFound))

	found := false
	for _, entry := range spy.entries {
		if entry.level == "debug" && strings.Contains(entry.msg, "record not found") {
			found = true
			break
		}
	}
	assert.True(t, found, "should log record not found at debug level")
}
