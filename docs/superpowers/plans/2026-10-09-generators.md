# Generators: One-Shot Agentic IRC Commands — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a stateless one-shot command family ("generators") whose first member `^summary [duration] [focus…]` summarizes recent channel activity from the IRC logs via a reusable, token-capped log-query pipeline, running the full agentic `runTurn` loop on an ephemeral (non-persisted) turn.

**Architecture:** Two new modules — `logquery.go` (irc_logs rows → rendered transcript → keep-newest token budget) and `generators.go` (`chat()` minus persistence: ephemeral turn through the unchanged `runTurn`). Config lives in a new hot-reloadable `config/generators.toml` (`GeneratorConfig` = embedded `AIConfig` + `prompt` + optional `[name.log]` block); generators register as ordinary queued config commands with an optional-args dispatch extension.

**Tech Stack:** Go 1.25 (`github.com/knivey/dave`, package `main`), BurntSushi/toml, GORM + glebarez/sqlite (log DBs), pkoukk/tiktoken-go (offline loader already vendored), testify.

**Spec:** `docs/superpowers/specs/2026-10-09-generators-design.md` — the plan argues from the spec; executors read both.

## Global Constraints

- Module `github.com/knivey/dave`; all root `.go` files are `package main`.
- Tests: table-driven + `t.Run()`, `github.com/stretchr/testify` (`assert`/`require`). Config loading tests use `createTestConfigDir(t, mainTOML, extraFiles map[string]string) string` from `config_test.go`.
- Every `logxi.New()` logger MUST call `SetLevel(logxi.LevelAll)` — package-level loggers do it in an `init()` block.
- Struct fields use TOML snake_case tags; new config files follow the reference-block + commented-example convention (see `config/chats.toml`).
- After every task: `go build ./... && go vet ./... && go fmt ./... && go test ./...` must pass.
- NEVER `git add -f`. Commit after every green test cycle.
- The shutdown path is single-source in `main.go` `shutdown()` — this feature adds no cleanup steps (no new long-lived goroutines).
- The offline tiktoken loader is already initialized in `tokencount.go`'s `init()` — never construct tiktoken encoders any other way; use `resolveEncodingForModel` + `getEncoder`.

## Review Focus

1. **Month-boundary windows** — `^summary` near midnight on the 1st spans two period files; both must be queried and merged in order. Test: `TestFetchChannelLogSpansMonthBoundary` (Task 2).
2. **Budget smaller than the newest line** — truncation must still keep at least one line (never an empty transcript). Test: `applyTokenBudget` "floor" case (Task 1).
3. **Duration-looking focus text** — `"24"`, `"12x"`, `"2w"`, `"0h"` must remain focus text, never error. Test: `TestParseWindowDuration` reject rows (Task 1).
4. **Empty window (no logs / logging disabled)** — must send `no_activity` and make NO LLM call. Test: `TestGeneratorNoActivitySendsNoticeWithoutLLMCall` (Task 5).
5. **Mixed channel casing** — IRC relays channel casing as typed; the query must match both the raw invoking param and its normalized form. Test: `TestFetchChannelLogMatchesChannelCaseVariants` (Task 2).

---

### Task 1: Log-query rendering, token budgeting, duration parsing (pure functions)

**Files:**
- Create: `logquery.go`
- Test: `logquery_test.go`

**Interfaces:**
- Consumes: `ircLog` struct (irclog.go), `resolveEncodingForModel`/`getEncoder` (tokencount.go).
- Produces (Task 2/5 rely on these exact names):
  - `type LogQuerySpec struct { Window time.Duration; Events []string; MaxTokens int }` (toml tags `window`/`events`/`max_tokens`)
  - `func applyLogQueryDefaults(spec *LogQuerySpec)`
  - `func renderLogLine(row ircLog) string`
  - `type transcriptLine struct { text string; row *ircLog }`
  - `func buildTranscriptLines(rows []ircLog) []transcriptLine`
  - `func applyTokenBudget(lines []transcriptLine, budget int, count func(string) int) (kept []transcriptLine, tokens int, dropped int)`
  - `func tokenCounterForModel(model string) func(string) int`
  - `func parseWindowDuration(s string) (time.Duration, bool)`

- [ ] **Step 1: Write the failing tests**

Create `logquery_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func mkRow(cmd, nick, target, msg string, at time.Time) ircLog {
	return ircLog{Network: "testnet", Channel: "#chan", Command: cmd,
		Nick: nick, Target: target, Message: msg, CreatedAt: at}
}

func TestRenderLogLine(t *testing.T) {
	ts := time.Date(2026, 10, 8, 14, 2, 0, 0, time.Local)
	cases := []struct {
		name string
		row  ircLog
		want string
	}{
		{"privmsg", mkRow("PRIVMSG", "alice", "#chan", "hello", ts), "[14:02] <alice> hello"},
		{"action", mkRow("PRIVMSG", "alice", "#chan", "\x01ACTION waves\x01", ts), "[14:02] * alice waves"},
		{"notice", mkRow("NOTICE", "bob", "#chan", "fyi", ts), "[14:02] -bob- fyi"},
		{"topic", mkRow("TOPIC", "carol", "#chan", "new topic", ts), "[14:02] *** carol set topic: new topic"},
		{"kick", mkRow("KICK", "op", "dave2", "flooding", ts), "[14:02] *** op kicked dave2 (flooding)"},
		{"join", mkRow("JOIN", "alice", "#chan", "", ts), "[14:02] *** alice joined"},
		{"part reason", mkRow("PART", "alice", "#chan", "bye", ts), "[14:02] *** alice parted (bye)"},
		{"part bare", mkRow("PART", "alice", "#chan", "", ts), "[14:02] *** alice parted"},
		{"quit reason", mkRow("QUIT", "alice", "", "gone", ts), "[14:02] *** alice quit (gone)"},
		{"nick", mkRow("NICK", "old", "new", "", ts), "[14:02] *** old is now known as new"},
		{"mode", mkRow("MODE", "op", "#chan", "+o bob", ts), "[14:02] *** op set mode +o bob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, renderLogLine(tc.row))
		})
	}
}

func TestBuildTranscriptLinesDaySeparators(t *testing.T) {
	d1 := time.Date(2026, 10, 7, 23, 59, 0, 0, time.Local)
	d2 := time.Date(2026, 10, 8, 0, 1, 0, 0, time.Local)
	rows := []ircLog{
		mkRow("PRIVMSG", "a", "#chan", "one", d1),
		mkRow("PRIVMSG", "b", "#chan", "two", d1.Add(time.Minute)),
		mkRow("PRIVMSG", "c", "#chan", "three", d2),
	}
	lines := buildTranscriptLines(rows)
	var texts []string
	for _, l := range lines {
		texts = append(texts, l.text)
	}
	assert.Equal(t, []string{
		"--- 2026-10-07 ---",
		"[23:59] <a> one",
		"[00:00] <b> two",
		"--- 2026-10-08 ---",
		"[00:01] <c> three",
	}, texts)
	// separator lines carry no row pointer; message lines do
	assert.Nil(t, lines[0].row)
	assert.NotNil(t, lines[1].row)
}

func TestApplyTokenBudget(t *testing.T) {
	mk := func(n int) []transcriptLine {
		var out []transcriptLine
		for i := 0; i < n; i++ {
			out = append(out, transcriptLine{text: string(rune('a' + i)), row: &ircLog{ID: int64(i + 1)}})
		}
		return out
	}
	// count fn: 10 tokens per line
	count := func(string) int { return 10 }

	t.Run("under budget unchanged", func(t *testing.T) {
		kept, tokens, dropped := applyTokenBudget(mk(3), 100, count)
		assert.Len(t, kept, 3)
		assert.Equal(t, 30, tokens)
		assert.Equal(t, 0, dropped)
	})

	t.Run("over budget keeps newest", func(t *testing.T) {
		kept, tokens, dropped := applyTokenBudget(mk(5), 30, count)
		assert.Len(t, kept, 3, "newest 3 of 5 lines fit 30 tokens")
		assert.Equal(t, "c", kept[0].text)
		assert.Equal(t, "e", kept[len(kept)-1].text)
		assert.Equal(t, 30, tokens)
		assert.Equal(t, 2, dropped)
	})

	t.Run("budget smaller than newest line keeps one line (floor)", func(t *testing.T) {
		kept, tokens, dropped := applyTokenBudget(mk(4), 5, count)
		assert.Len(t, kept, 1, "never return an empty transcript")
		assert.Equal(t, "d", kept[0].text)
		assert.Equal(t, 10, tokens)
		assert.Equal(t, 3, dropped)
	})

	t.Run("empty input", func(t *testing.T) {
		kept, tokens, dropped := applyTokenBudget(nil, 100, count)
		assert.Empty(t, kept)
		assert.Equal(t, 0, tokens)
		assert.Equal(t, 0, dropped)
	})
}

func TestParseWindowDuration(t *testing.T) {
	ok := []struct {
		in   string
		want time.Duration
	}{
		{"90s", 90 * time.Second},
		{"45m", 45 * time.Minute},
		{"12h", 12 * time.Hour},
		{"2d", 48 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"2h30m", 150 * time.Minute},
	}
	for _, tc := range ok {
		t.Run("ok "+tc.in, func(t *testing.T) {
			got, valid := parseWindowDuration(tc.in)
			assert.True(t, valid)
			assert.Equal(t, tc.want, got)
		})
	}
	reject := []string{"24", "12x", "2w", "0h", "h2", "", "1.5h", "-3h", "summary"}
	for _, in := range reject {
		t.Run("reject "+in, func(t *testing.T) {
			_, valid := parseWindowDuration(in)
			assert.False(t, valid, "%q must stay focus text, never parse as a window", in)
		})
	}
}

func TestTokenCounterForModel(t *testing.T) {
	cnt := tokenCounterForModel("qwen3-32b") // unknown -> o200k approx
	assert.Positive(t, cnt("hello world"))
	assert.Positive(t, cnt(""))
	fallback := tokenCounterForModel("") // no model at all — still counts
	assert.Equal(t, 1, fallback("abcd")) // chars/4 fallback path may vary; just require sane
	_ = fallback
}

func TestApplyLogQueryDefaults(t *testing.T) {
	spec := &LogQuerySpec{}
	applyLogQueryDefaults(spec)
	assert.Equal(t, 24*time.Hour, spec.Window)
	assert.Equal(t, 60000, spec.MaxTokens)
	assert.Equal(t, []string{"PRIVMSG", "NOTICE", "TOPIC", "KICK"}, spec.Events)

	set := &LogQuerySpec{Window: time.Hour, MaxTokens: 100, Events: []string{"PRIVMSG"}}
	applyLogQueryDefaults(set)
	assert.Equal(t, time.Hour, set.Window, "explicit values untouched")
	assert.Equal(t, 100, set.MaxTokens)
	assert.Equal(t, []string{"PRIVMSG"}, set.Events)
}
```

