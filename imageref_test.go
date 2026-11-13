package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	ref, err := registerImageRef(42, "view", "https://img.example.com/abc.png", imageRefVariant{MIME: "image/png", Width: 640, Height: 480, Data: []byte("pngbytes")}, imageRefVariant{})
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
	ref, err := registerImageRef(1, "user", "https://x/y.jpg", imageRefVariant{MIME: "image/jpeg", Width: 10, Height: 10, Data: []byte("j")}, imageRefVariant{})
	require.NoError(t, err)
	assert.Regexp(t, `^usr_[0-9a-f]{12}$`, ref)
}

func TestRegisterImageRefRefsAreUnique(t *testing.T) {
	setupImageRefTest(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ref, err := registerImageRef(1, "view", "u", imageRefVariant{MIME: "image/png", Width: 1, Height: 1, Data: []byte("b")}, imageRefVariant{})
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
	usrRef, err := registerImageRef(1, "user", "https://x/a.jpg", imageRefVariant{MIME: "image/jpeg", Width: 10, Height: 10, Data: []byte("USRB")}, imageRefVariant{})
	require.NoError(t, err)
	webRef, err := registerImageRef(1, "view", "https://x/b.png", imageRefVariant{MIME: "image/png", Width: 10, Height: 10, Data: []byte("WEBB")}, imageRefVariant{})
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
	oldRef, err := registerImageRef(1, "view", "u", imageRefVariant{MIME: "image/png", Width: 1, Height: 1, Data: []byte("old")}, imageRefVariant{})
	require.NoError(t, err)
	newRef, err := registerImageRef(1, "view", "u", imageRefVariant{MIME: "image/png", Width: 1, Height: 1, Data: []byte("new")}, imageRefVariant{})
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

func TestRegisterImageRefEditVariant(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(42, "view", "https://img.example.com/abc.png",
		imageRefVariant{MIME: "image/jpeg", Width: 100, Height: 80, Data: []byte("viewbytes")},
		imageRefVariant{MIME: "image/png", Width: 640, Height: 480, Data: []byte("editbytes")})
	require.NoError(t, err)

	row, err := resolveImageRef(ref)
	require.NoError(t, err)
	assert.Equal(t, "image/png", row.EditMIME)
	assert.Equal(t, 640, row.EditWidth)
	assert.Equal(t, 480, row.EditHeight)

	vd, err := os.ReadFile(row.FilePath)
	require.NoError(t, err)
	assert.Equal(t, []byte("viewbytes"), vd, "view file holds the view variant bytes")
	ed, err := os.ReadFile(row.EditFilePath)
	require.NoError(t, err)
	assert.Equal(t, []byte("editbytes"), ed, "edit file holds the edit variant bytes")
	assert.Equal(t, ".png", filepath.Ext(row.EditFilePath), "edit file ext follows the edit mime")
}

func TestRegisterImageRefWithoutEditVariant(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(42, "view", "https://img.example.com/abc.png",
		imageRefVariant{MIME: "image/jpeg", Width: 100, Height: 80, Data: []byte("viewbytes")},
		imageRefVariant{})
	require.NoError(t, err)

	row, err := resolveImageRef(ref)
	require.NoError(t, err)
	assert.Empty(t, row.EditFilePath, "no edit variant -> no edit file recorded")
	assert.NoFileExists(t, filepath.Join(config.ImagesDir, filepath.Base(row.FilePath)+".edit.jpg"))
}

func TestResolveImageRefArgsPrefersEditVariant(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "view", "https://x/b.png",
		imageRefVariant{MIME: "image/jpeg", Width: 100, Height: 80, Data: []byte("VIEWB")},
		imageRefVariant{MIME: "image/png", Width: 640, Height: 480, Data: []byte("EDITB")})
	require.NoError(t, err)

	args := map[string]any{"input_images": []string{ref}}
	require.NoError(t, resolveImageRefArgs(args))
	got := args["input_images"].([]string)
	assert.Equal(t, "data:image/png;base64,RURJVEI=", got[0],
		"the edit variant's bytes and mime ride the data URI, not the view variant's")
}

func TestResolveImageRefArgsFallsBackWhenEditFileMissing(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "view", "https://x/b.png",
		imageRefVariant{MIME: "image/jpeg", Width: 100, Height: 80, Data: []byte("VIEWB")},
		imageRefVariant{MIME: "image/png", Width: 640, Height: 480, Data: []byte("EDITB")})
	require.NoError(t, err)
	row, err := resolveImageRef(ref)
	require.NoError(t, err)
	require.NoError(t, os.Remove(row.EditFilePath))

	args := map[string]any{"input_images": []string{ref}}
	require.NoError(t, resolveImageRefArgs(args))
	got := args["input_images"].([]string)
	assert.Equal(t, "data:image/jpeg;base64,VklFV0I=", got[0],
		"a missing edit file degrades to the view variant instead of failing the edit")
}

