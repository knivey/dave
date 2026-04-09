package tables

import (
	"testing"
	"unicode/utf8"

	"github.com/knivey/dave/MarkdownToIRC/irc"
)

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
			stripped := irc.StripCodes(got)
			if tt.width < utf8.RuneCountInString(irc.StripCodes(tt.text)) {
				return
			}
			if utf8.RuneCountInString(stripped) != tt.width {
				t.Errorf("stripped width: expected %d, got %d", tt.width, utf8.RuneCountInString(stripped))
			}
		})
	}
}

func TestWrapCellText(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxWidth int
		expected []string
	}{
		{
			name:     "Empty",
			text:     "",
			maxWidth: 10,
			expected: []string{""},
		},
		{
			name:     "NoWrap",
			text:     "hello",
			maxWidth: 10,
			expected: []string{"hello"},
		},
		{
			name:     "WrapSingleLine",
			text:     "hello world foo",
			maxWidth: 10,
			expected: []string{"hello worl", "d foo"},
		},
		{
			name:     "MultiLine",
			text:     "line1\nline2",
			maxWidth: 10,
			expected: []string{"line1", "line2"},
		},
		{
			name:     "MultiLineWithEmpty",
			text:     "line1\n\nline3",
			maxWidth: 10,
			expected: []string{"line1", "", "line3"},
		},
		{
			name:     "WrapMultiLine",
			text:     "long line\nshort",
			maxWidth: 8,
			expected: []string{"long lin", "e", "short"},
		},
		{
			name:     "WithIRCCode",
			text:     "\x02bold text\x02",
			maxWidth: 6,
			expected: []string{"\x02bold\x02", "\x02text\x02"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapCellText(tt.text, tt.maxWidth)
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %d lines, got %d: %v", len(tt.expected), len(got), got)
			}
			for i, line := range got {
				if line != tt.expected[i] {
					t.Errorf("line %d: expected %q, got %q", i, tt.expected[i], line)
				}
			}
		})
	}
}

func TestRenderTable(t *testing.T) {
	tests := []struct {
		name     string
		data     TableData
		expected string
	}{
		{
			name: "SimpleTable",
			data: TableData{
				Rows: []TableRow{
					{{Text: "A", Align: AlignLeft}, {Text: "B", Align: AlignLeft}},
					{{Text: "1", Align: AlignLeft}, {Text: "2", Align: AlignLeft}},
				},
				HeaderRowCount: 1,
			},
			expected: "\n┌───┬───┐\n│ A │ B │\n├───┼───┤\n│ 1 │ 2 │\n└───┴───┘",
		},
		{
			name: "WithBoldAndItalic",
			data: TableData{
				Rows: []TableRow{
					{{Text: "\x02Header\x02", Align: AlignLeft}, {Text: "\x1DValue\x1D", Align: AlignLeft}},
					{{Text: "foo", Align: AlignLeft}, {Text: "\x02bar\x02", Align: AlignLeft}},
				},
				HeaderRowCount: 1,
			},
			expected: "\n┌────────┬───────┐\n│ \x02Header\x02 │ \x1DValue\x1D │\n├────────┼───────┤\n│ foo    │ \x02bar\x02   │\n└────────┴───────┘",
		},
		{
			name: "WithAlignmentAndCode",
			data: TableData{
				Rows: []TableRow{
					{{Text: "Left", Align: AlignLeft}, {Text: "Center", Align: AlignCenter}, {Text: "Right", Align: AlignRight}},
					{{Text: "\x030,90code\x03", Align: AlignLeft}, {Text: "middle", Align: AlignCenter}, {Text: "123", Align: AlignRight}},
				},
				HeaderRowCount: 1,
			},
			expected: "\n┌──────┬────────┬───────┐\n│ Left │ Center │ Right │\n├──────┼────────┼───────┤\n│ \x030,90code\x03 │ middle │   123 │\n└──────┴────────┴───────┘",
		},
		{
			name: "WithWrappingAndCodes",
			data: TableData{
				Rows: []TableRow{
					{{Text: "Short", Align: AlignLeft}, {Text: "\x02this is a very long bold cell that should wrap\x02", Align: AlignLeft}},
				},
				HeaderRowCount: 1,
			},
			expected: "\n┌───────┬──────────────────────────────────────────┐\n│ Short │ \x02this is a very long bold cell that\x02       │\n│       │ \x02should wrap\x02                              │\n└───────┴──────────────────────────────────────────┘",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderTable(tt.data)
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}
