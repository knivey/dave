package main

import (
	"strings"
	"testing"
)

func TestFormatCmd(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
		regex   string
		cmdName string
		want    string
	}{
		{
			name:    "simple command",
			trigger: "!",
			regex:   "chat",
			cmdName: "chat",
			want:    "!chat",
		},
		{
			name:    "command with regex suffix",
			trigger: "!",
			regex:   "chat.*",
			cmdName: "chat",
			want:    "!chat.* (regex)",
		},
		{
			name:    "trigger with dot",
			trigger: ".",
			regex:   "img",
			cmdName: "img",
			want:    ".img",
		},
		{
			name:    "different trigger",
			trigger: "dave:",
			regex:   "ask",
			cmdName: "ask",
			want:    "dave:ask",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatCmd(tt.trigger, tt.regex, tt.cmdName)
			if got != tt.want {
				t.Errorf("formatCmd() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDesc(t *testing.T) {
	tests := []struct {
		name         string
		desc         string
		detectImages bool
		want         string
	}{
		{
			name:         "empty description",
			desc:         "",
			detectImages: false,
			want:         "no description",
		},
		{
			name:         "normal description",
			desc:         "generates images",
			detectImages: false,
			want:         "generates images",
		},
		{
			name:         "with image detection",
			desc:         "chat with vision",
			detectImages: true,
			want:         "chat with vision [handles images]",
		},
		{
			name:         "empty with image detection",
			desc:         "",
			detectImages: true,
			want:         "no description [handles images]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatDesc(tt.desc, tt.detectImages)
			if got != tt.want {
				t.Errorf("formatDesc() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatToolInfo(t *testing.T) {
	tests := []struct {
		name      string
		mcpServer string
		tool      string
		want      string
	}{
		{
			name:      "basic tool info",
			mcpServer: "img-mcp",
			tool:      "generate_image",
			want:      "[img-mcp/generate_image]",
		},
		{
			name:      "enhance tool",
			mcpServer: "img-mcp",
			tool:      "enhance_and_generate",
			want:      "[img-mcp/enhance_and_generate]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatToolInfo(tt.mcpServer, tt.tool)
			if got != tt.want {
				t.Errorf("formatToolInfo() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatTable(t *testing.T) {
	tests := []struct {
		name    string
		entries []helpEntry
		want    []string
	}{
		{
			name:    "empty entries",
			entries: nil,
			want:    nil,
		},
		{
			name:    "empty slice",
			entries: []helpEntry{},
			want:    nil,
		},
		{
			name: "single entry",
			entries: []helpEntry{
				{cmd: "!test", info: "[ai]", desc: "test command"},
			},
			want: []string{"!test  [ai]  test command"},
		},
		{
			name: "multiple entries with alignment",
			entries: []helpEntry{
				{cmd: "!a", info: "[x]", desc: "short"},
				{cmd: "!longer", info: "[y]", desc: "longer description"},
			},
			want: []string{
				"!a       [x]  short",
				"!longer  [y]  longer description",
			},
		},
		{
			name: "entries with empty info",
			entries: []helpEntry{
				{cmd: "!img", info: "", desc: "generate image"},
			},
			want: []string{"!img  generate image"},
		},
		{
			name: "entries with different info lengths",
			entries: []helpEntry{
				{cmd: "!x", info: "[a]", desc: "desc1"},
				{cmd: "!y", info: "[abc]", desc: "desc2"},
			},
			want: []string{
				"!x  [a]    desc1",
				"!y  [abc]  desc2",
			},
		},
		{
			name: "regex commands",
			entries: []helpEntry{
				{cmd: "!chat.* (regex)", info: "[gpt-4]", desc: "chatty"},
			},
			want: []string{"!chat.* (regex)  [gpt-4]  chatty"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatTable(tt.entries)
			if len(got) != len(tt.want) {
				t.Errorf("formatTable() returned %d lines, want %d\ngot: %v\nwant: %v", len(got), len(tt.want), got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("formatTable()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestWrapLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "short line under max",
			line: "hello world",
			want: []string{"hello world"},
		},
		{
			name: "exact max length",
			line: "a" + string(make([]byte, 399)),
			want: []string{"a" + string(make([]byte, 399))},
		},
		{
			name: "single word over max",
			line: string(make([]byte, 401)),
			want: []string{string(make([]byte, 401))},
		},
		{
			name: "simple wrap",
			line: "word1 word2 word3",
			want: []string{"word1 word2 word3"},
		},
		{
			name: "long line wraps at boundary",
			line: "test " + strings.Repeat("x", 395) + " end",
			want: []string{
				"test " + strings.Repeat("x", 395),
				"end",
			},
		},
		{
			name: "multiple wraps",
			line: "a b c " + strings.Repeat("x", 200) + " d e f " + strings.Repeat("x", 200) + " g h",
			want: []string{
				"a b c " + strings.Repeat("x", 200) + " d e f",
				strings.Repeat("x", 200) + " g h",
			},
		},
		{
			name: "single character words",
			line: "a b c d e",
			want: []string{"a b c d e"},
		},
		{
			name: "empty string",
			line: "",
			want: []string{""},
		},
		{
			name: "only spaces",
			line: "   ",
			want: []string{"   "},
		},
		{
			name: "long words force wrap",
			line: "short " + strings.Repeat("x", 400) + " another",
			want: []string{
				"short",
				strings.Repeat("x", 400),
				"another",
			},
		},
		{
			name: "ends with space",
			line: "hello world ",
			want: []string{"hello world "},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapLine(tt.line)
			if len(got) != len(tt.want) {
				t.Errorf("wrapLine() returned %d parts, want %d\nline length: %d\ngot: %v\nwant: %v",
					len(got), len(tt.want), len(tt.line), got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("wrapLine()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
