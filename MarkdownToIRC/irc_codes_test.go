package markdowntoirc

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestFormatTableLine(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		width    int
		align    string
		expected string
	}{
		{
			name:     "LeftPlain",
			text:     "hi",
			width:    5,
			align:    "left",
			expected: "hi   ",
		},
		{
			name:     "RightPlain",
			text:     "hi",
			width:    5,
			align:    "right",
			expected: "   hi",
		},
		{
			name:     "CenterPlain",
			text:     "hi",
			width:    5,
			align:    "center",
			expected: " hi  ",
		},
		{
			name:     "LeftBold",
			text:     "\x02hi\x02",
			width:    5,
			align:    "left",
			expected: "\x02hi\x02   ",
		},
		{
			name:     "OverWidth",
			text:     "hello world",
			width:    5,
			align:    "left",
			expected: "hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatTableLine(tt.text, tt.width, tt.align)
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
			stripped := StripCodes(got)
			if tt.width < utf8.RuneCountInString(StripCodes(tt.text)) {
				return
			}
			if utf8.RuneCountInString(stripped) != tt.width {
				t.Errorf("stripped width: expected %d, got %d", tt.width, utf8.RuneCountInString(stripped))
			}
		})
	}
}
