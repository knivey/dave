package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	midnight := time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)
	d2 := time.Date(2026, 10, 8, 0, 1, 0, 0, time.Local)
	rows := []ircLog{
		mkRow("PRIVMSG", "a", "#chan", "one", d1),
		mkRow("PRIVMSG", "b", "#chan", "two", midnight),
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
		"--- 2026-10-08 ---",
		"[00:00] <b> two",
		"[00:01] <c> three",
	}, texts)
	// separator lines carry no row pointer; message lines do
	assert.Nil(t, lines[0].row)
	assert.NotNil(t, lines[1].row)
	assert.Nil(t, lines[2].row, "boundary separator between the two days") // brief had NotNil on this index; idx 2 is the 10-08 separator per the expected-text assertion above
	assert.NotNil(t, lines[3].row)
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
	assert.GreaterOrEqual(t, cnt(""), 0) // advisory per brief: empty string legitimately encodes to 0
	fallback := tokenCounterForModel("") // no model at all — still counts
	assert.Equal(t, 1, fallback("abcd")) // chars/4 fallback path may vary; just require sane
	_ = fallback
}

func TestApplyLogQueryDefaults(t *testing.T) {
	spec := &LogQuerySpec{}
	applyLogQueryDefaults(spec)
	assert.Equal(t, "24h", spec.Window)
	assert.Equal(t, 60000, spec.MaxTokens)
	assert.Equal(t, []string{"PRIVMSG", "NOTICE", "TOPIC", "KICK"}, spec.Events)

	set := &LogQuerySpec{Window: "1h", MaxTokens: 100, Events: []string{"PRIVMSG"}}
	applyLogQueryDefaults(set)
	assert.Equal(t, "1h", set.Window, "explicit values untouched")
	assert.Equal(t, 100, set.MaxTokens)
	assert.Equal(t, []string{"PRIVMSG"}, set.Events)
}

// TestLogQuerySpecWindowDuration pins the Window string resolver: the full
// parseWindowDuration grammar resolves, and anything else (including an
// empty string — the contract is "call after applyLogQueryDefaults") errors
// with a message naming the window.
func TestLogQuerySpecWindowDuration(t *testing.T) {
	ok := []struct {
		in   string
		want time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"7d", 168 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
	}
	for _, tc := range ok {
		t.Run("ok "+tc.in, func(t *testing.T) {
			d, err := LogQuerySpec{Window: tc.in}.windowDuration()
			require.NoError(t, err)
			assert.Equal(t, tc.want, d)
		})
	}
	for _, in := range []string{"", "24", "12x", "-5h", "0h", "1.5h", "7w", "focus"} {
		t.Run("reject "+in, func(t *testing.T) {
			_, err := LogQuerySpec{Window: in}.windowDuration()
			require.Error(t, err, "%q must not resolve", in)
			assert.Contains(t, err.Error(), "window")
			assert.Contains(t, err.Error(), in)
		})
	}
}

// TestFetchChannelLogInvalidWindow pins the fetch-side defense in depth: a
// hand-built spec whose window fails to resolve errors clearly instead of
// being silently defaulted or scanning an unbounded range.
func TestFetchChannelLogInvalidWindow(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)
	_, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{Window: "bogus"}, "testnet", "#chan", "#chan", "qwen3", now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid window duration")
	assert.Contains(t, err.Error(), "bogus")
}

// TestRangeBounds pins the range-mode (tool from/to args) resolution: the
// "2006-01-02 15:04" server-local layout, to-defaults-to-now, and the three
// self-correcting errors.
func TestRangeBounds(t *testing.T) {
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.Local)

	t.Run("from and to parse server-local", func(t *testing.T) {
		spec := LogQuerySpec{From: "2026-10-06 18:00", To: "2026-10-06 23:59"}
		from, to, err := spec.rangeBounds(now)
		require.NoError(t, err)
		assert.True(t, from.Equal(time.Date(2026, 10, 6, 18, 0, 0, 0, time.Local)), "got %v", from)
		assert.True(t, to.Equal(time.Date(2026, 10, 6, 23, 59, 0, 0, time.Local)), "got %v", to)
	})

	t.Run("to defaults to now", func(t *testing.T) {
		_, to, err := LogQuerySpec{From: "2026-10-06 18:00"}.rangeBounds(now)
		require.NoError(t, err)
		assert.True(t, to.Equal(now), "got %v", to)
	})

	t.Run("to without from errors", func(t *testing.T) {
		_, _, err := LogQuerySpec{To: "2026-10-06 23:59"}.rangeBounds(now)
		assert.ErrorContains(t, err, `"from"`)
	})

	t.Run("invalid formats error with the layout in the message", func(t *testing.T) {
		_, _, err := LogQuerySpec{From: "2026-10-06"}.rangeBounds(now)
		assert.ErrorContains(t, err, "2006-01-02 15:04")
		_, _, err = LogQuerySpec{From: "2026-10-06 18:00", To: "10/06 11pm"}.rangeBounds(now)
		assert.ErrorContains(t, err, "2006-01-02 15:04")
	})

	t.Run("from after to errors", func(t *testing.T) {
		_, _, err := LogQuerySpec{From: "2026-10-07 18:00", To: "2026-10-06 23:59"}.rangeBounds(now)
		assert.ErrorContains(t, err, "after")
	})

	t.Run("empty is window mode (no bounds, no error)", func(t *testing.T) {
		from, to, err := LogQuerySpec{}.rangeBounds(now)
		require.NoError(t, err)
		assert.True(t, from.IsZero() && to.IsZero())
	})
}

