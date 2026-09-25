package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// Import mode (`imgsite -import <dir>`): walks a ComfyUI outputs folder
// recursively and ingests every webp/PNG carrying an embedded workflow —
// non-destructively (originals are COPIED into the content-addressed
// store, never moved or deleted) and idempotently (a sha256 already
// known to the DB is skipped, so re-running the import is a report-only
// no-op).
//
// Design decisions (owner-approved, Sep 2026):
//   - created_at comes from the FILENAME (leading %Y-%m-%d-%H%M%S,
//     ComfyUI's output naming), not mtime — mtimes change whenever files
//     are copied or moved. Filename times are the ComfyUI host's local
//     time; the -tz flag declares that zone (default: the importing
//     machine's local zone). A `__N` batch suffix adds N milliseconds so
//     same-second batches keep their file order under the ms-precision
//     keyset ordering (see dbTimeFormat). Files with a dave note ALSO
//     parse a filename time use the filename time — the note carries no
//     timestamp, and consistent batch ordering beats it. Names that
//     don't match fall back to file mtime (UTC) with a per-file warning;
//     the batch is never aborted for it.
//   - Legacy images WITHOUT a dave original-prompt note import with an
//     EMPTY original_prompt and the workflow's final positive prompt in
//     enhanced_prompt — exactly what the standard extraction produces —
//     so they are searchable like any other enhanced-only (tier 2)
//     match. Images WITH a note import with full fidelity through the
//     same extraction path.
//   - Rows go through the same merge/insert code as uploads
//     (mergeUploadMetadata + applyMergedMetadata + dbInsertImage), so
//     FTS trigger rows, provenance NULL semantics, and column meanings
//     are identical. Provenance is empty; job_id empty unless a note
//     carries one; llm_generated comes from the note when present, else
//     false.
//   - No SSE events: the hub is not running offline, imported images
//     appear on the next page load. Run with the server STOPPED —
//     SQLite WAL tolerates a second writer, but a concurrent import
//     against a live server risks lock contention (documented).

// importFilenameLayout is the leading timestamp of a ComfyUI output
// filename (%Y-%m-%d-%H%M%S in strftime terms). Everything after the
// 17-byte prefix (the __N batch suffix, extension, ...) is ignored for
// parsing; the layout is all fixed-width numeric fields, so an exact
// byte-slice parse cannot run past the prefix.
const importFilenameLayout = "2006-01-02-150405"

// importBatchMaxMs caps the __N batch-suffix offset: created_at carries
// millisecond precision, so an index beyond 999ms would collide with the
// next second. Clamped (order stays monotonic as far as precision
// allows) rather than rejected — a >1000-image same-second batch is not
// a real scenario, and the files still import either way.
const importBatchMaxMs = 999

// importSummary is the import run's report. Per-file skips never fail
// the run; only fatal setup errors do (bad dir, config/DB open).
type importSummary struct {
	Imported          int
	SkippedDuplicate  int
	SkippedNoWorkflow int
	SkippedOther      int
	Errors            int

	// Inline thumbnail pass (see runImport): ready counts include dims
	// backfill from the decoded bounds.
	ThumbsReady  int
	ThumbsFailed int

	Warnings []string
}

// parseImportTZ resolves the -tz flag: empty means the importing
// machine's local zone (the common case — the archive sits on, or was
// copied via, the same host class that ran ComfyUI). Any name
// time.LoadLocation understands is accepted (IANA names like
// "America/New_York", "UTC", POSIX forms like "EST5EDT").
func parseImportTZ(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", name, err)
	}
	return loc, nil
}

// parseImportFilenameTime parses the leading timestamp of a ComfyUI
// output filename in the given zone. ok=false for names too short or not
// matching the layout — the digits are validated strictly, so an invalid
// month/day/hour or a stray letter fails rather than half-parsing.
func parseImportFilenameTime(name string, loc *time.Location) (time.Time, bool) {
	if len(name) < len(importFilenameLayout) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(importFilenameLayout, name[:len(importFilenameLayout)], loc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// parseImportBatchMillis extracts the __N batch-suffix offset (e.g.
// "2026-09-24-185355__3.webp" -> 3ms). Requires the double underscore
// directly after the timestamp prefix and at least one digit; the number
// ends at the first non-digit (the extension, another underscore, end of
// name). Missing/non-batch suffixes report ok=false — only a genuine
// __N gets the ordering offset. Values are clamped to importBatchMaxMs.
func parseImportBatchMillis(name string) (time.Duration, bool) {
	rest := name[len(importFilenameLayout):]
	if !strings.HasPrefix(rest, "__") {
		return 0, false
	}
	rest = rest[2:]
	n := -1 // -1 = no digit seen yet
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			break
		}
		if n < 0 {
			n = 0
		}
		n = n*10 + int(c-'0')
		if n > importBatchMaxMs {
			n = importBatchMaxMs
		}
	}
	if n < 0 {
		return 0, false
	}
	return time.Duration(n) * time.Millisecond, true
}

