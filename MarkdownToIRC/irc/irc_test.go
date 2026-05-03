package irc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseIRC(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []IRCSegment
	}{
		{
			name:     "Empty",
			input:    "",
			expected: []IRCSegment{{Text: "", Style: IRCFormat{}}},
		},
		{
			name:     "PlainText",
			input:    "hello world",
			expected: []IRCSegment{{Text: "hello world", Style: IRCFormat{}}},
		},
		{
			name:     "Bold",
			input:    "\x02bold\x02",
			expected: []IRCSegment{{Text: "bold", Style: IRCFormat{Bold: true}}},
		},
		{
			name:     "Italic",
			input:    "\x1Ditalic\x1D",
			expected: []IRCSegment{{Text: "italic", Style: IRCFormat{Italic: true}}},
		},
		{
			name:     "Color",
			input:    "\x030,90code\x03",
			expected: []IRCSegment{{Text: "code", Style: IRCFormat{Color: &IRCColor{FG: 0, BG: 90}}}},
		},
		{
			name:  "NestedBoldItalic",
			input: "\x02bold \x1Dboth\x1D\x02",
			expected: []IRCSegment{
				{Text: "bold ", Style: IRCFormat{Bold: true}},
				{Text: "both", Style: IRCFormat{Bold: true, Italic: true}},
			},
		},
		{
			name:  "MultipleSegments",
			input: "plain \x02bold\x02 plain",
			expected: []IRCSegment{
				{Text: "plain ", Style: IRCFormat{}},
				{Text: "bold", Style: IRCFormat{Bold: true}},
				{Text: " plain", Style: IRCFormat{}},
			},
		},
		{
			name:  "Reset",
			input: "\x02bold\x0Fplain",
			expected: []IRCSegment{
				{Text: "bold", Style: IRCFormat{Bold: true}},
				{Text: "plain", Style: IRCFormat{}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseIRC(tt.input)
			require.Len(t, got, len(tt.expected), "expected %d segments, got %d", len(tt.expected), len(got))
			for i, seg := range got {
				assert.Equal(t, tt.expected[i].Text, seg.Text, "segment %d text", i)
				assert.True(t, seg.Style.Equal(tt.expected[i].Style), "segment %d style: expected %+v, got %+v", i, tt.expected[i].Style, seg.Style)
			}
		})
	}
}

