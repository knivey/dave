package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/gorm"
)

// LogQuerySpec describes a channel-log retrieval for generator commands.
// It is the reusable "log retrieval -> LLM prompt input" contract: any
// command can carry one via [name.log] in generators.toml.
type LogQuerySpec struct {
	// Window is a duration STRING parsed with parseWindowDuration's grammar
	// (s/m/h/d units, compound allowed), NOT a TOML time.Duration field:
	// BurntSushi decodes durations via time.ParseDuration, which rejects the
	// d suffix — `window = "7d"` would be startup-fatal as a duration field.
	// Resolved via windowDuration() at config validation and query time.
	Window    string   `toml:"window"`     // duration string, d suffix allowed (e.g. "24h", "7d", "1d12h"); default "24h"
	Events    []string `toml:"events"`     // default PRIVMSG, NOTICE, TOPIC, KICK
	MaxTokens int      `toml:"max_tokens"` // default 60000
	// From/To are the query_channel_logs RANGE-MODE TOOL ARGUMENTS, never
	// config fields: a static range in [name.log] would be a mistake (config
	// windows are relative) — validateLogQuerySpec rejects them at load.
	// Format "2006-01-02 15:04" (logRangeLayout), server-local; From is
	// inclusive, To inclusive and empty = "now". Range mode overrides the
	// Window; window and from/to are mutually exclusive (the tool handler
	// enforces, queryBounds resolves).
	From string `toml:"from"` // tool argument, not config — date-time, e.g. "2026-10-06 18:00"
	To   string `toml:"to"`   // tool argument, not config — date-time; empty = now
}

const (
	defaultLogWindow    = "24h"
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
	if spec.Window == "" {
		spec.Window = defaultLogWindow
	}
	if spec.MaxTokens == 0 {
		spec.MaxTokens = defaultLogMaxTokens
	}
	if len(spec.Events) == 0 {
		spec.Events = defaultLogEvents
	}
}

// windowDuration resolves the Window string with parseWindowDuration's full
// grammar (d suffix + compound units). Call after applyLogQueryDefaults —
// an empty window means unset, not invalid.
func (s LogQuerySpec) windowDuration() (time.Duration, error) {
	d, ok := parseWindowDuration(s.Window)
	if !ok {
		return 0, fmt.Errorf("invalid window duration %q (expected e.g. \"24h\", \"7d\", \"1d12h\")", s.Window)
	}
	return d, nil
}

// logRangeLayout is the range-mode (tool from/to arguments) date-time
// layout, server-local — the clock the transcript itself renders.
const logRangeLayout = "2006-01-02 15:04"

// rangeBounds resolves the range-mode tool arguments into inclusive query
// bounds. To empty → now. Errors are self-correcting tool-result material:
// to without from, unparseable formats (message carries the layout), from
// after to. Both empty = window mode (zero bounds, no error).
func (s LogQuerySpec) rangeBounds(now time.Time) (time.Time, time.Time, error) {
	if s.From == "" && s.To == "" {
		return time.Time{}, time.Time{}, nil
	}
	if s.From == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("range query needs \"from\" (got only \"to\" %q)", s.To)
	}
	from, err := time.ParseInLocation(logRangeLayout, s.From, time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid \"from\" %q (expected date-time like \"2006-01-02 15:04\")", s.From)
	}
	to := now
	if s.To != "" {
		to, err = time.ParseInLocation(logRangeLayout, s.To, time.Local)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid \"to\" %q (expected date-time like \"2006-01-02 15:04\")", s.To)
		}
	}
	if from.After(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("\"from\" %s is after \"to\" %s", s.From, s.To)
	}
	return from, to, nil
}

// queryBounds resolves the retrieval bounds for one query: range mode
// (From/To tool arguments — To empty means now) when set, else the relative
// Window (defaulted). Returns inclusive [from, to].
func (s LogQuerySpec) queryBounds(now time.Time) (time.Time, time.Time, error) {
	if s.From != "" || s.To != "" {
		return s.rangeBounds(now)
	}
	window, err := s.windowDuration()
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return now.Add(-window), now, nil
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
			if cut >= len(lines) {
				// Floor: even the newest line busts the budget — keep it anyway.
				cut = len(lines) - 1
				acc = count(lines[cut].text)
			}
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

// LogWindowResult is the outcome of a channel-log query: the rendered,
// budgeted transcript plus the stats that feed notices and logs.
type LogWindowResult struct {
	Lines        []string // final transcript lines (marker first when truncated)
	Tokens       int      // token total of the kept lines
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
	// Range mode (explicit from/to tool arguments — to defaults to now)
	// overrides the relative window; the SQL bounds, period walk, budget,
	// and rendering below are range-agnostic.
	from, to, err := spec.queryBounds(now)
	if err != nil {
		return nil, err
	}

	var rows []ircLog
	var files []string
	for _, key := range logPeriodKeys(rotation, from, to) {
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
				network, []string{channelRaw, channelNorm}, spec.Events, from, to).
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
