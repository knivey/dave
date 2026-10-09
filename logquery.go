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
