package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenDBSQLiteReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dave.db")

	// Create a writable dave-style DB, then open it read-only.
	w, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, w.AutoMigrate(&Session{}, &User{}))
	require.NoError(t, w.Create(&Session{Slug: "aaaaaaaa", Network: "n", Channel: "#c", ChatCommand: "d", Status: "active"}).Error)
	sqlDB, _ := w.DB()
	sqlDB.Close()

	ro, err := openDB(DatabaseConfig{Driver: "sqlite", Path: path})
	require.NoError(t, err)

	var s Session
	require.NoError(t, ro.Where("slug = ?", "aaaaaaaa").First(&s).Error)
	assert.Equal(t, "n", s.Network)

	// Writes through the read-only handle must fail.
	err = ro.Exec("UPDATE sessions SET status = 'x'").Error
	assert.Error(t, err, "read-only handle must reject writes")
}

// TestOpenDBSQLiteWALFallbackStillReadOnly simulates dave dying without
// a clean close: a -wal file exists with no -shm. mode=ro cannot run
// recovery or create the -shm, so openDB must take its documented
// fallback (normal open + PRAGMA query_only=1) — and the read-only
// contract must hold on that path too, not just the mode=ro path.
func TestOpenDBSQLiteWALFallbackStillReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dave.db")
	w, err := gorm.Open(sqlite.Open(path+"?_pragma=journal_mode(WAL)"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, w.AutoMigrate(&Session{}, &User{}))
	require.NoError(t, w.Create(&Session{Slug: "aaaaaaaa", Network: "n", Channel: "#c", ChatCommand: "d", Status: "active"}).Error)

	// Copy main + wal WITHOUT the -shm: the crash image openDB must survive.
	tgt := t.TempDir()
	tpath := filepath.Join(tgt, "dave.db")
	for _, f := range []string{path, path + "-wal"} {
		b, readErr := os.ReadFile(f)
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(filepath.Join(tgt, filepath.Base(f)), b, 0o644))
	}
	sqlDB, _ := w.DB()
	sqlDB.Close()

	ro, err := openDB(DatabaseConfig{Driver: "sqlite", Path: tpath})
	require.NoError(t, err)

	// The fallback's normal open creates the -shm; the mode=ro path
	// never can. Its presence proves the fallback actually ran here.
	_, err = os.Stat(tpath + "-shm")
	require.NoError(t, err, "fallback must have created the -shm")

	// The -wal was recovered: the row written before the crash reads.
	var s Session
	require.NoError(t, ro.Where("slug = ?", "aaaaaaaa").First(&s).Error)
	assert.Equal(t, "n", s.Network)

	err = ro.Exec("UPDATE sessions SET status = 'x'").Error
	assert.Error(t, err, "query_only fallback must reject writes")
}

// TestModelColumnsMatchDave pins the column names sesssite's read models
// produce against the names dave's own structs (db.go) declare. The other
// tests AutoMigrate THESE models, so a typo'd tag would be self-consistent
// and invisible — production reads dave's real schema and would break.
// The frozen lists below are dave's column names for the subset the site
// reads; keep them in sync with dave's db.go when dave's schema evolves.
func TestModelColumnsMatchDave(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "s.db")), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Session{}, &User{}, &Message{}, &TurnUsage{}, &Compaction{}))

	expected := map[string][]string{
		"sessions":    {"id", "slug", "network", "channel", "chat_command", "first_message", "service", "model", "status", "created_at", "last_active", "deleted_at", "user_id", "cloned_from_id", "cloned_from_nick"},
		"users":       {"id", "slug", "network", "current_nick", "account", "created_at", "updated_at"},
		"messages":    {"id", "session_id", "role", "content", "tool_calls", "tool_call_id", "reasoning_content", "reasoning_summary", "multi_content", "archived", "compaction_id", "source_compaction_id", "superseded", "created_at", "user_id"},
		"turn_usage":  {"id", "session_id", "model", "service", "reasoning_effort", "prompt_tokens", "completion_tokens", "cached_tokens", "reasoning_tokens", "finish_reason", "api_path", "duration_ms", "created_at"},
		"compactions": {"id", "session_id", "summary_message_id", "first_archived_id", "last_archived_id", "archived_count", "service", "model", "trigger", "prompt_tokens", "completion_tokens", "duration_ms", "created_at"},
	}
	for table, want := range expected {
		rows, err := db.Raw("SELECT name FROM pragma_table_info(?)", table).Rows()
		require.NoError(t, err, table)
		var got []string
		for rows.Next() {
			var n string
			require.NoError(t, rows.Scan(&n))
			got = append(got, n)
		}
		rows.Close()
		sort.Strings(got)
		sort.Strings(want)
		assert.Equal(t, want, got, "table %s columns drifted from dave's schema", table)
	}
}
