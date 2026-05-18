package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestDBDirect(t *testing.T) *sqlx.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := initDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { closeDB(db) })
	return db
}

func TestInitDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "subdir", "test.db")
	db, err := initDB(dbPath)
	require.NoError(t, err)
	require.NotNil(t, db)
	closeDB(db)
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{"1h", 1 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"7d", 168 * time.Hour, false},
		{"30d", 720 * time.Hour, false},
		{"abc", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDuration(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, got)
			}
		})
	}
}

func TestDBInsertAndGetNote(t *testing.T) {
	db := setupTestDBDirect(t)

	note, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "hello world", 10000)
	require.NoError(t, err)
	assert.Equal(t, int64(1), note.ID)
	assert.Equal(t, "testnet", note.Network)
	assert.Equal(t, "#test", note.Channel)
	assert.Equal(t, int64(1), note.UserID)
	assert.Equal(t, "alice", note.Nick)
	assert.Equal(t, "topic", note.Key)
	assert.Equal(t, "hello world", note.Value)

	notes, err := dbGetNotesByKey(db, "testnet", "#test", "topic", "")
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "hello world", notes[0].Value)
}

func TestDBInsertTruncation(t *testing.T) {
	db := setupTestDBDirect(t)

	longValue := strings.Repeat("a", 150)
	note, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "key", longValue, 100)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("a", 100)+"[truncated]", note.Value)
}

func TestDBMultiValueKey(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "first", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "second", 10000)
	require.NoError(t, err)

	notes, err := dbGetNotesByKey(db, "testnet", "#test", "topic", "")
	require.NoError(t, err)
	assert.Len(t, notes, 2)
	assert.Equal(t, "second", notes[0].Value)
	assert.Equal(t, "first", notes[1].Value)
}

func TestDBGetNotesFilterNick(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "from alice", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 2, "bob", "topic", "from bob", 10000)
	require.NoError(t, err)

	notes, err := dbGetNotesByKey(db, "testnet", "#test", "topic", "bob")
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "from bob", notes[0].Value)
}

func TestDBDeleteNoteOwnership(t *testing.T) {
	db := setupTestDBDirect(t)

	note, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "mine", 10000)
	require.NoError(t, err)

	deleted, err := dbDeleteNote(db, note.ID, "testnet", "#test", 2)
	require.NoError(t, err)
	assert.False(t, deleted)

	deleted, err = dbDeleteNote(db, note.ID, "testnet", "#test", 1)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func TestDBDeleteNotesByKey(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "a1", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "a2", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 2, "bob", "topic", "b1", 10000)
	require.NoError(t, err)

	affected, err := dbDeleteNotesByKey(db, "testnet", "#test", 1, "topic")
	require.NoError(t, err)
	assert.Equal(t, int64(2), affected)

	notes, err := dbGetNotesByKey(db, "testnet", "#test", "topic", "")
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "b1", notes[0].Value)
}

func TestDBListKeys(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "v1", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "v2", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 1, "alice", "idea", "v3", 10000)
	require.NoError(t, err)

	keys, err := dbListKeys(db, "testnet", "#test", "", 0)
	require.NoError(t, err)
	require.Len(t, keys, 2)
	assert.Equal(t, "topic", keys[0].Key)
	assert.Equal(t, 2, keys[0].Count)
	assert.Equal(t, "idea", keys[1].Key)
	assert.Equal(t, 1, keys[1].Count)
}

func TestDBCountNotes(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "v1", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 1, "alice", "idea", "v2", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 2, "bob", "topic", "v3", 10000)
	require.NoError(t, err)

	count, err := dbCountNotes(db, "testnet", "#test", "", "", "")
	require.NoError(t, err)
	assert.Equal(t, 3, count)

	count, err = dbCountNotes(db, "testnet", "#test", "topic", "", "")
	require.NoError(t, err)
	assert.Equal(t, 2, count)

	count, err = dbCountNotes(db, "testnet", "#test", "", "alice", "")
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestDBPruneUserNotes(t *testing.T) {
	db := setupTestDBDirect(t)

	for i := 0; i < 5; i++ {
		_, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "key", string(rune('a'+i)), 10000)
		require.NoError(t, err)
	}

	affected, err := dbPruneUserNotes(db, "testnet", 1, 3)
	require.NoError(t, err)
	assert.Equal(t, int64(2), affected)

	count, err := dbCountNotes(db, "testnet", "#test", "", "", "")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestDBSearchNotes(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "the quick brown fox", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#test", 1, "alice", "topic", "lazy dog sleeps", 10000)
	require.NoError(t, err)

	notes, err := dbSearchNotes(db, "testnet", "#test", "quick", "", "", "", 0)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "the quick brown fox", notes[0].Value)
}

func TestDBRecentNotes(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "libera", "#test", 1, "alice", "notes", "note from alice", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "libera", "#test", 2, "bob", "notes", "note from bob", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "libera", "#test", 1, "alice", "idea", "an idea", 10000)
	require.NoError(t, err)

	notes, err := dbRecentNotes(db, "libera", "#test", "1h", "", "", 0)
	require.NoError(t, err)
	assert.Len(t, notes, 3)

	notes, err = dbRecentNotes(db, "libera", "#test", "1h", "notes", "", 0)
	require.NoError(t, err)
	assert.Len(t, notes, 2)

	notes, err = dbRecentNotes(db, "libera", "#test", "1h", "", "bob", 0)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "note from bob", notes[0].Value)

	notes, err = dbRecentNotes(db, "libera", "#test", "1h", "", "", 1)
	require.NoError(t, err)
	assert.Len(t, notes, 1)
}

func TestDBSearchNotesWithFilters(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "libera", "#test", 1, "alice", "topic", "the quick brown fox", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "libera", "#test", 2, "bob", "topic", "the quick brown cat", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "libera", "#test", 1, "alice", "notes", "a quick observation", 10000)
	require.NoError(t, err)

	notes, err := dbSearchNotes(db, "libera", "#test", "quick", "topic", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, notes, 2)

	notes, err = dbSearchNotes(db, "libera", "#test", "quick", "topic", "bob", "", 10)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "the quick brown cat", notes[0].Value)

	notes, err = dbSearchNotes(db, "libera", "#test", "quick", "", "", "1h", 10)
	require.NoError(t, err)
	assert.Len(t, notes, 3)
}

func TestDBListKeysFilterNick(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "libera", "#test", 1, "alice", "topic", "v1", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "libera", "#test", 2, "bob", "idea", "v2", 10000)
	require.NoError(t, err)

	keys, err := dbListKeys(db, "libera", "#test", "alice", 0)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "topic", keys[0].Key)
}

func TestDBChannelIsolation(t *testing.T) {
	db := setupTestDBDirect(t)

	_, err := dbInsertNote(db, "testnet", "#chan1", 1, "alice", "topic", "in chan1", 10000)
	require.NoError(t, err)
	_, err = dbInsertNote(db, "testnet", "#chan2", 1, "alice", "topic", "in chan2", 10000)
	require.NoError(t, err)

	notes, err := dbGetNotesByKey(db, "testnet", "#chan1", "topic", "")
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "in chan1", notes[0].Value)

	count, err := dbCountNotes(db, "testnet", "#chan2", "", "", "")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}
