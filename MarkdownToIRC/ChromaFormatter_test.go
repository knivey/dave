package markdowntoirc

import (
	"bytes"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIRCStyleFormat(t *testing.T) {
	tests := []struct {
		name  string
		style IRCStyle
		want  string
	}{
		{
			name:  "Empty",
			style: IRCStyle{},
			want:  "",
		},
		{
			name:  "BoldOnly",
			style: IRCStyle{Bold: true},
			want:  "\x02",
		},
		{
			name:  "ItalicOnly",
			style: IRCStyle{Italic: true},
			want:  "\x1D",
		},
		{
			name:  "UnderlineOnly",
			style: IRCStyle{Underline: true},
			want:  "\x1F",
		},
		{
			name:  "ColourOnly",
			style: IRCStyle{Colour: "04"},
			want:  "\x0304",
		},
		{
			name:  "BoldAndColour",
			style: IRCStyle{Bold: true, Colour: "04"},
			want:  "\x02\x0304",
		},
		{
			name:  "AllFlags",
			style: IRCStyle{Bold: true, Italic: true, Underline: true, Colour: "01"},
			want:  "\x02\x1D\x1F\x0301",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.style.Format()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIRCStyleDeltaFormat(t *testing.T) {
	tests := []struct {
		name string
		old  IRCStyle
		new  IRCStyle
		want string
	}{
		{
			name: "NoChange",
			old:  IRCStyle{Bold: true, Colour: "04"},
			new:  IRCStyle{Bold: true, Colour: "04"},
			want: "",
		},
		{
			name: "BoldToggleOn",
			old:  IRCStyle{},
			new:  IRCStyle{Bold: true},
			want: "\x02",
		},
		{
			name: "BoldToggleOff",
			old:  IRCStyle{Bold: true},
			new:  IRCStyle{},
			want: "\x02",
		},
		{
			name: "ColourChange",
			old:  IRCStyle{Colour: "04"},
			new:  IRCStyle{Colour: "09"},
			want: "\x0309",
		},
		{
			name: "ColourToNone",
			old:  IRCStyle{Colour: "04"},
			new:  IRCStyle{},
			want: "\x03",
		},
		{
			name: "NoneToColour",
			old:  IRCStyle{},
			new:  IRCStyle{Colour: "04"},
			want: "\x0304",
		},
		{
			name: "BoldAndColourChange",
			old:  IRCStyle{Bold: true, Colour: "04"},
			new:  IRCStyle{Italic: true, Colour: "09"},
			want: "\x02\x1D\x0309",
		},
		{
			name: "UnderlineToggle",
			old:  IRCStyle{},
			new:  IRCStyle{Underline: true},
			want: "\x1F",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.new.DeltaFormat(tt.old)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFindClosestColor(t *testing.T) {
	tests := []struct {
		name    string
		seeking chroma.Colour
		want    chroma.Colour
	}{
		{
			name:    "ExactWhite",
			seeking: chroma.MustParseColour("#FFFFFF"),
			want:    chroma.MustParseColour("#FFFFFF"),
		},
		{
			name:    "ExactBlack",
			seeking: chroma.MustParseColour("#000000"),
			want:    chroma.MustParseColour("#000000"),
		},
		{
			name:    "ExactRed",
			seeking: chroma.MustParseColour("#FF0000"),
			want:    chroma.MustParseColour("#FF0000"),
		},
		{
			name:    "NearWhite",
			seeking: chroma.MustParseColour("#F0F0F0"),
			want:    chroma.MustParseColour("#FFFFFF"),
		},
		{
			name:    "NearRed",
			seeking: chroma.MustParseColour("#EE0000"),
			want:    chroma.MustParseColour("#FF0000"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ircTable.findClosest(tt.seeking)
			assert.Equal(t, tt.want, got, "findClosest(%v)", tt.seeking)
		})
	}
}

func TestIRCStyleFromEntry(t *testing.T) {
	tests := []struct {
		name  string
		entry chroma.StyleEntry
		want  IRCStyle
	}{
		{
			name:  "EmptyEntry",
			entry: chroma.StyleEntry{},
			want:  IRCStyle{Colour: ircTable[ircTable.findClosest(chroma.Colour(0))]},
		},
		{
			name:  "BoldEntry",
			entry: chroma.StyleEntry{Bold: chroma.Yes},
			want:  IRCStyle{Bold: true, Colour: ircTable[ircTable.findClosest(chroma.Colour(0))]},
		},
		{
			name:  "ItalicEntry",
			entry: chroma.StyleEntry{Italic: chroma.Yes},
			want:  IRCStyle{Italic: true, Colour: ircTable[ircTable.findClosest(chroma.Colour(0))]},
		},
		{
			name:  "ColouredEntry",
			entry: chroma.StyleEntry{Colour: chroma.MustParseColour("#FF0000")},
			want:  IRCStyle{Colour: "04"},
		},
		{
			name:  "AllAttributes",
			entry: chroma.StyleEntry{Colour: chroma.MustParseColour("#FF0000"), Bold: chroma.Yes, Italic: chroma.Yes, Underline: chroma.Yes},
			want:  IRCStyle{Colour: "04", Bold: true, Italic: true, Underline: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IRCStyleFromEntry(tt.entry)
			assert.Equal(t, tt.want, got, "IRCStyleFromEntry() mismatch")
		})
	}
}

func TestIRCFormatterFormat(t *testing.T) {
	style, err := chroma.NewStyle("test", map[chroma.TokenType]string{
		chroma.NameKeyword: "#FF0000",
	})
	require.NoError(t, err, "NewStyle() error")

	tokens := []chroma.Token{
		{Type: chroma.Text, Value: "hello "},
		{Type: chroma.NameKeyword, Value: "func"},
		{Type: chroma.Text, Value: " world"},
	}

	it := chroma.Literator(tokens...)

	var buf bytes.Buffer
	f := &IRCFormatter{}
	err = f.Format(&buf, style, it)
	require.NoError(t, err, "Format() error")

	got := buf.String()

	assert.Contains(t, got, "hello ")
	assert.Contains(t, got, "func")
	assert.Contains(t, got, " world")
	assert.Contains(t, got, "\x0304")
}

func TestWriteTokenNewlines(t *testing.T) {
	tests := []struct {
		name  string
		style IRCStyle
		text  string
		want  string
	}{
		{
			name:  "NoNewlines",
			style: IRCStyle{Bold: true},
			text:  "hello",
			want:  "hello",
		},
		{
			name:  "SingleNewline",
			style: IRCStyle{Bold: true},
			text:  "line1\nline2",
			want:  "line1\n\x02line2",
		},
		{
			name:  "MultipleNewlines",
			style: IRCStyle{Colour: "04"},
			text:  "a\nb\nc",
			want:  "a\n\x0304b\n\x0304c",
		},
		{
			name:  "EmptyStyle",
			style: IRCStyle{},
			text:  "x\ny",
			want:  "x\ny",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeToken(&buf, tt.style, tt.text)
			got := buf.String()
			assert.Equal(t, tt.want, got)
		})
	}
}
