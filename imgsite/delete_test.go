package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── Argument parsing ──────────────────────────────────────────────────────

func TestParseDeleteIDs(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr string
	}{
		{"SingleID", "aQ3f9xK", []string{"aQ3f9xK"}, ""},
		{"CommaList", "aQ3f9xK,zZ9y8xB,qW3rt5L", []string{"aQ3f9xK", "zZ9y8xB", "qW3rt5L"}, ""},
		{"WhitespaceTolerated", " aQ3f9xK , zZ9y8xB ", []string{"aQ3f9xK", "zZ9y8xB"}, ""},
		{"DuplicatesDeduped", "aQ3f9xK,zZ9y8xB,aQ3f9xK", []string{"aQ3f9xK", "zZ9y8xB"}, ""},
		{"Empty", "", nil, "at least one image id"},
		{"WhitespaceOnly", "   ", nil, "at least one image id"},
		{"EmptyElement", "aQ3f9xK,,zZ9y8xB", nil, `invalid image id ""`},
		{"TrailingComma", "aQ3f9xK,", nil, `invalid image id ""`},
		{"TooShort", "abc", nil, `invalid image id "abc"`},
		{"TooLong", "abcdefgh", nil, `invalid image id "abcdefgh"`},
		{"BadChars", "abc12 !!", nil, "invalid image id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDeleteIDs(tt.in)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ─── Prompt snippet ────────────────────────────────────────────────────────

func TestDeletePromptSnippet(t *testing.T) {
	long := strings.Repeat("w", deleteSnippetChars+10)
	tests := []struct {
		name string
		img  dbImage
		want string
	}{
		{"OriginalPreferred", dbImage{OriginalPrompt: "the original", EnhancedPrompt: "the enhanced"}, "the original"},
		{"EnhancedFallback", dbImage{EnhancedPrompt: "the enhanced"}, "the enhanced"},
		{"BothEmpty", dbImage{}, "(no prompt)"},
		{"WhitespaceCollapsed", dbImage{OriginalPrompt: "  a \n\t b   c  "}, "a b c"},
		{"TruncatedWithEllipsis", dbImage{OriginalPrompt: long}, strings.Repeat("w", deleteSnippetChars) + "…"},
		{"ExactLengthUntouched", dbImage{OriginalPrompt: strings.Repeat("w", deleteSnippetChars)}, strings.Repeat("w", deleteSnippetChars)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := tt.img
			assert.Equal(t, tt.want, deletePromptSnippet(&img))
		})
	}
}

// ─── runDelete flow (in-process, temp DB) ──────────────────────────────────

// TestRunDeleteFlow drives the full CLI semantics against a temp DB:
// hide, per-id reporting, files retained, already-hidden on re-run, and
// unknown ids failing the run without blocking the rest.
func TestRunDeleteFlow(t *testing.T) {
	app := newTestApp(t, testConfig())

	hashA := insertImageWithFile(t, app, "delA001", []byte("image a"),
		func(img *dbImage) { img.OriginalPrompt = "shrew walkin down main street" })
	insertImageWithFile(t, app, "delB002", []byte("image b"),
		func(img *dbImage) { img.EnhancedPrompt = "enhanced-only prompt about otters" })

	t.Run("HidesBothAndReports", func(t *testing.T) {
		var out bytes.Buffer
		sum := runDelete(app.db, []string{"delA001", "delB002"}, &out)
		assert.Equal(t, deleteSummary{Hidden: 2}, sum)
		s := out.String()
		assert.Contains(t, s, `hidden delA001 — "shrew walkin down main street" (files retained)`)
		assert.Contains(t, s, `hidden delB002 — "enhanced-only prompt about otters" (files retained)`)

		img, err := dbGetImageByID(app.db, "delA001")
		require.NoError(t, err)
		assert.True(t, img.Hidden, "row flips hidden=1")
		img, err = dbGetImageByID(app.db, "delB002")
		require.NoError(t, err)
		assert.True(t, img.Hidden, "row flips hidden=1")
	})

	t.Run("FilesStillOnDisk", func(t *testing.T) {
		// Soft delete only — the store bytes are never purged (dedupe
		// shares them across rows; see dbHideImage's design note).
		assert.True(t, fileExists(app.store.OriginalPath(hashA)), "original a still on disk")
	})

	t.Run("SecondRunReportsAlreadyHidden", func(t *testing.T) {
		var out bytes.Buffer
		sum := runDelete(app.db, []string{"delA001", "delB002"}, &out)
		assert.Equal(t, deleteSummary{AlreadyHidden: 2}, sum)
		assert.Contains(t, out.String(), "already hidden: delA001 (nothing to do)")
		assert.Contains(t, out.String(), "already hidden: delB002 (nothing to do)")
	})

	t.Run("UnknownIDReportedOtherStillProcessed", func(t *testing.T) {
		insertImageWithFile(t, app, "delC003", []byte("image c"))
		var out bytes.Buffer
		sum := runDelete(app.db, []string{"zzzzz99", "delC003"}, &out)
		assert.Equal(t, deleteSummary{Hidden: 1, NotFound: 1}, sum)
		s := out.String()
		assert.Contains(t, s, "not found: zzzzz99")
		assert.Contains(t, s, "hidden delC003")

		img, err := dbGetImageByID(app.db, "delC003")
		require.NoError(t, err)
		assert.True(t, img.Hidden, "the good id was still hidden despite the unknown one")
	})
}

// TestRunDeleteHidesDBErrorPerID mirrors TestDeleteDBErrorReturns500: an
// injected dbHideImage failure is a per-id error line, the run
// continues, and the row stays visible.
func TestRunDeleteHidesDBErrorPerID(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImageWithFile(t, app, "delE004", []byte("image e"))
	insertImageWithFile(t, app, "delF005", []byte("image f"))

	orig := dbHideImageFn
	dbHideImageFn = func(db *sqlx.DB, id string) (bool, error) {
		if id == "delE004" {
			return false, fmt.Errorf("injected hide failure")
		}
		return orig(db, id)
	}
	t.Cleanup(func() { dbHideImageFn = orig })

	var out bytes.Buffer
	sum := runDelete(app.db, []string{"delE004", "delF005"}, &out)
	assert.Equal(t, deleteSummary{Hidden: 1, Errors: 1}, sum)
	assert.Contains(t, out.String(), "error: delE004: hide: injected hide failure")

	img, err := dbGetImageByID(app.db, "delE004")
	require.NoError(t, err)
	assert.False(t, img.Hidden, "failed hide leaves the row visible")
	img, err = dbGetImageByID(app.db, "delF005")
	require.NoError(t, err)
	assert.True(t, img.Hidden, "later id still processed after the error")
}

// TestRunDeleteConcurrentLossPinsGuardedUpdateBranch pins the branch the
// up-front img.Hidden check cannot reach: dbHideImageFn returning
// (false, nil) — the guarded UPDATE lost to another writer between the
// lookup and the hide — reports the 410-equivalent already-hidden line
// with the "hidden concurrently" detail, counted as AlreadyHidden (not
// Hidden, not Errors).
func TestRunDeleteConcurrentLossPinsGuardedUpdateBranch(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImageWithFile(t, app, "delH007", []byte("image h"))

	orig := dbHideImageFn
	dbHideImageFn = func(db *sqlx.DB, id string) (bool, error) {
		return false, nil // guarded UPDATE reports zero rows affected
	}
	t.Cleanup(func() { dbHideImageFn = orig })

	var out bytes.Buffer
	sum := runDelete(app.db, []string{"delH007"}, &out)
	assert.Equal(t, deleteSummary{AlreadyHidden: 1}, sum)
	assert.Contains(t, out.String(), "already hidden: delH007 (hidden concurrently)")
}

// ─── Shared semantics with the HTTP endpoint ───────────────────────────────

// TestDeleteSharesHTTPPathSemantics proves the CLI is the same soft
// delete as DELETE /api/images/<id>, in both directions against the
// same DB: a row hidden through HTTP reads "already hidden" to the CLI,
// and a row hidden through the CLI answers 410 to the HTTP endpoint —
// the exact matrix TestDeleteImageHandlerMatrix pins for HTTP-only
// repeats (200 → 410, hidden=1, files retained).
func TestDeleteSharesHTTPPathSemantics(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)

	t.Run("HTTPHiddenRowIsAlreadyHiddenToCLI", func(t *testing.T) {
		ur := uploadOne(t, ts)
		require.Equal(t, http.StatusOK, doDelete(t, ts, testAPIKey, ur.ID).StatusCode)

		var out bytes.Buffer
		sum := runDelete(app.db, []string{ur.ID}, &out)
		assert.Equal(t, deleteSummary{AlreadyHidden: 1}, sum,
			"CLI sees the HTTP-hidden row exactly like the handler's own 410 branch")
		assert.Contains(t, out.String(), "already hidden: "+ur.ID)
	})

	t.Run("CLIHiddenRowIs410ToHTTP", func(t *testing.T) {
		ur := uploadOne(t, ts)
		var out bytes.Buffer
		sum := runDelete(app.db, []string{ur.ID}, &out)
		assert.Equal(t, deleteSummary{Hidden: 1}, sum)

		assert.Equal(t, http.StatusGone, doDelete(t, ts, testAPIKey, ur.ID).StatusCode,
			"HTTP endpoint reports 410 for a CLI-hidden row — same guarded UPDATE, same winner election")

		img, err := dbGetImageByID(app.db, ur.ID)
		require.NoError(t, err)
		assert.True(t, img.Hidden)
		_, err = os.Stat(app.store.OriginalPath(img.SHA256))
		assert.NoError(t, err, "files retained after the CLI hide")
	})
}

