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

func init() {
	loggerM.SetLevel(logxi.LevelAll)
}

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
	{ID: 2, Name: "create_users_from_sessions", Up: createUsersFromSessions},
	{ID: 3, Name: "normalize_channels_and_reindex", Up: normalizeChannelsAndReindex},
	{ID: 4, Name: "drop_sessions_nick", Up: dropSessionsNick},
	{ID: 5, Name: "add_users_flagged_columns", Up: addUsersFlaggedColumns},
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

// createUsersFromSessions backfills the users table from existing session data.
// For each distinct (network, nick) pair in sessions, creates a User row and
// links sessions to it via user_id. Uses rfc1459 casemapping as default since
// we don't know what casemapping was active when the sessions were created.
// Idempotent: skips if all sessions already have user_id set.
func createUsersFromSessions(db *gorm.DB) error {
	if !db.Migrator().HasTable("sessions") {
		return nil
	}

	if !db.Migrator().HasColumn(&Session{}, "nick") {
		return nil
	}

	type nickPair struct {
		Network string
		Nick    string
	}

	var pairs []nickPair
	result := db.Model(&Session{}).
		Select("DISTINCT network, nick").
		Where("user_id IS NULL").
		Find(&pairs)
	if result.Error != nil {
		return fmt.Errorf("querying distinct nicks: %w", result.Error)
	}
	if len(pairs) == 0 {
		return nil
	}

	loggerM.Info("backfilling users from sessions", "distinct_nicks", len(pairs))

	now := time.Now()
	for _, p := range pairs {
		norm := normalizeIRC(p.Nick, "rfc1459")

		var existingUser User
		err := db.Where("network = ? AND normalized_nick = ?", p.Network, norm).First(&existingUser).Error
		if err == nil {
			result := db.Model(&Session{}).
				Where("network = ? AND nick = ? AND user_id IS NULL", p.Network, p.Nick).
				Update("user_id", existingUser.ID)
			if result.Error != nil {
				return fmt.Errorf("linking sessions to existing user: %w", result.Error)
			}
			continue
		}

		user := User{
			Network:        p.Network,
			CurrentNick:    p.Nick,
			NormalizedNick: norm,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := db.Create(&user).Error; err != nil {
			return fmt.Errorf("creating user for %s/%s: %w", p.Network, p.Nick, err)
		}

		result := db.Model(&Session{}).
			Where("network = ? AND nick = ? AND user_id IS NULL", p.Network, p.Nick).
			Update("user_id", user.ID)
		if result.Error != nil {
			return fmt.Errorf("linking sessions to new user: %w", result.Error)
		}
		loggerM.Info("created user from sessions", "network", p.Network, "nick", p.Nick, "user_id", user.ID)
	}

	return nil
}

func normalizeChannelsAndReindex(db *gorm.DB) error {
	if !db.Migrator().HasTable("sessions") {
		return nil
	}

	type channelPair struct {
		ID      int64
		Channel string
	}
	var pairs []channelPair
	if err := db.Model(&Session{}).Select("id, channel").Find(&pairs).Error; err != nil {
		return fmt.Errorf("querying session channels: %w", err)
	}

	normalized := 0
	for _, p := range pairs {
		norm := normalizeIRC(p.Channel, "rfc1459")
		if norm != p.Channel {
			if err := db.Model(&Session{}).Where("id = ?", p.ID).Update("channel", norm).Error; err != nil {
				return fmt.Errorf("normalizing channel for session %d: %w", p.ID, err)
			}
			normalized++
		}
	}
	if normalized > 0 {
		loggerM.Info("normalized session channels", "count", normalized)
	}

	var pendingJobs []PendingJob
	if err := db.Where("channel IS NOT NULL").Find(&pendingJobs).Error; err == nil {
		pendingNormalized := 0
		for _, pj := range pendingJobs {
			if pj.Channel == nil {
				continue
			}
			norm := normalizeIRC(*pj.Channel, "rfc1459")
			if norm != *pj.Channel {
				if err := db.Model(&PendingJob{}).Where("id = ?", pj.ID).Update("channel", norm).Error; err != nil {
					return fmt.Errorf("normalizing channel for pending job %d: %w", pj.ID, err)
				}
				pendingNormalized++
			}
		}
		if pendingNormalized > 0 {
			loggerM.Info("normalized pending_job channels", "count", pendingNormalized)
		}
	}

	if db.Migrator().HasIndex(&Session{}, "idx_sessions_user") {
		if err := db.Migrator().DropIndex(&Session{}, "idx_sessions_user"); err != nil {
			loggerM.Warn("could not drop old idx_sessions_user", "error", err)
		}
	}

	if err := db.AutoMigrate(&Session{}); err != nil {
		return fmt.Errorf("re-creating session indexes: %w", err)
	}

	return nil
}

func dropSessionsNick(db *gorm.DB) error {
	if !db.Migrator().HasColumn(&Session{}, "nick") {
		return nil
	}

	switch db.Dialector.Name() {
	case "sqlite":
		return db.Exec("ALTER TABLE sessions DROP COLUMN nick").Error
	case "postgres":
		return db.Exec("ALTER TABLE sessions DROP COLUMN IF EXISTS nick").Error
	default:
		return db.Migrator().DropColumn(&Session{}, "nick")
	}
}

// addUsersFlaggedColumns adds the Flagged + FlaggedReason columns and the
// idx_users_flagged index on the users table. Idempotent: columns/index are
// only added if missing. Existing rows default flagged=false, reason=”.
func addUsersFlaggedColumns(db *gorm.DB) error {
	migrator := db.Migrator()

	if !migrator.HasColumn(&User{}, "flagged") {
		var ddl string
		switch db.Dialector.Name() {
		case "sqlite":
			ddl = "ALTER TABLE users ADD COLUMN flagged INTEGER NOT NULL DEFAULT 0"
		case "postgres":
			ddl = "ALTER TABLE users ADD COLUMN flagged BOOLEAN NOT NULL DEFAULT FALSE"
		default:
			if err := migrator.AddColumn(&User{}, "Flagged"); err != nil {
				return fmt.Errorf("adding flagged column: %w", err)
			}
			ddl = ""
		}
		if ddl != "" {
			if err := db.Exec(ddl).Error; err != nil {
				return fmt.Errorf("adding flagged column: %w", err)
			}
		}
	}

	if !migrator.HasColumn(&User{}, "flagged_reason") {
		var ddl string
		switch db.Dialector.Name() {
		case "sqlite":
			ddl = "ALTER TABLE users ADD COLUMN flagged_reason TEXT NOT NULL DEFAULT ''"
		case "postgres":
			ddl = "ALTER TABLE users ADD COLUMN flagged_reason TEXT NOT NULL DEFAULT ''"
		default:
			if err := migrator.AddColumn(&User{}, "FlaggedReason"); err != nil {
				return fmt.Errorf("adding flagged_reason column: %w", err)
			}
			ddl = ""
		}
		if ddl != "" {
			if err := db.Exec(ddl).Error; err != nil {
				return fmt.Errorf("adding flagged_reason column: %w", err)
			}
		}
	}

	if !migrator.HasIndex(&User{}, "idx_users_flagged") {
		if err := db.Exec("CREATE INDEX idx_users_flagged ON users(flagged)").Error; err != nil {
			return fmt.Errorf("creating idx_users_flagged: %w", err)
		}
	}

	return nil
}
