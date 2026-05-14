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
	{ID: 6, Name: "add_users_last_nick", Up: addUsersLastNick},
	{ID: 7, Name: "convert_sentinels_to_released_column", Up: convertSentinelsToReleasedColumn},
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

func addUsersLastNick(db *gorm.DB) error {
	migrator := db.Migrator()

	if !migrator.HasColumn(&User{}, "last_nick") {
		var ddl string
		switch db.Dialector.Name() {
		case "sqlite":
			ddl = "ALTER TABLE users ADD COLUMN last_nick TEXT NOT NULL DEFAULT ''"
		case "postgres":
			ddl = "ALTER TABLE users ADD COLUMN last_nick TEXT NOT NULL DEFAULT ''"
		default:
			return fmt.Errorf("unsupported dialect %s for add_users_last_nick", db.Dialector.Name())
		}
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("adding last_nick column: %w", err)
		}
	}

	result := db.Exec("UPDATE users SET last_nick = current_nick WHERE last_nick = '' AND current_nick != ''")
	if result.Error != nil {
		return fmt.Errorf("backfilling last_nick from current_nick: %w", result.Error)
	}
	if result.RowsAffected > 0 {
		loggerM.Info("backfilled last_nick from current_nick", "rows", result.RowsAffected)
	}

	return nil
}

