package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	gormlogger "gorm.io/gorm/logger"
)

var logDBQueries = os.Getenv("DAVE_DB_QUERIES") == "1"

type gormLogxiLogger struct {
	logger        logxi.Logger
	level         gormlogger.LogLevel
	slowThreshold time.Duration
}

func newGormLogger(l logxi.Logger) *gormLogxiLogger {
	return &gormLogxiLogger{
		logger:        l,
		level:         gormlogger.Info,
		slowThreshold: 200 * time.Millisecond,
	}
}

func (g *gormLogxiLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *g
	newLogger.level = level
	return &newLogger
}

func (g *gormLogxiLogger) Info(_ context.Context, msg string, args ...interface{}) {
	if g.level >= gormlogger.Info {
		g.logger.Info(fmt.Sprintf(msg, args...))
	}
}

func (g *gormLogxiLogger) Warn(_ context.Context, msg string, args ...interface{}) {
	if g.level >= gormlogger.Warn {
		g.logger.Warn(fmt.Sprintf(msg, args...))
	}
}

func (g *gormLogxiLogger) Error(_ context.Context, msg string, args ...interface{}) {
	if g.level >= gormlogger.Error {
		g.logger.Error(fmt.Sprintf(msg, args...))
	}
}

func fmtDuration(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%dns", d)
	}
}

func (g *gormLogxiLogger) Trace(_ context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	if g.level < gormlogger.Info {
		return
	}

	elapsed := time.Since(begin)
	dur := fmtDuration(elapsed)

	if err != nil && !errors.Is(err, gormlogger.ErrRecordNotFound) {
		sql, rows := fc()
		g.logger.Error("database error",
			"error", err,
			"duration", dur,
			"rows", rows,
			"sql", sql,
		)
		return
	}

	if g.slowThreshold > 0 && elapsed > g.slowThreshold {
		sql, rows := fc()
		g.logger.Warn("slow query",
			"duration", dur,
			"rows", rows,
			"sql", sql,
		)
		return
	}

	if errors.Is(err, gormlogger.ErrRecordNotFound) {
		g.logger.Debug("record not found", "duration", dur)
		return
	}

	if logDBQueries {
		sql, rows := fc()
		g.logger.Debug("query",
			"duration", dur,
			"rows", rows,
			"sql", sql,
		)
	}
}