// TestDeleteExcludedFromGalleryAndSearch mirrors the DB-level half of
// TestDeletedImageExcludedEverywhere for the CLI path: a runDelete-hidden
// row disappears from the gallery page query and from search (the FTS
// row keeps its tokens; the join-back filters hidden=0).
func TestDeleteExcludedFromGalleryAndSearch(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "delG006", "2026-09-24 01:00:00", "needle pending cli deletion", "")

	rows, err := dbGetGalleryPage(app.db, "", "", 48)
	require.NoError(t, err)
	require.Len(t, rows, 1, "visible before the delete")

	var out bytes.Buffer
	sum := runDelete(app.db, []string{"delG006"}, &out)
	require.Equal(t, deleteSummary{Hidden: 1}, sum)

	rows, err = dbGetGalleryPage(app.db, "", "", 48)
	require.NoError(t, err)
	assert.Empty(t, rows, "gallery excludes the hidden row")

	res := runSearchFor(t, app, "needle", searchCursor{}, 48)
	assert.Empty(t, res.Hits, "search excludes the hidden row (hidden=0 join-back)")
}

// ─── Summary rendering ─────────────────────────────────────────────────────

func TestWriteDeleteSummary(t *testing.T) {
	var b bytes.Buffer
	writeDeleteSummary(&b, deleteSummary{Hidden: 2, AlreadyHidden: 1, NotFound: 1}, "/data/imgsite.db")

	s := b.String()
	assert.Contains(t, s, "delete summary")
	assert.Contains(t, s, "hidden:          2")
	assert.Contains(t, s, "already hidden:  1")
	assert.Contains(t, s, "not found:       1")
	assert.Contains(t, s, "/data/imgsite.db", "DB path reported")
	assert.Contains(t, s, "UPDATE images SET hidden=0 WHERE id='<id>'", "reversibility note with the restore SQL")
	assert.Contains(t, s, "files retained")
}