Note: `TestTokenCounterForModel`'s last two assertions are advisory — the fallback for a nil encoder is `len([]rune(s))/4 + 1` style; keep it loose (`assert.Positive` / `>= 0`) if exact values differ. `TestBuildTranscriptLinesDaySeparators` expects `[00:00] <b> two` — the row is `d1.Add(time.Minute)`; set it explicitly to `23:59+1m = 00:00` same day-boundary handling, i.e. use `time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)` for row two if the minute arithmetic surprises you. The invariant under test is: separator exactly when the formatted date changes.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestRenderLogLine|TestBuildTranscriptLines|TestApplyTokenBudget|TestParseWindowDuration|TestTokenCounterForModel|TestApplyLogQueryDefaults' ./...`
Expected: FAIL — `undefined: renderLogLine` (and the rest).

- [ ] **Step 3: Write `logquery.go`**

```go
package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// LogQuerySpec describes a channel-log retrieval for generator commands.
// It is the reusable "log retrieval -> LLM prompt input" contract: any
// command can carry one via [name.log] in generators.toml.
type LogQuerySpec struct {
	Window    time.Duration `toml:"window"`     // default 24h
	Events    []string      `toml:"events"`     // default PRIVMSG, NOTICE, TOPIC, KICK
	MaxTokens int           `toml:"max_tokens"` // default 60000
}

const (
	defaultLogWindow    = 24 * time.Hour
	defaultLogMaxTokens = 60000
)

var defaultLogEvents = []string{"PRIVMSG", "NOTICE", "TOPIC", "KICK"}

// knownLogEvents is the set of irc commands enqueueFromEvent can log.
var knownLogEvents = map[string]bool{
	"PRIVMSG": true, "NOTICE": true, "JOIN": true, "PART": true,
	"QUIT": true, "KICK": true, "NICK": true, "TOPIC": true, "MODE": true,
}

// applyLogQueryDefaults fills unset fields with the documented defaults.
// Callers: config validation (load time) and fetchChannelLogFrom (defense).
func applyLogQueryDefaults(spec *LogQuerySpec) {
	if spec.Window == 0 {
		spec.Window = defaultLogWindow
	}
	if spec.MaxTokens == 0 {
		spec.MaxTokens = defaultLogMaxTokens
	}
	if len(spec.Events) == 0 {
		spec.Events = defaultLogEvents
	}
}

// renderLogLine renders one irc_logs row as a token-cheap transcript line.
// Timestamps are server-local, matching the writer's periodKey convention.
func renderLogLine(row ircLog) string {
	ts := row.CreatedAt.Format("15:04")
	switch row.Command {
	case "PRIVMSG":
		if strings.HasPrefix(row.Message, "\x01ACTION ") && strings.HasSuffix(row.Message, "\x01") {
			action := strings.TrimSuffix(strings.TrimPrefix(row.Message, "\x01ACTION "), "\x01")
			return fmt.Sprintf("[%s] * %s %s", ts, row.Nick, action)
		}
		return fmt.Sprintf("[%s] <%s> %s", ts, row.Nick, row.Message)
	case "NOTICE":
		return fmt.Sprintf("[%s] -%s- %s", ts, row.Nick, row.Message)
	case "TOPIC":
		return fmt.Sprintf("[%s] *** %s set topic: %s", ts, row.Nick, row.Message)
	case "KICK":
		return fmt.Sprintf("[%s] *** %s kicked %s (%s)", ts, row.Nick, row.Target, row.Message)
	case "JOIN":
		return fmt.Sprintf("[%s] *** %s joined", ts, row.Nick)
	case "PART":
		if row.Message != "" {
			return fmt.Sprintf("[%s] *** %s parted (%s)", ts, row.Nick, row.Message)
		}
		return fmt.Sprintf("[%s] *** %s parted", ts, row.Nick)
	case "QUIT":
		if row.Message != "" {
			return fmt.Sprintf("[%s] *** %s quit (%s)", ts, row.Nick, row.Message)
		}
		return fmt.Sprintf("[%s] *** %s quit", ts, row.Nick)
	case "NICK":
		return fmt.Sprintf("[%s] *** %s is now known as %s", ts, row.Nick, row.Target)
	case "MODE":
		return fmt.Sprintf("[%s] *** %s set mode %s", ts, row.Nick, row.Message)
	}
	return fmt.Sprintf("[%s] *** %s %s %s", ts, row.Nick, row.Command, row.Message)
}

// transcriptLine is one rendered line of the transcript. row is nil for
// day-separator lines (they carry no log row).
type transcriptLine struct {
	text string
	row  *ircLog
}

// buildTranscriptLines renders rows in order, inserting a date separator
// whenever the (server-local) day changes between consecutive rows.
func buildTranscriptLines(rows []ircLog) []transcriptLine {
	var lines []transcriptLine
	var curDay string
	for i := range rows {
		day := rows[i].CreatedAt.Format("2006-01-02")
		if day != curDay {
			lines = append(lines, transcriptLine{text: "--- " + day + " ---"})
			curDay = day
		}
		lines = append(lines, transcriptLine{text: renderLogLine(rows[i]), row: &rows[i]})
	}
	return lines
}

// applyTokenBudget keeps the NEWEST lines whose cumulative token count fits
// budget (keep-newest-and-disclose truncation, spec §Token accounting).
// A transcript is never reduced to zero lines: if even the newest line busts
// the budget it is kept anyway (floor). Returns the kept lines, their token
// total, and the number of dropped head lines.
func applyTokenBudget(lines []transcriptLine, budget int, count func(string) int) ([]transcriptLine, int, int) {
	if len(lines) == 0 {
		return nil, 0, 0
	}
	total := 0
	for _, l := range lines {
		total += count(l.text)
	}
	if total <= budget {
		return lines, total, 0
	}
	acc := 0
	cut := len(lines) - 1 // floor: keep at least the newest line
	busted := false
	for i := len(lines) - 1; i >= 0; i-- {
		c := count(lines[i].text)
		if acc+c > budget {
			cut = i + 1
			busted = true
			break
		}
		acc += c
	}
	if !busted {
		// total > budget but the tail walk never tripped — impossible unless
		// count is non-deterministic; keep the floor.
		acc = count(lines[cut].text)
	}
	kept := lines[cut:]
	return kept, acc, cut
}

// tokenCounterForModel returns a per-line token counter using the offline
// tiktoken loader (encoding resolved from the model name; unknown models get
// o200k_base as a compression approximation). Falls back to runes/4 when no
// encoder can be constructed — budgeting must never hard-fail.
func tokenCounterForModel(model string) func(string) int {
	name, _ := resolveEncodingForModel(model)
	enc, err := getEncoder(name)
	if err != nil {
		return func(s string) int { return len([]rune(s))/4 + 1 }
	}
	return func(s string) int { return len(enc.EncodeOrdinary(s)) }
}

// windowArgRe matches a leading command argument that is entirely one or
// more <digits><unit> groups, e.g. "90m", "12h", "2d", "1d12h". Anything
// else is focus text by design (spec §Executor — no bad_duration error).
var windowArgRe = regexp.MustCompile(`^(?:\d+[smhd])+$`)
var windowUnitRe = regexp.MustCompile(`(\d+)([smhd])`)

// parseWindowDuration parses a duration argument with day support. The
// second return is false when s is not a window at all (caller keeps it as
// focus text) or is zero ("0h").
func parseWindowDuration(s string) (time.Duration, bool) {
	if !windowArgRe.MatchString(s) {
		return 0, false
	}
	var total time.Duration
	for _, m := range windowUnitRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		switch m[2] {
		case "s":
			total += time.Duration(n) * time.Second
		case "m":
			total += time.Duration(n) * time.Minute
		case "h":
			total += time.Duration(n) * time.Hour
		case "d":
			total += time.Duration(n) * 24 * time.Hour
		}
	}
	return total, total > 0
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestRenderLogLine|TestBuildTranscriptLines|TestApplyTokenBudget|TestParseWindowDuration|TestTokenCounterForModel|TestApplyLogQueryDefaults' -v .`
Expected: PASS (all subtests).

- [ ] **Step 5: Full suite + commit**

Run: `go build ./... && go vet ./... && go fmt ./... && go test ./...`

```bash
git add logquery.go logquery_test.go
git commit -m "feat(generators): log-query rendering, token budgeting, duration parsing"
```

---

### Task 2: Log retrieval across period files (`fetchChannelLog`)

**Files:**
- Modify: `logquery.go`
- Test: `logquery_test.go`

**Interfaces:**
- Consumes: Task 1 functions; `ircLog` + `LogWriter.openDB` (irclog.go — reuse for creating test fixtures with the production schema); `config.Logging` via `readConfig`.
- Produces (Task 5 relies on):
  - `type LogWindowResult struct { Lines []string; Tokens int; Truncated bool; DroppedLines, TotalLines int; FirstKept, LastKept time.Time; Files []string }`
  - `var errLogWindowTooLarge` (sentinel error)
  - `const logQueryRowCap = 1000000`
  - `func fetchChannelLog(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error)` — reads `config.Logging` (Dir, Rotation); `channelRaw` and `channelNorm` are matched via `channel IN (?, ?)` (mixed-casing history; spec §Channel matching).
  - `func fetchChannelLogFrom(dir, rotation string, spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error)` — testable core.

- [ ] **Step 1: Write the failing tests**

Append to `logquery_test.go`:

