package main

import (
	"os"
	"path/filepath"
	"testing"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	_, err := sessionMgr.CreateSession("testnet", "#test", "user", "cmd", "svc", "model")
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
