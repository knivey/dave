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

// Safety mode (`imgsite -safety <id>[,<id>…] safe|unsafe|unknown`): the
// admin's manual override for images.safety — the tool that clears the
// accumulated 'unknown' pile (pre-safety-history rows, generations whose
// automatic vet failed or timed out) and corrects mis-vetted verdicts.
// 'unknown' is a full reset, not a third verdict: the row returns to
// default-deny on the safe host exactly as if no verdict existed. Every
// set goes through dbSetSafety — the unguarded `UPDATE images SET
// safety = ? WHERE id = ?` — because applying over ANY prior value is
// the tool's entire purpose (see dbSetSafety's design note for why it
// deliberately differs from dbHideImage's hidden=0 guard). The row
// lookup rides dbGetImageByIDFn like delete mode, so unknown ids are
// 404-equivalents distinguished up front, and the pre-update row
// supplies the prompt snippet and the old verdict for the transition
// line.
//
// Offline op like -import/-delete: no SSE hub exists in CLI mode, so no
// event is published — safe-site visibility changes on the next page
// load (the verdict is visibility state, not content; the card itself
// never moves). Run with the server stopped (or accept that connected
// pages refresh later); documented in the usage text and README.
//
// No confirmation prompt: the operation is exactly as reversible as the
// next invocation — undo by running -safety again with another value —
// which the summary prints after every run.

// parseSafetyIDs validates the -safety argument: one id or a
// comma-separated list, each exactly 7 base62 chars (validImageID — the
// public-route shape). Whitespace around entries is tolerated; empty
// entries and malformed ids are usage errors, returned as errors so
// main can print the usage text and exit 2. Duplicate ids are deduped
// preserving first occurrence — a repeated id is a finger-slip, and
// without dedupe it would report a confusing second transition line.
func parseSafetyIDs(spec string) ([]string, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, fmt.Errorf("-safety needs at least one image id (comma-separated), e.g. -safety aQ3f9xK,zZ9y8xB safe")
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

// parseSafetyValue validates the verdict positional: exactly one of
// safe | unsafe | unknown (case-sensitive — the DB invariant is
// lowercase-only, and guessing at the admin's intent behind "SAFE"
// buys nothing). Anything else is a usage error so it never reaches
// the DB; unknown is fully valid (reset), not an error.
func parseSafetyValue(v string) (string, error) {
	switch v {
	case safetySafe, safetyUnsafe, safetyUnknown:
		return v, nil
	}
	return "", fmt.Errorf("invalid safety value %q: expected safe, unsafe, or unknown", v)
}

// safetySummary is the safety run's report. Per-id misses never abort
// the run — every id is attempted exactly once — but any of them makes
// the process exit 1.
type safetySummary struct {
	Set      int
	NotFound int
	Errors   int
}

// runSafety sets the verdict on each id through dbSetSafety. out
// receives the per-id report lines; the returned summary carries the
// counts. No id ever aborts the loop: an unknown id or DB hiccup is a
// per-id line and the run moves on, so one bad id in a batch never
// leaves the rest unclassified.
//
// Exit-shape note (deliberate divergence from runDelete): a row
// already at the requested value still counts as Set — the run's
// contract is "these rows now have verdict V", and such a row
// satisfies it. -delete exits 1 on an already-hidden row only to
// mirror the HTTP endpoint's 410; -safety has no HTTP analogue whose
// error matrix to mirror. SQLite counts a matched-but-unchanged row in
// rows-affected, so dbSetSafety reports it as a normal set and the
// transition line says "(was safe)" etc.
func runSafety(db *sqlx.DB, ids []string, value string, out io.Writer) safetySummary {
	var s safetySummary
	for _, id := range ids {
		// Resolve the row first — dbGetImageByID includes hidden rows
		// (a hidden image can hold a verdict; hiding and classifying
		// are independent axes) — for the snippet and the old verdict
		// the transition line reports.
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
		changed, err := dbSetSafetyFn(db, id, value)
		if err != nil {
			s.Errors++
			fmt.Fprintf(out, "error: %s: set: %v\n", id, err)
			continue
		}
		if !changed {
			// Zero rows affected after a successful lookup: the row
			// vanished between the lookup and the UPDATE. No hard
			// delete exists in the system, so this is a tripwire
			// path, surfaced like delete mode's concurrent-loss line.
			s.NotFound++
			fmt.Fprintf(out, "not found: %s (row vanished between lookup and update)\n", id)
			continue
		}
		s.Set++
		// deletePromptSnippet is shared with delete mode — the same
		// recognize-the-image helper (original prompt preferred,
		// enhanced fallback, capped).
		fmt.Fprintf(out, "set safety=%s %s — %q (was %s)\n", value, id, deletePromptSnippet(img), img.Safety)
	}
	return s
}

// writeSafetySummary prints the end-of-run report, mirroring
// writeDeleteSummary: the verdict written (so a misread run is
// obvious), counts, the DB path written to (so a mistargeted run is
// obvious), and the reversibility note. No SSE events were published;
// safe-site visibility follows on the next page load, and the verdict
// survives re-extract (only an explicit -safety run changes it).
func writeSafetySummary(w io.Writer, s safetySummary, value, dbPath string) {
	fmt.Fprintln(w, "safety summary")
	fmt.Fprintf(w, "  value:          %s\n", value)
	fmt.Fprintf(w, "  set:            %d\n", s.Set)
	fmt.Fprintf(w, "  not found:      %d\n", s.NotFound)
	fmt.Fprintf(w, "  errors:         %d\n", s.Errors)
	fmt.Fprintf(w, "  database:       %s\n", dbPath)
	fmt.Fprintln(w, "  offline verdict write (no live-update events; safe-site visibility")
	fmt.Fprintln(w, "  changes on the next page load); undo: run -safety again with another value")
}

// safetyMain is the CLI shell for safety mode: config load (same file
// resolution as serve/import/delete mode) and read-write DB open
// (initDB — the same path the other offline modes use), then runSafety
// + summary. Returns the process exit code: 1 for fatal setup errors
// (config/DB open) and for any per-id miss — not-found, lookup, or set
// failure; 0 only when every requested id was set by this run. Usage-
// class errors (bad -safety argument or verdict value) are caught in
// main before this runs.
func safetyMain(exeDir, configPath string, ids []string, value string) int {
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		return 1
	}
	cfg.Database.Resolved = resolvePath(exeDir, cfg.Database.Path)

	// No store wiring needed: the verdict is one metadata column —
	// files, thumbs, and every other row field are untouched.
	db, err := initDB(cfg.Database.Resolved)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database error: %v\n", err)
		return 1
	}
	defer closeDB(db)

	loggerSafety.Info("safety started", "ids", strings.Join(ids, ","), "value", value)
	sum := runSafety(db, ids, value, os.Stdout)

	fmt.Println()
	writeSafetySummary(os.Stdout, sum, value, cfg.Database.Resolved)
	loggerSafety.Info("safety complete",
		"value", value,
		"set", sum.Set,
		"not_found", sum.NotFound,
		"errors", sum.Errors,
	)
	if sum.Set == len(ids) {
		return 0
	}
	return 1
}
