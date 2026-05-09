package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/gorm"
)

var loggerM = logxi.New("migrations")

// When you remove a NOT NULL column from a GORM struct (e.g. Session), GORM's
// AutoMigrate won't drop it — it only adds missing columns and indexes. The old
// column stays with its NOT NULL constraint, and all INSERTs fail because the Go
// code no longer provides a value for it. Add a migration here to drop the column.
//
// Each migration should be idempotent — guard with HasColumn/HasIndex checks so
// re-running against a fresh or already-migrated DB is safe.
type migration struct {
	ID   int
	Name string
	Up   func(db *gorm.DB) error
}

type schemaMigration struct {
	ID        int64     `gorm:"primaryKey;autoIncrement"`
	Migration int       `gorm:"not null;uniqueIndex"`
	Name      string    `gorm:"not null"`
	AppliedAt time.Time `gorm:"not null"`
}

var migrations = []migration{
	{ID: 1, Name: "drop_sessions_context_key", Up: dropSessionsContextKey},
}

func runMigrations(db *gorm.DB, dbPath string) error {
	if err := db.AutoMigrate(&schemaMigration{}); err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	for _, m := range migrations {
		var count int64
		db.Model(&schemaMigration{}).Where("migration = ?", m.ID).Count(&count)
		if count > 0 {
			continue
		}

		loggerM.Info("running migration", "id", m.ID, "name", m.Name)

		isSQLite := db.Dialector.Name() == "sqlite"
		var backupPath string

		if isSQLite && dbPath != "" {
			var err error
			backupPath, err = backupSQLite(dbPath)
			if err != nil {
				return fmt.Errorf("migration %d (%s): backup failed: %w", m.ID, m.Name, err)
			}
			loggerM.Info("created DB backup", "path", backupPath)
		}

		if err := m.Up(db); err != nil {
			if backupPath != "" {
				loggerM.Error("migration failed, backup available", "id", m.ID, "name", m.Name, "error", err, "backup", backupPath)
			}
			return fmt.Errorf("migration %d (%s): %w", m.ID, m.Name, err)
		}

		db.Create(&schemaMigration{
			Migration: m.ID,
			Name:      m.Name,
			AppliedAt: time.Now(),
		})

		loggerM.Info("migration complete", "id", m.ID, "name", m.Name)
	}

	return nil
}

func backupSQLite(dbPath string) (string, error) {
	src, err := os.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("opening db for backup: %w", err)
	}
	defer src.Close()

	fi, err := src.Stat()
	if err != nil {
		return "", fmt.Errorf("stat db: %w", err)
	}
	dbSize := fi.Size()

	var stat syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(dbPath), &stat); err != nil {
		return "", fmt.Errorf("checking disk space: %w", err)
	}
	freeSpace := int64(stat.Bavail) * int64(stat.Bsize)
	if freeSpace < dbSize+1024 {
		return "", fmt.Errorf("insufficient disk space: need %d bytes, have %d free", dbSize+1024, freeSpace)
	}

	backupPath := dbPath + ".pre-migration.bak"
	dst, err := os.Create(backupPath)
	if err != nil {
		return "", fmt.Errorf("creating backup file: %w", err)
	}
	defer dst.Close()

	if _, err := io.Copy(dst, src); err != nil {
		os.Remove(backupPath)
		return "", fmt.Errorf("copying db to backup: %w", err)
	}

	if err := dst.Sync(); err != nil {
		os.Remove(backupPath)
		return "", fmt.Errorf("syncing backup: %w", err)
	}

	return backupPath, nil
}

func dropSessionsContextKey(db *gorm.DB) error {
	if !db.Migrator().HasColumn(&Session{}, "context_key") {
		return nil
	}

	if db.Migrator().HasIndex(&Session{}, "idx_sessions_context_key") {
		if err := db.Migrator().DropIndex(&Session{}, "idx_sessions_context_key"); err != nil {
			return fmt.Errorf("dropping index idx_sessions_context_key: %w", err)
		}
	}

	switch db.Dialector.Name() {
	case "sqlite":
		return db.Exec("ALTER TABLE sessions DROP COLUMN context_key").Error
	case "postgres":
		return db.Exec("ALTER TABLE sessions DROP COLUMN IF EXISTS context_key").Error
	default:
		return db.Migrator().DropColumn(&Session{}, "context_key")
	}
}