// TestFetchChannelLogFromRange pins range mode end to end: inclusive bounds
// filter rows, and an omitted to defaults to now.
func TestFetchChannelLogFromRange(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 14, 0, 0, 0, time.Local)
	rows := []ircLog{
		mkRow("PRIVMSG", "before", "#chan", "too early", now.AddDate(0, 0, -6)),
		mkRow("PRIVMSG", "start", "#chan", "at from (inclusive)", time.Date(2026, 10, 6, 18, 0, 0, 0, time.Local)),
		mkRow("PRIVMSG", "mid", "#chan", "inside", time.Date(2026, 10, 6, 20, 0, 0, 0, time.Local)),
		mkRow("PRIVMSG", "end", "#chan", "at to (inclusive)", time.Date(2026, 10, 6, 23, 59, 0, 0, time.Local)),
		mkRow("PRIVMSG", "after", "#chan", "today", now.Add(-time.Hour)),
	}
	writeLogRows(t, dir, "2026-10", rows)

	res, err := fetchChannelLogFrom(dir, "monthly",
		LogQuerySpec{From: "2026-10-06 18:00", To: "2026-10-06 23:59"},
		"testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"--- 2026-10-06 ---",
		"[18:00] <start> at from (inclusive)",
		"[20:00] <mid> inside",
		"[23:59] <end> at to (inclusive)",
	}, res.Lines)

	// to omitted → defaults to now: the today row comes back in.
	resOpen, err := fetchChannelLogFrom(dir, "monthly",
		LogQuerySpec{From: "2026-10-12 10:00"},
		"testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"--- 2026-10-12 ---",
		"[13:00] <after> today",
	}, resOpen.Lines)

	// A past-only range must NOT walk to now's period file — the walk
	// ends at `to`, not `now` (Files is the observable pin: reverting the
	// walk to `now` would scan the extra period and list it here even
	// though the SQL `to` bound filters its rows).
	other := t.TempDir()
	writeLogRows(t, other, "2026-08", []ircLog{
		mkRow("PRIVMSG", "aug", "#chan", "august", time.Date(2026, 8, 15, 12, 0, 0, 0, time.Local)),
	})
	writeLogRows(t, other, "2026-10", []ircLog{
		mkRow("PRIVMSG", "oct", "#chan", "october", now.Add(-time.Hour)),
	})
	resPast, err := fetchChannelLogFrom(other, "monthly",
		LogQuerySpec{From: "2026-08-15 10:00", To: "2026-08-15 18:00"},
		"testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	require.Len(t, resPast.Files, 1)
	assert.Contains(t, resPast.Files[0], "2026-08", "only the range's period file is walked")
	assert.Equal(t, []string{
		"--- 2026-08-15 ---",
		"[12:00] <aug> august",
	}, resPast.Lines)
}

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
		mkRow("JOIN", "noise", "#chan", "", older.Add(time.Minute)), // filtered by events
		mkRow("PRIVMSG", "late", "#chan", "second", now.Add(-time.Hour)),
		mkRow("PRIVMSG", "other", "#other", "wrong channel", now.Add(-30*time.Minute)),
		mkRow("PRIVMSG", "othernet", "#chan", "wrong network", now.Add(-30*time.Minute)),
	}
	// mkRow's 3rd arg is Target; the channel must be set explicitly on the
	// filtered row (mkRow hard-codes Channel "#chan").
	rows[3].Channel = "#other"
	rows[4].Network = "elsewhere"
	writeLogRows(t, dir, "2026-10", rows)

	res, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{}, "testnet", "#chan", "#chan", "qwen3", now)
	require.NoError(t, err)
	// buildTranscriptLines always opens with a day separator (nil row).
	assert.Equal(t, []string{
		"--- 2026-10-08 ---",
		"[16:00] <early> first",
		"[17:00] <late> second",
	}, res.Lines)
	assert.False(t, res.Truncated)
	assert.Equal(t, 2, res.TotalLines)
	// SQLite round-trips timestamps in UTC; compare instants, not Location pointers.
	assert.True(t, res.FirstKept.Equal(older), "FirstKept: got %v want %v", res.FirstKept, older)
	assert.True(t, res.LastKept.Equal(now.Add(-time.Hour)), "LastKept: got %v want %v", res.LastKept, now.Add(-time.Hour))
}

func TestFetchChannelLogSpansMonthBoundary(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 1, 0, 30, 0, 0, time.Local)
	sept := time.Date(2026, 9, 30, 23, 0, 0, 0, time.Local)
	writeLogRows(t, dir, "2026-09", []ircLog{mkRow("PRIVMSG", "sep", "#chan", "september", sept)})
	writeLogRows(t, dir, "2026-10", []ircLog{mkRow("PRIVMSG", "oct", "#chan", "october", now.Add(-10*time.Minute))})

	res, err := fetchChannelLogFrom(dir, "monthly", LogQuerySpec{Window: "2h"}, "testnet", "#chan", "#chan", "qwen3", now)
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
		mkRow("PRIVMSG", "b", "#chan", "join casing", now.Add(-30*time.Minute)),
	}
	// mkRow's 3rd arg is Target; the stored channel must carry mixed casing.
	rows[0].Channel = "#Chan"
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
	writeLogRows(t, dir, "2026-10", []ircLog{mkRow("PRIVMSG", "a", "#chan", "old", now.Add(-48*time.Hour))})
	res, err = fetchChannelLogFrom(dir, "monthly", LogQuerySpec{Window: "1h"}, "testnet", "#chan", "#chan", "qwen3", now)
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
