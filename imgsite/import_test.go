package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
	// Embedded tzdata keeps the zone-conversion tests hermetic regardless
	// of the host's zoneinfo installation.
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noNoteGraphJSON builds a legacy API-format graph WITHOUT the
// dave_original_prompt node — the shape of the oldest archive files,
// where the embedded workflow is the only metadata there is.
func noNoteGraphJSON(positive string) string {
	return noNoteGraphJSONOpts(positive, true)
}

// noNoteGraphJSONOpts optionally omits the EmptyLatentImage node,
// producing a graph whose extraction leaves width/height NULL (coverage
// for the dims backfill in the thumbnail pass).
func noNoteGraphJSONOpts(positive string, withLatent bool) string {
	g := map[string]workflowNode{
		"55": {ClassType: "UNETLoader", Inputs: map[string]any{"unet_name": "krea2_turbo_int8_convrot.safetensors"}},
		"60": {ClassType: "CLIPLoader", Inputs: map[string]any{"clip_name": "qwen3vl_4b_fp8_scaled.safetensors"}},
		"65": {ClassType: "VAELoader", Inputs: map[string]any{"vae_name": "qwen_image_vae.safetensors"}},
		"10": {ClassType: "CLIPTextEncode", Inputs: map[string]any{"text": positive}},
		"11": {ClassType: "ConditioningZeroOut", Inputs: map[string]any{"conditioning": []any{"10", 0}}},
		"3": {ClassType: "KSampler", Inputs: map[string]any{
			"seed":         json.Number("9034043319117902157"),
			"steps":        json.Number("8"),
			"cfg":          json.Number("1.0"),
			"denoise":      json.Number("1.0"),
			"sampler_name": "euler",
			"scheduler":    "simple",
			"positive":     []any{"10", 0},
			"negative":     []any{"11", 0},
			"model":        []any{"55", 0},
		}},
	}
	if withLatent {
		g["70"] = workflowNode{ClassType: "EmptyLatentImage", Inputs: map[string]any{
			"width": json.Number("64"), "height": json.Number("32"),
		}}
		g["3"].Inputs["latent_image"] = []any{"70", 0}
	}
	b, err := json.Marshal(g)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// realWebPWithPayload rebuilds the plain.webp fixture around a different
// EXIF workflow payload, keeping the genuine VP8 image data — a legacy
// file that actually decodes (1920x1080), for thumbnail/dims coverage.
func realWebPWithPayload(t *testing.T, payload string) []byte {
	t.Helper()
	src := loadFixture(t, "plain.webp")
	require.GreaterOrEqual(t, len(src), 12, "fixture must be a RIFF/webp")
	require.Equal(t, "RIFF", string(src[0:4]))
	require.Equal(t, "WEBP", string(src[8:12]))

	out := append([]byte(nil), src[:12]...)
	off := 12
	for off+8 <= len(src) {
		fourcc := string(src[off : off+4])
		size := int(binary.LittleEndian.Uint32(src[off+4 : off+8]))
		require.LessOrEqual(t, off+8+size, len(src), "fixture chunk overruns file")
		// Advance by the ORIGINAL chunk extent — a replaced EXIF body
		// has a different length and must not move the walk cursor.
		next := off + 8 + size + (size & 1)
		body := src[off+8 : off+8+size]
		if fourcc == "EXIF" {
			body = buildTIFF("prompt:"+payload, false)
		}
		out = append(out, fourcc...)
		var sz [4]byte
		binary.LittleEndian.PutUint32(sz[:], uint32(len(body)))
		out = append(out, sz[:]...)
		out = append(out, body...)
		if len(body)%2 == 1 {
			out = append(out, 0) // odd-size chunk pad per the RIFF spec
		}
		off = next
	}
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

// writeLegacyWebP writes a synthetic webp carrying a no-note workflow
// with the given positive prompt. Not a decodable image (synthetic VP8X
// body) — fine for the import pipeline, which never decodes on the
// insert path.
func writeLegacyWebP(t *testing.T, path, positive string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	data := buildWebP(buildTIFF("prompt:"+noNoteGraphJSON(positive), false), true)
	require.NoError(t, os.WriteFile(path, data, 0644))
	return path
}

// ─── Filename timestamp parsing ───────────────────────────────────────────

func TestParseImportFilenameTime(t *testing.T) {
	want := time.Date(2026, 9, 24, 18, 53, 55, 0, time.UTC)
	tests := []struct {
		name string
		in   string
		want time.Time
		ok   bool
	}{
		{"BatchSuffix", "2026-09-24-185355__0.webp", want, true},
		{"ExactNoSuffix", "2026-09-24-185355", want, true},
		{"ArbitrarySuffixIgnored", "2026-09-24-185355_00001_.png", want, true},
		{"UnderscoreSeparatorRejected", "2026-09-24_185355.webp", time.Time{}, false},
		{"TooShort", "2026-09-24-1853.webp", time.Time{}, false},
		{"NotATimestamp", "ComfyUI_00123.webp", time.Time{}, false},
		{"InvalidMonth", "2026-13-24-185355.webp", time.Time{}, false},
		{"InvalidDay", "2026-09-32-185355.webp", time.Time{}, false},
		{"InvalidHour", "2026-09-24-255355.webp", time.Time{}, false},
		{"Empty", "", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseImportFilenameTime(tt.in, time.UTC)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.True(t, got.Equal(tt.want), "got %v want %v", got, tt.want)
			}
		})
	}
}