```go
func writeLogRows(t *testing.T, dir, key string, rows []ircLog) {
	t.Helper()
	lw := &LogWriter{cfg: LoggingConfig{Dir: dir}, log: newTestLogger()}
	db, err := lw.openDB(key)
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, db.Create(&rows).Error)
}

func TestLogPeriodKeys(t *testing.T) {
	from := time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)
	to := time.Date(2026, 10, 2, 9, 0, 0, 0, time.Local)
	assert.Equal(t, []string{"2026-09", "2026-10"}, logPeriodKeys("monthly", from, to))
	assert.Equal(t, []string{"2026"}, logPeriodKeys("yearly", from, to))
	same := time.Date(2026, 10, 8, 1, 0, 0, 0, time.Local)
	assert.Equal(t, []string{"2026-10"}, logPeriodKeys("monthly", same, same.Add(time.Hour)))
}

func TestFetchChannelLogFiltersAndOrders(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.Local)
	older := now.Add(-2 * time.Hour) // same period file
	rows := []ircLog{
		mkRow("PRIVMSG", "early", "#chan", "first", older),
		mkRow("JOIN", "noise", "#chan", "", older.Add(time.Minute)),   // filtered by events
		mkRow("PRIVMSG", "late", "#chan", "second", now.Add(-time.Hour)),
		mkRow("PRIVMSG", "other", "#other", "wrong channel", now.Add(-30*time.Minute)),
		mkRow("PRIVMSG", "othernet", "#chan", "wrong network", now.Add(-30*time.Minute)),
	}
	rows[3].Network = "testnet"
	rows[4].Network = "elsewhere"
	writeLogRows(t, dir, "2026-10", rows)

	res, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{}, "testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.Equal(t, []string{"[16:00] <early> first", "[17:00] <late> second"}, res.Lines)
	assert.False(t, res.Truncated)
	assert.Equal(t, 2, res.TotalLines)
	assert.Equal(t, older, res.FirstKept)
	assert.Equal(t, now.Add(-time.Hour), res.LastKept)
}

func TestFetchChannelLogSpansMonthBoundary(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 1, 0, 30, 0, 0, time.Local)
	sept := time.Date(2026, 9, 30, 23, 0, 0, 0, time.Local)
	writeLogRows(t, dir, "2026-09", []ircLog{mkRow("PRIVMSG", "sep", "#chan", "september", sept)})
	writeLogRows(t, dir, "2026-10", []ircLog{mkRow("PRIVMSG", "oct", "#chan", "october", now.Add(-10 * time.Minute))})

	res, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{Window: 2 * time.Hour}, "testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.Len(t, res.Files, 2, "both period files must be queried")
	assert.Equal(t, []string{
		"--- 2026-09-30 ---",
		"[23:00] <sep> september",
		"--- 2026-10-01 ---",
		"[00:20] <oct> october",
	}, res.Lines)
}

func TestFetchChannelLogMatchesChannelCaseVariants(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)
	rows := []ircLog{
		mkRow("PRIVMSG", "a", "#Chan", "typed casing", now.Add(-time.Hour)),
		mkRow("PRIVMSG", "b", "#chan", "join casing", now.Add(-30 * time.Minute)),
	}
	writeLogRows(t, dir, "2026-10", rows)

	// invoking event carried "#Chan"; normalized form is "#chan"
	res, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{}, "testnet", "#Chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.Equal(t, 2, res.TotalLines, "both casings of the channel must match")
}

func TestFetchChannelLogEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)
	// no files at all
	res, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{}, "testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.Equal(t, 0, res.TotalLines)
	assert.Empty(t, res.Lines)

	// file exists but window has nothing
	writeLogRows(t, dir, "2026-10", []ircLog{mkRow("PRIVMSG", "a", "#chan", "old", now.Add(-48 * time.Hour))})
	res, err = fetchChannelLogFrom(dir, "monthly", LogQuerySpec{Window: time.Hour}, "testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.Equal(t, 0, res.TotalLines)
}

func TestFetchChannelLogTruncationMarker(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)
	var rows []ircLog
	for i := 0; i < 50; i++ {
		rows = append(rows, mkRow("PRIVMSG", "u", "#chan", strings.Repeat("x", 100), now.Add(-time.Duration(50-i)*time.Minute)))
	}
	writeLogRows(t, dir, "2026-10", rows)

	res, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{MaxTokens: 200}, "testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.True(t, res.Truncated)
	assert.Positive(t, res.DroppedLines)
	assert.NotEmpty(t, res.Lines)
	assert.Contains(t, res.Lines[0], "earlier lines omitted to fit the 200-token budget", "marker must be the first line")
	assert.Equal(t, 1, len(res.Files))
}
```

(`strings` is already imported by the file after Task 1.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestLogPeriodKeys|TestFetchChannelLog' -v .`
Expected: FAIL — `undefined: logPeriodKeys`, `undefined: fetchChannelLogFrom`.

- [ ] **Step 3: Implement retrieval in `logquery.go`**

Add to `logquery.go` (new imports: `errors`, `os`, `path/filepath`, `sort`, `gorm.io/gorm`, `github.com/glebarez/sqlite`, `logxi`):

```go
// LogWindowResult is the outcome of a channel-log query: the rendered,
// budgeted transcript plus the stats that feed notices and logs.
type LogWindowResult struct {
	Lines        []string  // final transcript lines (marker first when truncated)
	Tokens       int       // token total of the kept lines
	Truncated    bool
	DroppedLines int
	TotalLines   int // rows rendered before budgeting
	FirstKept    time.Time
	LastKept     time.Time
	Files        []string
}

// logQueryRowCap bounds the scan: the token cap bounds the prompt but not
// the rows pulled into memory for an absurd window like 3650d.
const logQueryRowCap = 1000000

var errLogWindowTooLarge = errors.New("log window exceeds row cap")

var logQueryLogger logxi.Logger

func init() {
	logQueryLogger = logxi.New("irclog.query")
	logQueryLogger.SetLevel(logxi.LevelAll)
}

// logPeriodKeys lists the rotation period keys (writer's periodKey format)
// covering [from, to], ascending. Mirrors LogWriter.periodKey — do not
// touch the writer's live handles.
func logPeriodKeys(rotation string, from, to time.Time) []string {
	layout := "2006-01"
	stepMonths := 1
	var start time.Time
	if rotation == "yearly" {
		layout = "2006"
		stepMonths = 12
		start = time.Date(from.Year(), 1, 1, 0, 0, 0, 0, from.Location())
	} else {
		start = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, from.Location())
	}
	var keys []string
	for p := start; !p.After(to); p = p.AddDate(0, stepMonths, 0) {
		keys = append(keys, p.Format(layout))
	}
	return keys
}

// openLogReadHandle opens a period file for reading with its OWN short-lived
// gorm handle (WAL readers coexist with the live LogWriter; the writer's
// lw.mu/handles are never touched). busy_timeout coexists with writer flushes.
func openLogReadHandle(path string) (*gorm.DB, error) {
	dialector := sqlite.Open(path + "?_pragma=busy_timeout(5000)")
	db, err := gorm.Open(dialector, &gorm.Config{Logger: newGormLogger(logQueryLogger)})
	if err != nil {
		return nil, fmt.Errorf("opening log database %s: %w", path, err)
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	return db, nil
}

func closeLogDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.Close()
	}
}

// fetchChannelLog resolves the log directory/rotation from the live config
// and delegates to fetchChannelLogFrom.
func fetchChannelLog(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
	var dir, rotation string
	readConfig(func() {
		dir = config.Logging.Dir
		rotation = config.Logging.Rotation
	})
	return fetchChannelLogFrom(dir, rotation, spec, network, channelRaw, channelNorm, model, now)
}

// fetchChannelLogFrom queries every covering period file, renders the
// transcript, and applies the keep-newest token budget. channelRaw and
// channelNorm are matched together (IRC relays channel casing as typed, so
// one window can contain both casings — the SQL analogue of the codebase's
// normalize-at-lookup rule).
func fetchChannelLogFrom(dir, rotation string, spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
	applyLogQueryDefaults(&spec)
	from := now.Add(-spec.Window)

	var rows []ircLog
	var files []string
	for _, key := range logPeriodKeys(rotation, from, now) {
		path := filepath.Join(dir, key+".db")
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue // no logs that period
			}
			return nil, err
		}
		db, err := openLogReadHandle(path)
		if err != nil {
			return nil, err
		}
		var batch []ircLog
		err = db.
			Where("network = ? AND channel IN ? AND command IN ? AND created_at >= ? AND created_at <= ?",
				network, []string{channelRaw, channelNorm}, spec.Events, from, now).
			Order("created_at asc, id asc").
			Find(&batch).Error
		closeLogDB(db)
		if err != nil {
			return nil, fmt.Errorf("querying %s: %w", path, err)
		}
		rows = append(rows, batch...)
		files = append(files, path)
	}
	if len(rows) > logQueryRowCap {
		return nil, fmt.Errorf("%w: %d rows (cap %d)", errLogWindowTooLarge, len(rows), logQueryRowCap)
	}

	// Defensive order: period files are disjoint and ascending, but sort so
	// degraded inputs can never scramble the transcript.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})

	lines := buildTranscriptLines(rows)
	kept, tokens, dropped := applyTokenBudget(lines, spec.MaxTokens, tokenCounterForModel(model))

	res := &LogWindowResult{
		Tokens:       tokens,
		Truncated:    dropped > 0,
		DroppedLines: dropped,
		TotalLines:   len(rows),
		Files:        files,
	}
	if dropped > 0 {
		marker := fmt.Sprintf("[... %d earlier lines omitted to fit the %d-token budget ...]", dropped, spec.MaxTokens)
		kept = append([]transcriptLine{{text: marker}}, kept...)
	}
	for _, l := range kept {
		res.Lines = append(res.Lines, l.text)
		if l.row != nil {
			if res.FirstKept.IsZero() {
				res.FirstKept = l.row.CreatedAt
			}
			res.LastKept = l.row.CreatedAt
		}
	}
	return res, nil
}
```

Note: `TotalLines` is `len(rows)` (real coverage), while `DroppedLines` counts dropped rendered lines (may include separators) — the marker wording says "lines", which stays truthful.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestLogPeriodKeys|TestFetchChannelLog' -v .`
Expected: PASS. If `TestFetchChannelLogTruncationMarker` flakes on token counts (o200k on 100 `x` chars), it is still deterministic — 100 x-chars encode to a fixed count; adjust nothing unless the assertion is wrong.

- [ ] **Step 5: Full suite + commit**

Run: `go build ./... && go vet ./... && go fmt ./... && go test ./...`

```bash
git add logquery.go logquery_test.go
git commit -m "feat(generators): fetchChannelLog retrieval across rotated period files"
```

---

### Task 3: Generator config plumbing (`GeneratorConfig`, loading, validation)

**Files:**
- Modify: `config.go` — `Commands` struct (line ~227), `loadCommandsDir` (line ~874), `validateCommands` (line ~905)
- Test: `config_test.go` (append)

**Interfaces:**
- Consumes: `LogQuerySpec` + `applyLogQueryDefaults` + `knownLogEvents` (Task 1), `validateAIConfig`/`validateMCPRefsFor`/`validateAndSetAPIUserTemplate` (config.go).
- Produces (Tasks 5/6/7 rely on):
  - `type GeneratorConfig struct { AIConfig; Prompt string \`toml:"prompt"\`; Log *LogQuerySpec \`toml:"log"\` }`
  - `Commands.Generators map[string]GeneratorConfig` (loaded from `generators.toml`; missing file = empty map)
  - Validation: service exists; system template parses+validates; MCP refs valid; log block defaults + `Window > 0` (after defaults), `MaxTokens > 0` (after defaults), `Events ⊆ knownLogEvents`.

- [ ] **Step 1: Write the failing tests**

Append to `config_test.go`:

```go
const testGeneratorsServices = `
[svc]
baseurl = "http://localhost"
`

func TestLoadConfigDirGenerators(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": testGeneratorsServices,
		"generators.toml": `
[summary]
service = "svc"
model = "qwen3"
description = "Summarize activity"
prompt = "Summarize the following channel activity."
system = "You are {{.BotNick}}."
[summary.log]
window = "12h"
events = ["PRIVMSG", "KICK"]
max_tokens = 30000

[fakenews]
service = "svc"
model = "qwen3"
description = "Fake news"
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)

	sum := cfg.Commands.Generators["summary"]
	require.NotNil(t, sum, "summary generator loaded")
	assert.Equal(t, "svc", sum.AIConfig.Service)
	assert.Equal(t, "summary", sum.AIConfig.Name, "Name set to section key")
	assert.Equal(t, "Summarize the following channel activity.", sum.Prompt)
	require.NotNil(t, sum.Log, "log block parsed")
	assert.Equal(t, 12*time.Hour, sum.Log.Window)
	assert.Equal(t, []string{"PRIVMSG", "KICK"}, sum.Log.Events)
	assert.Equal(t, 30000, sum.Log.MaxTokens)
	require.NotNil(t, sum.AIConfig.SystemTmpl, "system template parsed like chats")

	fn := cfg.Commands.Generators["fakenews"]
	require.NotNil(t, fn, "fakenews generator loaded")
	assert.Nil(t, fn.Log, "no log block -> nil")
}

