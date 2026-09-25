package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jmoiron/sqlx"
)

// Delete mode (`imgsite -delete <id>[,<id>…]`): the admin's offline soft
// delete for inappropriate images. Single-sourced with the HTTP endpoint:
// every hide goes through dbHideImage — the exact guarded
// `UPDATE images SET hidden = 1 … WHERE hidden = 0` the
// DELETE /api/images/<id> handler runs — so the CLI can never diverge
// from the wire path's semantics (hidden=1, files retained, never a
// purge; the guarded UPDATE elects exactly one winner). The row lookup
// likewise rides dbGetImageByIDFn so unknown ids (404-equivalent) and
// already-hidden rows (410-equivalent) are distinguished up front, the
// same split handleDeleteImage performs.
//
// Offline op like -import: no SSE hub exists in CLI mode, so no
// image-hidden event is published — connected live pages keep the card
// until their next load. Run with the server stopped (or accept that
// live pages refresh later); documented in the usage text and README.
//
// No confirmation prompt: the operation is a reversible soft hide
// (restore: UPDATE images SET hidden=0 WHERE id='…'), which the summary
// prints after every run.

// deleteSnippetChars caps the prompt echo in the per-id success line —
// enough to recognize the image, not enough to spam the terminal when
// hiding a batch.
const deleteSnippetChars = 48

// parseDeleteIDs validates the -delete argument: one id or a
// comma-separated list, each exactly 7 base62 chars (validImageID — the
// public-route shape). Whitespace around entries is tolerated; empty
// entries and malformed ids are usage errors, returned as errors so
// main can print the usage text and exit 2. Duplicate ids are deduped
// preserving first occurrence — a repeated id is a finger-slip, and
// without dedupe it would report "already hidden" and fail the run.
func parseDeleteIDs(spec string) ([]string, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("-delete needs at least one image id (comma-separated), e.g. -delete aQ3f9xK or -delete aQ3f9xK,zZ9y8xB")
	}
	var ids []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(spec, ",") {
		id := strings.TrimSpace(part)
		if !validImageID(id) {
			return nil, fmt.Errorf("invalid image id %q: ids are exactly 7 base62 characters", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// deletePromptSnippet picks the most human-meaningful prompt text to
// echo back: the original prompt when present (what the user actually
// typed), else the enhanced prompt; whitespace collapsed, capped with
// an ellipsis. "(no prompt)" when both are empty — legacy imports carry
// an empty original prompt, and a meta-less row can have neither.
func deletePromptSnippet(img *dbImage) string {
	s := img.OriginalPrompt
	if s == "" {
		s = img.EnhancedPrompt
	}
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "(no prompt)"
	}
	if r := []rune(s); len(r) > deleteSnippetChars {
		return string(r[:deleteSnippetChars]) + "…"
	}
	return s
}

// deleteSummary is the delete run's report. Per-id misses never abort
// the run — every id is attempted exactly once — but any of them makes
// the process exit 1 (mirroring the HTTP 404/410 matrix).
type deleteSummary struct {
	Hidden        int
	AlreadyHidden int
	NotFound      int
	Errors        int
}

// runDelete hides each id through the same dbHideImage call the HTTP
// DELETE handler uses. out receives the per-id report lines; the
// returned summary carries the counts. No id ever aborts the loop: an
// unknown id or DB hiccup is a per-id line and the run moves on, so one
// bad id in a batch never leaves the rest unhidden.
func runDelete(db *sqlx.DB, ids []string, out io.Writer) deleteSummary {
	var s deleteSummary
	for _, id := range ids {
		// Resolve the row first — handleDeleteImage's 404-vs-410 split:
		// dbGetImageByID includes hidden rows so a known-but-hidden id
		// gets the distinct already-hidden line, not "not found".
		img, err := dbGetImageByIDFn(db, id)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				s.NotFound++
				fmt.Fprintf(out, "not found: %s\n", id)
				continue
			}
			s.Errors++
			fmt.Fprintf(out, "error: %s: lookup: %v\n", id, err)
			continue
		}
		if img.Hidden {
			s.AlreadyHidden++
			fmt.Fprintf(out, "already hidden: %s (nothing to do)\n", id)
			continue
		}
		hid, err := dbHideImageFn(db, id)
		if err != nil {
			s.Errors++
			fmt.Fprintf(out, "error: %s: hide: %v\n", id, err)
			continue
		}
		if !hid {
			// Lost the guarded UPDATE — the row was hidden between the
			// lookup and the UPDATE (another writer). Same outcome as
			// the HTTP handler's 410 branch; nothing re-published.
			s.AlreadyHidden++
			fmt.Fprintf(out, "already hidden: %s (hidden concurrently)\n", id)
			continue
		}
		s.Hidden++
		fmt.Fprintf(out, "hidden %s — %q (files retained)\n", id, deletePromptSnippet(img))
	}
	return s
}

// writeDeleteSummary prints the end-of-run report, mirroring
// writeImportSummary: counts, the DB path written to (so a mistargeted
// run is obvious), and the reversibility note — the exact SQL that
// undoes a hide. No SSE events were published; live pages refresh on
// their next load.
func writeDeleteSummary(w io.Writer, s deleteSummary, dbPath string) {
	fmt.Fprintln(w, "delete summary")
	fmt.Fprintf(w, "  hidden:          %d\n", s.Hidden)
	fmt.Fprintf(w, "  already hidden:  %d\n", s.AlreadyHidden)
	fmt.Fprintf(w, "  not found:       %d\n", s.NotFound)
	fmt.Fprintf(w, "  errors:          %d\n", s.Errors)
	fmt.Fprintf(w, "  database:        %s\n", dbPath)
	fmt.Fprintln(w, "  reversible soft hide (files retained, no live-update events);")
	fmt.Fprintf(w, "  restore with: UPDATE images SET hidden=0 WHERE id='<id>'\n")
}

// deleteMain is the CLI shell for delete mode: config load (same file
// resolution as serve/import mode) and read-write DB open (initDB —
// the same path import uses), then runDelete + summary. Returns the
// process exit code: 1 for fatal setup errors (config/DB open) and for
// any per-id miss — not-found, already-hidden, or lookup failure —
// mirroring the HTTP 404/410 error statuses; 0 only when every
// requested id was hidden by this run. Usage-class errors (bad -delete
// argument) are caught by parseDeleteIDs in main before this runs.
func deleteMain(exeDir, configPath string, ids []string) int {
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	cfg.Database.Resolved = resolvePath(exeDir, cfg.Database.Path)

	// No store wiring needed: the hide never touches files — that is
	// the point of the soft delete (content-addressed storage is
	// shared by dedupe; see dbHideImage's design note).
	db, err := initDB(cfg.Database.Resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database error: %v\n", err)
		return 1
	}
	defer closeDB(db)

	loggerDelete.Info("delete started", "ids", strings.Join(ids, ","))
	sum := runDelete(db, ids, os.Stdout)

	fmt.Println()
	writeDeleteSummary(os.Stdout, sum, cfg.Database.Resolved)
	loggerDelete.Info("delete complete",
		"hidden", sum.Hidden,
		"already_hidden", sum.AlreadyHidden,
		"not_found", sum.NotFound,
		"errors", sum.Errors,
	)
	if sum.Hidden == len(ids) {
		return 0
	}
	return 1
}
