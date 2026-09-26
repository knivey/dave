package main

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

// dbTimeFormat is the storage format for created_at: UTC with
// MILLISECOND precision, generated in Go so uploads never depend on the
// server's clock configuration. Seconds are not enough — the keyset
// ordering is (created_at DESC, id DESC) and ids are random, so two
// images completing inside the same second would order arbitrarily;
// with max_workers > 1 that genuinely happens (Sep 2026 production:
// generations finishing back-to-back). Fixed-width ms strings keep
// lexicographic == chronological among new rows. Legacy rows written
// at second precision sort BEFORE any ms row in the same second
// (string prefix comparison) — at most a one-second inversion for
// pre-migration data, accepted.
const dbTimeFormat = "2006-01-02 15:04:05.000"

// dbTimeFormatLegacy is the pre-millisecond format. parseDBTime
// accepts both so old rows and old-format cursors keep working.
const dbTimeFormatLegacy = "2006-01-02 15:04:05"

// parseDBTime parses a stored created_at in either precision.
func parseDBTime(s string) (time.Time, bool) {
	if t, err := time.Parse(dbTimeFormat, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(dbTimeFormatLegacy, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

const (
	thumbStatusPending = "pending"
	thumbStatusReady   = "ready"
	thumbStatusFailed  = "failed"
	// meta_source values recording which side contributed metadata:
	// "exif" (EXIF/workflow only), "upload" (form meta only),
	// "upload+exif" (both).
	metaSourceEXIF          = "exif"
	metaSourceUpload        = "upload"
	metaSourceUploadAndEXIF = "upload+exif"

	// images.safety values (migration 003). 'unknown' is default-deny:
	// the safe site shows only allowed-network origins ∪ safety='safe',
	// so rows without a verdict stay invisible there until one lands
	// (upload meta or EXIF-note re-extract backfill today; the planned
	// -safety admin CLI will add manual marking). A verdict, once
	// stored, is only ever changed by an explicit write — merges and
	// re-extracts preserve it.
	safetyUnknown = "unknown"
	safetySafe    = "safe"
	safetyUnsafe  = "unsafe"
)

// dbImage mirrors the images table. Graph-derived columns are populated by
// the extraction milestone; until then they ride along as zero values.
type dbImage struct {
	ID          string `db:"id"`
	SHA256      string `db:"sha256"`
	Filename    string `db:"filename"`
	MimeType    string `db:"mime_type"`
	SizeBytes   int64  `db:"size_bytes"`
	Width       *int   `db:"width"`
	Height      *int   `db:"height"`
	CreatedAt   string `db:"created_at"`
	ThumbStatus string `db:"thumb_status"`
	Hidden      bool   `db:"hidden"`

	OriginalPrompt string  `db:"original_prompt"`
	EnhancedPrompt string  `db:"enhanced_prompt"`
	NegativePrompt string  `db:"negative_prompt"`
	Reasoning      string  `db:"reasoning"`
	JobID          *string `db:"job_id"`
	LLMGenerated   bool    `db:"llm_generated"`
	Network        *string `db:"network"`
	Channel        *string `db:"channel"`
	Nick           *string `db:"nick"`
	WorkflowName   *string `db:"workflow_name"`
	// Safety is one of safetyUnknown | safetySafe | safetyUnsafe —
	// never "" and never NULL after insert (dbInsertImage normalizes a
	// zero value to 'unknown'; migration 003 backfilled all
	// pre-existing rows the same way).
	Safety string `db:"safety"`

	Seed      *int64   `db:"seed"`
	Steps     *int     `db:"steps"`
	Cfg       *float64 `db:"cfg"`
	Denoise   *float64 `db:"denoise"`
	Sampler   *string  `db:"sampler"`
	Scheduler *string  `db:"scheduler"`
	ModelUnet *string  `db:"model_unet"`
	ModelClip *string  `db:"model_clip"`
	ModelVae  *string  `db:"model_vae"`
	Loras     *string  `db:"loras"`

	WorkflowJSON string `db:"workflow_json"`
	MetaSource   string `db:"meta_source"`
}

func initDB(dbPath string) (*sqlx.DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating database directory %s: %w", dir, err)
		}
	}

	sqldb, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", dbPath, err)
	}

	sqldb.SetMaxOpenConns(1)

	if err := goose.SetDialect("sqlite3"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("setting goose dialect: %w", err)
	}

	goose.SetBaseFS(embedMigrations)

	if err := goose.Up(sqldb, "migrations"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	db := sqlx.NewDb(sqldb, "sqlite")

	loggerDB.Info("Database initialized", "path", dbPath)
	return db, nil
}

func closeDB(db *sqlx.DB) {
	if db != nil {
		db.Close()
	}
}

func dbInsertImage(db *sqlx.DB, img *dbImage) error {
	// safety normalization at the write boundary: a zero-value ("")
	// Safety — any hand-built dbImage, e.g. the import path or test
	// fixtures — lands as the column's semantic default 'unknown', so
	// the DB invariant (values are exactly unknown|safe|unsafe) holds
	// for every writer without each caller remembering to set it.
	_, err := db.NamedExec(
		`INSERT INTO images (
			id, sha256, filename, mime_type, size_bytes, width, height,
			created_at, thumb_status, hidden,
			original_prompt, enhanced_prompt, negative_prompt, reasoning,
			job_id, llm_generated, network, channel, nick, workflow_name,
			safety,
			seed, steps, cfg, denoise, sampler, scheduler,
			model_unet, model_clip, model_vae, loras,
			workflow_json, meta_source
		) VALUES (
			:id, :sha256, :filename, :mime_type, :size_bytes, :width, :height,
			:created_at, :thumb_status, :hidden,
			:original_prompt, :enhanced_prompt, :negative_prompt, :reasoning,
			:job_id, :llm_generated, :network, :channel, :nick, :workflow_name,
			COALESCE(NULLIF(:safety, ''), 'unknown'),
			:seed, :steps, :cfg, :denoise, :sampler, :scheduler,
			:model_unet, :model_clip, :model_vae, :loras,
			:workflow_json, :meta_source
		)`, img)
	return err
}

// dbGetImageByID fetches one row including hidden ones (callers distinguish
// 404 from 410). Returns sql.ErrNoRows when the id is unknown.
func dbGetImageByID(db *sqlx.DB, id string) (*dbImage, error) {
	var img dbImage
	err := db.Get(&img, `SELECT * FROM images WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	return &img, nil
}

func dbImageIDExists(db *sqlx.DB, id string) (bool, error) {
	var exists bool
	err := db.Get(&exists, `SELECT EXISTS(SELECT 1 FROM images WHERE id = ?)`, id)
	return exists, err
}

// dbImageSHAExists reports whether any row already stores this hash.
// Hidden rows count as present: their bytes are in the store, and import
// dedupe must not resurrect a soft-deleted gallery entry. Used by import
// mode — the upload path deliberately does NOT call this (uploads mint a
// fresh row per generation even for known hashes; imports must be
// idempotent instead).
func dbImageSHAExists(db *sqlx.DB, sha string) (bool, error) {
	var exists bool
	err := db.Get(&exists, `SELECT EXISTS(SELECT 1 FROM images WHERE sha256 = ?)`, sha)
	return exists, err
}

// dbGetNewerImage returns the image immediately NEWER than the keyset
// cursor (created_at DESC, id DESC ordering — "newer" sorts before the
// cursor), filtering hidden rows. It is the details page's prev link and
// half of the neighbors API. sql.ErrNoRows maps to (nil, nil): no newer
// image exists, which is the normal end-of-gallery case.
//
// The row-value form `(created_at, id) > (?, ?)` lets SQLite range-seek
// idx_images_created directly; the equivalent OR-shaped predicate plans
// as MULTI-INDEX OR + temp b-tree (guarded by TestKeysetQueriesUseIndex-
// Seek in db_test.go).
func dbGetNewerImage(db *sqlx.DB, createdAt, id string) (*dbImage, error) {
	var img dbImage
	err := db.Get(&img, `
		SELECT * FROM images
		WHERE hidden = 0
		  AND (created_at, id) > (?, ?)
		ORDER BY created_at ASC, id ASC
		LIMIT 1`, createdAt, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &img, nil
}

// dbGetOlderImage is dbGetNewerImage's mirror: the image immediately
// OLDER than the cursor (the next link / gallery paging direction).
func dbGetOlderImage(db *sqlx.DB, createdAt, id string) (*dbImage, error) {
	var img dbImage
	err := db.Get(&img, `
		SELECT * FROM images
		WHERE hidden = 0
		  AND (created_at, id) < (?, ?)
		ORDER BY created_at DESC, id DESC
		LIMIT 1`, createdAt, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &img, nil
}

// dbGetPendingThumbIDs returns every row still awaiting thumbnails; the
// worker's startup re-scan enqueues them so a crash between INSERT and
// generation never strands placeholders forever.
func dbGetPendingThumbIDs(db *sqlx.DB) ([]string, error) {
	var ids []string
	err := db.Select(&ids, `SELECT id FROM images WHERE thumb_status = ? ORDER BY created_at DESC, id DESC`, thumbStatusPending)
	return ids, err
}

// dbUpdateThumbStatus flips thumb_status (terminal 'failed' today; a
// future admin action may reset to 'pending' for a retry).
func dbUpdateThumbStatus(db *sqlx.DB, id, status string) error {
	_, err := db.Exec(`UPDATE images SET thumb_status = ? WHERE id = ?`, status, id)
	return err
}

// dbUpdateThumbReady marks the row ready and backfills the decoded bounds
// ONLY where the columns are NULL — extraction owns them when the graph
// had a latent node; decoding is the fallback, not an overwrite.
func dbUpdateThumbReady(db *sqlx.DB, id string, width, height int) error {
	_, err := db.Exec(
		`UPDATE images SET thumb_status = ?, width = COALESCE(width, ?), height = COALESCE(height, ?) WHERE id = ?`,
		thumbStatusReady, width, height, id)
	return err
}

// dbGetGalleryPage returns one keyset page (created_at DESC, id DESC),
// hidden-filtered. Empty after* yields the first page. Callers fetch
// limit+1 rows to detect has-more.
func dbGetGalleryPage(db *sqlx.DB, afterCreatedAt, afterID string, limit int) ([]dbImage, error) {
	var rows []dbImage
	var err error
	if afterCreatedAt == "" {
		err = db.Select(&rows,
			`SELECT * FROM images WHERE hidden = 0 ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	} else {
		err = db.Select(&rows,
			`SELECT * FROM images WHERE hidden = 0 AND (created_at, id) < (?, ?)
			 ORDER BY created_at DESC, id DESC LIMIT ?`, afterCreatedAt, afterID, limit)
	}
	return rows, err
}

// dbHideImage soft-deletes a row: hidden=1, nothing else. DESIGN NOTE
// — soft delete only, never byte deletion: the store is content
// addressed and dedupe means N rows can share one stored file, so
// removing bytes from disk would pull the ground out from under sibling
// rows; and hidden routes answer 410 forever, which is the honest
// response for a URL that was already pasted into IRC. The UPDATE is
// guarded with `hidden = 0` and reports rows-affected so two racing
// DELETEs elect exactly one winner — the loser sees false (→ HTTP
// 410 from the handler) instead of re-publishing the image-hidden
// event for a row that is already gone.
//
// FTS note: the images_fts_au trigger only fires for UPDATEs of the
// prompt columns, so hiding deliberately leaves the row's tokens in the
// index — every search path joins back to images and filters
// hidden = 0 (covered in search_test.go).
func dbHideImage(db *sqlx.DB, id string) (bool, error) {
	res, err := db.Exec(`UPDATE images SET hidden = 1 WHERE id = ? AND hidden = 0`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// nullStr maps "" to SQL NULL for the display-only provenance columns, so
// later queries can distinguish "not provided" from "empty".
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// dbGetImagesWithWorkflow returns every row carrying an embedded workflow
// graph (hidden rows included — re-extraction is metadata-only and must not
// resurrect or alter visibility state).
func dbGetImagesWithWorkflow(db *sqlx.DB) ([]dbImage, error) {
	var rows []dbImage
	err := db.Select(&rows,
		`SELECT * FROM images WHERE workflow_json IS NOT NULL AND workflow_json != '' ORDER BY id ASC`)
	return rows, err
}

// dbUpdateImageMetadata rewrites exactly the metadata columns the upload
// INSERT writes — never thumb_status, hidden, sha256, filename, mime_type,
// size_bytes, or created_at. Updating original_prompt/enhanced_prompt fires
// the FTS 'delete' dance via the images_fts_au trigger, so search stays in
// sync with the rewritten prompts. width/height use COALESCE — fresh graph
// dims win when present, but a graph with no recognizable latent source
// must not NULL out dimensions the thumb worker backfilled from the decoded
// image (that ownership rule mirrors dbUpdateThumbReady's own
// fill-only-when-NULL COALESCE). safety uses the same guarded shape for
// strings: an empty payload value ("") keeps the stored verdict — a
// hand-built update struct can never clobber a classification — while an
// explicit verdict (the re-extract backfill writes one that way) lands.
func dbUpdateImageMetadata(db *sqlx.DB, img *dbImage) error {
	_, err := db.NamedExec(
		`UPDATE images SET
			original_prompt = :original_prompt,
			enhanced_prompt = :enhanced_prompt,
			negative_prompt = :negative_prompt,
			reasoning = :reasoning,
			job_id = :job_id,
			llm_generated = :llm_generated,
			network = :network,
			channel = :channel,
			nick = :nick,
			workflow_name = :workflow_name,
			safety = COALESCE(NULLIF(:safety, ''), safety),
			seed = :seed,
			steps = :steps,
			cfg = :cfg,
			denoise = :denoise,
			sampler = :sampler,
			scheduler = :scheduler,
			model_unet = :model_unet,
			model_clip = :model_clip,
			model_vae = :model_vae,
			loras = :loras,
			workflow_json = :workflow_json,
			width = COALESCE(:width, width),
			height = COALESCE(:height, height),
			meta_source = :meta_source
		 WHERE id = :id`, img)
	return err
}

func ptrValue[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