func TestLoadConfigDirGeneratorsMissingFileOK(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": testGeneratorsServices,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.Empty(t, cfg.Commands.Generators)
}

func TestLoadConfigDirGeneratorsLogDefaults(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": testGeneratorsServices,
		"generators.toml": `
[g]
service = "svc"
[g.log]
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	log := cfg.Commands.Generators["g"].Log
	require.NotNil(t, log)
	assert.Equal(t, 24*time.Hour, log.Window)
	assert.Equal(t, 60000, log.MaxTokens)
	assert.Equal(t, []string{"PRIVMSG", "NOTICE", "TOPIC", "KICK"}, log.Events)
}

func TestLoadConfigDirRejectsInvalidGenerators(t *testing.T) {
	cases := []struct {
		name string
		toml string
	}{
		{"bad service", `
[g]
service = "missing"
`},
		{"bad event", `
[g]
service = "svc"
[g.log]
events = ["PRIVMSG", "WALLOPS"]
`},
		{"negative window", `
[g]
service = "svc"
[g.log]
window = "-5h"
`},
		{"negative max tokens", `
[g]
service = "svc"
[g.log]
max_tokens = -1
`},
		{"bad system template", `
[g]
service = "svc"
system = "{{.Unclosed"
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := createTestConfigDir(t, "", map[string]string{
				"services.toml": testGeneratorsServices,
				"generators.toml": tc.toml,
			})
			defer os.RemoveAll(dir)
			_, err := loadConfigDir(dir)
			require.Error(t, err)
		})
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestLoadConfigDirGenerators|TestLoadConfigDirRejectsInvalidGenerators' -v .`
Expected: FAIL — `cfg.Commands.Generators` undefined.

- [ ] **Step 3: Implement config plumbing**

In `config.go`:

1. `Commands` struct (line ~227) gains a field:

```go
type Commands struct {
	Completions map[string]AIConfig
	Chats       map[string]AIConfig
	Tools       map[string]MCPCommandConfig
	Generators  map[string]GeneratorConfig
}
```

2. Below `MCPCommandConfig` (near line ~246) add:

```go
// GeneratorConfig is a one-shot agentic command (spec
// docs/superpowers/specs/2026-10-09-generators-design.md): an AIConfig
// plus a default instruction and an optional channel-log query whose
// transcript is appended to the user message. Commands with Log set get
// the "[duration] [focus...]" argument grammar; without it, plain
// required args. BurntSushi decodes the embedded AIConfig fields at the
// parent level and [name.log] into Log.
type GeneratorConfig struct {
	AIConfig
	Prompt string       `toml:"prompt"`
	Log    *LogQuerySpec `toml:"log"`
}
```

3. In `loadCommandsDir` (line ~874), after the Tools block:

```go
	commands.Generators = make(map[string]GeneratorConfig)
	if err := loadCommandFile(filepath.Join(dir, "generators.toml"), &commands.Generators); err != nil {
		return commands, fmt.Errorf("loading generators: %w", err)
	}
```

4. In `validateCommands` (line ~905), after the Tools loop (before `return nil`):

```go
	for name, cfg := range commands.Generators {
		ai, err := validateAIConfig(cfg.AIConfig, name, "generators", config)
		if err != nil {
			return err
		}
		cfg.AIConfig = ai
		if cfg.System != "" {
			tmpl, err := template.New(name + "_system").Parse(cfg.System)
			if err != nil {
				return fmt.Errorf("commands.generators.%s system prompt template parse error: %w", name, err)
			}
			cfg.SystemTmpl = tmpl
			if err := validateTemplate(cfg.SystemTmpl); err != nil {
				return fmt.Errorf("commands.generators.%s system prompt template validation error: %w", name, err)
			}
		}
		if err := validateMCPRefsFor("generators", name, cfg.MCPs, config); err != nil {
			return err
		}
		if err := validateAndSetAPIUserTemplate(&cfg.AIConfig, name, "generators"); err != nil {
			return err
		}
		if cfg.Log != nil {
			if err := validateLogQuerySpec(cfg.Log, name); err != nil {
				return err
			}
		}
		commands.Generators[name] = cfg
	}
```

5. Add `validateLogQuerySpec` near `validateInjectionRole` (line ~1004):

```go
// validateLogQuerySpec applies defaults then rejects values that can only be
// mistakes. Explicit zero is indistinguishable from unset and takes the
// default (documented); negatives are errors.
func validateLogQuerySpec(spec *LogQuerySpec, name string) error {
	applyLogQueryDefaults(spec)
	if spec.Window < 0 {
		return fmt.Errorf("commands.generators.%s log window must be a positive duration (got %s)", name, spec.Window)
	}
	if spec.MaxTokens < 0 {
		return fmt.Errorf("commands.generators.%s log max_tokens must be positive (got %d)", name, spec.MaxTokens)
	}
	for _, ev := range spec.Events {
		if !knownLogEvents[ev] {
			return fmt.Errorf("commands.generators.%s log events: %q is not a loggable IRC command", name, ev)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestLoadConfigDirGenerators|TestLoadConfigDirRejectsInvalidGenerators' -v .`
Expected: PASS.

- [ ] **Step 5: Full suite + commit**

Run: `go build ./... && go vet ./... && go fmt ./... && go test ./...`

```bash
git add config.go config_test.go
git commit -m "feat(generators): GeneratorConfig + generators.toml loading and validation"
```

---

### Task 4: Ephemeral turn context + runner guards

**Files:**
- Modify: `turncontext.go`, `aiCmds.go` (`storeUsage` ~line 168, `handleResponseIDSave` ~line 642, `getTools`/`toolDefsForConfig` ~lines 433-461, `chatRunner` struct ~line 178)
- Test: `turncontext_test.go` (create), `aiCmds_test.go` (append)

**Interfaces:**
- Consumes: nothing new.
- Produces (Task 5 relies on):
  - `turnContext.ephemeral bool` field; `func newEphemeralTurnContext(initial []ChatMessage) *turnContext` (Add skips `sessionMgr`)
  - `chatRunner.ephemeral bool` field
  - `func mcpToolDefsForConfig(cfg AIConfig) []Tool` (extracted from `toolDefsForConfig`)
  - Behavior: ephemeral runners skip response-id persistence; `storeUsage` writes `TurnUsage` rows with SessionID 0 when `cr.ephemeral`; ephemeral `getTools()` offers no builtin tools.

- [ ] **Step 1: Write the failing tests**

Create `turncontext_test.go`:

```go
package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEphemeralTurnAddSkipsPersistence(t *testing.T) {
	setupTestDB(t)

	var before int64
	require.NoError(t, theDB.Model(&Message{}).Count(&before).Error)

	turn := newEphemeralTurnContext([]ChatMessage{{Role: RoleSystem, Content: "sys"}})
	turn.Add(ChatMessage{Role: RoleUser, Content: "hi"})
	turn.Add(ChatMessage{Role: RoleAssistant, Content: "yo"})

	assert.Len(t, turn.Messages(), 3, "in-memory accumulation works")
	assert.True(t, turn.ephemeral)

	var after int64
	require.NoError(t, theDB.Model(&Message{}).Count(&after).Error)
	assert.Equal(t, before, after, "ephemeral Add must not persist rows")
}

func TestRegularTurnAddPersists(t *testing.T) {
	setupTestDB(t)
	sid := createTestSession(t, "testnet", "#st", "shrew", "cmd", "svc", "m")

	var before int64
	require.NoError(t, theDB.Model(&Message{}).Count(&before).Error)

	turn := newTurnContext(sid, nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "hi"})

	var after int64
	require.NoError(t, theDB.Model(&Message{}).Count(&after).Error)
	assert.Equal(t, before+1, after, "regular turns still persist")
}
```