// ─── CLI surface (subprocess against the real binary) ─────────────────────

// buildImgsiteTestBinary builds the package's binary once (the repo's
// buildDaveTestBinary pattern: one build, many cheap execs — a build
// per subtest would blow subprocess budgets on cold caches). No context
// timeout on the build; the outer `go test` timeout bounds it.
func buildImgsiteTestBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "imgsite-testbin")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("building imgsite test binary: %v\n%s", err, out)
	}
	return bin
}

// runImgsiteCLI execs the test binary and returns combined output plus
// exit code. The config argument is appended last (flags-before-positional
// is the documented CLI shape).
func runImgsiteCLI(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("running %s %v: %v\n%s", bin, args, err, out)
	}
	return string(out), code
}

// seedCLIRow opens the temp DB (initDB runs the embedded migrations —
// the same read-write open path importMain/deleteMain use), inserts a
// row, and closes. The subprocess then operates on identical state.
func seedCLIRow(t *testing.T, dbPath, id, prompt string) {
	t.Helper()
	db, err := initDB(dbPath)
	require.NoError(t, err)
	defer db.Close()
	img := dbImage{
		ID:             id,
		SHA256:         "seed" + id,
		Filename:       id + ".webp",
		MimeType:       "application/octet-stream",
		SizeBytes:      1,
		CreatedAt:      "2026-09-25 00:00:00.000",
		ThumbStatus:    thumbStatusPending,
		OriginalPrompt: prompt,
	}
	require.NoError(t, dbInsertImage(db, &img))
}