// TestParseImportFilenameTimezone pins the local→UTC conversion for the
// stored created_at string, across a DST boundary of the same zone.
func TestParseImportFilenameTimezone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	t.Run("SummerEDT", func(t *testing.T) {
		got, ok := parseImportFilenameTime("2026-09-24-185355__0.webp", ny)
		require.True(t, ok)
		assert.Equal(t, "2026-09-24 22:53:55.000", got.UTC().Format(dbTimeFormat),
			"EDT is UTC-4: 18:53:55 local -> 22:53:55 UTC")
	})
	t.Run("WinterEST", func(t *testing.T) {
		got, ok := parseImportFilenameTime("2026-01-15-093000__0.webp", ny)
		require.True(t, ok)
		assert.Equal(t, "2026-01-15 14:30:00.000", got.UTC().Format(dbTimeFormat),
			"EST is UTC-5: 09:30:00 local -> 14:30:00 UTC")
	})
	t.Run("UTCIdentity", func(t *testing.T) {
		got, ok := parseImportFilenameTime("2026-09-24-185355__0.webp", time.UTC)
		require.True(t, ok)
		assert.Equal(t, "2026-09-24 18:53:55.000", got.UTC().Format(dbTimeFormat))
	})
}

// ─── Batch suffix parsing ─────────────────────────────────────────────────