Append to `aiCmds_test.go`:

```go
func TestEphemeralRunnerGuards(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	t.Run("ephemeral getTools excludes builtins", func(t *testing.T) {
		fixtureServer := &MCPServer{
			Tools: []*mcp.Tool{{Name: "generate_image", Description: "gen"}},
		}
		origServers := mcpServers
		mcpServers = map[string]*MCPServer{"img-mcp": fixtureServer}
		t.Cleanup(func() { mcpServers = origServers })

		cfg := AIConfig{Name: "tabloid", MCPs: []string{"img-mcp"}}
		reg := &chatRunner{cfg: cfg}
		ephe := &chatRunner{cfg: cfg, ephemeral: true}

		names := func(tools []Tool) map[string]bool {
			out := map[string]bool{}
			for _, tl := range tools {
				out[tl.Function.Name] = true
			}
			return out
		}
		assert.True(t, names(reg.getTools())["register_background_job"], "regular runner keeps builtins")
		assert.True(t, names(ephe.getTools())["generate_image"], "ephemeral keeps MCP tools")
		assert.False(t, names(ephe.getTools())["register_background_job"], "ephemeral drops builtins")
	})

	t.Run("ephemeral storeUsage writes session-0 row", func(t *testing.T) {
		cr := &chatRunner{
			cfg:       AIConfig{Name: "summary", Model: "qwen3", Service: "svc"},
			logger:    newTestLogger(),
			ephemeral: true,
			// sessionID stays 0
		}
		var before int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&before).Error)
		cr.storeUsage(&Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, FinishReason: "stop"}, "chat/completions", 100)
		var after int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&before).Error)
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&after).Error)
		assert.Equal(t, before+1, after, "ephemeral usage rows ARE written (session 0 attribution)")
		var row TurnUsage
		require.NoError(t, theDB.Order("id desc").First(&row).Error)
		assert.Equal(t, int64(0), row.SessionID)
		assert.Equal(t, "qwen3", row.Model)
		assert.Equal(t, "svc", row.Service)
	})

	t.Run("non-ephemeral session-0 runner still skips usage rows", func(t *testing.T) {
		cr := &chatRunner{
			cfg:    AIConfig{Name: "x"},
			logger: newTestLogger(),
			// sessionID 0, ephemeral false — legacy defensive skip
		}
		var before int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&before).Error)
		cr.storeUsage(&Usage{PromptTokens: 1}, "chat/completions", 1)
		var after int64
		require.NoError(t, theDB.Model(&TurnUsage{}).Count(&after).Error)
		assert.Equal(t, before, after)
	})

	t.Run("ephemeral handleResponseIDSave does not persist", func(t *testing.T) {
		sid := createTestSession(t, "testnet", "#st", "shrew", "cmd", "svc", "m")
		cr := &chatRunner{cfg: AIConfig{Name: "g"}, logger: newTestLogger(), sessionID: sid, ephemeral: true}
		got := cr.handleResponseIDSave("resp_1", "text", nil, "")
		assert.Equal(t, "resp_1", got, "return value contract preserved")
		var sess Session
		require.NoError(t, theDB.First(&sess, sid).Error)
		assert.Nil(t, sess.ResponseID, "no response id persisted for ephemeral runs")
	})
}
```

Note: check the `Usage` struct field names in `aiCmds.go` (`PromptTokens` etc. — used by `logUsage`) and the `Session.ResponseID` field type (`*string`) in `db.go` before finalizing; adjust the literals if they differ.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestEphemeralTurnAddSkipsPersistence|TestEphemeralRunnerGuards' -v .`
Expected: FAIL — `undefined: newEphemeralTurnContext`, `unknown field "ephemeral"`.

- [ ] **Step 3: Implement**

1. `turncontext.go` — extend and gate:

```go
type turnContext struct {
	sessionID int64
	messages  []ChatMessage
	// ephemeral marks a stateless (generator) turn: Add accumulates in
	// memory only and never touches sessionMgr. Spec §Ephemeral Turn
	// Machinery — this is the single persistence seam runTurn flows through.
	ephemeral bool
}

// newEphemeralTurnContext builds a non-persisted turn seeded with initial
// messages (system + user for generators).
func newEphemeralTurnContext(initial []ChatMessage) *turnContext {
	return &turnContext{messages: initial, ephemeral: true}
}
```

and in `Add`:

```go
func (tc *turnContext) Add(msg ChatMessage) {
	tc.messages = append(tc.messages, msg)
	if tc.ephemeral || sessionMgr == nil {
		return
	}
	if err := sessionMgr.AddMessage(tc.sessionID, msg); err != nil {
		loggerTC.Error("Failed to add message", "session", tc.sessionID, "error", err)
	}
}
```

2. `aiCmds.go` — `chatRunner` struct gains `ephemeral bool` after `convID`:

```go
	sessionID    int64
	convID       string
	// ephemeral marks a generator run: no session row backs this runner.
	// Gates: response-id persistence (handleResponseIDSave), builtin tool
	// offering (getTools), and the session-0 usage-row exception
	// (storeUsage). Everything else in runTurn is untouched.
	ephemeral    bool
```

3. `storeUsage` (line ~170) — change the guard:

```go
	if theDB == nil || usage == nil || (cr.sessionID == 0 && !cr.ephemeral) {
		return
	}
```

4. `handleResponseIDSave` (line ~642) — first statement:

```go
	if cr.ephemeral {
		return currentResponseID
	}
```

5. `getTools`/`toolDefsForConfig` (lines ~433-461) — extract the MCP-only core so `/tokencount` and compaction counting stay byte-identical for chats:

```go
func (cr *chatRunner) getTools() []Tool {
	if cr.ephemeral {
		// Generators never offer the builtin LLM tools:
		// register_background_job delivery is session-bound; ban tools are
		// chat-moderation concerns. MCP tools work normally.
		return mcpToolDefsForConfig(cr.cfg)
	}
	return toolDefsForConfig(cr.cfg)
}

// mcpToolDefsForConfig is the config's live MCP tools minus its hidden set.
// Extracted from toolDefsForConfig so the ephemeral (generator) path can
// take exactly this subset without the builtins.
func mcpToolDefsForConfig(cfg AIConfig) []Tool {
	var hiddenMCPTools []string
	readConfig(func() {
		hiddenMCPTools = cfg.resolveHiddenMCPTools(config.MCPToolSets)
	})
	return getMCPTools(cfg.MCPs, hiddenMCPTools)
}

func toolDefsForConfig(cfg AIConfig) []Tool {
	mcpTools := mcpToolDefsForConfig(cfg)
	if len(mcpTools) > 0 {
		mcpTools = append(mcpTools, getBuiltinToolDefs(cfg, cfg.DisabledBuiltinTools)...)
	}
	return mcpTools
}
```