// TestDeleteCLIEndToEnd covers the real user surface: flag conventions,
// exit-code discipline (0 all-hidden, 1 per-id misses, 2 usage errors),
// and the summary output.
func TestDeleteCLIEndToEnd(t *testing.T) {
	bin := buildImgsiteTestBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "imgsite.db")
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(
		"[auth]\napi_key = %q\n\n[database]\npath = %q\n", testAPIKey, dbPath)), 0644))

	seedCLIRow(t, dbPath, "cliA001", "cli fixture prompt a")
	seedCLIRow(t, dbPath, "cliB002", "cli fixture prompt b")

	t.Run("DeleteHidesAndExitsZero", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-delete", "cliA001,cliB002", cfgPath)
		require.Equal(t, 0, code, "every id hidden by this run\n%s", out)
		assert.Contains(t, out, `hidden cliA001 — "cli fixture prompt a" (files retained)`)
		assert.Contains(t, out, "delete summary")
		assert.Contains(t, out, "UPDATE images SET hidden=0 WHERE id='<id>'")
	})

	t.Run("SecondRunAlreadyHiddenExitsOne", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-delete", "cliA001", cfgPath)
		require.Equal(t, 1, code, "already-hidden mirrors the HTTP 410\n%s", out)
		assert.Contains(t, out, "already hidden: cliA001")
	})

	t.Run("UnknownIDExitsOneOtherStillHidden", func(t *testing.T) {
		seedCLIRow(t, dbPath, "cliC003", "cli fixture prompt c")
		out, code := runImgsiteCLI(t, bin, "-delete", "zzzzz99,cliC003", cfgPath)
		require.Equal(t, 1, code, "the unknown id fails the run\n%s", out)
		assert.Contains(t, out, "not found: zzzzz99")
		assert.Contains(t, out, "hidden cliC003")
	})

	t.Run("RowStateAfterRuns", func(t *testing.T) {
		db, err := initDB(dbPath)
		require.NoError(t, err)
		defer db.Close()
		for _, id := range []string{"cliA001", "cliB002", "cliC003"} {
			img, err := dbGetImageByID(db, id)
			require.NoError(t, err)
			assert.True(t, img.Hidden, "%s hidden=1 after the CLI runs", id)
		}
	})

	t.Run("EmptyArgumentIsUsageError", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-delete", "", cfgPath)
		require.Equal(t, 2, code, "explicit empty -delete is a usage error, not serve mode\n%s", out)
		assert.Contains(t, out, "-delete needs at least one image id")
		assert.Contains(t, out, "usage:")
	})

	t.Run("ImportAndDeleteMutuallyExclusive", func(t *testing.T) {
		for _, args := range [][]string{
			{"-import", filepath.Join(dir, "nope"), "-delete", "cliA001", cfgPath},
			{"-delete", "cliA001", "-import", filepath.Join(dir, "nope"), cfgPath},
		} {
			out, code := runImgsiteCLI(t, bin, args...)
			require.Equal(t, 2, code, "args %v\n%s", args, out)
			assert.Contains(t, out, "-import and -delete are mutually exclusive")
		}
	})

	t.Run("InvalidIDIsUsageError", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-delete", "short", cfgPath)
		require.Equal(t, 2, code, "malformed ids never reach the DB\n%s", out)
		assert.Contains(t, out, `invalid image id "short"`)
	})

	t.Run("ImportEmptyArgumentIsUsageError", func(t *testing.T) {
		// Same presence-detection trap as -delete: an explicitly empty
		// -import must be a usage error, not a silent fallthrough into
		// serve mode (which would block the subprocess until timeout).
		out, code := runImgsiteCLI(t, bin, "-import", "", cfgPath)
		require.Equal(t, 2, code, "explicit empty -import is a usage error, not serve mode\n%s", out)
		assert.Contains(t, out, "-import needs a directory")
	})
}
