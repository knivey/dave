package main

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrationCreatesFullSchema(t *testing.T) {
	db := setupTestDB(t)

	tables := []string{"images", "images_fts"}
	for _, name := range tables {
		var got string
		err := db.Get(&got, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name)
		require.NoError(t, err, "table %s must exist", name)
		assert.Equal(t, name, got)
	}

	triggers := []string{"images_fts_ai", "images_fts_ad", "images_fts_au"}
	for _, name := range triggers {
		var got string
		err := db.Get(&got, `SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name)
		require.NoError(t, err, "trigger %s must exist", name)
	}

	indexes := []string{"idx_images_created", "idx_images_sha"}
	for _, name := range indexes {
		var got string
		err := db.Get(&got, `SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name)
		require.NoError(t, err, "index %s must exist", name)
	}
}

func TestInsertFetchRoundtrip(t *testing.T) {
	db := setupTestDB(t)
	seed := int64(5702895218061442231)
	steps := 8
	cfgv := 1.0
	denoise := 1.0
	loras := `[{"name":"flux.safetensors","strength":0.8}]`

	in := &dbImage{
		ID:             "abc1234",
		SHA256:         strings.Repeat("ab", 32),
		Filename:       "2026-09-23-234552__0.webp",
		MimeType:       "image/webp",
		SizeBytes:      12345,
		CreatedAt:      "2026-09-24 03:12:00",
		ThumbStatus:    thumbStatusPending,
		OriginalPrompt: "shrew walkin down main street",
		EnhancedPrompt: "A cute anthropomorphic brown shrew",
		NegativePrompt: "",
		Reasoning:      "The user described a shrew.",
		JobID:          nullStr("ed974b6d"),
		LLMGenerated:   true,
		Network:        nullStr("libera"),
		Channel:        nullStr("#dave"),
		Nick:           nullStr("knivey"),
		WorkflowName:   nullStr("zimage-turbo"),
		Seed:           &seed,
		Steps:          &steps,
		Cfg:            &cfgv,
		Denoise:        &denoise,
		Loras:          &loras,
		WorkflowJSON:   `{"3":{"class_type":"KSampler"}}`,
		MetaSource:     metaSourceUpload,
	}
	require.NoError(t, dbInsertImage(db, in))

	got, err := dbGetImageByID(db, "abc1234")
	require.NoError(t, err)
	assert.Equal(t, in.ID, got.ID)
	assert.Equal(t, in.SHA256, got.SHA256)
	assert.Equal(t, in.Filename, got.Filename)
	assert.Equal(t, in.MimeType, got.MimeType)
	assert.Equal(t, in.SizeBytes, got.SizeBytes)
	assert.Equal(t, in.CreatedAt, got.CreatedAt)
	assert.Equal(t, in.ThumbStatus, got.ThumbStatus)
	assert.Equal(t, in.OriginalPrompt, got.OriginalPrompt)
	assert.Equal(t, in.EnhancedPrompt, got.EnhancedPrompt)
	assert.Equal(t, in.Reasoning, got.Reasoning)
	assert.Equal(t, "ed974b6d", ptrValue(got.JobID))
	assert.True(t, got.LLMGenerated)
	assert.Equal(t, "libera", ptrValue(got.Network))
	assert.Equal(t, "#dave", ptrValue(got.Channel))
	assert.Equal(t, "knivey", ptrValue(got.Nick))
	assert.Equal(t, "zimage-turbo", ptrValue(got.WorkflowName))
	require.NotNil(t, got.Seed)
	assert.EqualValues(t, seed, *got.Seed)
	require.NotNil(t, got.Steps)
	assert.Equal(t, steps, *got.Steps)
	require.NotNil(t, got.Cfg)
	assert.InDelta(t, cfgv, *got.Cfg, 0.0001)
	assert.Nil(t, got.Width, "width stays NULL in milestone 1")
	assert.Nil(t, got.Height)
	assert.False(t, got.Hidden)
	assert.Equal(t, in.WorkflowJSON, got.WorkflowJSON)
	assert.Equal(t, in.MetaSource, got.MetaSource)
}

