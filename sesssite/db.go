package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// openDB opens dave's database READ-ONLY — sesssite has no write path,
// by design (spec: "strictly read-only over dave's DB"). The only
// place sesssite's models create tables is the test suite's AutoMigrate.
//
// sqlite: mode=ro DSN (WAL readers work when dave is running and keeps
// -shm alive; if the ro open fails because dave is stopped and no
// -wal/-shm exists, fall back to a normal open with
// PRAGMA query_only=1 — SQL-level write rejection, which still honors
// the read-only contract). postgres: the DSN should belong to a
// read-only role; the code never issues writes.
func openDB(cfg DatabaseConfig) (*gorm.DB, error) {
	switch cfg.Driver {
	case "postgres":
		db, err := gorm.Open(postgres.Open(cfg.DSN), &gorm.Config{Logger: newGormLogger(loggerDB)})
		if err != nil {
			return nil, fmt.Errorf("opening postgres: %w", err)
		}
		return db, nil
	default:
		abs, err := filepath.Abs(cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("resolving db path: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("database %s: %w", abs, err)
		}
		dsn := "file:" + abs + "?mode=ro&_pragma=busy_timeout(5000)"
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: newGormLogger(loggerDB)})
		if err == nil {
			return db, nil
		}
		dsn = "file:" + abs + "?_pragma=busy_timeout(5000)&_pragma=query_only(1)"
		db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: newGormLogger(loggerDB)})
		if err != nil {
			return nil, fmt.Errorf("opening sqlite read-only: %w", err)
		}
		return db, nil
	}
}