func TestCleanupImageRefsByAgeRemovesEditFile(t *testing.T) {
	setupImageRefTest(t)
	oldRef, err := registerImageRef(1, "view", "u",
		imageRefVariant{MIME: "image/png", Width: 1, Height: 1, Data: []byte("old")},
		imageRefVariant{MIME: "image/png", Width: 2, Height: 2, Data: []byte("oldedit")})
	require.NoError(t, err)
	newRef, err := registerImageRef(1, "view", "u",
		imageRefVariant{MIME: "image/png", Width: 1, Height: 1, Data: []byte("new")},
		imageRefVariant{MIME: "image/png", Width: 2, Height: 2, Data: []byte("newedit")})
	require.NoError(t, err)

	var oldRow, newRow ImageRef
	require.NoError(t, theDB.Where("ref = ?", oldRef).First(&oldRow).Error)
	require.NoError(t, theDB.Where("ref = ?", newRef).First(&newRow).Error)

	cutoff := time.Now().Add(-48 * time.Hour)
	require.NoError(t, theDB.Model(&ImageRef{}).Where("ref = ?", oldRef).Update("created_at", cutoff).Error)

	n, err := cleanupImageRefsByAge(1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	assert.NoFileExists(t, oldRow.FilePath, "old view file removed")
	assert.NoFileExists(t, oldRow.EditFilePath, "old edit file removed")
	assert.FileExists(t, newRow.FilePath, "kept ref's view file untouched")
	assert.FileExists(t, newRow.EditFilePath, "kept ref's edit file untouched")
}

func TestSweepOrphanImageFilesKnowsEditPaths(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "view", "u",
		imageRefVariant{MIME: "image/png", Width: 1, Height: 1, Data: []byte("kept")},
		imageRefVariant{MIME: "image/jpeg", Width: 2, Height: 2, Data: []byte("keptedit")})
	require.NoError(t, err)
	var row ImageRef
	require.NoError(t, theDB.Where("ref = ?", ref).First(&row).Error)

	orphan := filepath.Join(config.ImagesDir, "web_orphanedfile.edit.png")
	require.NoError(t, os.WriteFile(orphan, []byte("x"), 0o644))
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(orphan, past, past))

	n, err := sweepOrphanImageFiles(config.ImagesDir, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NoFileExists(t, orphan)
	assert.FileExists(t, row.EditFilePath, "registered edit file untouched by the sweep")
}

func TestSanitizeToolArgsForLog(t *testing.T) {
	longB64 := strings.Repeat("QUJD", 500) // 2000 chars of plausible base64
	dataURI := "data:image/jpeg;base64," + longB64
	wantCollapsed := fmt.Sprintf("data:image/jpeg;base64,[… %d chars]", len(longB64))

	t.Run("collapses data URIs in []any input_images", func(t *testing.T) {
		args := map[string]any{"input_images": []any{dataURI, "gen_abc_0"}}
		san := sanitizeToolArgsForLog(args)
		entries, ok := san["input_images"].([]any)
		require.True(t, ok, "[]any shape preserved")
		assert.Equal(t, wantCollapsed, entries[0], "data URI collapsed to prefix + length")
		assert.Equal(t, "gen_abc_0", entries[1], "gen ref unchanged")
	})

	t.Run("collapses data URIs in []string input_images", func(t *testing.T) {
		args := map[string]any{"input_images": []string{dataURI, "gen_abc_0"}}
		san := sanitizeToolArgsForLog(args)
		entries, ok := san["input_images"].([]string)
		require.True(t, ok, "[]string shape preserved")
		assert.Equal(t, wantCollapsed, entries[0])
		assert.Equal(t, "gen_abc_0", entries[1])
	})

	t.Run("recurses into nested maps and slices", func(t *testing.T) {
		args := map[string]any{
			"opts": map[string]any{"img": dataURI, "refs": []any{[]string{dataURI}}},
		}
		san := sanitizeToolArgsForLog(args)
		inner, ok := san["opts"].(map[string]any)
		require.True(t, ok, "nested map walked")
		assert.Equal(t, wantCollapsed, inner["img"])
		nested, ok := inner["refs"].([]any)
		require.True(t, ok)
		innerSlice, ok := nested[0].([]string)
		require.True(t, ok, "slice inside slice walked")
		assert.Equal(t, wantCollapsed, innerSlice[0])
	})

	t.Run("ordinary strings pass through byte-unchanged", func(t *testing.T) {
		prompt := "draw a cat, then a dog, then, finally, a bird"
		args := map[string]any{
			"prompt":   prompt,
			"mention":  "the payload data:image/png;base64,AAAA here is mid-string, not a URI",
			"dataless": "data:image/png",
			"semi":     "foo;base64,bar",
		}
		san := sanitizeToolArgsForLog(args)
		assert.Equal(t, prompt, san["prompt"], "prompt text must stay readable")
		assert.Equal(t, args["mention"], san["mention"], "no data: prefix -> verbatim")
		assert.Equal(t, args["dataless"], san["dataless"], "data: prefix but no ;base64, -> verbatim")
		assert.Equal(t, args["semi"], san["semi"], ";base64, but no data: prefix -> verbatim")
	})

	t.Run("original map is not mutated", func(t *testing.T) {
		args := map[string]any{"input_images": []any{dataURI}}
		_ = sanitizeToolArgsForLog(args)
		entries := args["input_images"].([]any)
		require.Len(t, entries, 1)
		assert.Equal(t, dataURI, entries[0], "original still holds the full URI — the MCP call needs it")
	})

	t.Run("non-string scalars pass through", func(t *testing.T) {
		raw := []byte("bytes")
		args := map[string]any{"n": 42, "f": 1.5, "b": true, "z": nil, "raw": raw}
		san := sanitizeToolArgsForLog(args)
		assert.Equal(t, 42, san["n"])
		assert.Equal(t, 1.5, san["f"])
		assert.Equal(t, true, san["b"])
		assert.Nil(t, san["z"])
		assert.Equal(t, raw, san["raw"])
	})

	t.Run("nil args stay nil", func(t *testing.T) {
		assert.Nil(t, sanitizeToolArgsForLog(nil))
	})
}

func TestSweepOrphanImageFiles(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "view", "u", imageRefVariant{MIME: "image/png", Width: 1, Height: 1, Data: []byte("kept")}, imageRefVariant{})
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