func TestGetImageByIDUnknown(t *testing.T) {
	db := setupTestDB(t)

	_, err := dbGetImageByID(db, "zzzzzzz")

	require.Error(t, err)
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestImageIDExists(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{
		ID: "abc1234", SHA256: strings.Repeat("cd", 32), Filename: "x.png",
		MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-24 00:00:00",
		ThumbStatus: thumbStatusPending, MetaSource: metaSourceUpload,
	}
	require.NoError(t, dbInsertImage(db, in))

	exists, err := dbImageIDExists(db, "abc1234")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = dbImageIDExists(db, "zzzzzzz")
	require.NoError(t, err)
	assert.False(t, exists)
}

// TestShaNotUnique pins the dedupe design: N rows may share one sha256.
func TestShaNotUnique(t *testing.T) {
	db := setupTestDB(t)
	base := dbImage{
		SHA256: strings.Repeat("ef", 32), Filename: "x.png",
		MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-24 00:00:00",
		ThumbStatus: thumbStatusPending, MetaSource: metaSourceUpload,
	}
	for _, id := range []string{"aaa0001", "bbb0002"} {
		row := base
		row.ID = id
		require.NoError(t, dbInsertImage(db, &row))
	}

	var n int
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images WHERE sha256 = ?`, base.SHA256))
	assert.Equal(t, 2, n)
}

// TestFTSTriggersKeepIndexInSync exercises the external-content 'delete'
// dance: insert feeds the index, update swaps tokens, delete removes them.
func TestFTSTriggersKeepIndexInSync(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{
		ID: "aaa0001", SHA256: strings.Repeat("01", 32), Filename: "x.png",
		MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-24 00:00:00",
		ThumbStatus: thumbStatusPending,
		// "shrews" (plural) on purpose: porter stemming must match "shrew".
		OriginalPrompt: "shrews walkin down main street",
		EnhancedPrompt: "A cute anthropomorphic brown shrew",
		MetaSource:     metaSourceUpload,
	}
	require.NoError(t, dbInsertImage(db, in))

	var n int
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH 'shrew'`))
	assert.Equal(t, 1, n, "insert trigger feeds the index (porter stems shrews->shrew)")

	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH 'brown'`))
	assert.Equal(t, 1, n, "enhanced prompt indexed in its own column")

	_, err := db.Exec(`UPDATE images SET original_prompt = 'totally different words' WHERE id = ?`, in.ID)
	require.NoError(t, err)
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH 'shrews'`))
	assert.Equal(t, 1, n, "enhanced column still matches after unrelated-column-safe update")
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH 'different'`))
	assert.Equal(t, 1, n, "update trigger swaps old tokens for new")
	// The original-prompt-only term 'walkin' must be gone after the update
	// rewrote that column.
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH 'walkin'`))
	assert.Equal(t, 0, n)

	_, err = db.Exec(`DELETE FROM images WHERE id = ?`, in.ID)
	require.NoError(t, err)
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH 'different'`))
	assert.Equal(t, 0, n, "delete trigger runs the FTS 'delete' dance")
}

// TestSubstringFTSMigration pins migration 002 (trigram side table for
// tier-3 acceleration): the table and its trigger trio exist after the
// full chain runs, the one-time 'rebuild' indexes rows that predate the
// migration, and the triggers keep the table in sync for inserts,
// prompt updates, and deletes.
func TestSubstringFTSMigration(t *testing.T) {
	db := setupTestDB(t)

	var got string
	require.NoError(t, db.Get(&got,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'images_substring_fts'`))
	for _, name := range []string{"images_substring_fts_ai", "images_substring_fts_ad", "images_substring_fts_au"} {
		require.NoError(t, db.Get(&got,
			`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name),
			"trigger %s must exist", name)
	}

	// Rebuild path: roll back to v1 (before the side table), insert a
	// row through 001's triggers only, re-run 002, and assert the
	// 'rebuild' command picked the pre-existing row up. The insert is
	// raw SQL on purpose: a row that predates migration 002 was written
	// by a binary that also predated 003's safety column (and any later
	// column), so dbInsertImage — which always targets the CURRENT
	// schema — must not be used here.
	require.NoError(t, goose.DownTo(db.DB, "migrations", 1))
	_, err := db.Exec(`INSERT INTO images
		(id, sha256, filename, mime_type, size_bytes, created_at, thumb_status, original_prompt, meta_source)
		VALUES ('sub0001', ?, 'x.png', 'image/png', 1, '2026-09-24 00:00:00', 'pending', 'a cowshrew grazes', 'upload')`,
		strings.Repeat("05", 32))
	require.NoError(t, err)
	require.NoError(t, goose.Up(db.DB, "migrations"))

	var n int
	require.NoError(t, db.Get(&n,
		`SELECT COUNT(*) FROM images_substring_fts WHERE images_substring_fts MATCH '"cowshrew"'`))
	assert.Equal(t, 1, n, "one-time rebuild indexed the pre-migration row")

	// Live sync: rows written after the migration ride the triggers.
	require.NoError(t, dbInsertImage(db, &dbImage{
		ID: "sub0002", SHA256: strings.Repeat("06", 32), Filename: "x.png",
		MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-24 01:00:00",
		ThumbStatus: thumbStatusPending, OriginalPrompt: "plain words",
		MetaSource: metaSourceUpload,
	}))
	require.NoError(t, db.Get(&n,
		`SELECT COUNT(*) FROM images_substring_fts WHERE images_substring_fts MATCH '"plain"'`))
	assert.Equal(t, 1, n, "insert trigger feeds the trigram table")

	_, err = db.Exec(`UPDATE images SET enhanced_prompt = 'totally cowshrew here' WHERE id = ?`, "sub0002")
	require.NoError(t, err)
	require.NoError(t, db.Get(&n,
		`SELECT COUNT(*) FROM images_substring_fts WHERE images_substring_fts MATCH '"cowshrew"'`))
	assert.Equal(t, 2, n, "update trigger swaps tokens in the trigram table")

	_, err = db.Exec(`DELETE FROM images WHERE id = ?`, "sub0002")
	require.NoError(t, err)
	require.NoError(t, db.Get(&n,
		`SELECT COUNT(*) FROM images_substring_fts WHERE images_substring_fts MATCH '"plain"'`))
	assert.Equal(t, 0, n, "delete trigger runs the trigram 'delete' dance")
}

// TestKeysetQueriesUseIndexSeek pins the query plan of the row-value
// keyset predicates: they must range-seek idx_images_created, never fall
// back to a temp b-tree (which the old OR-shaped predicates planned as).
func TestKeysetQueriesUseIndexSeek(t *testing.T) {
	db := setupTestDB(t)
	queries := []string{
		`SELECT * FROM images WHERE hidden = 0 AND (created_at, id) > ('2026-01-01 00:00:00', 'aaaaaaa') ORDER BY created_at ASC, id ASC LIMIT 1`,
		`SELECT * FROM images WHERE hidden = 0 AND (created_at, id) < ('2026-01-01 00:00:00', 'aaaaaaa') ORDER BY created_at DESC, id DESC LIMIT 1`,
	}
	for _, q := range queries {
		rows, err := db.Query("EXPLAIN QUERY PLAN " + q)
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, notused, detail string
			require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		require.NotEmpty(t, plan, "EQP returned no plan rows for %s", q)
		joined := strings.Join(plan, " | ")
		assert.NotContains(t, joined, "USE TEMP B-TREE", "query must not sort through a temp b-tree: %s", joined)
		assert.Contains(t, joined, "idx_images_created", "query must seek the keyset index: %s", joined)
	}
}

// TestFTSPrefixQuery guards the prefix='2 3 4' index config.
func TestFTSPrefixQuery(t *testing.T) {
	db := setupTestDB(t)
	in := &dbImage{
		ID: "bbb0002", SHA256: strings.Repeat("02", 32), Filename: "x.png",
		MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-24 00:00:00",
		ThumbStatus:    thumbStatusPending,
		OriginalPrompt: "shrew comin in hot",
		MetaSource:     metaSourceUpload,
	}
	require.NoError(t, dbInsertImage(db, in))

	var n int
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_fts WHERE images_fts MATCH 'shre*'`))
	assert.Equal(t, 1, n, "2-char prefix token must use the prefix index")
}

