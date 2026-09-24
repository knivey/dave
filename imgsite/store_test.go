package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testHash(b byte) string {
	return strings.Repeat(string(b), 64)
}

func TestStoreWriteOriginalFanoutLayout(t *testing.T) {
	base := t.TempDir()
	store := NewStore(filepath.Join(base, "images"), filepath.Join(base, "thumbs"))
	hash := testHash('a')

	wrote, err := store.WriteOriginal(hash, []byte("image-bytes"))
	require.NoError(t, err)
	assert.True(t, wrote, "first write stores the file")

	path := store.OriginalPath(hash)
	assert.Equal(t, filepath.Join(base, "images", "aa", hash), path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("image-bytes"), data)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0644), info.Mode().Perm(), "files carry no execute bits")

	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), dirInfo.Mode().Perm())
}

func TestStoreWriteOriginalDedupes(t *testing.T) {
	base := t.TempDir()
	store := NewStore(filepath.Join(base, "images"), filepath.Join(base, "thumbs"))
	hash := testHash('b')

	wrote, err := store.WriteOriginal(hash, []byte("first"))
	require.NoError(t, err)
	assert.True(t, wrote)

	wrote, err = store.WriteOriginal(hash, []byte("second attempt"))
	require.NoError(t, err)
	assert.False(t, wrote, "existing hash skips the write")

	data, err := os.ReadFile(store.OriginalPath(hash))
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), data, "content-addressed file is immutable once written")
}

func TestStoreWriteOriginalShortHashRejected(t *testing.T) {
	store := NewStore(t.TempDir(), t.TempDir())

	_, err := store.WriteOriginal("a", []byte("x"))

	require.Error(t, err, "hash shorter than the fan-out prefix is rejected")
}

func TestStoreThumbPathLayout(t *testing.T) {
	base := t.TempDir()
	store := NewStore(filepath.Join(base, "images"), filepath.Join(base, "thumbs"))
	hash := testHash('c')

	assert.Equal(t,
		filepath.Join(base, "thumbs", "cc", hash+"-480.jpg"),
		store.ThumbPath(hash, 480),
		"thumb layout is <thumbs>/<hash[:2]>/<hash>-<size>.jpg")
}

// TestStoreFindThumbPathWidthFallback pins the serving-side resolution
// used by handleThumb: exact current-config width first, then any
// <hash>-*.jpg (closest width wins), so ready rows survive a width
// hot-reload without regeneration.
func TestStoreFindThumbPathWidthFallback(t *testing.T) {
	base := t.TempDir()
	store := NewStore(filepath.Join(base, "images"), filepath.Join(base, "thumbs"))
	hash := testHash('d')
	require.NoError(t, store.WriteThumb(hash, 480, []byte("small-derivative")))
	require.NoError(t, store.WriteThumb(hash, 1280, []byte("display-derivative")))

	t.Run("ExactWidthWins", func(t *testing.T) {
		p, ok := store.FindThumbPath(hash, 480)
		require.True(t, ok)
		assert.Equal(t, store.ThumbPath(hash, 480), p)
	})

	t.Run("ReloadedWidthFallsBackToClosest", func(t *testing.T) {
		// 360 (reloaded small_width) is closer to 480 than to 1280.
		p, ok := store.FindThumbPath(hash, 360)
		require.True(t, ok)
		assert.Equal(t, store.ThumbPath(hash, 480), p)

		// 1024 (reloaded display_width) is closer to 1280 than to 480.
		p, ok = store.FindThumbPath(hash, 1024)
		require.True(t, ok)
		assert.Equal(t, store.ThumbPath(hash, 1280), p)

		// Equidistant preference stays deterministic (sorted glob order:
		// the smaller width file).
		p, ok = store.FindThumbPath(hash, 880)
		require.True(t, ok)
		assert.Equal(t, store.ThumbPath(hash, 480), p)
	})

	t.Run("NoDerivativesIsMiss", func(t *testing.T) {
		_, ok := store.FindThumbPath(testHash('e'), 480)
		assert.False(t, ok, "hash with no derivative files does not resolve")
	})

	t.Run("ShortHashRejected", func(t *testing.T) {
		_, ok := store.FindThumbPath("a", 480)
		assert.False(t, ok)
	})
}