// runImport walks dir recursively (ComfyUI organizes outputs into date
// subfolders) and imports every webp/PNG whose bytes carry an embedded
// workflow through the standard extraction/merge/insert pipeline. out
// receives the per-file report lines; the returned summary carries the
// counts. A fatal error (dir unreadable / not a directory) is returned;
// every per-file outcome is a report line + counter, never an abort.
func runImport(cfg Config, db *sqlx.DB, store *Store, dir string, tz *time.Location, out io.Writer) (importSummary, error) {
	var sum importSummary

	info, err := os.Stat(dir)
	if err != nil {
		return sum, fmt.Errorf("import directory: %w", err)
	}
	if !info.IsDir() {
		return sum, fmt.Errorf("import path %s is not a directory", dir)
	}

	var importedIDs []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry (permissions, broken mount): report and
			// keep walking the rest of the tree.
			sum.Errors++
			fmt.Fprintf(out, "error: %s: %v\n", path, err)
			loggerImport.Error("import: walking entry failed", "path", path, "error", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}

		// Only what the extraction pipeline accepts: webp (RIFF/EXIF
		// chunk) and PNG (tEXt). Everything else is a visible, counted
		// skip — the owner decides what to do with strays.
		switch strings.ToLower(filepath.Ext(path)) {
		case ".webp", ".png":
		default:
			sum.SkippedOther++
			fmt.Fprintf(out, "skip (not webp/png): %s\n", rel)
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			sum.Errors++
			fmt.Fprintf(out, "error: %s: %v\n", rel, err)
			return nil
		}
		digest := sha256.Sum256(data)
		hashHex := hex.EncodeToString(digest[:])

		// Dedup FIRST (before any store write or extraction work): a
		// sha256 already present in the DB means the bytes were either
		// uploaded via dave before or imported by a previous run.
		// Unlike the upload path (which mints a second gallery row for
		// the same hash), import skips — re-runs must be idempotent.
		// Hidden rows count as present: the file is in the store and a
		// soft-deleted entry must not resurrect itself.
		exists, err := dbImageSHAExists(db, hashHex)
		if err != nil {
			sum.Errors++
			fmt.Fprintf(out, "error: %s: dedupe check: %v\n", rel, err)
			return nil
		}
		if exists {
			sum.SkippedDuplicate++
			fmt.Fprintf(out, "skip (duplicate, sha256 %s…): %s\n", hashHex[:12], rel)
			return nil
		}

		api, _, found := extractEmbeddedWorkflows(data)
		if !found {
			sum.SkippedNoWorkflow++
			fmt.Fprintf(out, "skip (no embedded workflow): %s\n", rel)
			return nil
		}
		md, exifOK := ExtractMetadata(api)
		if !exifOK {
			sum.SkippedNoWorkflow++
			fmt.Fprintf(out, "skip (embedded workflow unparseable): %s\n", rel)
			return nil
		}

		// created_at: filename timestamp in the import timezone, with
		// the __N batch offset keeping same-second batches ordered.
		// Fallback: file mtime (UTC) + a warning — never an abort.
		// Applies uniformly to noted and unnoted files (the note has no
		// timestamp of its own).
		base := filepath.Base(path)
		var createdAt string
		if t, ok := parseImportFilenameTime(base, tz); ok {
			if ms, ok := parseImportBatchMillis(base); ok {
				t = t.Add(ms)
			}
			createdAt = t.UTC().Format(dbTimeFormat)
		} else {
			fi, err := d.Info()
			if err != nil {
				sum.Errors++
				fmt.Fprintf(out, "error: %s: stat for mtime fallback: %v\n", rel, err)
				return nil
			}
			createdAt = fi.ModTime().UTC().Format(dbTimeFormat)
			warn := fmt.Sprintf("%s: filename %q does not match %s; using file mtime %s",
				rel, base, importFilenameLayout, createdAt)
			sum.Warnings = append(sum.Warnings, warn)
			fmt.Fprintf(out, "warning: %s\n", warn)
		}

		// Same merge as an upload with no meta form field: extraction is
		// the only contributor (meta_source="exif"), legacy graphs leave
		// original_prompt empty while the sampler's positive prompt
		// lands in enhanced_prompt, and noted graphs round-trip their
		// full payload.
		merged := mergeUploadMetadata(UploadMeta{}, md, true)

		mimeType := http.DetectContentType(data)
		if !isAllowedImageMIME(mimeType) {
			mimeType = "application/octet-stream"
		}

		// Copy into the content-addressed store (deduped by WriteOriginal
		// itself); the source file is never touched.
		if _, err := store.WriteOriginal(hashHex, data); err != nil {
			sum.Errors++
			fmt.Fprintf(out, "error: %s: store write: %v\n", rel, err)
			return nil
		}

		id, err := generateImageID(func(id string) (bool, error) {
			return dbImageIDExists(db, id)
		})
		if err != nil {
			sum.Errors++
			fmt.Fprintf(out, "error: %s: id generation: %v\n", rel, err)
			return nil
		}

		img := dbImage{
			ID:          id,
			SHA256:      hashHex,
			Filename:    sanitizeUploadFilename(base),
			MimeType:    mimeType,
			SizeBytes:   int64(len(data)),
			CreatedAt:   createdAt,
			ThumbStatus: thumbStatusPending,
		}
		applyMergedMetadata(&img, merged)
		if err := dbInsertImage(db, &img); err != nil {
			sum.Errors++
			fmt.Fprintf(out, "error: %s: db insert: %v\n", rel, err)
			return nil
		}
		sum.Imported++
		importedIDs = append(importedIDs, id)
		fmt.Fprintf(out, "imported %s as %s (%s)\n", rel, id, createdAt)
		return nil
	})
	if err != nil {
		return sum, fmt.Errorf("walking %s: %w", dir, err)
	}

	// Thumbnails + dims, without hand-holding. Imported rows are
	// inserted pending with dims from the graph when it has a latent
	// node and NULL otherwise — exactly like uploads — and the worker's
	// startup re-scan WOULD finish them on the next server start
	// (dbUpdateThumbReady COALESCE-backfills NULL dims from the decoded
	// bounds). But the re-scan's enqueue is non-blocking against a
	// bounded queue (cap 256): a large import would need several
	// restarts to drain. Running the worker's own per-row pipeline here
	// — process, not the raw runJob, so failures flip thumb_status to
	// 'failed' exactly like the worker would — makes the import
	// self-contained after every row is committed. Interruption stays
	// safe either way: unprocessed rows keep thumb_status 'pending' and
	// the next server start picks up the remainder. The throwaway
	// thumbWorker exists for process/runJob's recover boundary (one
	// hostile image must not kill a long import); no goroutines are
	// started, and its onThumbReady default is a no-op.
	tw := newThumbWorker(func() Config { return cfg }, db, store)
	for _, id := range importedIDs {
		if err := tw.process(id); err != nil {
			sum.ThumbsFailed++
			fmt.Fprintf(out, "thumbnail failed %s: %v\n", id, err)
			continue
		}
		sum.ThumbsReady++
	}

	return sum, nil
}