func TestParseImportBatchMillis(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	tests := []struct {
		name string
		in   string
		want time.Duration
		ok   bool
	}{
		{"Zero", "2026-09-24-185355__0.webp", 0, true},
		{"One", "2026-09-24-185355__1.webp", ms(1), true},
		{"Ten", "2026-09-24-185355__10.webp", ms(10), true},
		{"Max", "2026-09-24-185355__999.webp", ms(999), true},
		{"ClampedAt999", "2026-09-24-185355__1000.webp", ms(999), true},
		{"ClampedHuge", "2026-09-24-185355__999999999.webp", ms(999), true},
		{"DigitsThenJunkCount", "2026-09-24-185355__7abc.webp", ms(7), true},
		{"NoSuffix", "2026-09-24-185355.webp", 0, false},
		{"SingleUnderscore", "2026-09-24-185355_0.webp", 0, false},
		{"DashSuffix", "2026-09-24-185355-2.png", 0, false},
		{"EmptySuffix", "2026-09-24-185355__.webp", 0, false},
		{"ExactLengthNoSuffix", "2026-09-24-185355", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseImportBatchMillis(tt.in)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestParseImportTZ(t *testing.T) {
	t.Run("EmptyMeansLocal", func(t *testing.T) {
		loc, err := parseImportTZ("")
		require.NoError(t, err)
		assert.Same(t, time.Local, loc)
	})
	t.Run("IANAName", func(t *testing.T) {
		loc, err := parseImportTZ("America/New_York")
		require.NoError(t, err)
		assert.Equal(t, "America/New_York", loc.String())
	})
	t.Run("UTC", func(t *testing.T) {
		loc, err := parseImportTZ("UTC")
		require.NoError(t, err)
		assert.Same(t, time.UTC, loc)
	})
	t.Run("UnknownZoneFails", func(t *testing.T) {
		_, err := parseImportTZ("Not/AZone")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Not/AZone")
	})
}

// ─── Import flow ──────────────────────────────────────────────────────────

// rowByFilename fetches the imported row for a source filename.
func rowByFilename(t *testing.T, app *App, filename string) dbImage {
	t.Helper()
	var img dbImage
	require.NoError(t, app.db.Get(&img, `SELECT * FROM images WHERE filename = ?`, filename),
		"row for %s", filename)
	return img
}

func TestRunImportFlow(t *testing.T) {
	app := newTestApp(t, testConfig())
	cfg := testConfig()
	root := t.TempDir()

	// Legacy no-note webp inside a date subfolder (recursion).
	writeLegacyWebP(t, filepath.Join(root, "2024-01", "2024-01-15-093000__0.webp"),
		"legacy subfolder prompt about llamas")
	// Legacy no-note at top level, batch index 1 -> +1ms.
	writeLegacyWebP(t, filepath.Join(root, "2024-01-15-093001__1.webp"),
		"legacy top level prompt about otters")
	// Real production-style fixture (dave note present): full-fidelity
	// import; created_at still from the filename, __7 -> +7ms.
	plainData := loadFixture(t, "plain.webp")
	require.NoError(t, os.WriteFile(filepath.Join(root, "2024-01-15-093002__7.webp"), plainData, 0644))
	// Real DECODABLE webp with a no-note, no-latent-dims graph: dims
	// insert NULL and the inline thumbnail pass backfills them from the
	// decoded bounds (1920x1080).
	realLegacy := realWebPWithPayload(t, noNoteGraphJSONOpts("legacy real pixels about newts", false))
	require.NoError(t, os.WriteFile(filepath.Join(root, "2024-01-15-093003__0.webp"), realLegacy, 0644))
	// Mixed-case extensions import: the gate lowercases the extension
	// and extraction sniffs container content, not the name.
	writeLegacyWebP(t, filepath.Join(root, "2024-01-15-093004__0.WebP"),
		"mixed case webp prompt about foxes")
	writeLegacyWebP(t, filepath.Join(root, "2024-01-15-093005__0.PNG"),
		"mixed case png prompt about wolves")
	// webp container without an EXIF chunk -> skipped-no-workflow.
	require.NoError(t, os.WriteFile(filepath.Join(root, "nowf_001.webp"), buildWebP(nil, false), 0644))
	// Non-image -> skipped-other.
	require.NoError(t, os.WriteFile(filepath.Join(root, "readme.txt"), []byte("notes"), 0644))
	// No-note webp with an unparseable name -> mtime fallback + warning.
	mtimePath := writeLegacyWebP(t, filepath.Join(root, "manual_export.webp"),
		"manually exported prompt about herons")
	mtime := time.Date(2020, 5, 1, 10, 11, 12, 0, time.UTC)
	require.NoError(t, os.Chtimes(mtimePath, mtime, mtime))

	var report bytes.Buffer
	sum, err := runImport(cfg, app.db, app.store, root, time.UTC, &report)
	require.NoError(t, err)

	assert.Equal(t, 7, sum.Imported)
	assert.Equal(t, 0, sum.SkippedDuplicate)
	assert.Equal(t, 1, sum.SkippedNoWorkflow)
	assert.Equal(t, 1, sum.SkippedOther)
	assert.Equal(t, 0, sum.Errors)
	assert.Equal(t, 2, sum.ThumbsReady, "the two real webps (noted fixture + legacy rebuild) decode")
	assert.Equal(t, 5, sum.ThumbsFailed, "the five synthetic-container fixtures do not decode")
	require.Len(t, sum.Warnings, 1)
	assert.Contains(t, sum.Warnings[0], "manual_export.webp")
	assert.Contains(t, report.String(), "skip (no embedded workflow): nowf_001.webp")
	assert.Contains(t, report.String(), "skip (not webp/png): readme.txt")

	t.Run("LegacyRowShape", func(t *testing.T) {
		sub := rowByFilename(t, app, "2024-01-15-093000__0.webp")
		assert.Equal(t, "2024-01-15 09:30:00.000", sub.CreatedAt, "filename time, UTC, ms precision")
		assert.Empty(t, sub.OriginalPrompt, "no dave note -> empty original prompt")
		assert.Equal(t, "legacy subfolder prompt about llamas", sub.EnhancedPrompt,
			"final positive prompt lands in enhanced_prompt (tier-2 searchable)")
		assert.Nil(t, sub.JobID)
		assert.False(t, sub.LLMGenerated)
		assert.Nil(t, sub.Network)
		assert.Nil(t, sub.Channel)
		assert.Nil(t, sub.Nick)
		assert.Equal(t, metaSourceEXIF, sub.MetaSource)
		assert.NotEmpty(t, sub.WorkflowJSON)
		require.NotNil(t, sub.Seed)
		assert.EqualValues(t, 9034043319117902157, *sub.Seed, "graph params extract like any upload")
		require.NotNil(t, sub.Width)
		assert.Equal(t, 64, *sub.Width, "graph latent dims land at insert, upload-parity")
		require.NotNil(t, sub.Height)
		assert.Equal(t, 32, *sub.Height)
		assert.Equal(t, thumbStatusFailed, sub.ThumbStatus,
			"synthetic fixture is not decodable: inline thumbnail pass marks failed (worker semantics), row survives")
		// Non-destructive: the source file is still there and a copy
		// lives in the store.
		assert.FileExists(t, filepath.Join(root, "2024-01", "2024-01-15-093000__0.webp"))
		assert.True(t, fileExists(app.store.OriginalPath(sub.SHA256)), "original copied into the store")
	})

	t.Run("BatchOffsetApplied", func(t *testing.T) {
		batch := rowByFilename(t, app, "2024-01-15-093001__1.webp")
		assert.Equal(t, "2024-01-15 09:30:01.001", batch.CreatedAt, "__1 suffix adds 1ms")
	})

	t.Run("NotedFileFullFidelityWithFilenameTime", func(t *testing.T) {
		noted := rowByFilename(t, app, "2024-01-15-093002__7.webp")
		assert.Equal(t, "2024-01-15 09:30:02.007", noted.CreatedAt,
			"filename time wins over the note (which carries no timestamp); __7 adds 7ms")
		assert.Equal(t, "shrew comin in hot", noted.OriginalPrompt)
		assert.Equal(t, fixtureEnhancedPlain, noted.EnhancedPrompt)
		assert.Equal(t, "4eef6f8b", ptrValue(noted.JobID))
		assert.Equal(t, thumbStatusReady, noted.ThumbStatus, "real webp: inline thumbnail pass succeeds")
		require.NotNil(t, noted.Width)
		assert.Equal(t, 1920, *noted.Width)
		require.NotNil(t, noted.Height)
		assert.Equal(t, 1080, *noted.Height)
		assert.True(t, fileExists(app.store.ThumbPath(noted.SHA256, cfg.Thumbnails.SmallWidth)),
			"small derivative written")
	})

	t.Run("RealLegacyNoLatentDimsBackfilled", func(t *testing.T) {
		real := rowByFilename(t, app, "2024-01-15-093003__0.webp")
		assert.Equal(t, "2024-01-15 09:30:03.000", real.CreatedAt)
		assert.Empty(t, real.OriginalPrompt)
		assert.Equal(t, "legacy real pixels about newts", real.EnhancedPrompt)
		assert.Equal(t, thumbStatusReady, real.ThumbStatus, "decodable real webp: thumbs generated inline")
		require.NotNil(t, real.Width, "NULL dims backfilled from the decoded bounds")
		assert.Equal(t, 1920, *real.Width)
		require.NotNil(t, real.Height)
		assert.Equal(t, 1080, *real.Height)
	})

	t.Run("MixedCaseExtensionsImport", func(t *testing.T) {
		w := rowByFilename(t, app, "2024-01-15-093004__0.WebP")
		assert.Equal(t, "2024-01-15 09:30:04.000", w.CreatedAt)
		assert.Equal(t, "mixed case webp prompt about foxes", w.EnhancedPrompt)
		p := rowByFilename(t, app, "2024-01-15-093005__0.PNG")
		assert.Equal(t, "2024-01-15 09:30:05.000", p.CreatedAt)
		assert.Equal(t, "mixed case png prompt about wolves", p.EnhancedPrompt)
	})

	t.Run("MtimeFallback", func(t *testing.T) {
		mt := rowByFilename(t, app, "manual_export.webp")
		assert.Equal(t, "2020-05-01 10:11:12.000", mt.CreatedAt, "fallback to file mtime in UTC")
		assert.Equal(t, "manually exported prompt about herons", mt.EnhancedPrompt)
	})

	t.Run("IdempotentRerun", func(t *testing.T) {
		var report2 bytes.Buffer
		sum2, err := runImport(cfg, app.db, app.store, root, time.UTC, &report2)
		require.NoError(t, err)

		assert.Equal(t, 0, sum2.Imported)
		assert.Equal(t, 7, sum2.SkippedDuplicate, "every previously imported hash is a duplicate")
		assert.Equal(t, 1, sum2.SkippedNoWorkflow, "workflow-less file is re-examined and re-skipped")
		assert.Equal(t, 1, sum2.SkippedOther)
		assert.Equal(t, 0, sum2.Errors)
		assert.Empty(t, sum2.Warnings, "mtime fallback is not re-triggered: nothing was imported")

		var n int
		require.NoError(t, app.db.Get(&n, `SELECT COUNT(*) FROM images`))
		assert.Equal(t, 7, n, "no new rows on re-run")
	})
}

// TestImportDedupAgainstUploadedRow pins that bytes already uploaded
// through dave (not just a previous import run) are skipped.
func TestImportDedupAgainstUploadedRow(t *testing.T) {
	app := newTestApp(t, testConfig())
	data := buildWebP(buildTIFF("prompt:"+noNoteGraphJSON("dupe check prompt"), false), true)
	insertImageWithFile(t, app, "dup0001", data) // the "uploaded via dave" row

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2024-02-02-020202__0.webp"), data, 0644))

	sum, err := runImport(testConfig(), app.db, app.store, dir, time.UTC, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 0, sum.Imported)
	assert.Equal(t, 1, sum.SkippedDuplicate)

	var n int
	require.NoError(t, app.db.Get(&n, `SELECT COUNT(*) FROM images`))
	assert.Equal(t, 1, n, "no second row for known bytes")
}

// TestRunImportFatalErrors: per-file problems never abort, but a bad
// directory is fatal.
func TestRunImportFatalErrors(t *testing.T) {
	app := newTestApp(t, testConfig())

	_, err := runImport(testConfig(), app.db, app.store, filepath.Join(t.TempDir(), "nope"), time.UTC, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "import directory")

	aFile := filepath.Join(t.TempDir(), "file.webp")
	require.NoError(t, os.WriteFile(aFile, []byte("x"), 0644))
	_, err = runImport(testConfig(), app.db, app.store, aFile, time.UTC, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

// TestImportPNGContainer exercises the import flow's .png arm end to
// end with a PNG-container fixture (tEXt "prompt" chunk — ComfyUI's
// standard SaveImage convention; same machinery as extract_test.go's
// TestExtractPNGText): the row lands with the enhanced prompt populated
// and the filename-derived created_at, exactly like the webp arm.
func TestImportPNGContainer(t *testing.T) {
	app := newTestApp(t, testConfig())
	dir := t.TempDir()
	png := buildPNG("prompt", noNoteGraphJSON("png container prompt about badgers"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "2023-12-31-235959__4.png"), png, 0644))

	sum, err := runImport(testConfig(), app.db, app.store, dir, time.UTC, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Imported)

	row := rowByFilename(t, app, "2023-12-31-235959__4.png")
	assert.Equal(t, "2023-12-31 23:59:59.004", row.CreatedAt, "filename time with the __4 batch offset")
	assert.Empty(t, row.OriginalPrompt, "no dave note in the tEXt-carried graph")
	assert.Equal(t, "png container prompt about badgers", row.EnhancedPrompt)
	assert.Equal(t, metaSourceEXIF, row.MetaSource)
	assert.NotEmpty(t, row.WorkflowJSON)
}

// ─── FTS: imported legacy images are searchable ────────────────────────────

// TestImportedLegacyImageSearchableTier2 pins the owner's tiering
// decision: a legacy import has NO original_prompt, so its enhanced
// (final positive) prompt makes it findable exactly like any other
// enhanced-only match — tier 2, strictly below an original-prompt hit.
func TestImportedLegacyImageSearchableTier2(t *testing.T) {
	app := newTestApp(t, testConfig())
	dir := t.TempDir()
	writeLegacyWebP(t, filepath.Join(dir, "2025-06-01-120000__0.webp"),
		"the shrew dances at midnight")

	sum, err := runImport(testConfig(), app.db, app.store, dir, time.UTC, io.Discard)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Imported)

	imported := rowByFilename(t, app, "2025-06-01-120000__0.webp")

	// An original-prompt match must outrank the imported row.
	insertSearchRow(t, app, "orig0001", "2026-09-24 01:00:00", "shrew comin in hot", "")

	res := runSearchFor(t, app, "shrew", searchCursor{}, 48)
	require.Len(t, res.Hits, 2)
	assert.Equal(t, "orig0001", res.Hits[0].img.ID)
	assert.Equal(t, 1, res.Hits[0].tier)
	assert.Equal(t, imported.ID, res.Hits[1].img.ID, "imported row found via its enhanced prompt")
	assert.Equal(t, 2, res.Hits[1].tier)
}

// ─── Summary rendering ────────────────────────────────────────────────────

func TestWriteImportSummary(t *testing.T) {
	var b bytes.Buffer
	writeImportSummary(&b, importSummary{
		Imported:          3,
		SkippedDuplicate:  2,
		SkippedNoWorkflow: 1,
		SkippedOther:      1,
		Warnings:          []string{"somefile.webp: filename fallback"},
	}, "/data/imgsite.db", "/data/images")

	s := b.String()
	assert.Contains(t, s, "import summary")
	assert.Contains(t, s, "3")
	assert.Contains(t, s, "somefile.webp: filename fallback")
	assert.Contains(t, s, "/data/imgsite.db", "DB path reported")
	assert.Contains(t, s, "/data/images", "store path reported")
}