// convertSentinelsToReleasedColumn replaces the sentinel-in-normalized_nick
// scheme (",quit,<id>" for released users and ",flagged,..." for flagged
// users) with a proper `released BOOLEAN` column plus a partial unique index
// on `(network, normalized_nick) WHERE released = false AND flagged = false`.
//
// Rationale: the original sentinels encoded uniqueness in a string payload
// (because the old idx_users_nick was a full UNIQUE index, so two released
// rows in the same network would collide on `,quit,`). The payload itself
// was opaque to the code — nothing ever parsed `<id>` back out. This
// migration moves that signalling into a real column where it belongs.
//
// Legacy data caveat: the old `releaseUserNick` cleared `current_nick=”`
// at release time. Migration #6 backfills `last_nick = current_nick`, so
// rows that were already released before #6 ran will end up with empty
// `last_nick` too — there is no surviving copy of the original nick on
// disk. For those rows this migration sets `released=true` and leaves the
// `,quit,<id>` sentinel sitting in `normalized_nick` as an inert tombstone.
// The released-nick fallback in `resolveUserOnce` cannot match them. An
// ERROR-level log line surfaces the count so admins running the migration
// see this happen once.
//
// Steps:
//  1. Add the `released` column (defaults false).
//  2. Backfill: for rows whose normalized_nick is a ",quit,*" sentinel,
//     set released=true and restore normalized_nick + current_nick from
//     the last_nick column (added by migration #6). Rows with empty
//     last_nick become tombstones (see caveat above).
//  3. Backfill flagged sentinels (",flagged,*") similarly. Their `flagged`
//     bool is already true (set at creation time in resolveUserFallback).
//  4. Resolve any duplicate (network, normalized_nick) among the now-active
//     rows by marking the older row released=true.
//  5. Drop the old full unique index idx_users_nick.
//  6. Create the new partial unique index idx_users_nick_active.
//  7. Drop the last_nick column (info now lives in current_nick again).
func convertSentinelsToReleasedColumn(db *gorm.DB) error {
	migrator := db.Migrator()

	// Step 1: add `released` column if missing.
	if !migrator.HasColumn(&User{}, "released") {
		var ddl string
		switch db.Dialector.Name() {
		case "sqlite":
			ddl = "ALTER TABLE users ADD COLUMN released INTEGER NOT NULL DEFAULT 0"
		case "postgres":
			ddl = "ALTER TABLE users ADD COLUMN released BOOLEAN NOT NULL DEFAULT FALSE"
		default:
			return fmt.Errorf("unsupported dialect %s for convert_sentinels_to_released_column", db.Dialector.Name())
		}
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("adding released column: %w", err)
		}
	}

	if !migrator.HasIndex(&User{}, "idx_users_released") {
		if err := db.Exec("CREATE INDEX idx_users_released ON users(released)").Error; err != nil {
			return fmt.Errorf("creating idx_users_released: %w", err)
		}
	}

	hasLastNick := migrator.HasColumn(&User{}, "last_nick")

	// Step 2: backfill released sentinels.
	// Sentinels look like ",quit,<id>" — we identify them by the prefix.
	// We restore the real nick from last_nick (migration #6 backfilled it).
	if hasLastNick {
		type sentinelRow struct {
			ID             int64
			Network        string
			NormalizedNick string
			CurrentNick    string
			LastNick       string
		}
		var rows []sentinelRow
		if err := db.Raw(
			"SELECT id, network, normalized_nick, current_nick, last_nick FROM users WHERE normalized_nick LIKE ',quit,%'",
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("querying released sentinel rows: %w", err)
		}
		releasedCount := 0
		tombstoneCount := 0
		for _, r := range rows {
			restoreNorm := normalizeIRC(r.LastNick, "rfc1459")
			restoreCurrent := r.LastNick
			if r.LastNick == "" {
				// No history to restore — leave the sentinel in place. The
				// row stays usable as a placeholder (released=true) but the
				// released-nick fallback cannot match it. See the function
				// docstring's "Legacy data caveat" for context.
				if err := db.Exec(
					"UPDATE users SET released = ? WHERE id = ?", true, r.ID,
				).Error; err != nil {
					return fmt.Errorf("marking released row %d: %w", r.ID, err)
				}
				releasedCount++
				tombstoneCount++
				continue
			}
			if err := db.Exec(
				"UPDATE users SET released = ?, normalized_nick = ?, current_nick = ? WHERE id = ?",
				true, restoreNorm, restoreCurrent, r.ID,
			).Error; err != nil {
				return fmt.Errorf("restoring released row %d: %w", r.ID, err)
			}
			releasedCount++
		}
		if releasedCount > 0 {
			loggerM.Info("converted released sentinels to released column", "rows", releasedCount)
		}
		if tombstoneCount > 0 {
			// Surface this loudly: these rows are released but carry the
			// original ",quit,<id>" sentinel as their normalized_nick. The
			// released-nick fallback can never match them, so any users
			// they originally represented will be created fresh on next
			// interaction. Admins may want to merge those manually via
			// /usermerge once the new identities have built up some
			// evidence (account, known hosts).
			loggerM.Error("legacy_released_rows_preserved_as_tombstones_nick_history_lost",
				"count", tombstoneCount,
				"explanation", "rows released under the old sentinel scheme had current_nick cleared before migration #6 could capture it; migration #7 left their ,quit,<id> sentinel in normalized_nick. They are not matchable by the released-nick fallback.")
		}

		// Step 3: backfill flagged sentinels.
		// flagged column is already true; we only need to restore the real
		// normalized_nick + current_nick from last_nick (if available).
		var flaggedRows []sentinelRow
		if err := db.Raw(
			"SELECT id, network, normalized_nick, current_nick, last_nick FROM users WHERE normalized_nick LIKE ',flagged,%'",
		).Scan(&flaggedRows).Error; err != nil {
			return fmt.Errorf("querying flagged sentinel rows: %w", err)
		}
		flaggedCount := 0
		for _, r := range flaggedRows {
			if r.LastNick == "" {
				// No nick history to restore; leave the sentinel in place.
				continue
			}
			restoreNorm := normalizeIRC(r.LastNick, "rfc1459")
			restoreCurrent := r.LastNick
			if err := db.Exec(
				"UPDATE users SET normalized_nick = ?, current_nick = ? WHERE id = ?",
				restoreNorm, restoreCurrent, r.ID,
			).Error; err != nil {
				return fmt.Errorf("restoring flagged row %d: %w", r.ID, err)
			}
			flaggedCount++
		}
		if flaggedCount > 0 {
			loggerM.Info("restored flagged sentinel nicks", "rows", flaggedCount)
		}
	}

	// Step 4: resolve any duplicate (network, normalized_nick) among the
	// active rows we just restored. If two active rows now collide on the
	// same nick (e.g. user A quit, someone else took the nick, both got
	// their nick "restored" by the backfill), prefer the more recently
	// updated row and release the older one.
	type dupGroup struct {
		Network        string
		NormalizedNick string
		Cnt            int
	}
	var dups []dupGroup
	if err := db.Raw(
		`SELECT network, normalized_nick, COUNT(*) as cnt
		 FROM users
		 WHERE released = ? AND flagged = ?
		 GROUP BY network, normalized_nick
		 HAVING COUNT(*) > 1`,
		false, false,
	).Scan(&dups).Error; err != nil {
		return fmt.Errorf("scanning for duplicate active nicks: %w", err)
	}
	for _, g := range dups {
		// Pull all colliding rows ordered by updated_at desc; keep the
		// freshest as the active owner, mark older ones released.
		var colliders []User
		if err := db.Where(
			"network = ? AND normalized_nick = ? AND released = ? AND flagged = ?",
			g.Network, g.NormalizedNick, false, false,
		).Order("updated_at DESC, id DESC").Find(&colliders).Error; err != nil {
			return fmt.Errorf("loading colliders for %s/%s: %w", g.Network, g.NormalizedNick, err)
		}
		for i := 1; i < len(colliders); i++ {
			loggerM.Info("resolving duplicate active nick by releasing older row",
				"keep_user_id", colliders[0].ID,
				"release_user_id", colliders[i].ID,
				"network", g.Network,
				"normalized_nick", g.NormalizedNick)
			if err := db.Model(&User{}).Where("id = ?", colliders[i].ID).
				Update("released", true).Error; err != nil {
				return fmt.Errorf("releasing duplicate row %d: %w", colliders[i].ID, err)
			}
		}
	}

	// Step 5: drop the old full unique index if it exists. Both sqlite and
	// postgres accept "DROP INDEX IF EXISTS".
	if migrator.HasIndex(&User{}, "idx_users_nick") {
		switch db.Dialector.Name() {
		case "sqlite", "postgres":
			if err := db.Exec("DROP INDEX IF EXISTS idx_users_nick").Error; err != nil {
				return fmt.Errorf("dropping idx_users_nick: %w", err)
			}
		default:
			return fmt.Errorf("unsupported dialect %s for convert_sentinels_to_released_column", db.Dialector.Name())
		}
	}

	// Step 6: create the partial unique index. SQLite and Postgres both
	// support `CREATE UNIQUE INDEX ... WHERE ...` (partial indexes).
	if !migrator.HasIndex(&User{}, "idx_users_nick_active") {
		var ddl string
		switch db.Dialector.Name() {
		case "sqlite":
			ddl = "CREATE UNIQUE INDEX idx_users_nick_active ON users(network, normalized_nick) WHERE released = 0 AND flagged = 0"
		case "postgres":
			ddl = "CREATE UNIQUE INDEX idx_users_nick_active ON users(network, normalized_nick) WHERE released = FALSE AND flagged = FALSE"
		default:
			return fmt.Errorf("unsupported dialect %s for convert_sentinels_to_released_column", db.Dialector.Name())
		}
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("creating idx_users_nick_active: %w", err)
		}
	}

	// Step 7: drop the last_nick column. Its info now lives in current_nick
	// (no longer cleared at release time).
	if hasLastNick {
		switch db.Dialector.Name() {
		case "sqlite":
			if err := db.Exec("ALTER TABLE users DROP COLUMN last_nick").Error; err != nil {
				return fmt.Errorf("dropping last_nick column: %w", err)
			}
		case "postgres":
			if err := db.Exec("ALTER TABLE users DROP COLUMN IF EXISTS last_nick").Error; err != nil {
				return fmt.Errorf("dropping last_nick column: %w", err)
			}
		default:
			return fmt.Errorf("unsupported dialect %s for convert_sentinels_to_released_column", db.Dialector.Name())
		}
	}

	return nil
}
