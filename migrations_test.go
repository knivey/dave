package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertMigrationSession(t *testing.T, network, channel, nick, chatCmd, service, model, status string) int64 {
	t.Helper()
	convID := "migration-test-conv"
	var id int64
	rows, err := theDB.Raw(
		"INSERT INTO sessions (network, channel, nick, chat_command, conv_id, service, model, status, first_message, created_at, last_active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', datetime('now'), datetime('now')) RETURNING id",
		network, channel, nick, chatCmd, convID, service, model, status,
	).Rows()
	require.NoError(t, err, "insert migration session")
	defer rows.Close()
	if rows.Next() {
		require.NoError(t, rows.Scan(&id))
	}
	return id
}

func setupMigrationDB(t *testing.T) (dbPath string, cleanup func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath = filepath.Join(tmpDir, "test.db")
	db, err := initDB(DatabaseConfig{Path: dbPath, MaxAgeDays: 90}, logxi.New("test"))
	require.NoError(t, err, "initDB")
	theDB = db
	sessionMgr = NewSessionManager(db)
	return dbPath, func() {
		sessionMgr = nil
		theDB = nil
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	}
}

func setupMigrationDBLegacy(t *testing.T) (dbPath string, cleanup func()) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath = filepath.Join(tmpDir, "test.db")
	db, err := initDB(DatabaseConfig{Path: dbPath, MaxAgeDays: 90}, logxi.New("test"))
	require.NoError(t, err, "initDB")
	theDB = db
	sessionMgr = NewSessionManager(db)
	theDB.Exec("ALTER TABLE sessions ADD COLUMN nick TEXT NOT NULL DEFAULT ''")
	return dbPath, func() {
		sessionMgr = nil
		theDB = nil
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	}
}

func TestMigration1_DropContextKeyIdempotent(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()

	require.False(t, theDB.Migrator().HasColumn(&Session{}, "context_key"),
		"fresh DB should not have context_key column")

	err := dropSessionsContextKey(theDB)
	assert.NoError(t, err, "should succeed on DB without the column")

	err = dropSessionsContextKey(theDB)
	assert.NoError(t, err, "should be idempotent")
	_ = dbPath
}

func TestMigration1_DropContextKeyWithColumn(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()

	theDB.Exec("ALTER TABLE sessions ADD COLUMN context_key TEXT NOT NULL DEFAULT ''")
	theDB.Exec("CREATE INDEX idx_sessions_context_key ON sessions(context_key)")

	require.True(t, theDB.Migrator().HasColumn(&Session{}, "context_key"),
		"column should exist after adding it")

	err := dropSessionsContextKey(theDB)
	require.NoError(t, err, "should drop the column and index")

	assert.False(t, theDB.Migrator().HasColumn(&Session{}, "context_key"),
		"column should be gone after migration")

	err = dropSessionsContextKey(theDB)
	assert.NoError(t, err, "should be idempotent after dropping")
	_ = dbPath
}

func TestMigration1_AllowsInsertAfterDrop(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()

	theDB.Exec("ALTER TABLE sessions ADD COLUMN context_key TEXT NOT NULL DEFAULT ''")
	theDB.Exec("CREATE INDEX idx_sessions_context_key ON sessions(context_key)")

	require.NoError(t, dropSessionsContextKey(theDB))

	userID := ensureTestUser(t, "testnet", "user")
	_, err := sessionMgr.CreateSession("testnet", "#test", userID, "cmd", "svc", "model")
	require.NoError(t, err, "should be able to insert session after dropping context_key")
	_ = dbPath
}

func TestRunMigrations_SkipsApplied(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()

	require.NoError(t, runMigrations(theDB, dbPath), "first run")

	require.NoError(t, runMigrations(theDB, dbPath), "second run should be no-op")

	var count int64
	theDB.Model(&schemaMigration{}).Count(&count)
	assert.Equal(t, int64(len(migrations)), count, "should not duplicate migration records")
}

func TestRunMigrations_CreatesSchemaTable(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()

	require.NoError(t, runMigrations(theDB, dbPath))

	assert.True(t, theDB.Migrator().HasTable(&schemaMigration{}), "schema_migrations table should exist")
	_ = dbPath
}

func TestBackupSQLite(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()

	backupPath, err := backupSQLite(dbPath)
	require.NoError(t, err, "backupSQLite")

	_, err = os.Stat(backupPath)
	require.NoError(t, err, "backup file should exist")
	defer os.Remove(backupPath)

	orig, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	backup, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	assert.Equal(t, orig, backup, "backup should match original")
}