(Preserve the existing DESIGN NOTE comment above `toolDefsForConfig`; keep `getTools`'s one-line body style.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestEphemeral|TestRegularTurnAddPersists|TestToolDefsForConfig' -v .`
Expected: PASS including the pre-existing `TestToolDefsForConfig` (regression: chat tool assembly unchanged).

- [ ] **Step 5: Full suite + commit**

Run: `go build ./... && go vet ./... && go fmt ./... && go test ./...`

```bash
git add turncontext.go turncontext_test.go aiCmds.go aiCmds_test.go
git commit -m "feat(generators): ephemeral turn context and runner guards"
```

---

### Task 5: Generator notices + executor (`generators.go`)

**Files:**
- Create: `generators.go`
- Modify: `notices.go` (`NoticesConfig` ~line 14, defaults setter)
- Test: `generators_test.go` (create)

**Interfaces:**
- Consumes: Tasks 1-4 products; `newChatRunnerFn` (main.go ~line 164), `buildSystemPromptData` (main.go ~51), `asyncResultRole` (guidance.go ~74), `resolvedUserFromCtx`, `normalizeIRC`/`getCasemapping`, `formatDuration`, `expandNotice`/`getNotices`/`errorMsg`/`warnMsg`, `splitFirstWord` (main.go ~296), `setupTestDB`/`setupNoticesDefaults`/`newStreamTestRunner`/`streamChunk`/`drainOutput` (test helpers).
- Produces (Task 6 relies on):
  - `func generator(network Network, c *girc.Client, e girc.Event, cfg GeneratorConfig, ctx context.Context, output chan<- string, resolvedUser *User, args ...string)`
  - `var fetchChannelLogFn = fetchChannelLog` (package var — test injection point)
  - `GeneratorNotices` in `NoticesConfig.Generators` with fields `NoActivity`, `Truncated`, `WindowTooLarge` (toml `no_activity`, `truncated`, `window_too_large`)

- [ ] **Step 1: Write the failing tests**

Create `generators_test.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func genEvent(channel string) girc.Event {
	return girc.Event{
		Source: &girc.Source{Name: "shrew", Ident: "~s", Host: "example.com"},
		Params: []string{channel},
	}
}

// withGeneratorRunner swaps newChatRunnerFn for one returning a real runner
// against an httptest server, restoring it after the test.
func withGeneratorRunner(t *testing.T, cfg AIConfig, handler http.HandlerFunc) (outputCh chan string) {
	t.Helper()
	outputCh = make(chan string, 64)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	orig := newChatRunnerFn
	newChatRunnerFn = func(network Network, client *girc.Client, c AIConfig, ctx context.Context, out chan<- string) chatRunnerInterface {
		cr := newStreamTestRunner(t, server, c, 0, outputCh)
		cr.ctx = ctx
		cr.outputCh = out
		return cr
	}
	t.Cleanup(func() { newChatRunnerFn = orig })
	return outputCh
}

func drainGenOutput(t *testing.T, ch chan string, n int) []string {
	t.Helper()
	return drainOutput(t, ch, n, 2*time.Second)
}

func TestGeneratorLogCommandBuildsEphemeralTurn(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotPath, gotBody string
	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		buf := new(strings.Builder)
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"all quiet"},"finish_reason":null}]}`)+
			streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
			"data: [DONE]\n\n")
	})

	// Seed a log query result via the injectable fetch fn.
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, n2 time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{
			Lines:      []string{"[11:00] <alice> hi"},
			Tokens:     5, TotalLines: 1,
			FirstKept: now.Add(-time.Hour), LastKept: now.Add(-time.Hour),
			Files: []string{"x.db"},
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	var messagesBefore int64
	require.NoError(t, theDB.Model(&Message{}).Count(&messagesBefore).Error)

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary", Model: "qwen3", Streaming: true,
		StreamTimeout: 5 * time.Second, Timeout: 10 * time.Second, System: "You are {{.BotNick}}."},
		Prompt: "Summarize the following channel activity.",
		Log:    &LogQuerySpec{}}

	ctx := context.Background()
	go func() {
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, ctx, outputCh, &User{ID: 1, Nick: "shrew"}, "12h", "focus on drama")
	}()

	lines := drainGenOutput(t, outputCh, 16)
	joined := strings.Join(lines, "\n")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "exactly one LLM call")
	assert.Contains(t, joined, "all quiet")
	assert.Contains(t, gotPath, "chat/completions")
	assert.Contains(t, gotBody, "focus on drama", "focus text is the instruction")
	assert.Contains(t, gotBody, "[11:00] <alice> hi", "transcript appended to user message")
	assert.Contains(t, gotBody, "Summarize the following channel activity.", "default instruction used when no focus")
	assert.NotContains(t, gotBody, "12h", "duration token must not leak into the prompt")

	var messagesAfter int64
	require.NoError(t, theDB.Model(&Message{}).Count(&messagesAfter).Error)
	assert.Equal(t, messagesBefore, messagesAfter, "ephemeral turn persists nothing")
}

func TestGeneratorNoActivitySendsNoticeWithoutLLMCall(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})

	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1}, "6h")
	}()
	<-done

	lines := drainGenOutput(t, outputCh, 4)
	joined := strings.Join(lines, "\n")
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls), "no LLM call on empty window")
	assert.Contains(t, joined, "No logged activity", "no_activity notice sent")
}

func TestGeneratorTruncationNotice(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{Lines: []string{"[11:00] <a> x"}, Tokens: 59000, Truncated: true,
			DroppedLines: 120, TotalLines: 420}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{MaxTokens: 60000}}
	go func() {
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1})
	}()
	lines := drainGenOutput(t, outputCh, 8)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "420", "truncation notice carries totals")
	assert.Contains(t, joined, "120", "dropped count present")
}

func TestGeneratorWindowTooLargeNotice(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return nil, fmt.Errorf("window too large: %d rows (cap %d): %w", 2000000, logQueryRowCap, errLogWindowTooLarge)
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1}, "3650d")
	}()
	<-done
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls))
	lines := drainGenOutput(t, outputCh, 4)
	assert.Contains(t, strings.Join(lines, "\n"), "row cap", "window_too_large notice sent")
}

func TestGeneratorNonLogArgsPassedThrough(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotBody string
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "fakenews", Model: "qwen3", System: "snarky"}, Prompt: "default topic"}
	go func() {
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1}, "the moon landing")
	}()
	drainGenOutput(t, outputCh, 8)
	assert.Contains(t, gotBody, "the moon landing", "args become the user message verbatim")
}

func TestGeneratorErrorsWrappedFromFetch(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)
	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return nil, errors.New("disk exploded")
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })
	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1})
	}()
	<-done
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls))
	lines := drainGenOutput(t, outputCh, 4)
	assert.Contains(t, strings.Join(lines, "\n"), "disk exploded", "generic fetch errors surface to the user")
}
```

Also append to `notices_test.go`:

```go
func TestGeneratorNoticesDefaults(t *testing.T) {
	nc := &NoticesConfig{}
	setNoticesDefaults(nc)
	assert.NotEmpty(t, nc.Generators.NoActivity)
	assert.Contains(t, nc.Generators.Truncated, "{kept}")
	assert.Contains(t, nc.Generators.Truncated, "{total}")
	assert.Contains(t, nc.Generators.WindowTooLarge, "{cap}")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestGenerator|TestGeneratorNoticesDefaults' -v .`
Expected: FAIL — `undefined: generator`, `undefined: fetchChannelLogFn`, `nc.Generators` undefined.

- [ ] **Step 3: Implement notices then the executor**

1. `notices.go` — add to `NoticesConfig` (after `LLM`):

```go
	Generators GeneratorNotices `toml:"generators"`
```

and the type near the other notice types:

```go
type GeneratorNotices struct {
	NoActivity     string `toml:"no_activity"`
	Truncated      string `toml:"truncated"`
	WindowTooLarge string `toml:"window_too_large"`
}
```

In `setNoticesDefaults` add:

```go
	if n.Generators.NoActivity == "" {
		n.Generators.NoActivity = "No logged activity found in the last {window}."
	}
	if n.Generators.Truncated == "" {
		n.Generators.Truncated = "Log truncated to fit the token budget: kept {kept} of {total} lines ({tokens}/{budget} tokens)."
	}
	if n.Generators.WindowTooLarge == "" {
		n.Generators.WindowTooLarge = "That window is too large ({rows} rows > {cap} cap); narrow the duration."
	}
```

(Check how `setNoticesDefaults` is spelled/nested in `notices.go` and match its receiver/style exactly.)

2. Create `generators.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lrstanley/girc"
)

// fetchChannelLogFn is the injection point for tests (same pattern as
// connectMCPServerImpl).
var fetchChannelLogFn = fetchChannelLog

// generator is the executor for one-shot generator commands (spec
// docs/superpowers/specs/2026-10-09-generators-design.md): chat() minus
// persistence. It renders the system prompt, builds one user message
// (optionally carrying the token-capped log transcript), and runs the
// UNCHANGED runTurn agentic loop on an ephemeral turn — streaming, tools,
// empty-response retry all apply. No session row is created; usage rows are
// attributed with SessionID 0.
func generator(network Network, c *girc.Client, e girc.Event, cfg GeneratorConfig, ctx context.Context, output chan<- string, resolvedUser *User, args ...string) {
	runner := newChatRunnerFn(network, c, cfg.AIConfig, ctx, output).(*chatRunner)
	nick := e.Source.Name
	channelRaw := e.Params[0]
	channel := normalizeIRC(channelRaw, getCasemapping(network.Name))
	if resolvedUser == nil {
		resolvedUser = resolvedUserFromCtx(ctx)
	}
	if resolvedUser == nil {
		var err error
		resolvedUser, err = resolveIRCUser(network, c, e)
		if err != nil {
			runner.logger.Error("failed to resolve user in generator()", "error", err)
		}
		proceed, _ := handleResolveResult(c, e, resolvedUser, err)
		if !proceed {
			return
		}
	}
	if resolvedUser == nil {
		runner.logger.Warn("generator() got nil resolved user, dropping message", "nick", nick)
		return
	}
	if resolvedUser.Flagged {
		runner.logger.Warn("generator() proceeding with flagged user",
			"user_id", resolvedUser.ID, "nick", nick, "reason", resolvedUser.FlaggedReason)
	}
	runner.userID = resolvedUser.ID
	runner.hostmask = e.Source.Name + "!" + e.Source.Ident + "@" + e.Source.Host
	runner.setChannel(channel, nick, resolvedUser.ID)
	runner.ephemeral = true

	userText := ""
	if len(args) > 0 {
		userText = args[0]
	}

	if cfg.Log != nil {
		spec := *cfg.Log
		focus := userText
		if userText != "" {
			first, rest, has := splitFirstWord(userText)
			if d, ok := parseWindowDuration(first); ok {
				spec.Window = d
				focus = ""
				if has {
					focus = rest
				}
			}
		}
		now := time.Now()
		lw, err := fetchChannelLogFn(spec, network.Name, channelRaw, channel, cfg.Model, now)
		if err != nil {
			if errors.Is(err, errLogWindowTooLarge) {
				runner.sendError(expandNotice(getNotices().Generators.WindowTooLarge, map[string]string{
					"rows": fmt.Sprintf("%d", logQueryRowCap), "cap": fmt.Sprintf("%d", logQueryRowCap),
				}))
				return
			}
			runner.sendError(err.Error())
			runner.logger.Error("generator log query failed", "error", err)
			return
		}
		if lw.TotalLines == 0 {
			runner.sendError(expandNotice(getNotices().Generators.NoActivity, map[string]string{
				"window": formatDuration(spec.Window),
			}))
			return
		}
		if lw.Truncated {
			runner.sendWarning(expandNotice(getNotices().Generators.Truncated, map[string]string{
				"kept":    fmt.Sprintf("%d", lw.TotalLines-lw.DroppedLines),
				"total":   fmt.Sprintf("%d", lw.TotalLines),
				"tokens":  fmt.Sprintf("%d", lw.Tokens),
				"budget":  fmt.Sprintf("%d", spec.MaxTokens),
			}))
		}
		runner.logger.Info("generator log query",
			"window", spec.Window.String(), "files", len(lw.Files),
			"lines", lw.TotalLines, "dropped", lw.DroppedLines,
			"tokens", lw.Tokens, "budget", spec.MaxTokens,
			"truncated", lw.Truncated)

		instruction := focus
		if instruction == "" {
			instruction = cfg.Prompt
		}
		if instruction == "" {
			instruction = "Summarize the following channel activity."
		}
		var b strings.Builder
		b.WriteString(instruction)
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "Channel activity for %s on %s, last %s (%d lines, %d tokens):\n",
			channel, network.Name, formatDuration(spec.Window), lw.TotalLines, lw.Tokens)
		b.WriteString(strings.Join(lw.Lines, "\n"))
		userText = b.String()
	} else if userText == "" {
		userText = cfg.Prompt // defensive: dispatch requires args for non-log commands
	}

	systemContent := cfg.System
	if cfg.SystemTmpl != nil {
		data := buildSystemPromptData(network, c, channel, nick)
		data.AsyncResultRole = asyncResultRole(cfg.AIConfig)
		var buf strings.Builder
		if err := cfg.SystemTmpl.Execute(&buf, data); err != nil {
			runner.logger.Error("system prompt template execution error:", err)
		} else {
			systemContent = buf.String()
		}
	}

	turn := newEphemeralTurnContext([]ChatMessage{
		{Role: RoleSystem, Content: systemContent},
		{Role: RoleUser, Content: userText},
	})
	runner.runTurn(turn)
}
```

