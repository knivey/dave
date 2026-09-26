package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── Argument parsing ──────────────────────────────────────────────────────

func TestParseSafetyIDs(t *testing.T) {
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
			got, err := parseSafetyIDs(tt.in)
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

func TestParseSafetyValue(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{"Safe", "safe", safetySafe, ""},
		{"Unsafe", "unsafe", safetyUnsafe, ""},
		{"UnknownResets", "unknown", safetyUnknown, ""},
		{"Empty", "", "", `invalid safety value ""`},
		{"Banana", "banana", "", `invalid safety value "banana"`},
		{"CaseSensitive", "Safe", "", `invalid safety value "Safe"`},
		{"Padded", " safe", "", `invalid safety value " safe"`},
		{"TrailingNewline", "safe\n", "", `invalid safety value "safe\n"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSafetyValue(tt.in)
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

// ─── runSafety flow (in-process, temp DB) ──────────────────────────────────

// TestRunSafetyFlow drives the full CLI semantics against a temp DB:
// every value settable (unknown = reset), transition reporting from
// the pre-update lookup, same-value re-sets satisfying the run,
// unknown ids failing the run without blocking the rest, and the
// safe-site visibility flip the verdict exists to control.
func TestRunSafetyFlow(t *testing.T) {
	app := newTestApp(t, testConfig())

	insertImageWithFile(t, app, "safA001", []byte("image a"),
		func(img *dbImage) { img.OriginalPrompt = "shrew walkin down main street" })
	insertImageWithFile(t, app, "safB002", []byte("image b"),
		func(img *dbImage) { img.EnhancedPrompt = "enhanced-only prompt about otters" })

	requireSafety := func(t *testing.T, id, want string) {
		t.Helper()
		img, err := dbGetImageByID(app.db, id)
		require.NoError(t, err)
		assert.Equal(t, want, img.Safety, "%s safety after the run", id)
	}

	t.Run("SetsEachValueAndReportsTransitions", func(t *testing.T) {
		var out bytes.Buffer
		sum := runSafety(app.db, []string{"safA001", "safB002"}, safetySafe, &out)
		assert.Equal(t, safetySummary{Set: 2}, sum)
		s := out.String()
		assert.Contains(t, s, `set safety=safe safA001 — "shrew walkin down main street" (was unknown)`)
		assert.Contains(t, s, `set safety=safe safB002 — "enhanced-only prompt about otters" (was unknown)`)
		requireSafety(t, "safA001", safetySafe)
		requireSafety(t, "safB002", safetySafe)

		out.Reset()
		sum = runSafety(app.db, []string{"safA001", "safB002"}, safetyUnsafe, &out)
		assert.Equal(t, safetySummary{Set: 2}, sum)
		assert.Contains(t, out.String(), "set safety=unsafe safA001")
		assert.Contains(t, out.String(), "(was safe)")
		requireSafety(t, "safA001", safetyUnsafe)
		requireSafety(t, "safB002", safetyUnsafe)

		// unknown = reset: the row goes back to no-verdict.
		out.Reset()
		sum = runSafety(app.db, []string{"safA001", "safB002"}, safetyUnknown, &out)
		assert.Equal(t, safetySummary{Set: 2}, sum)
		assert.Contains(t, out.String(), "set safety=unknown safA001")
		assert.Contains(t, out.String(), "(was unsafe)")
		requireSafety(t, "safA001", safetyUnknown)
		requireSafety(t, "safB002", safetyUnknown)
	})

	t.Run("SameValueReSetStillCounts", func(t *testing.T) {
		// Unlike -delete — where a repeat exits 1 mirroring the HTTP
		// 410 — a repeat set to the same value satisfies the run's
		// contract (the row HAS the requested verdict afterwards);
		// there is no HTTP analogue whose error matrix to mirror.
		runSafety(app.db, []string{"safA001"}, safetySafe, io.Discard)

		var out bytes.Buffer
		sum := runSafety(app.db, []string{"safA001"}, safetySafe, &out)
		assert.Equal(t, safetySummary{Set: 1}, sum)
		assert.Contains(t, out.String(), "set safety=safe safA001")
		assert.Contains(t, out.String(), "(was safe)")
		requireSafety(t, "safA001", safetySafe)
	})

	t.Run("UnknownIDReportedOtherStillProcessed", func(t *testing.T) {
		insertImageWithFile(t, app, "safC003", []byte("image c"))
		var out bytes.Buffer
		sum := runSafety(app.db, []string{"zzzzz99", "safC003"}, safetySafe, &out)
		assert.Equal(t, safetySummary{Set: 1, NotFound: 1}, sum)
		s := out.String()
		assert.Contains(t, s, "not found: zzzzz99")
		assert.Contains(t, s, "set safety=safe safC003")

		requireSafety(t, "safC003", safetySafe)
	})

	t.Run("SafeVerdictFlipsSafeSiteVisibility", func(t *testing.T) {
		// The point of the tool: a NULL-network (default-deny) row
		// becomes safe-site visible through safety='safe' alone, and
		// an unknown reset flips it back off.
		sc := safeSiteCtx(safeSiteTestConfig().SafeSite)
		insertImageWithFile(t, app, "safD004", []byte("image d"))

		img, err := dbGetImageByID(app.db, "safD004")
		require.NoError(t, err)
		assert.False(t, siteCanSee(sc, img), "NULL network + unknown safety is default-deny")

		runSafety(app.db, []string{"safD004"}, safetySafe, io.Discard)
		img, err = dbGetImageByID(app.db, "safD004")
		require.NoError(t, err)
		assert.True(t, siteCanSee(sc, img), "safety='safe' verdict makes the row safe-site visible")

		runSafety(app.db, []string{"safD004"}, safetyUnknown, io.Discard)
		img, err = dbGetImageByID(app.db, "safD004")
		require.NoError(t, err)
		assert.False(t, siteCanSee(sc, img), "unknown reset returns the row to default-deny")
	})
}

// TestRunSafetyDBErrorPerID mirrors TestRunDeleteHidesDBErrorPerID: an
// injected dbSetSafety failure is a per-id error line, the run
// continues, and the row keeps its previous verdict.
func TestRunSafetyDBErrorPerID(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImageWithFile(t, app, "safE004", []byte("image e"))
	insertImageWithFile(t, app, "safF005", []byte("image f"))

	orig := dbSetSafetyFn
	dbSetSafetyFn = func(db *sqlx.DB, id, safety string) (bool, error) {
		if id == "safE004" {
			return false, fmt.Errorf("injected set failure")
		}
		return orig(db, id, safety)
	}
	t.Cleanup(func() { dbSetSafetyFn = orig })

	var out bytes.Buffer
	sum := runSafety(app.db, []string{"safE004", "safF005"}, safetySafe, &out)
	assert.Equal(t, safetySummary{Set: 1, Errors: 1}, sum)
	assert.Contains(t, out.String(), "error: safE004: set: injected set failure")

	img, err := dbGetImageByID(app.db, "safE004")
	require.NoError(t, err)
	assert.Equal(t, safetyUnknown, img.Safety, "failed set leaves the verdict untouched")
	img, err = dbGetImageByID(app.db, "safF005")
	require.NoError(t, err)
	assert.Equal(t, safetySafe, img.Safety, "later id still processed after the error")
}

// TestRunSafetyVanishedRowPinsZeroRowsBranch pins the branch the
// up-front lookup cannot reach: dbSetSafetyFn returning (false, nil) —
// the UPDATE matched zero rows because the row vanished between the
// lookup and the write (no hard delete exists in the system, so this
// is a tripwire path) — reports a not-found line with the concurrent
// detail, counted as NotFound (not Set, not Errors).
func TestRunSafetyVanishedRowPinsZeroRowsBranch(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImageWithFile(t, app, "safG006", []byte("image g"))

	orig := dbSetSafetyFn
	dbSetSafetyFn = func(db *sqlx.DB, id, safety string) (bool, error) {
		return false, nil // UPDATE reports zero rows affected
	}
	t.Cleanup(func() { dbSetSafetyFn = orig })

	var out bytes.Buffer
	sum := runSafety(app.db, []string{"safG006"}, safetyUnsafe, &out)
	assert.Equal(t, safetySummary{NotFound: 1}, sum)
	assert.Contains(t, out.String(), "not found: safG006 (row vanished between lookup and update)")
}

// ─── Summary rendering ─────────────────────────────────────────────────────

func TestWriteSafetySummary(t *testing.T) {
	var b bytes.Buffer
	writeSafetySummary(&b, safetySummary{Set: 2, NotFound: 1}, "unsafe", "/data/imgsite.db")

	s := b.String()
	assert.Contains(t, s, "safety summary")
	assert.Contains(t, s, "value:          unsafe")
	assert.Contains(t, s, "set:            2")
	assert.Contains(t, s, "not found:      1")
	assert.Contains(t, s, "errors:         0")
	assert.Contains(t, s, "/data/imgsite.db", "DB path reported")
	assert.Contains(t, s, "no live-update events")
	assert.Contains(t, s, "another value")
}

// ─── CLI surface (subprocess against the real binary) ─────────────────────

// TestSafetyCLIEndToEnd covers the real user surface: the
// value-as-first-positional convention, exit-code discipline (0 every
// id set — including same-value re-sets, 1 per-id misses, 2 usage
// errors), and the summary output.
func TestSafetyCLIEndToEnd(t *testing.T) {
	bin := buildImgsiteTestBinary(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "imgsite.db")
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(
		"[auth]\napi_key = %q\n\n[database]\npath = %q\n", testAPIKey, dbPath)), 0644))

	seedCLIRow(t, dbPath, "cliA001", "cli fixture prompt a")
	seedCLIRow(t, dbPath, "cliB002", "cli fixture prompt b")

	t.Run("SetSafeExitsZero", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "cliA001,cliB002", "safe", cfgPath)
		require.Equal(t, 0, code, "every id set by this run\n%s", out)
		assert.Contains(t, out, `set safety=safe cliA001 — "cli fixture prompt a" (was unknown)`)
		assert.Contains(t, out, "safety summary")
		assert.Contains(t, out, "value:          safe")
		assert.Contains(t, out, "database:")
	})

	t.Run("ReSetSameValueStillExitsZero", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "cliA001", "safe", cfgPath)
		require.Equal(t, 0, code, "a satisfied no-op re-set is not a miss (no HTTP 410 analogue)\n%s", out)
		assert.Contains(t, out, "(was safe)")
	})

	t.Run("UnknownIDExitsOneOtherStillSet", func(t *testing.T) {
		seedCLIRow(t, dbPath, "cliC003", "cli fixture prompt c")
		out, code := runImgsiteCLI(t, bin, "-safety", "zzzzz99,cliC003", "unsafe", cfgPath)
		require.Equal(t, 1, code, "the unknown id fails the run\n%s", out)
		assert.Contains(t, out, "not found: zzzzz99")
		assert.Contains(t, out, "set safety=unsafe cliC003")
	})

	t.Run("ResetToUnknownExitsZero", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "cliB002", "unknown", cfgPath)
		require.Equal(t, 0, code, "unknown reset is a normal set\n%s", out)
		assert.Contains(t, out, "set safety=unknown cliB002")
		assert.Contains(t, out, "(was safe)")
	})

	t.Run("RowStateAfterRuns", func(t *testing.T) {
		db, err := initDB(dbPath)
		require.NoError(t, err)
		defer db.Close()
		for _, id := range []string{"cliA001", "cliB002", "cliC003"} {
			img, err := dbGetImageByID(db, id)
			require.NoError(t, err)
			assert.False(t, img.Hidden, "%s untouched by -safety", id)
		}
		want := map[string]string{"cliA001": safetySafe, "cliB002": safetyUnknown, "cliC003": safetyUnsafe}
		for id, w := range want {
			img, err := dbGetImageByID(db, id)
			require.NoError(t, err)
			assert.Equal(t, w, img.Safety, "%s verdict after the CLI runs", id)
		}
	})

	t.Run("MissingValueIsUsageError", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "cliA001")
		require.Equal(t, 2, code, "a missing verdict value is a usage error, not serve mode\n%s", out)
		assert.Contains(t, out, "-safety needs a verdict value")
		assert.Contains(t, out, "usage:")
	})

	t.Run("EmptyArgumentIsUsageError", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "", "safe", cfgPath)
		require.Equal(t, 2, code, "explicit empty -safety is a usage error, not serve mode\n%s", out)
		assert.Contains(t, out, "-safety needs at least one image id")
		assert.Contains(t, out, "usage:")
	})

	t.Run("EmptyValueIsUsageError", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "cliA001", "", cfgPath)
		require.Equal(t, 2, code, "explicit empty value is a usage error\n%s", out)
		assert.Contains(t, out, `invalid safety value ""`)
	})

	t.Run("InvalidValueIsUsageError", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "cliA001", "banana", cfgPath)
		require.Equal(t, 2, code, "invalid values never reach the DB\n%s", out)
		assert.Contains(t, out, `invalid safety value "banana"`)
	})

	t.Run("ConfigWithoutValueIsUsageError", func(t *testing.T) {
		// The positional-order trap: with the value missing, the
		// config path parses as the verdict value and is rejected —
		// the run never silently targets the wrong config.
		out, code := runImgsiteCLI(t, bin, "-safety", "cliA001", cfgPath)
		require.Equal(t, 2, code, "config-as-value must be rejected\n%s", out)
		assert.Contains(t, out, "invalid safety value")
	})

	t.Run("InvalidIDIsUsageError", func(t *testing.T) {
		out, code := runImgsiteCLI(t, bin, "-safety", "short", "safe", cfgPath)
		require.Equal(t, 2, code, "malformed ids never reach the DB\n%s", out)
		assert.Contains(t, out, `invalid image id "short"`)
	})

	t.Run("ImportAndSafetyMutuallyExclusive", func(t *testing.T) {
		// Flag pairs must precede the positional verdict value (Go's
		// flag package stops parsing at the first non-flag arg), so
		// "both orders" interleaves the flags and leaves the value at
		// the end — the same shape delete_test.go's both-orders cases
		// reduce to once the value is positional.
		for _, args := range [][]string{
			{"-import", filepath.Join(dir, "nope"), "-safety", "cliA001", "safe", cfgPath},
			{"-safety", "cliA001", "-import", filepath.Join(dir, "nope"), "safe", cfgPath},
		} {
			out, code := runImgsiteCLI(t, bin, args...)
			require.Equal(t, 2, code, "args %v\n%s", args, out)
			assert.Contains(t, out, "-import and -safety are mutually exclusive")
		}
	})

	t.Run("DeleteAndSafetyMutuallyExclusive", func(t *testing.T) {
		for _, args := range [][]string{
			{"-delete", "cliA001", "-safety", "cliB002", "safe", cfgPath},
			{"-safety", "cliB002", "-delete", "cliA001", "safe", cfgPath},
		} {
			out, code := runImgsiteCLI(t, bin, args...)
			require.Equal(t, 2, code, "args %v\n%s", args, out)
			assert.Contains(t, out, "-delete and -safety are mutually exclusive")
		}
	})
}