func TestBackupSQLite_PathError(t *testing.T) {
	_, err := backupSQLite("/proc/nonexistent/path/test.db")
	assert.Error(t, err, "should fail for nonexistent path")
}

func TestMigration2_CreateUsersFromSessions(t *testing.T) {
	dbPath, cleanup := setupMigrationDBLegacy(t)
	defer cleanup()

	sid1 := insertMigrationSession(t, "testnet", "#chan", "UserOne", "cmd", "svc", "model", "completed")
	sid2 := insertMigrationSession(t, "testnet", "#chan", "UserTwo", "cmd", "svc", "model", "completed")
	sid3 := insertMigrationSession(t, "testnet", "#chan2", "UserOne", "cmd", "svc", "model", "active")

	require.NoError(t, createUsersFromSessions(theDB))

	var users []User
	theDB.Order("id").Find(&users)
	require.Len(t, users, 2)
	assert.Equal(t, "testnet", users[0].Network)
	assert.Equal(t, "UserOne", users[0].CurrentNick)
	assert.Equal(t, "userone", users[0].NormalizedNick)
	assert.Equal(t, "UserTwo", users[1].CurrentNick)
	assert.Equal(t, "usertwo", users[1].NormalizedNick)

	var s1 Session
	theDB.First(&s1, sid1)
	assert.NotNil(t, s1.UserID)
	assert.Equal(t, users[0].ID, *s1.UserID)

	var s2 Session
	theDB.First(&s2, sid2)
	assert.NotNil(t, s2.UserID)
	assert.Equal(t, users[1].ID, *s2.UserID)

	var s3 Session
	theDB.First(&s3, sid3)
	assert.NotNil(t, s3.UserID)
	assert.Equal(t, users[0].ID, *s3.UserID, "UserOne in #chan2 should map to same user")
	_ = dbPath
}

func TestMigration2_Idempotent(t *testing.T) {
	dbPath, cleanup := setupMigrationDBLegacy(t)
	defer cleanup()

	insertMigrationSession(t, "testnet", "#chan", "UserOne", "cmd", "svc", "model", "completed")

	require.NoError(t, createUsersFromSessions(theDB))

	var count1 int64
	theDB.Model(&User{}).Count(&count1)
	assert.Equal(t, int64(1), count1)

	require.NoError(t, createUsersFromSessions(theDB))

	var count2 int64
	theDB.Model(&User{}).Count(&count2)
	assert.Equal(t, int64(1), count2, "should not create duplicate users")
	_ = dbPath
}

func TestMigration2_EmptyDB(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()

	err := createUsersFromSessions(theDB)
	assert.NoError(t, err, "should succeed on empty DB")
	_ = dbPath
}

func TestMigration2_MergesCaseVariants(t *testing.T) {
	dbPath, cleanup := setupMigrationDBLegacy(t)
	defer cleanup()

	theDB.Exec("INSERT INTO sessions (network, channel, nick, chat_command, conv_id, service, model, status, first_message, created_at, last_active) VALUES ('testnet', '#chan', 'TestNick', 'cmd', 'conv1', 'svc', 'model', 'active', '', datetime('now'), datetime('now'))")
	theDB.Exec("INSERT INTO sessions (network, channel, nick, chat_command, conv_id, service, model, status, first_message, created_at, last_active) VALUES ('testnet', '#chan', 'testnick', 'cmd', 'conv2', 'svc', 'model', 'active', '', datetime('now'), datetime('now'))")

	require.NoError(t, createUsersFromSessions(theDB))

	var users []User
	theDB.Find(&users)
	require.Len(t, users, 1, "case variants should map to single user")
	assert.Equal(t, "testnick", users[0].NormalizedNick)
	_ = dbPath
}

func TestMigration3_NormalizeChannels(t *testing.T) {
	_, cleanup := setupMigrationDBLegacy(t)
	defer cleanup()

	insertMigrationSession(t, "testnet", "#TestChan", "UserOne", "cmd", "svc", "model", "completed")
	insertMigrationSession(t, "testnet", "#UPPER", "UserTwo", "cmd", "svc", "model", "active")

	require.NoError(t, normalizeChannelsAndReindex(theDB))

	var sessions []Session
	theDB.Order("id").Find(&sessions)
	require.Len(t, sessions, 2)
	assert.Equal(t, "#testchan", sessions[0].Channel)
	assert.Equal(t, "#upper", sessions[1].Channel)
}