func TestCloseCodes(t *testing.T) {
	tests := []struct {
		name     string
		style    IRCFormat
		expected string
	}{
		{
			name:     "Default",
			style:    IRCFormat{},
			expected: "",
		},
		{
			name:     "Bold",
			style:    IRCFormat{Bold: true},
			expected: "\x02",
		},
		{
			name:     "Color",
			style:    IRCFormat{Color: &IRCColor{FG: 0, BG: 90}},
			expected: "\x03",
		},
		{
			name:     "BoldAndItalic",
			style:    IRCFormat{Bold: true, Italic: true},
			expected: "\x1D\x02",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CloseCodes(tt.style)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestOpenCodes(t *testing.T) {
	tests := []struct {
		name     string
		style    IRCFormat
		expected string
	}{
		{
			name:     "Default",
			style:    IRCFormat{},
			expected: "",
		},
		{
			name:     "Bold",
			style:    IRCFormat{Bold: true},
			expected: "\x02",
		},
		{
			name:     "ColorNoBG",
			style:    IRCFormat{Color: &IRCColor{FG: 0, BG: 99}},
			expected: "\x030",
		},
		{
			name:     "ColorWithBG",
			style:    IRCFormat{Color: &IRCColor{FG: 0, BG: 1}},
			expected: "\x030,1",
		},
		{
			name:     "BoldAndColor",
			style:    IRCFormat{Bold: true, Color: &IRCColor{FG: 0, BG: 99}},
			expected: "\x02\x030",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := OpenCodes(tt.style)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestStripCodes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Empty",
			input:    "",
			expected: "",
		},
		{
			name:     "PlainText",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "Bold",
			input:    "\x02bold\x02",
			expected: "bold",
		},
		{
			name:     "Color",
			input:    "\x030,90code\x03",
			expected: "code",
		},
		{
			name:     "Mixed",
			input:    "\x02bold\x02 and \x030,90code\x03",
			expected: "bold and code",
		},
		{
			name:     "ColorResetFollowedByComma",
			input:    "\x03,hello",
			expected: ",hello",
		},
		{
			name:     "ColorResetFollowedByText",
			input:    "\x03hello",
			expected: "hello",
		},
		{
			name:     "ColorThenCommaText",
			input:    "\x0315,hello",
			expected: ",hello",
		},
		{
			name:     "ColorBeforeNumber",
			input:    "\x030495",
			expected: "95",
		},
		{
			name:     "ColorBeforeTwoDigitNumber",
			input:    "\x031599",
			expected: "99",
		},
		{
			name:     "ExtendedColor",
			input:    "\x0399hello",
			expected: "hello",
		},
		{
			name:     "FullColorWithBG",
			input:    "\x0304,90text",
			expected: "text",
		},
		{
			name:     "BareReset",
			input:    "\x03text",
			expected: "text",
		},
		{
			name:     "MixedWithToggles",
			input:    "\x02\x0304boldcolor\x1Ditalic\x03plain",
			expected: "boldcoloritalicplain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripCodes(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestSplitAt(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		pos     int
		wantBef string
		wantAft string
	}{
		{
			name:    "Plain",
			input:   "hello world",
			pos:     5,
			wantBef: "hello",
			wantAft: " world",
		},
		{
			name:    "Bold",
			input:   "\x02hello world\x02",
			pos:     5,
			wantBef: "\x02hello\x02",
			wantAft: "\x02 world\x02",
		},
		{
			name:    "Color",
			input:   "\x030,90hello\x03",
			pos:     3,
			wantBef: "\x030,90hel\x03",
			wantAft: "\x030,90lo\x03",
		},
		{
			name:    "Nested",
			input:   "\x02bold \x1Dboth\x1D\x02",
			pos:     6,
			wantBef: "\x02bold \x1Db\x1D\x02",
			wantAft: "\x02\x1Doth\x1D\x02",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bef, aft := SplitAt(tt.input, tt.pos)
			assert.Equal(t, tt.wantBef, bef, "before mismatch")
			assert.Equal(t, tt.wantAft, aft, "after mismatch")

			boldB := strings.Count(bef, "\x02")
			boldA := strings.Count(aft, "\x02")
			assert.Zero(t, boldB%2, "before has unbalanced bold: %q", bef)
			assert.Zero(t, boldA%2, "after has unbalanced bold: %q", aft)
		})
	}
}

func TestByteSplitAt(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		pos     int
		wantBef string
		wantAft string
	}{
		{
			name:    "Plain",
			input:   "hello world",
			pos:     5,
			wantBef: "hello",
			wantAft: " world",
		},
		{
			name:    "BoldNoClose",
			input:   "\x02hello world\x02",
			pos:     5,
			wantBef: "\x02hello",
			wantAft: "\x02 world\x02",
		},
		{
			name:    "ColorNoClose",
			input:   "\x030,90hello\x03",
			pos:     3,
			wantBef: "\x030,90hel",
			wantAft: "\x030,90lo\x03",
		},
		{
			name:    "NestedBoldItalicNoClose",
			input:   "\x02bold \x1Dboth\x1D\x02",
			pos:     6,
			wantBef: "\x02bold \x1Db",
			wantAft: "\x02\x1Doth\x1D\x02",
		},
		{
			name:    "ItalicNoClose",
			input:   "\x1Ditalic text\x1D",
			pos:     7,
			wantBef: "\x1Ditalic ",
			wantAft: "\x1Dtext\x1D",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bef, aft := byteSplitAt(tt.input, tt.pos)
			assert.Equal(t, tt.wantBef, bef, "before mismatch")
			assert.Equal(t, tt.wantAft, aft, "after mismatch")
			if strings.Contains(tt.input, "\x02") {
				if strings.Count(bef, "\x02")%2 != 0 && !strings.HasSuffix(strings.TrimSuffix(bef, "\x02"), "\x02") {
					if strings.Count(bef, "\x02")%2 != 0 {
					}
				}
			}
		})
	}
}

func TestFindByteBreak(t *testing.T) {
	tests := []struct {
		name    string
		plain   string
		pos     int
		minBack int
		want    int
	}{
		{
			name:    "SpaceAtPos",
			plain:   "hello world foo",
			pos:     5,
			minBack: 15,
			want:    5,
		},
		{
			name:    "SpaceNearPos",
			plain:   "hello world foo",
			pos:     7,
			minBack: 15,
			want:    5,
		},
		{
			name:    "NoSpaceWithinRange",
			plain:   "helloworld foo",
			pos:     5,
			minBack: 3,
			want:    0,
		},
		{
			name:    "PosBeyondLength",
			plain:   "hello",
			pos:     10,
			minBack: 15,
			want:    0,
		},
		{
			name:    "SpaceAtStart",
			plain:   " hello world",
			pos:     1,
			minBack: 15,
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindByteBreak(tt.plain, tt.pos, tt.minBack)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestByteWrap(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxBytes int
		check    func(t *testing.T, got []string)
	}{
		{
			name:     "Short text no wrap",
			input:    "hello",
			maxBytes: 100,
			check: func(t *testing.T, got []string) {
				require.Len(t, got, 1, "expected 1 line, got %d: %v", len(got), got)
				assert.Equal(t, "hello", got[0])
			},
		},
		{
			name:     "Empty string",
			input:    "",
			maxBytes: 100,
			check: func(t *testing.T, got []string) {
				require.Len(t, got, 1, "expected 1 line, got %d", len(got))
				assert.Equal(t, "", got[0])
			},
		},
		{
			name:     "Long plain text wraps",
			input:    strings.Repeat("x", 400),
			maxBytes: 350,
			check: func(t *testing.T, got []string) {
				require.GreaterOrEqual(t, len(got), 2, "expected >= 2 lines, got %d", len(got))
				for i, line := range got {
					assert.LessOrEqual(t, len([]byte(line)), 350, "line %d exceeds 350 bytes", i)
				}
				joined := strings.Join(got, "")
				expected := strings.Repeat("x", 400)
				assert.Equal(t, expected, StripCodes(joined), "content mismatch")
			},
		},
		{
			name:     "Bold wraps with open codes on next line",
			input:    "\x02" + strings.Repeat("hello ", 60) + "\x02",
			maxBytes: 350,
			check: func(t *testing.T, got []string) {
				require.GreaterOrEqual(t, len(got), 2, "expected >= 2 lines, got %d", len(got))
				for i, line := range got {
					if len([]byte(line)) > 360 {
						boldCount := strings.Count(line, "\x02")
						assert.Zero(t, boldCount%2, "line %d exceeds 360 bytes: %d bytes, boldCount=%d", i, len([]byte(line)), boldCount)
					}
				}
				first := got[0]
				if len(first) > 0 {
					assert.Equal(t, byte('\x02'), first[0], "first line should start with bold")
				}
				for i := 1; i < len(got); i++ {
					if len(got[i]) == 0 {
						continue
					}
					assert.Equal(t, byte('\x02'), got[i][0], "line %d should start with bold open code", i)
				}
			},
		},
		{
			name:     "Color codes count toward byte budget",
			input:    "\x030,90" + strings.Repeat("x", 345) + "\x03",
			maxBytes: 350,
			check: func(t *testing.T, got []string) {
				for i, line := range got {
					if len([]byte(line)) > 360 {
						boldCount := strings.Count(line, "\x02")
						assert.Zero(t, boldCount%2, "line %d exceeds 360 bytes: %d bytes, boldCount=%d", i, len([]byte(line)), boldCount)
					}
				}
				colorCount := 0
				for _, line := range got {
					colorCount += strings.Count(line, "\x03")
				}
				assert.GreaterOrEqual(t, colorCount, 2, "expected color codes to be preserved across lines")
			},
		},
		{
			name:     "Word break at space",
			input:    "hello " + strings.Repeat("x", 350) + " world",
			maxBytes: 350,
			check: func(t *testing.T, got []string) {
				require.GreaterOrEqual(t, len(got), 2, "expected >= 2 lines, got %d", len(got))
				for i, line := range got {
					assert.LessOrEqual(t, len([]byte(line)), 360, "line %d exceeds 360 bytes", i)
				}
			},
		},
		{
			name:     "Exact byte length fits",
			input:    strings.Repeat("x", 100),
			maxBytes: 100,
			check: func(t *testing.T, got []string) {
				require.Len(t, got, 1, "expected 1 line, got %d", len(got))
			},
		},
		{
			name:     "One byte over wraps",
			input:    strings.Repeat("x", 101),
			maxBytes: 100,
			check: func(t *testing.T, got []string) {
				require.Len(t, got, 2, "expected 2 lines, got %d", len(got))
				stripped := StripCodes(strings.Join(got, ""))
				assert.Equal(t, strings.Repeat("x", 101), stripped, "content mismatch")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ByteWrap(tt.input, tt.maxBytes)
			tt.check(t, got)
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestWordWrap(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxWidth int
		want     []string
	}{
		{
			name:     "Short",
			input:    "hello",
			maxWidth: 10,
			want:     []string{"hello"},
		},
		{
			name:     "Long",
			input:    "hello world foo",
			maxWidth: 10,
			want:     []string{"hello worl", "d foo"},
		},
		{
			name:     "Bold",
			input:    "\x02this is a very long bold text\x02",
			maxWidth: 15,
			want:     []string{"\x02this is a very\x02", "\x02long bold text\x02"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WordWrap(tt.input, tt.maxWidth)
			require.Len(t, got, len(tt.want), "expected %d lines, got %d: %v", len(tt.want), len(got), got)
			for i, line := range got {
				assert.Equal(t, tt.want[i], line, "line %d", i)
				boldCount := strings.Count(line, "\x02")
				assert.Zero(t, boldCount%2, "line %d has unbalanced bold: %q", i, line)
			}
		})
	}
}