// TestDbHideImageConcurrentSingleWinner races two dbHideImage calls on
// one visible row. The `hidden = 0` guard means exactly one UPDATE may
// match whatever the arrival order: the winner reports true (→ HTTP 200
// + the single image-hidden publish), the loser false (→ 410, no
// re-publish). Same single-winner contract as img-mcp's terminal-update
// fencing, exercised here at the db layer.
func TestDbHideImageConcurrentSingleWinner(t *testing.T) {
	db := setupTestDB(t)

	const rounds = 10
	for i := 0; i < rounds; i++ {
		id := fmt.Sprintf("r%06d", i)
		require.NoError(t, dbInsertImage(db, &dbImage{
			ID: id, SHA256: strings.Repeat("03", 32), Filename: "x.png",
			MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-24 00:00:00",
			ThumbStatus: thumbStatusPending, MetaSource: metaSourceUpload,
		}), "round %d: seed visible row", i)

		// Both goroutines must finish before any assertion (require
		// inside a goroutine would call t.FailNow off the test
		// goroutine), so results — including errors — are collected
		// and judged after wg.Wait.
		results := make([]bool, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for g := range results {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				results[g], errs[g] = dbHideImage(db, id)
			}(g)
		}
		wg.Wait()

		for g, err := range errs {
			require.NoError(t, err, "round %d goroutine %d", i, g)
		}
		winners := 0
		for _, hid := range results {
			if hid {
				winners++
			}
		}
		require.Equal(t, 1, winners, "round %d: exactly one rows-affected=1 winner, one 410 loser", i)

		var hidden bool
		require.NoError(t, db.Get(&hidden, `SELECT hidden FROM images WHERE id = ?`, id))
		assert.True(t, hidden, "round %d: row must end hidden regardless of arrival order", i)
	}
}

