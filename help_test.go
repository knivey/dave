package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert.Equal(t, tt.want, got, "formatCmd()")
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
			assert.Equal(t, tt.want, got, "formatDesc()")
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
			assert.Equal(t, tt.want, got, "formatToolInfo()")
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
			require.Len(t, got, len(tt.want), "formatTable() line count\ngot: %v\nwant: %v", got, tt.want)
			for i := range got {
				assert.Equal(t, tt.want[i], got[i], "formatTable()[%d]", i)
			}
		})
	}
}

func TestBuildHelpText(t *testing.T) {
	text := buildHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "testbot")
	assert.Contains(t, text, "!stop")
	assert.Contains(t, text, "!support")
	assert.NotEmpty(t, text)
}

func TestBuildHelpTextWithDisabledCommands(t *testing.T) {
	network := Network{
		DisabledCommands: []string{"stop"},
	}
	text := buildHelpText("testbot", "!", network)
	assert.Contains(t, text, "testbot")
	assert.NotContains(t, text, "!stop")
	assert.Contains(t, text, "!support")
}

func TestBuildHelpTextWithCompletions(t *testing.T) {
	config.Commands.Completions = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat", Service: "openai", Model: "gpt-4"},
	}
	defer func() { config.Commands.Completions = nil }()
	text := buildHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "Completions:")
	assert.Contains(t, text, "!chat")
}

func TestBuildHelpTextWithChats(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"ask": {Name: "ask", Regex: "ask", Service: "openai", Model: "gpt-4"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "Chats:")
	assert.Contains(t, text, "!ask")
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
			require.Len(t, got, len(tt.want), "wrapLine() part count\nline length: %d\ngot: %v\nwant: %v", len(tt.line), got, tt.want)
			for i := range got {
				assert.Equal(t, tt.want[i], got[i], "wrapLine()[%d]", i)
			}
		})
	}
}

func TestBuildPastebinHelpText(t *testing.T) {
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "# testbot Help")
	assert.Contains(t, text, "!support")
	assert.Contains(t, text, "## Example Session")
	assert.Contains(t, text, "<alice>")
	assert.NotEmpty(t, text)
}

func TestBuildPastebinHelpTextUsesBotnickAndTrigger(t *testing.T) {
	text := buildPastebinHelpText("mybot", ".", Network{})
	assert.Contains(t, text, "# mybot Help")
	assert.Contains(t, text, "mybot, your message here")
	assert.Contains(t, text, ".stop")
	assert.Contains(t, text, "<mybot>")
}

func TestBuildPastebinHelpTextWithChats(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"ask": {Name: "ask", Regex: "ask", Service: "openai", Model: "gpt-4", Description: "ask questions"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "## Chat Commands")
	assert.Contains(t, text, "!ask")
	assert.Contains(t, text, "gpt-4")
	assert.Contains(t, text, "ask questions")
	assert.Contains(t, text, "## Example Session")
	chatIdx := strings.Index(text, "## Chat Commands")
	exampleIdx := strings.Index(text, "## Example Session")
	assert.True(t, chatIdx < exampleIdx, "Chat Commands should come before Example Session")
}

func TestBuildPastebinHelpTextWithCompletions(t *testing.T) {
	config.Commands.Completions = map[string]AIConfig{
		"complete": {Name: "complete", Regex: "complete", Service: "openai", Model: "gpt-4"},
	}
	defer func() { config.Commands.Completions = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "## Completions")
	assert.Contains(t, text, "!complete")
}

func TestBuildPastebinHelpTextWithTools(t *testing.T) {
	config.Commands.Tools = map[string]MCPCommandConfig{
		"img": {Name: "img", Regex: "img", MCP: "img-mcp", Tool: "generate_image", Description: "generate an image"},
	}
	defer func() { config.Commands.Tools = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "## Tool Commands")
	assert.Contains(t, text, "!img")
	assert.Contains(t, text, "img-mcp")
	assert.Contains(t, text, "generate_image")
}