func TestMigration3_Idempotent(t *testing.T) {
	_, cleanup := setupMigrationDBLegacy(t)
	defer cleanup()

	insertMigrationSession(t, "testnet", "#Already", "UserOne", "cmd", "svc", "model", "completed")

	require.NoError(t, normalizeChannelsAndReindex(theDB))
	require.NoError(t, normalizeChannelsAndReindex(theDB))

	var s Session
	theDB.First(&s)
	assert.Equal(t, "#already", s.Channel)
}

func TestMigration3_EmptyDB(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	err := normalizeChannelsAndReindex(theDB)
	assert.NoError(t, err)
}

func TestMigration4_DropSessionsNick(t *testing.T) {
	_, cleanup := setupMigrationDBLegacy(t)
	defer cleanup()

	require.True(t, theDB.Migrator().HasColumn(&Session{}, "nick"),
		"nick column should exist after setupMigrationDBLegacy")

	require.NoError(t, dropSessionsNick(theDB))

	assert.False(t, theDB.Migrator().HasColumn(&Session{}, "nick"),
		"nick column should be gone after migration")
}

func TestMigration4_Idempotent(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	require.False(t, theDB.Migrator().HasColumn(&Session{}, "nick"),
		"fresh DB should not have nick column")

	err := dropSessionsNick(theDB)
	assert.NoError(t, err, "should succeed on DB without nick column")

	err = dropSessionsNick(theDB)
	assert.NoError(t, err, "should be idempotent")
}

func TestMigration4_AllowsInsertAfterDrop(t *testing.T) {
	_, cleanup := setupMigrationDBLegacy(t)
	defer cleanup()

	require.NoError(t, dropSessionsNick(theDB))

	userID := ensureTestUser(t, "testnet", "user")
	_, err := sessionMgr.CreateSession("testnet", "#test", userID, "cmd", "svc", "model")
	require.NoError(t, err, "should be able to insert session after dropping nick")
}

// TestMigration6_AddUsersLastNick verifies that migration #6 historically
// added the last_nick column and backfilled it from current_nick. The
// column is later dropped by migration #7 — so we cannot assert via the
// User struct (which no longer has LastNick). We query the raw column.
//
// Migration #6 is now a transitional step that exists only to give #7 a
// source of nick history when upgrading old DBs that still have sentinel
// values in normalized_nick.
func TestMigration6_AddUsersLastNick(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	userID := ensureTestUser(t, "testnet", "ActiveUser")

	require.NoError(t, addUsersLastNick(theDB), "first run re-adds the column")
	require.NoError(t, addUsersLastNick(theDB), "idempotent")

	type row struct {
		CurrentNick string
		LastNick    string
	}
	var r row
	require.NoError(t, theDB.Raw(
		"SELECT current_nick, last_nick FROM users WHERE id = ?", userID,
	).Scan(&r).Error)
	assert.Equal(t, "ActiveUser", r.CurrentNick)
	assert.Equal(t, "ActiveUser", r.LastNick, "last_nick should be backfilled from current_nick")
}

// TestMigration7_ConvertSentinelsToReleasedColumn covers the conversion of
// the ",quit,<id>" / ",flagged,..." sentinel-in-normalized_nick scheme into
// a proper released column with a partial unique index.
//
// Because setupMigrationDB already runs ALL migrations (including #7) to
// completion, the DB is already in the post-migration state. We simulate
// an old DB by manually creating sentinel rows AFTER migration and then
// re-running convertSentinelsToReleasedColumn — but the migrator's
// schema_migrations table already has #7, so we'd need to call the
// function directly. The migration is idempotent in the sense that the
// backfill query is filtered by LIKE patterns; re-running it just picks
// up any new sentinel rows.
func TestMigration7_ConvertSentinelsToReleasedColumn(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	// Pre-condition: last_nick column was dropped by #7.
	assert.False(t, theDB.Migrator().HasColumn(&User{}, "last_nick"),
		"last_nick should be dropped after #7")

	// Pre-condition: partial unique index exists.
	assert.True(t, theDB.Migrator().HasIndex(&User{}, "idx_users_nick_active"),
		"idx_users_nick_active should exist after #7")

	// Pre-condition: released column exists.
	assert.True(t, theDB.Migrator().HasColumn(&User{}, "released"),
		"released column should exist after #7")
}