// TestSafetyMigrationAddsColumn pins migration 003: the images.safety
// column exists, NOT NULL, defaulting to 'unknown' (default-deny for
// the safe site — unclassified rows stay invisible there until a
// verdict lands). Three probes: a raw INSERT omitting the column must
// land 'unknown' (the DEFAULT itself), dbInsertImage must normalize a
// zero-value Safety to 'unknown' (the Go-side invariant: ” never
// reaches the DB), and real verdicts round-trip.
func TestSafetyMigrationAddsColumn(t *testing.T) {
	db := setupTestDB(t)

	// The migration default itself: a raw INSERT omitting safety.
	_, err := db.Exec(`INSERT INTO images
		(id, sha256, filename, mime_type, size_bytes, created_at)
		VALUES ('saf0001', ?, 'x.png', 'image/png', 1, '2026-09-26 00:00:00')`,
		strings.Repeat("07", 32))
	require.NoError(t, err)
	var s string
	require.NoError(t, db.Get(&s, `SELECT safety FROM images WHERE id = 'saf0001'`))
	assert.Equal(t, safetyUnknown, s, "omitted column must take the DEFAULT 'unknown'")

	// dbInsertImage: zero-value struct normalizes to 'unknown'; verdicts
	// round-trip.
	for _, tt := range []struct {
		id     string
		safety string
		want   string
	}{
		{"saf0002", "", safetyUnknown},
		{"saf0003", safetySafe, safetySafe},
		{"saf0004", safetyUnsafe, safetyUnsafe},
	} {
		require.NoError(t, dbInsertImage(db, &dbImage{
			ID: tt.id, SHA256: strings.Repeat("08", 32), Filename: "x.png",
			MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-26 00:00:00",
			ThumbStatus: thumbStatusPending, Safety: tt.safety,
		}), "insert %s", tt.id)
		got, err := dbGetImageByID(db, tt.id)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got.Safety, "id %s", tt.id)
	}

	// NOT NULL: the column can never hold NULL.
	var notnull int
	require.NoError(t, db.Get(&notnull,
		`SELECT "notnull" FROM pragma_table_info('images') WHERE name = 'safety'`))
	assert.Equal(t, 1, notnull, "safety must be NOT NULL — NULL never occurs")
}