func TestBuildPastebinHelpTextDisabledCommands(t *testing.T) {
	network := Network{
		DisabledCommands: []string{"stop"},
	}
	text := buildPastebinHelpText("testbot", "!", network)
	assert.NotContains(t, text, "!stop")
	assert.Contains(t, text, "!support")
}

func TestBuildPastebinHelpTextChatBeforeCompletions(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat", Service: "openai", Model: "gpt-4"},
	}
	config.Commands.Completions = map[string]AIConfig{
		"complete": {Name: "complete", Regex: "complete", Service: "openai", Model: "gpt-4"},
	}
	defer func() {
		config.Commands.Chats = nil
		config.Commands.Completions = nil
	}()
	text := buildPastebinHelpText("testbot", "!", Network{})
	chatIdx := strings.Index(text, "## Chat Commands")
	compIdx := strings.Index(text, "## Completions")
	assert.True(t, chatIdx < compIdx, "Chat Commands should come before Completions")
}

func TestBuildPastebinHelpTextGFMTables(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat", Service: "openai", Model: "gpt-4", Description: "general chat"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "| Command | Regex | Service | Model | Media | Description |")
	assert.Contains(t, text, "| `!chat` |  | openai | gpt-4 |  | general chat |")
}

func TestBuildPastebinHelpTextExampleUsesTrigger(t *testing.T) {
	text := buildPastebinHelpText("dave", "dave:", Network{})
	assert.Contains(t, text, "dave:chat")
	assert.Contains(t, text, "dave:stop")
	assert.Contains(t, text, "<dave>")
}

func TestBuildPastebinHelpTextRegexMarker(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat.*", Service: "openai", Model: "gpt-4", Description: "chat"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "✱")
	assert.NotContains(t, text, "(regex)")
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if strings.Contains(line, "✱") {
			assert.Contains(t, line, "| ✱ |", "✱ should be in its own Regex column")
		}
	}
}

func TestBuildPastebinHelpTextPipeEscaped(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"img": {Name: "img", Regex: "img|image", Service: "openai", Model: "gpt-4", Description: "images"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	expected := "`!img" + `\|` + "image`"
	assert.Contains(t, text, expected)
	assert.NotContains(t, text, "| `!img|image`")
}

func TestBuildPastebinHelpTextDetectImagesCol(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat", Service: "openai", Model: "gpt-4o", DetectImages: true, Description: "vision chat"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "| `!chat` |  | openai | gpt-4o | 🖼️ | vision chat |")
}

func TestBuildPastebinHelpTextNoStopInExample(t *testing.T) {
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.NotContains(t, text, "Session paused")
}

func TestBuildPastebinHelpTextHistoryPipeEscaped(t *testing.T) {
	setupTestDB(t)
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "## History & Sessions")
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if strings.Contains(line, "sessions") && strings.Contains(line, "List sessions") {
			assert.Contains(t, line, `\|`, "pipe in [nick|*] should be escaped")
			assert.NotContains(t, line, "[nick|*]", "raw pipe should be escaped")
		}
		if strings.Contains(line, "clone") && strings.Contains(line, "Clone") {
			assert.Contains(t, line, `\|`, "pipe in [nick|id] should be escaped")
			assert.NotContains(t, line, "[nick|id]", "raw pipe should be escaped")
		}
	}
}

func TestBuildPastebinHelpTextNoRegexExplanation(t *testing.T) {
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.NotContains(t, text, "Commands marked with ✱ use pattern matching")
}

func TestBuildPastebinHelpTextDescPipeEscaped(t *testing.T) {
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Regex: "chat", Service: "openai", Model: "gpt-4", Description: "chat with user | group support"},
	}
	defer func() { config.Commands.Chats = nil }()
	text := buildPastebinHelpText("testbot", "!", Network{})
	assert.Contains(t, text, "&#124;")
	assert.NotContains(t, text, "| group support |")
}