// TestMigration7_ReplaysOnLegacySentinels simulates an upgrade from a DB
// that still has legacy ",quit,*" sentinels by undoing parts of #7 and
// running it again. Verifies that backfill restores normalized_nick from
// last_nick and marks the row released.
func TestMigration7_ReplaysOnLegacySentinels(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	// Re-add last_nick so the backfill has something to read.
	require.NoError(t, theDB.Exec(
		"ALTER TABLE users ADD COLUMN last_nick TEXT NOT NULL DEFAULT ''",
	).Error)

	userID := ensureTestUser(t, "testnet", "RealNick")
	// Simulate the legacy state: nick replaced by sentinel, current_nick
	// cleared, last_nick holding the original nick.
	require.NoError(t, theDB.Exec(
		"UPDATE users SET normalized_nick = ?, current_nick = '', last_nick = ?, released = ? WHERE id = ?",
		",quit,42", "RealNick", false, userID,
	).Error)

	// Drop the partial unique index so we can re-run #7 cleanly.
	_ = theDB.Exec("DROP INDEX IF EXISTS idx_users_nick_active").Error

	require.NoError(t, convertSentinelsToReleasedColumn(theDB), "replay")

	type row struct {
		NormalizedNick string
		CurrentNick    string
		Released       bool
	}
	var r row
	require.NoError(t, theDB.Raw(
		"SELECT normalized_nick, current_nick, released FROM users WHERE id = ?", userID,
	).Scan(&r).Error)
	assert.Equal(t, "realnick", r.NormalizedNick, "normalized_nick restored from last_nick")
	assert.Equal(t, "RealNick", r.CurrentNick, "current_nick restored from last_nick")
	assert.True(t, r.Released, "released should be true for legacy ,quit, sentinel rows")
}

// TestMigration7_ResolvesDuplicates verifies that the duplicate-resolution
// step in migration #7 marks the older of two colliding active rows as
// released so the partial unique index can be created.
//
// We stage the collision directly: two rows with released=false, flagged=false,
// same (network, normalized_nick). This is a state the OLD full UNIQUE index
// could not have allowed, but it can arise from DB-state drift, manual admin
// fixes, or migration replays. We must drop the partial unique index first
// so the inserts themselves can land. ensureTestUser cannot stage this
// because it dedupes by (network, normalized_nick) — we use raw inserts.
func TestMigration7_ResolvesDuplicates(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	// Drop the partial unique index so we can insert duplicate rows.
	_ = theDB.Exec("DROP INDEX IF EXISTS idx_users_nick_active").Error

	now := time.Now()
	older := &User{
		Network:        "testnet",
		CurrentNick:    "Dupe",
		NormalizedNick: "dupe",
		Released:       false,
		Flagged:        false,
		CreatedAt:      now.Add(-2 * time.Hour),
		UpdatedAt:      now.Add(-2 * time.Hour),
	}
	require.NoError(t, theDB.Create(older).Error)

	newer := &User{
		Network:        "testnet",
		CurrentNick:    "Dupe",
		NormalizedNick: "dupe",
		Released:       false,
		Flagged:        false,
		CreatedAt:      now.Add(-1 * time.Hour),
		UpdatedAt:      now.Add(-1 * time.Hour),
	}
	require.NoError(t, theDB.Create(newer).Error)
	require.NotEqual(t, older.ID, newer.ID, "two distinct rows on the same nick")

	// Run #7 again. The released-sentinel and flagged-sentinel loops
	// touch nothing (no sentinels). The duplicate detector should fire,
	// pick the newer row, and release the older one.
	require.NoError(t, convertSentinelsToReleasedColumn(theDB), "duplicate-resolution path")

	type row struct {
		Released bool
	}
	var rOlder, rNewer row
	require.NoError(t, theDB.Raw("SELECT released FROM users WHERE id = ?", older.ID).Scan(&rOlder).Error)
	require.NoError(t, theDB.Raw("SELECT released FROM users WHERE id = ?", newer.ID).Scan(&rNewer).Error)
	assert.True(t, rOlder.Released, "older row should be released by duplicate resolver")
	assert.False(t, rNewer.Released, "newer row should stay active")

	// Partial unique index should have been recreated.
	assert.True(t, theDB.Migrator().HasIndex(&User{}, "idx_users_nick_active"),
		"partial unique index should be recreated after resolution")
}