// TestSafetyUpdatePreservedWhenPayloadEmpty pins the
// dbUpdateImageMetadata guard: an update payload whose Safety is ""
// (a hand-built struct that never went through applyMergedMetadata)
// must not clobber a stored verdict, while an explicit verdict — how
// the re-extract backfill path writes one — still lands.
func TestSafetyUpdatePreservedWhenPayloadEmpty(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, dbInsertImage(db, &dbImage{
		ID: "safupd1", SHA256: strings.Repeat("09", 32), Filename: "x.png",
		MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-26 00:00:00",
		ThumbStatus: thumbStatusPending, Safety: safetySafe,
	}))

	img, err := dbGetImageByID(db, "safupd1")
	require.NoError(t, err)

	// Empty payload safety: the stored 'safe' survives.
	img.Safety = ""
	img.OriginalPrompt = "rewritten by re-extract"
	require.NoError(t, dbUpdateImageMetadata(db, img))
	got, err := dbGetImageByID(db, "safupd1")
	require.NoError(t, err)
	assert.Equal(t, safetySafe, got.Safety, "empty update payload must never clobber a verdict")
	assert.Equal(t, "rewritten by re-extract", got.OriginalPrompt, "sanity: the update itself did run")

	// Explicit verdict still writes (the re-extract backfill shape).
	img.Safety = safetyUnsafe
	require.NoError(t, dbUpdateImageMetadata(db, img))
	got, err = dbGetImageByID(db, "safupd1")
	require.NoError(t, err)
	assert.Equal(t, safetyUnsafe, got.Safety, "explicit verdicts update (backfill)")
}

// TestGalleryOrdersMillisecondPrecise pins why created_at carries
// millisecond precision: two images completing inside the same second
// must order by their actual completion time, not by the random base62
// id tiebreak (which made a later image sort before an earlier one).
func TestGalleryOrdersMillisecondPrecise(t *testing.T) {
	app := newTestApp(t, testConfig())

	// Same second. The LATER image carries the lexically SMALLER id:
	// under the old second-precision format the id DESC tiebreak would
	// have sorted the EARLIER image first (wrong) — this fixture makes
	// the test discriminate between the two behaviors.
	insertImageWithFile(t, app, "aaalate0", pngBytes("late"), func(img *dbImage) {
		img.CreatedAt = "2026-09-24 10:00:00.900"
	})
	insertImageWithFile(t, app, "zzzearly", pngBytes("early"), func(img *dbImage) {
		img.CreatedAt = "2026-09-24 10:00:00.100"
	})

	rows, err := dbGetGalleryPage(app.db, "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "aaalate0", rows[0].ID, "later completion sorts first despite lexically smaller id")
	assert.Equal(t, "zzzearly", rows[1].ID)
}

// TestParseDBTimeLegacyRows pins the dual-precision parser: legacy
// second-format rows and cursors keep parsing after the ms migration.
func TestParseDBTimeLegacyRows(t *testing.T) {
	for _, s := range []string{"2026-09-24 10:00:00.123", "2026-09-24 10:00:00"} {
		_, ok := parseDBTime(s)
		assert.True(t, ok, "parse %q", s)
	}
	_, ok := parseDBTime("not a time")
	assert.False(t, ok)

	// Legacy cursors still validate.
	_, _, ok = parseKeysetCursor("2026-09-24 10:00:00~abc0123")
	assert.True(t, ok, "legacy-format cursor accepted")
	_, _, ok = parseKeysetCursor("2026-09-24 10:00:00.123~abc0123")
	assert.True(t, ok, "ms-format cursor accepted")
}
