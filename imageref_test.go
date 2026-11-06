package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupImageRefTest(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	dir := t.TempDir()
	config.ImagesDir = dir
	t.Cleanup(func() { config.ImagesDir = "" })
}

func TestRegisterImageRefRoundTrip(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(42, "view", "https://img.example.com/abc.png", "image/png", 640, 480, []byte("pngbytes"))
	require.NoError(t, err)
	assert.Regexp(t, `^web_[0-9a-f]{12}$`, ref, "view origin mints web_ refs")

	row, err := resolveImageRef(ref)
	require.NoError(t, err)
	assert.Equal(t, int64(42), row.SessionID)
	assert.Equal(t, "view", row.Origin)
	assert.Equal(t, "https://img.example.com/abc.png", row.SourceURL)
	assert.Equal(t, 640, row.Width)
	assert.Equal(t, 480, row.Height)

	data, err := os.ReadFile(row.FilePath)
	require.NoError(t, err)
	assert.Equal(t, []byte("pngbytes"), data, "file stores the encoded bytes")
	assert.Equal(t, ".png", filepath.Ext(row.FilePath))
}

func TestRegisterImageRefUserOriginPrefix(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "user", "https://x/y.jpg", "image/jpeg", 10, 10, []byte("j"))
	require.NoError(t, err)
	assert.Regexp(t, `^usr_[0-9a-f]{12}$`, ref)
}

func TestRegisterImageRefRefsAreUnique(t *testing.T) {
	setupImageRefTest(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ref, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("b"))
		require.NoError(t, err)
		require.False(t, seen[ref], "ref %s minted twice", ref)
		seen[ref] = true
	}
}

func TestResolveImageRefUnknown(t *testing.T) {
	setupImageRefTest(t)
	_, err := resolveImageRef("usr_deadbeefcafe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown image ref")
}

func TestCleanupImageRefsByAge(t *testing.T) {
	setupImageRefTest(t)
	oldRef, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("old"))
	require.NoError(t, err)
	newRef, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("new"))
	require.NoError(t, err)

	cutoff := time.Now().Add(-48 * time.Hour)
	require.NoError(t, theDB.Model(&ImageRef{}).Where("ref = ?", oldRef).Update("created_at", cutoff).Error)

	n, err := cleanupImageRefsByAge(1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	_, err = resolveImageRef(oldRef)
	require.Error(t, err, "old ref deleted")
	row, err := resolveImageRef(newRef)
	require.NoError(t, err)
	_, statErr := os.Stat(row.FilePath)
	assert.NoError(t, statErr, "kept ref's file still exists")
}

func TestSweepOrphanImageFiles(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("kept"))
	require.NoError(t, err)
	var keptRow ImageRef
	require.NoError(t, theDB.Where("ref = ?", ref).First(&keptRow).Error)

	orphan := filepath.Join(config.ImagesDir, "web_orphanedfile.png")
	require.NoError(t, os.WriteFile(orphan, []byte("x"), 0o644))
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(orphan, past, past))

	n, err := sweepOrphanImageFiles(config.ImagesDir, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NoFileExists(t, orphan)
	assert.FileExists(t, keptRow.FilePath, "registered file untouched")
}
