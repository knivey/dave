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

func TestResolveImageRefArgsRewritesUsrWebPassesGen(t *testing.T) {
	setupImageRefTest(t)
	usrRef, err := registerImageRef(1, "user", "https://x/a.jpg", "image/jpeg", 10, 10, []byte("USRB"))
	require.NoError(t, err)
	webRef, err := registerImageRef(1, "view", "https://x/b.png", "image/png", 10, 10, []byte("WEBB"))
	require.NoError(t, err)

	entries := []string{usrRef, "gen_abc_0", webRef, "data:image/png;base64,QQ=="}

	// Shape 1: []string (programmatic callers)
	args := map[string]any{"input_images": append([]string(nil), entries...)}
	require.NoError(t, resolveImageRefArgs(args))
	got := args["input_images"].([]string)
	assert.Equal(t, "data:image/jpeg;base64,VVNSQg==", got[0], "usr_ ref rewritten from the stored file's bytes")
	assert.Equal(t, "gen_abc_0", got[1], "gen_ passthrough")
	assert.Equal(t, "data:image/png;base64,V0VCQg==", got[2], "web_ ref rewritten")
	assert.Equal(t, "data:image/png;base64,QQ==", got[3], "existing data URI untouched")

	// Shape 2: []any (what json.Unmarshal of model args produces)
	anyEntries := make([]any, len(entries))
	for i, e := range entries {
		anyEntries[i] = e
	}
	args2 := map[string]any{"input_images": anyEntries}
	require.NoError(t, resolveImageRefArgs(args2))
	got2 := args2["input_images"].([]any)
	assert.Equal(t, "data:image/jpeg;base64,VVNSQg==", got2[0])
	assert.Equal(t, "gen_abc_0", got2[1])
	assert.Equal(t, "data:image/png;base64,V0VCQg==", got2[2])

	// No input_images -> no-op, no error
	plain := map[string]any{"prompt": "hi"}
	require.NoError(t, resolveImageRefArgs(plain))
}

func TestResolveImageRefArgsUnresolvableRef(t *testing.T) {
	setupImageRefTest(t)
	args := map[string]any{"input_images": []any{"usr_deadbeefcafe"}}
	err := resolveImageRefArgs(args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usr_deadbeefcafe")
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