// writeImportSummary prints the end-of-run report: counts, warnings, and
// the DB/store paths the import wrote to (so a mistargeted run is
// obvious before anything else touches the data).
func writeImportSummary(w io.Writer, s importSummary, dbPath, storePath string) {
	fmt.Fprintln(w, "import summary")
	fmt.Fprintf(w, "  imported:                %d\n", s.Imported)
	fmt.Fprintf(w, "  skipped (duplicate):     %d\n", s.SkippedDuplicate)
	fmt.Fprintf(w, "  skipped (no workflow):   %d\n", s.SkippedNoWorkflow)
	fmt.Fprintf(w, "  skipped (not webp/png):  %d\n", s.SkippedOther)
	fmt.Fprintf(w, "  errors:                  %d\n", s.Errors)
	fmt.Fprintf(w, "  thumbnails ready:        %d\n", s.ThumbsReady)
	fmt.Fprintf(w, "  thumbnails failed:       %d\n", s.ThumbsFailed)
	fmt.Fprintf(w, "  warnings:                %d\n", len(s.Warnings))
	for _, warn := range s.Warnings {
		fmt.Fprintf(w, "    - %s\n", warn)
	}
	fmt.Fprintf(w, "  database: %s\n", dbPath)
	fmt.Fprintf(w, "  store:    %s\n", storePath)
}

// importMain is the CLI shell for import mode: config load (same file
// resolution as serve mode), DB open, then runImport + summary. Returns
// the process exit code — non-zero ONLY for fatal setup errors (bad
// -tz, bad dir, config/DB failure); per-file skips and thumbnail
// failures never fail the run.
func importMain(exeDir, configPath, dir, tzName string) int {
	tz, err := parseImportTZ(tzName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	cfg.Database.Resolved = resolvePath(exeDir, cfg.Database.Path)
	cfg.Storage.ResolvedPath = resolvePath(exeDir, cfg.Storage.Path)
	cfg.Storage.ResolvedThumbsPath = resolvePath(exeDir, cfg.Storage.ThumbsPath)

	db, err := initDB(cfg.Database.Resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database error: %v\n", err)
		return 1
	}
	defer closeDB(db)

	store := NewStore(cfg.Storage.ResolvedPath, cfg.Storage.ResolvedThumbsPath)

	loggerImport.Info("import started", "dir", dir, "tz", tz.String())
	sum, err := runImport(cfg, db, store, dir, tz, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}

	fmt.Println()
	writeImportSummary(os.Stdout, sum, cfg.Database.Resolved, cfg.Storage.ResolvedPath)
	loggerImport.Info("import complete",
		"imported", sum.Imported,
		"skipped_duplicate", sum.SkippedDuplicate,
		"skipped_no_workflow", sum.SkippedNoWorkflow,
		"skipped_other", sum.SkippedOther,
		"errors", sum.Errors,
		"thumbs_ready", sum.ThumbsReady,
		"thumbs_failed", sum.ThumbsFailed,
		"warnings", len(sum.Warnings),
	)
	return 0
}
