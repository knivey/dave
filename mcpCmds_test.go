package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteToolTemplate(t *testing.T) {
	tests := []struct {
		name         string
		template     string
		toolResult   string
		wantContains []string
	}{
		{
			name:         "SimpleTemplate",
			template:     "Queued: {{.queued}}, Running: {{.running}}",
			toolResult:   `{"queued": 0, "running": 1}`,
			wantContains: []string{"Queued: 0, Running: 1"},
		},
		{
			name:         "TemplateWithTable",
			template:     "Jobs:{{table .jobs \"id,status\"}}",
			toolResult:   `{"jobs": [{"id": "abc", "status": "running"}]}`,
			wantContains: []string{"abc", "running"},
		},
		{
			name:         "TemplateWithContext",
			template:     "Nick: {{._nick}}, Channel: {{._channel}}",
			toolResult:   `{"ok": true}`,
			wantContains: []string{"Nick: testnick", "Channel: #test"},
		},
		{
			name:         "TemplateTrimSpace",
			template:     "\n\nResult: {{.count}}\n\n",
			toolResult:   `{"count": 5}`,
			wantContains: []string{"Result: 5"},
		},
		{
			name:         "TemplateEmptyArray",
			template:     "Queue: {{.queued}}{{if .jobs}}{{table .jobs \"id\"}}{{end}}",
			toolResult:   `{"queued": 0, "jobs": []}`,
			wantContains: []string{"Queue: 0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := template.New("test").Funcs(toolTemplateFuncMap).Parse(tt.template)
			require.NoError(t, err)

			var data map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.toolResult), &data))

			data["_nick"] = "testnick"
			data["_channel"] = "#test"
			data["_network"] = "testnet"

			var buf strings.Builder
			err = executeToolTemplate(tmpl, data, &buf)
			assert.NoError(t, err)

			result := strings.TrimSpace(buf.String())
			for _, want := range tt.wantContains {
				assert.Contains(t, result, want, "expected to contain %q", want)
			}
		})
	}
}

func TestExecuteToolTemplate_FallbackOnError(t *testing.T) {
	tmpl, err := template.New("bad").Funcs(toolTemplateFuncMap).Parse("{{.foo.Bar}}")
	require.NoError(t, err)

	data := map[string]any{"foo": "not a struct"}
	var buf strings.Builder
	err = executeToolTemplate(tmpl, data, &buf)

	assert.Error(t, err)
}

// TestMcpCmdUnresolvableImageRefAborts pins the direct tools.toml
// interception wiring: both mcpCmd's shared funnel (the resolveImageRefArgsFn
// call after injectScopeArgs, which every command — sync AND async — passes
// through) and mcpCmdAsync's own copy of the block. A cfg.Args input_images
// entry holding an unresolvable usr_ ref must abort the command BEFORE any
// MCP call is made, surfacing the Tools.Failed error notice naming the dead
// ref. The REAL ref store is exercised (setupImageRefTest with nothing
// registered, so the lookup genuinely fails) — no seam override — matching
// the LLM-path pin TestExecuteToolCallsAbortsUnresolvableRef.
//
// "No call made" is observed via output shape: exactly one line, the abort
// notice. If the interception were missing, callMCPToolWithTimeoutContext
// would run against the empty stub server registered by
// registeredTestMCPToolName and add a second, ref-less error notice (or
// worse) before returning.
func TestMcpCmdUnresolvableImageRefAborts(t *testing.T) {
	setupImageRefTest(t)
	setupNoticesDefaults(t)

	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})
	e := girc.Event{Command: girc.PRIVMSG, Source: &girc.Source{Name: "shrew"}, Params: []string{"#test"}}
	deadRefArgs := func() map[string]any {
		return map[string]any{"input_images": []any{"usr_deadbeefcafe"}}
	}

	assertAbortNotice := func(t *testing.T, lines []string) {
		t.Helper()
		require.NotEmpty(t, lines, "expected the abort notice on the output channel")
		require.Len(t, lines, 1, "exactly one message — the abort notice; a made MCP call would add output, got %q", lines)
		assert.Contains(t, lines[0], "usr_deadbeefcafe", "notice names the dead ref")
		assert.Contains(t, lines[0], "MCP tool call failed", "uses the Tools.Failed notice")
	}

	t.Run("sync command", func(t *testing.T) {
		tool := registeredTestMCPToolName(t)
		cfg := MCPCommandConfig{
			Name:    "editdead",
			MCP:     "serverRefArgs",
			Tool:    tool,
			Sync:    true,
			Timeout: 5 * time.Second,
			Args:    deadRefArgs(),
		}
		outputCh := make(chan string, 4)

		mcpCmd(Network{Name: "testnet"}, client, e, cfg, context.Background(), outputCh)

		assertAbortNotice(t, drainOutput(t, outputCh, 4, time.Second))
	})

	t.Run("async command aborts at the shared funnel", func(t *testing.T) {
		tool := registeredTestMCPToolName(t)
		cfg := MCPCommandConfig{
			Name:      "editdead",
			MCP:       "serverRefArgs",
			Tool:      tool,
			AsyncTool: tool,
			Sync:      false,
			Timeout:   5 * time.Second,
			Args:      deadRefArgs(),
		}
		outputCh := make(chan string, 4)

		mcpCmd(Network{Name: "testnet"}, client, e, cfg, context.Background(), outputCh)

		// Sync = false never reaches mcpCmdAsync here: the shared funnel at
		// the top of mcpCmd aborts first — that IS the direct async wiring.
		assertAbortNotice(t, drainOutput(t, outputCh, 4, time.Second))
	})

	t.Run("async handoff block", func(t *testing.T) {
		tool := registeredTestMCPToolName(t)
		cfg := MCPCommandConfig{
			Name:      "editdead",
			MCP:       "serverRefArgs",
			Tool:      tool,
			AsyncTool: tool,
			Sync:      false,
			Timeout:   5 * time.Second,
		}
		outputCh := make(chan string, 4)

		// Drive mcpCmdAsync directly (what mcpCmd hands off to once its own
		// interception passes) so the async copy of the block is pinned too.
		mcpCmdAsync(Network{Name: "testnet"}, cfg, context.Background(), outputCh,
			deadRefArgs(), "", "#test", "shrew", 1, newLogger("test.mcp.editdead"))

		assertAbortNotice(t, drainOutput(t, outputCh, 4, time.Second))
	})
}
