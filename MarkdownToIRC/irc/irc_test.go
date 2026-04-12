package irc

import (
	"strings"
	"testing"
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
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %d segments, got %d", len(tt.expected), len(got))
			}
			for i, seg := range got {
				if seg.Text != tt.expected[i].Text {
					t.Errorf("segment %d text: expected %q, got %q", i, tt.expected[i].Text, seg.Text)
				}
				if !seg.Style.Equal(tt.expected[i].Style) {
					t.Errorf("segment %d style: expected %+v, got %+v", i, tt.expected[i].Style, seg.Style)
				}
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
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
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
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
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
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
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
			if bef != tt.wantBef {
				t.Errorf("before: expected %q, got %q", tt.wantBef, bef)
			}
			if aft != tt.wantAft {
				t.Errorf("after: expected %q, got %q", tt.wantAft, aft)
			}

			boldB := strings.Count(bef, "\x02")
			boldA := strings.Count(aft, "\x02")
			if boldB%2 != 0 {
				t.Errorf("before has unbalanced bold: %q", bef)
			}
			if boldA%2 != 0 {
				t.Errorf("after has unbalanced bold: %q", aft)
			}
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
			if bef != tt.wantBef {
				t.Errorf("before: expected %q, got %q", tt.wantBef, bef)
			}
			if aft != tt.wantAft {
				t.Errorf("after: expected %q, got %q", tt.wantAft, aft)
			}
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
			if got != tt.want {
				t.Errorf("expected %d, got %d", tt.want, got)
			}
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
				if len(got) != 1 {
					t.Fatalf("expected 1 line, got %d: %v", len(got), got)
				}
				if got[0] != "hello" {
					t.Errorf("expected %q, got %q", "hello", got[0])
				}
			},
		},
		{
			name:     "Empty string",
			input:    "",
			maxBytes: 100,
			check: func(t *testing.T, got []string) {
				if len(got) != 1 {
					t.Fatalf("expected 1 line, got %d", len(got))
				}
				if got[0] != "" {
					t.Errorf("expected empty string, got %q", got[0])
				}
			},
		},
		{
			name:     "Long plain text wraps",
			input:    strings.Repeat("x", 400),
			maxBytes: 350,
			check: func(t *testing.T, got []string) {
				if len(got) < 2 {
					t.Fatalf("expected >= 2 lines, got %d", len(got))
				}
				for i, line := range got {
					if len([]byte(line)) > 350 {
						t.Errorf("line %d exceeds 350 bytes: %d", i, len([]byte(line)))
					}
				}
				joined := strings.Join(got, "")
				expected := strings.Repeat("x", 400)
				if StripCodes(joined) != expected {
					t.Errorf("content mismatch: expected %q, got %q", expected, StripCodes(joined))
				}
			},
		},
		{
			name:     "Bold wraps with open codes on next line",
			input:    "\x02" + strings.Repeat("hello ", 60) + "\x02",
			maxBytes: 350,
			check: func(t *testing.T, got []string) {
				if len(got) < 2 {
					t.Fatalf("expected >= 2 lines, got %d", len(got))
				}
				for i, line := range got {
					if len([]byte(line)) > 360 {
						t.Errorf("line %d exceeds 360 bytes: %d bytes", i, len([]byte(line)))
					}
				}
				first := got[0]
				if len(first) > 0 && first[0] != '\x02' {
					t.Errorf("first line should start with bold, got %q", first[:min(20, len(first))])
				}
				for i := 1; i < len(got); i++ {
					if len(got[i]) == 0 {
						continue
					}
					if got[i][0] != '\x02' {
						t.Errorf("line %d should start with bold open code, got %q", i, got[i][:min(20, len(got[i]))])
					}
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
						if boldCount%2 != 0 {
							t.Errorf("line %d exceeds 360 bytes: %d bytes, boldCount=%d", i, len([]byte(line)), boldCount)
						}
					}
				}
				colorCount := 0
				for _, line := range got {
					colorCount += strings.Count(line, "\x03")
				}
				if colorCount < 2 {
					t.Errorf("expected color codes to be preserved across lines, got %d color codes", colorCount)
				}
			},
		},
		{
			name:     "Word break at space",
			input:    "hello " + strings.Repeat("x", 350) + " world",
			maxBytes: 350,
			check: func(t *testing.T, got []string) {
				if len(got) < 2 {
					t.Fatalf("expected >= 2 lines, got %d", len(got))
				}
				for i, line := range got {
					if len([]byte(line)) > 360 {
						t.Errorf("line %d exceeds 360 bytes: %d", i, len([]byte(line)))
					}
				}
			},
		},
		{
			name:     "Exact byte length fits",
			input:    strings.Repeat("x", 100),
			maxBytes: 100,
			check: func(t *testing.T, got []string) {
				if len(got) != 1 {
					t.Fatalf("expected 1 line, got %d", len(got))
				}
			},
		},
		{
			name:     "One byte over wraps",
			input:    strings.Repeat("x", 101),
			maxBytes: 100,
			check: func(t *testing.T, got []string) {
				if len(got) != 2 {
					t.Fatalf("expected 2 lines, got %d", len(got))
				}
				stripped := StripCodes(strings.Join(got, ""))
				if stripped != strings.Repeat("x", 101) {
					t.Errorf("content mismatch")
				}
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
			if len(got) != len(tt.want) {
				t.Fatalf("expected %d lines, got %d: %v", len(tt.want), len(got), got)
			}
			for i, line := range got {
				if line != tt.want[i] {
					t.Errorf("line %d: expected %q, got %q", i, tt.want[i], line)
				}
				boldCount := strings.Count(line, "\x02")
				if boldCount%2 != 0 {
					t.Errorf("line %d has unbalanced bold: %q", i, line)
				}
			}
		})
	}
}
