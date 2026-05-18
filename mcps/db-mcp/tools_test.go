package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestEnv(t *testing.T) (*ToolHandlers, *sqlx.DB) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		Server: ServerConfig{Name: "test", Version: "0.1.0"},
		Database: DatabaseConfig{
			Path:            filepath.Join(dir, "test.db"),
			MaxValueSize:    10000,
			MaxNotesPerUser: 500,
		},
	}
	db, err := initDB(cfg.Database.Path)
	require.NoError(t, err)
	t.Cleanup(func() { closeDB(db) })
	h := NewToolHandlers(cfg, db)
	return h, db
}

func testScope() scopeFields {
	return scopeFields{
		Network: "libera",
		Channel: "#test",
		UserID:  1,
		Nick:    "alice",
	}
}

func TestHandlePutNote(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, out, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "hello world",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), out.ID)
	assert.Equal(t, "topic", out.Key)
	assert.NotEmpty(t, out.CreatedAt)
}

func TestHandlePutNoteEmptyKey(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, _, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "",
		Value:       "hello",
	})
	assert.Error(t, err)
}

func TestHandleGetNotes(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, _, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "first",
	})
	require.NoError(t, err)

	_, _, err = h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "second",
	})
	require.NoError(t, err)

	_, out, err := h.handleGetNotes(context.Background(), nil, GetNotesInput{
		scopeFields: s,
		Key:         "topic",
	})
	require.NoError(t, err)
	require.Len(t, out.Notes, 2)
	assert.Equal(t, "second", out.Notes[0].Value)
	assert.Equal(t, "first", out.Notes[1].Value)
}

func TestHandleGetNotesEmpty(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, out, err := h.handleGetNotes(context.Background(), nil, GetNotesInput{
		scopeFields: s,
		Key:         "nonexistent",
	})
	require.NoError(t, err)
	assert.Empty(t, out.Notes)
}

func TestHandleDeleteNoteNotOwned(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, putOut, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "mine",
	})
	require.NoError(t, err)

	bobScope := testScope()
	bobScope.UserID = 2
	bobScope.Nick = "bob"

	_, _, err = h.handleDeleteNote(context.Background(), nil, DeleteNoteInput{
		scopeFields: bobScope,
		ID:          putOut.ID,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "note not found or not owned by you")
}

func TestHandleListKeys(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, _, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "v1",
	})
	require.NoError(t, err)

	_, _, err = h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "v2",
	})
	require.NoError(t, err)

	_, _, err = h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "idea",
		Value:       "v3",
	})
	require.NoError(t, err)

	_, out, err := h.handleListKeys(context.Background(), nil, ListKeysInput{
		scopeFields: s,
	})
	require.NoError(t, err)
	require.Len(t, out.Keys, 2)
	assert.Equal(t, "topic", out.Keys[0].Key)
	assert.Equal(t, 2, out.Keys[0].Count)
	assert.Equal(t, "idea", out.Keys[1].Key)
	assert.Equal(t, 1, out.Keys[1].Count)
}

func TestHandleCountNotes(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, _, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "hello",
	})
	require.NoError(t, err)

	_, out, err := h.handleCountNotes(context.Background(), nil, CountNotesInput{
		scopeFields: s,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, out.Count)
}

func TestHandleSearchNotes(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, _, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "the quick brown fox",
	})
	require.NoError(t, err)

	_, out, err := h.handleSearchNotes(context.Background(), nil, SearchNotesInput{
		scopeFields: s,
		Query:       "brown fox",
	})
	require.NoError(t, err)
	require.Len(t, out.Notes, 1)
	assert.Equal(t, "the quick brown fox", out.Notes[0].Value)
	assert.Equal(t, 1, out.Total)
}

func TestHandleDeleteNotesByKey(t *testing.T) {
	h, _ := setupTestEnv(t)
	s := testScope()

	_, _, err := h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "first",
	})
	require.NoError(t, err)

	_, _, err = h.handlePutNote(context.Background(), nil, PutNoteInput{
		scopeFields: s,
		Key:         "topic",
		Value:       "second",
	})
	require.NoError(t, err)

	_, out, err := h.handleDeleteNotes(context.Background(), nil, DeleteNotesInput{
		scopeFields: s,
		Key:         "topic",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), out.DeletedCount)
}