Note: `buildSystemPromptData(network, c, channel, nick)` takes a `*girc.Client` — `c` may be nil in tests; check its body (main.go ~51) — if it dereferences the client (ChanNicks lookup), guard like `chat()` tolerates; if tests crash, pass a real girc client from the test (`girc.New(girc.Config{...})` without connecting — see `newMockBot` in `jobManager_test.go`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestGenerator' -v .`
Expected: PASS (all seven tests).

- [ ] **Step 5: Full suite + commit**

Run: `go build ./... && go vet ./... && go fmt ./... && go test ./...`

```bash
git add generators.go generators_test.go notices.go notices_test.go
git commit -m "feat(generators): ephemeral executor with log transcript injection and notices"
```

---

### Task 6: Registration + optional-args dispatch

**Files:**
- Modify: `main.go` (`registerCommandsLocked` ~line 404, globals ~line 352), `irc_handlers.go` (dispatch ~line 512, `getServiceForConfigCmd` ~line 634)
- Test: `main_test.go` or `irc_handlers_test.go` (append — follow wherever `registerCommands` tests live)

**Interfaces:**
- Consumes: `generator` (Task 5), `Commands.Generators` (Task 3).
- Produces:
  - `var configCmdOptionalArgs map[string]bool` global (rebuilt by `registerCommandsLocked`)
  - Dispatch: log-fed generator triggers match with OR without args; non-log generators require args.
  - `getServiceForConfigCmd` returns generator services.

- [ ] **Step 1: Write the failing tests**

Append (to `main_test.go`; adapt to its existing helpers):

```go
func TestRegisterGenerators(t *testing.T) {
	cmds := Commands{}
	cmds.Generators = map[string]GeneratorConfig{
		"summary": {AIConfig: AIConfig{Name: "summary", Service: "svc"}, Log: &LogQuerySpec{}},
		"fakenews": {AIConfig: AIConfig{Name: "fakenews", Service: "svc", Aliases: []string{"fn"}}},
	}
	require.NoError(t, registerCommands(cmds))
	t.Cleanup(func() {
		// reset maps to avoid leaking into other tests
		require.NoError(t, registerCommands(Commands{
			Completions: map[string]AIConfig{}, Chats: map[string]AIConfig{}, Tools: map[string]MCPCommandConfig{}, Generators: map[string]GeneratorConfig{},
		}))
	})

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()
	assert.NotNil(t, configCmds["summary"], "summary registered")
	assert.NotNil(t, configCmds["fakenews"])
	assert.NotNil(t, configCmds["fn"], "alias registered")
	assert.Equal(t, "fakenews", configCmdNames["fn"])
	assert.True(t, configCmdOptionalArgs["summary"], "log-fed generator takes optional args")
	assert.False(t, configCmdOptionalArgs["fakenews"])
	assert.False(t, configCmdTakesArgs["summary"], "optional-args commands are NOT takesArgs")
	assert.True(t, configCmdTakesArgs["fakenews"], "non-log generator requires args")
	assert.False(t, chatCmds["summary"], "generators never join chatCmds (no context to clear)")
}

func TestRegisterGeneratorsConflictDetected(t *testing.T) {
	cmds := Commands{
		Chats: map[string]AIConfig{"dupe": {Name: "dupe", Service: "svc"}},
		Generators: map[string]GeneratorConfig{"dupe": {AIConfig: AIConfig{Name: "dupe", Service: "svc"}}},
	}
	err := registerCommands(cmds)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts with")
}

func TestDispatchOptionalArgsGenerator(t *testing.T) {
	// Wire a generator into the live maps and drive the match predicate the
	// way handleTrigger computes it.
	cmds := Commands{Generators: map[string]GeneratorConfig{
		"summary": {AIConfig: AIConfig{Name: "summary", Service: "svc"}, Log: &LogQuerySpec{}},
	}}
	require.NoError(t, registerCommands(cmds))
	t.Cleanup(func() {
		require.NoError(t, registerCommands(Commands{
			Completions: map[string]AIConfig{}, Chats: map[string]AIConfig{}, Tools: map[string]MCPCommandConfig{}, Generators: map[string]GeneratorConfig{},
		}))
	})

	commandsMutex.RLock()
	_, ok := configCmds["summary"]
	optional := configCmdOptionalArgs["summary"]
	takesArgs := configCmdTakesArgs["summary"]
	commandsMutex.RUnlock()
	require.True(t, ok)

	// The predicate handleTrigger applies (irc_handlers.go):
	match := func(hasArgs bool) bool {
		return takesArgs == hasArgs || (optional && hasArgs)
	}
	assert.True(t, match(false), "bare ^summary dispatches")
	assert.True(t, match(true), "^summary 6h dispatches")
}
```

And in `irc_handlers_test.go` (or wherever handler tests live — search for existing `getServiceForConfigCmd` or `handleTrigger` tests first and mirror their setup):

```go
func TestGetServiceForConfigCmdGenerators(t *testing.T) {
	commandsMutex.Lock()
	origCmds := configCmds
	origNames := configCmdNames
	configCmds = map[string]CmdFunc{"summary": nil}
	configCmdNames = map[string]string{"summary": "summary"}
	commandsMutex.Unlock()
	defer func() {
		commandsMutex.Lock()
		configCmds = origCmds
		configCmdNames = origNames
		commandsMutex.Unlock()
	}()

	readConfig(func() {
		config.Commands.Generators = map[string]GeneratorConfig{
			"summary": {AIConfig: AIConfig{Name: "summary", Service: "local-llm"}},
		}
	})
	t.Cleanup(func() {
		readConfig(func() { config.Commands.Generators = map[string]GeneratorConfig{} })
	})

	assert.Equal(t, "local-llm", getServiceForConfigCmd("summary"))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestRegisterGenerators|TestDispatchOptionalArgsGenerator|TestGetServiceForConfigCmdGenerators' -v .`
Expected: FAIL — `undefined: configCmdOptionalArgs`.

- [ ] **Step 3: Implement registration and dispatch**

1. `main.go` globals (near line 352-356):

```go
var configCmds map[string]CmdFunc
var configCmdNames map[string]string
var rateExemptCmds map[string]bool
var chatCmds map[string]bool
var configCmdTakesArgs map[string]bool
// configCmdOptionalArgs marks triggers that match both bare and with-args
// invocations (log-fed generators: "^summary" and "^summary 6h focus").
var configCmdOptionalArgs map[string]bool
```

2. In `registerCommandsLocked` (line ~405) add to the reset block:

```go
	newTakesArgs := make(map[string]bool)
	newOptionalArgs := make(map[string]bool)
```

3. After the Tools loop (line ~487), before the map assignments:

```go
	for name, c := range cmds.Generators {
		logger.Debug("added Generators command", c)
		if err := addTrigger(name, name, "generators"); err != nil {
			return err
		}
		for _, alias := range c.Aliases {
			if err := addTrigger(alias, name, "generators"); err != nil {
				return err
			}
		}
		gc := c
		handler := func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			generator(network, client, e, gc, ctx, output, resolvedUserFromCtx(ctx), args...)
		}
		register := func(trigger string) {
			newConfigCmds[trigger] = handler
			newConfigCmdNames[trigger] = name
			if gc.Log != nil {
				newOptionalArgs[trigger] = true // bare AND with-args both dispatch
			} else {
				newTakesArgs[trigger] = true
			}
		}
		register(name)
		for _, alias := range c.Aliases {
			register(alias)
		}
	}
```

4. At the assignment tail (line ~489-493):

```go
	configCmds = newConfigCmds
	configCmdNames = newConfigCmdNames
	rateExemptCmds = newExemptCmds
	chatCmds = newChatCmds
	configCmdTakesArgs = newTakesArgs
	configCmdOptionalArgs = newOptionalArgs
	return nil
```

5. `irc_handlers.go` dispatch (line ~513-516) — extend the predicate:

```go
		triggerWord, rest, hasArgs := splitFirstWord(stripped)
		if cmd, ok := configCmds[triggerWord]; ok {
			takesArgs := configCmdTakesArgs[triggerWord]
			// Optional-args commands (log-fed generators) dispatch bare
			// (takesArgs=false == hasArgs=false) and with args via the
			// second clause.
			if takesArgs == hasArgs || (configCmdOptionalArgs[triggerWord] && hasArgs) {
```

(body of the `if` unchanged — args stay `[]string{rest}`).

6. `getServiceForConfigCmd` (irc_handlers.go line ~634) — add the Generators branch before the closing of `readConfig`:

```go
		if c, ok := config.Commands.Generators[canonical]; ok {
			svc = c.AIConfig.Service
			return
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestRegisterGenerators|TestDispatchOptionalArgsGenerator|TestGetServiceForConfigCmdGenerators' -v .`
Expected: PASS.

- [ ] **Step 5: Full suite + commit**

Run: `go build ./... && go vet ./... && go fmt ./... && go test ./...`

```bash
git add main.go irc_handlers.go main_test.go irc_handlers_test.go
git commit -m "feat(generators): register generator commands with optional-args dispatch"
```

---

### Task 7: Help listings, shipped config files, AGENTS.md

**Files:**
- Modify: `help.go` (`buildHelpText` ~line 20, `buildPastebinHelpText` ~line 162), `config/generators.toml` (create), `config/notices.toml` (append section), `AGENTS.md` (Architecture bullet)
- Test: `help_test.go` (append)

**Interfaces:**
- Consumes: `Commands.Generators` (Task 3), notice defaults (Task 5).
- Produces: help text lists a `Generators:` group (both plain and pastebin help); `config/generators.toml` follows the config documentation convention; AGENTS.md documents the subsystem.

- [ ] **Step 1: Write the failing test**

Append to `help_test.go` (mirror the existing help tests' config-seeding style):

```go
func TestBuildHelpTextListsGenerators(t *testing.T) {
	readConfig(func() {
		config.Commands.Generators = map[string]GeneratorConfig{
			"summary": {AIConfig: AIConfig{Name: "summary", Description: "Summarize recent channel activity"}},
		}
	})
	t.Cleanup(func() {
		readConfig(func() { config.Commands.Generators = map[string]GeneratorConfig{} })
	})

	net := Network{Name: "testnet"}
	text := buildHelpText("dave", "!", net)
	assert.Contains(t, text, "Generators:")
	assert.Contains(t, text, "summary", "generator trigger listed")
	assert.Contains(t, text, "Summarize recent channel activity")

	paste := buildPastebinHelpText("dave", "!", net)
	assert.Contains(t, paste, "summary")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run 'TestBuildHelpTextListsGenerators' -v .`
Expected: FAIL — no `Generators:` section.

- [ ] **Step 3: Implement**

1. `help.go` — in `buildHelpText`, after the Chats group (~line 100), add (mirroring the surrounding style exactly):

```go
	var generators map[string]GeneratorConfig
	readConfig(func() {
		generators = make(map[string]GeneratorConfig, len(config.Commands.Generators))
		for k, v := range config.Commands.Generators {
			generators[k] = v
		}
	})
	filteredGenerators := make(map[string]GeneratorConfig)
	for k, v := range generators {
		if !isNetworkCommandDisabled(network, k) {
			filteredGenerators[k] = v
		}
	}
	if len(filteredGenerators) > 0 {
		lines = append(lines, "\x02Generators:\x02")
		for _, l := range formatTable(sortedGeneratorEntries(trigger, filteredGenerators)) {
			lines = append(lines, "  "+l)
		}
	}
```

and the helper near `sortedAIConfigEntries` (~line 555):

```go
// sortedGeneratorEntries mirrors sortedAIConfigEntries for GeneratorConfig
// (trigger words sorted, canonical first).
func sortedGeneratorEntries(trigger string, m map[string]GeneratorConfig) []helpEntry {
	entries := make([]helpEntry, 0, len(m))
	for _, c := range m {
		entries = append(entries, buildAIConfigEntry(trigger, c.AIConfig))
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].cmds[0] < entries[j].cmds[0]
	})
	return entries
}
```

(Match `sortedAIConfigEntries`' actual sort/copy style — read it first and mirror; if it sorts by more keys, copy that.)

2. `buildPastebinHelpText` — mirror the same addition using the pastebin entry style (`sortedPastebinEntries` equivalent: a `sortedPastebinGeneratorEntries` that builds entries the same way `sortedPastebinEntries` does for chats, with `cmds: []string{trigger + c.Name}` plus aliases).

3. Create `config/generators.toml` following the config documentation convention (reference block + commented example + live sections). Reference block must list: every AIConfig option that applies (service, model, aliases, description, system, streaming, rendermarkdown, maxtokens, maxcompletiontokens, temperature, topp, stop, presencepenalty, frequencypenalty, timeout, streamtimeout, load_notice, toolverbose, mcps, hidden_mcp_tools, hidden_mcp_tool_sets, extra_body, chat_template_kwargs, api_user) plus the generator-only options:

```toml
# Generators: one-shot, stateless agentic commands. The prompt is built from
# the system template + one user message (your args, plus the channel-log
# transcript when [name.log] is present), then run through the full agentic
# tool loop once. Nothing is persisted: no session, no messages, no compaction.
#
# Generator-only options (per [section]):
#   prompt               (string)      Default instruction when the caller gives no focus text.
#                                       Fallback chain: focus text > prompt > built-in default.
#
# Log-fed commands add a sub-table (argument grammar becomes "^name [duration] [focus...]",
# duration like 90m / 12h / 2d / 1d12h; anything else is focus text):
#   [section.log]
#   window               (duration, default: "24h")     How far back to read channel logs.
#   events               (string array, default: ["PRIVMSG", "NOTICE", "TOPIC", "KICK"])
#                                       Loggable events: PRIVMSG, NOTICE, JOIN, PART, QUIT, KICK, NICK, TOPIC, MODE.
#   max_tokens           (int, default: 60000)          Token budget for the transcript; over budget the
#                                       NEWEST lines are kept and a truncation marker is prepended.
#
# Not useful for generators (stateless): previous_response_id, detectimages,
# disabled_builtin_tools (builtin LLM tools are never offered to generators).
# toolverbose = false silences the 🔧 tool-call notices — recommended for
# output-sensitive commands (tabloid, fakenews).
#
# Requires [logging] enabled in config.toml — log-fed commands read data/logs/*.db.
#
# Example with all generator-only options:
# [example]
# service = "local"
# model = "qwen3-32b"
# streaming = true
# rendermarkdown = true
# toolverbose = false
# mcps = ["img-mcp"]
# hidden_mcp_tools = ["generate_image_async", "enhance_and_generate_async", "wait_for_job", "job_status", "list_jobs", "cancel_job"]
# prompt = "Write a satirical article about the given topic."
# system = "You are {{.BotNick}}, a tabloid hack for {{.Channel}} on {{.Network}}."
# [example.log]
# window = "24h"
# events = ["PRIVMSG", "NOTICE", "TOPIC", "KICK"]
# max_tokens = 60000

[summary]
description = "Summarize recent channel activity"
service = "local"
model = "qwen3-32b"
streaming = true
rendermarkdown = true
prompt = "Summarize the following channel activity."
system = """\
You are {{.BotNick}}, summarizing recent activity in {{.Channel}} on {{.Network}}.
Cover the main topics, notable events, and who participated. Be concise but
specific. If the transcript begins with a truncation marker, say that coverage
starts mid-window.\
"""
[summary.log]
window = "24h"
events = ["PRIVMSG", "NOTICE", "TOPIC", "KICK"]
max_tokens = 60000
```

(Adjust `service`/`model` to whatever the checked-in `config/` uses for other AI commands; the shipped file must load against `config/services.toml` or the bot dies at startup — check before committing.)

4. Append to `config/notices.toml` (match the file's existing section style):

```toml
[generators]
# Generator command notices (summary/tabloid/fakenews...). Hot-reloadable.
# no_activity = "No logged activity found in the last {window}."
# truncated = "Log truncated to fit the token budget: kept {kept} of {total} lines ({tokens}/{budget} tokens)."
# window_too_large = "That window is too large ({rows} rows > {cap} cap); narrow the duration."
```

5. AGENTS.md — add one Architecture bullet after the notices.go bullet (concise, facts only):

```markdown
- **Generators** (`generators.go` + `logquery.go`, Oct 2026; spec `docs/superpowers/specs/2026-10-09-generators-design.md`): one-shot stateless agentic commands (`^summary [duration] [focus…]`, later tabloid/fakenews) configured in `config/generators.toml` (`GeneratorConfig` = embedded AIConfig + `prompt` + optional `[name.log]`). `logquery.go` is the reusable retrieval pipeline: `irc_logs` rows from the rotated period files (own short-lived WAL-reader handles, never the writer's) → rendered transcript (`buildTranscriptLines`, day separators) → keep-newest token budget (`applyTokenBudget`, offline tiktoken, floor keeps ≥1 line, truncation marker prepended) with channel matched as `channel IN (raw, normalized)` (lookup-time casemapping, SQL analogue of the config-key rule). The executor runs the UNCHANGED `runTurn` agentic loop on an ephemeral turn (`turnContext.ephemeral` gates `sessionMgr` writes — the single persistence seam; `chatRunner.ephemeral` gates response-id persistence, builtin tool offering, and lets `storeUsage` write SessionID-0 usage rows; apiLogger already no-ops at session 0). Log-fed triggers are optional-args (`configCmdOptionalArgs`); non-log generators require args; both queue normally (stop/rate/bans free). Async MCP tools are usable via `wait_for_job` but example configs hide them; `register_background_job` and ban tools are never offered (session-bound / chat concerns). Guarded by `TestRenderLogLine`/`TestApplyTokenBudget`/`TestParseWindowDuration`/`TestFetchChannelLog*`/`TestEphemeralTurnAddSkipsPersistence`/`TestEphemeralRunnerGuards`/`TestGeneratorLogCommandBuildsEphemeralTurn`/`TestGeneratorNoActivitySendsNoticeWithoutLLMCall`/`TestRegisterGenerators`/`TestDispatchOptionalArgsGenerator`.
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestBuildHelpTextListsGenerators' -v . && go test ./...`
Expected: PASS. Also verify the shipped config loads: `go run . config` is NOT needed — instead run the config loading path via the existing config tests, and eyeball `config/generators.toml` against `config/services.toml` service names.

- [ ] **Step 5: Full suite + fmt + vet + commit**

Run: `go build ./... && go vet ./... && go fmt ./... && go test ./...`

```bash
git add help.go help_test.go config/generators.toml config/notices.toml AGENTS.md docs/superpowers/specs/2026-10-09-generators-design.md
git commit -m "feat(generators): help listings, shipped config, docs"
```

---

## Self-Review (completed during planning)

1. **Spec coverage**: log pipeline (Tasks 1-2), config surface + validation (Task 3), ephemeral machinery + all three runner guards (Task 4), executor + notices + no-activity/truncation/row-cap behavior (Task 5), registration/dispatch/queue-service (Task 6), help/config-docs/AGENTS.md (Task 7). `toolverbose` needs no code (existing knob, documented in Task 7's reference block). TurnUsage SessionID-0 rows covered in Task 4. Spec's "no api-log files" needs no code (apiLogger no-ops at 0 — noted in Task 4 notes).
2. **Placeholder scan**: none — every step carries real code.
3. **Type consistency**: `LogQuerySpec`/`LogWindowResult`/`fetchChannelLog` signatures identical across Tasks 1→2→5; `GeneratorConfig` fields used in 3/5/6/7 match; `configCmdOptionalArgs` named consistently in 6.
4. **Review Focus**: all five lines have tests pinned (Tasks 1, 2, 5).
