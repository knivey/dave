# Tool Output Templates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional Go `text/template` formatting to sync MCP tool command output so raw JSON results render as human-readable IRC messages, with a `table` template function for rendering arrays as box-drawn tables.

**Architecture:** New `template` field on `MCPCommandConfig` parsed at config load time. At runtime, sync tool results are `json.Unmarshal`ed into `map[string]any`, context keys injected, template executed, output split by newlines and sent to IRC. Custom `table` function wraps the existing `MarkdownToIRC/tables` package.

**Tech Stack:** Go `text/template`, `encoding/json`, existing `MarkdownToIRC/tables` package, `github.com/stretchr/testify`

**Spec:** `docs/superpowers/specs/2026-05-29-tool-output-templates-design.md`

---

### Task 1: Add `table` template function

**Files:**
- Create: `templateFuncs.go` — new file for tool template function map and `table` implementation
- Test: `templateFuncs_test.go`

- [ ] **Step 1: Write failing tests for `table` template function**

Create `templateFuncs_test.go`:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTableFunc(t *testing.T) {
	tests := []struct {
		name     string
		slice    any
		columns  string
		want     string
		wantErr  bool
	}{
		{
			name:    "NilSlice",
			slice:   nil,
			columns: "a,b",
			want:    "",
		},
		{
			name:    "EmptySlice",
			slice:   []any{},
			columns: "a,b",
			want:    "",
		},
		{
			name:    "NotSlice",
			slice:   "not a slice",
			columns: "a",
			wantErr: true,
		},
		{
			name: "SingleRow",
			slice: []any{
				map[string]any{"name": "alice", "age": float64(30)},
			},
			columns: "name,age",
			want:    "\n┌───────┬─────┐\n│ name  │ age │\n├───────┼─────┤\n│ alice │ 30  │\n└───────┴─────┘",
		},
		{
			name: "MultipleRows",
			slice: []any{
				map[string]any{"job_id": "abc", "status": "running"},
				map[string]any{"job_id": "def", "status": "done"},
			},
			columns: "job_id,status",
			want:    "\n┌────────┬─────────┐\n│ job_id │ status  │\n├────────┼─────────┤\n│ abc    │ running │\n│ def    │ done    │\n└────────┴─────────┘",
		},
		{
			name: "MissingField",
			slice: []any{
				map[string]any{"name": "alice"},
			},
			columns: "name,missing",
			want:    "\n┌───────┬─────────┐\n│ name  │ missing │\n├───────┼─────────┤\n│ alice │         │\n└───────┴─────────┘",
		},
		{
			name: "ItemNotMap",
			slice: []any{"not a map"},
			columns: "a",
			wantErr: true,
		},
		{
			name: "Float64Values",
			slice: []any{
				map[string]any{"eta": float64(56), "count": float64(0)},
			},
			columns: "eta,count",
			want:    "\n┌─────┬───────┐\n│ eta │ count │\n├─────┼───────┤\n│ 56  │ 0     │\n└─────┴───────┘",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tableFunc(tt.slice, tt.columns)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestTableFunc -v`
Expected: FAIL — `tableFunc` undefined

- [ ] **Step 3: Implement `tableFunc` and function map**

Create `templateFuncs.go`:

```go
package main

import (
	"fmt"
	"strings"

	"github.com/knivey/dave/MarkdownToIRC/tables"
)

func tableFunc(slice any, columns string) (string, error) {
	items, ok := slice.([]any)
	if !ok {
		return "", fmt.Errorf("table: expected array, got %T", slice)
	}
	if len(items) == 0 {
		return "", nil
	}

	colNames := strings.Split(columns, ",")
	for i := range colNames {
		colNames[i] = strings.TrimSpace(colNames[i])
	}

	headerRow := make(tables.TableRow, len(colNames))
	for i, name := range colNames {
		headerRow[i] = tables.TableCell{Text: name}
	}

	var rows []tables.TableRow
	rows = append(rows, headerRow)

	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return "", fmt.Errorf("table: expected object, got %T", item)
		}
		row := make(tables.TableRow, len(colNames))
		for i, name := range colNames {
			val := obj[name]
			row[i] = tables.TableCell{Text: fmt.Sprintf("%v", val)}
		}
		rows = append(rows, row)
	}

	return tables.RenderTable(tables.TableData{
		Rows:           rows,
		HeaderRowCount: 1,
	}), nil
}

var toolTemplateFuncMap = map[string]any{
	"table": tableFunc,
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestTableFunc -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add templateFuncs.go templateFuncs_test.go
git commit -m "feat: add table template function for tool output formatting"
```

---

### Task 2: Add `OutputTemplate` field and config validation

**Files:**
- Modify: `config.go:208-220` — add fields to `MCPCommandConfig`
- Modify: `config.go:865-884` — add template parsing/validation in tools loop
- Test: `config_test.go`

- [ ] **Step 1: Write failing tests for template validation**

Add to `config_test.go`:

```go
func TestToolOutputTemplateValidation(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr bool
		errMsg  string
	}{
		{
			name: "ValidTemplate",
			toml: `
[tools.test]
mcp = "srv"
tool = "my_tool"
sync = true
template = "Result: {{.count}}"
`,
			wantErr: false,
		},
		{
			name: "ValidTemplateWithTable",
			toml: `
[tools.test]
mcp = "srv"
tool = "my_tool"
sync = true
template = "{{table .items \"name,age\"}}"
`,
			wantErr: false,
		},
		{
			name: "InvalidTemplateSyntax",
			toml: `
[tools.test]
mcp = "srv"
tool = "my_tool"
sync = true
template = "{{.unclosed"
`,
			wantErr: true,
			errMsg:  "template parse error",
		},
		{
			name: "UnknownFunction",
			toml: `
[tools.test]
mcp = "srv"
tool = "my_tool"
sync = true
template = "{{.data | unknownFunc}}"
`,
			wantErr: true,
			errMsg:  "template parse error",
		},
		{
			name: "NoTemplate",
			toml: `
[tools.test]
mcp = "srv"
tool = "my_tool"
sync = true
`,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := createTestConfigDir(t, map[string]string{
				"config.toml": "trigger = \"!\"\n[networks.test]\nserver = \"irc.test.net\"\nchannels = [\"#test\"]",
				"mcps.toml":   "[srv]\ntransport = \"stdio\"\ncommand = \"echo\"\ntimeout = \"30s\"",
				"tools.toml":  tt.toml,
			})
			var cfg Config
			err := loadConfigDirOrPanic(dir, &cfg)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
				return
			}
			assert.NoError(t, err)
		})
	}
}
```

Note: `createTestConfigDir` is the existing helper in `config_test.go` that creates a temp dir with the given files. If `loadConfigDirOrPanic` is not suitable (it calls `os.Exit`), use the non-fatal path directly via `loadConfigDir`. Check which is appropriate — `loadConfigDir` returns errors without exiting. The test should use whichever function the existing config tests use.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestToolOutputTemplateValidation -v`
Expected: FAIL — template field not parsed/validated yet

- [ ] **Step 3: Add `OutputTemplate` and `outputTmpl` fields to `MCPCommandConfig`**

In `config.go`, modify the `MCPCommandConfig` struct (around line 208):

```go
type MCPCommandConfig struct {
	Name           string
	Regex          string
	MCP            string              `toml:"mcp"`
	Tool           string              `toml:"tool"`
	Arg            string              `toml:"arg"`
	Args           map[string]any      `toml:"args"`
	Timeout        time.Duration       `toml:"timeout"`
	SkipBusy       bool                `toml:"skipbusy"`
	Description    string
	Sync           bool                `toml:"sync"`
	AsyncTool      string              `toml:"async_tool"`
	OutputTemplate string              `toml:"template"`
	outputTmpl     *template.Template  // parsed, unexported
}
```

The `"text/template"` import already exists in `config.go`.

- [ ] **Step 4: Add template parsing/validation in the tools validation loop**

In `config.go`, after line 882 (`if cfg.Tool == "" { ... }`) and before `commands.Tools[name] = cfg` (line 883), add:

```go
		if cfg.OutputTemplate != "" {
			tmpl, err := template.New(name + "_output").Funcs(toolTemplateFuncMap).Parse(cfg.OutputTemplate)
			if err != nil {
				return fmt.Errorf("commands.tools.%s template parse error: %w", name, err)
			}
			dummyData := map[string]any{
				"example": "test",
				"items":   []any{map[string]any{"field": "val"}},
				"_nick":   "test",
				"_channel": "#test",
				"_network": "test",
			}
			var buf strings.Builder
			if err := tmpl.Execute(&buf, dummyData); err != nil {
				return fmt.Errorf("commands.tools.%s template validation error: %w", name, err)
			}
			cfg.outputTmpl = tmpl
		}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -run TestToolOutputTemplateValidation -v`
Expected: PASS

- [ ] **Step 6: Run full test suite**

Run: `go test ./...`
Expected: All tests pass

- [ ] **Step 7: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: add output template field and validation to MCPCommandConfig"
```

---

### Task 3: Add template execution to `mcpCmd` sync path

**Files:**
- Modify: `mcpCmds.go:76-82` — add template execution between result text extraction and output
- Test: `mcpCmds_test.go` — new test file

- [ ] **Step 1: Write failing tests for template execution in `mcpCmd`**

Create `mcpCmds_test.go`:

```go
package main

import (
	"context"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMcpCmd_TemplateExecution(t *testing.T) {
	origMap := mcpToolToServer
	defer func() { mcpToolToServer = origMap }()

	tests := []struct {
		name           string
		template       string
		toolResult     string
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:       "SimpleTemplate",
			template:   "Queued: {{.queued}}, Running: {{.running}}",
			toolResult: `{"queued": 0, "running": 1}`,
			wantContains: []string{"Queued: 0, Running: 1"},
		},
		{
			name:       "TemplateWithTable",
			template:   "Jobs:{{table .jobs \"id,status\"}}",
			toolResult: `{"jobs": [{"id": "abc", "status": "running"}]}`,
			wantContains: []string{"abc", "running", "│"},
		},
		{
			name:       "TemplateWithContext",
			template:   "Nick: {{._nick}}, Channel: {{._channel}}",
			toolResult: `{"ok": true}`,
			wantContains: []string{"Nick: testnick", "Channel: #test"},
		},
		{
			name:       "TemplateTrimSpace",
			template:   "\n\nResult: {{.count}}\n\n",
			toolResult: `{"count": 5}`,
			wantContains: []string{"Result: 5"},
		},
		{
			name:       "TemplateEmptyArray",
			template:   "Queue: {{.queued}}{{if .jobs}}{{table .jobs \"id\"}}{{end}}",
			toolResult: `{"queued": 0, "jobs": []}`,
			wantContains: []string{"Queue: 0"},
			wantNotContain: []string{"│"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := template.New("test_output").Funcs(toolTemplateFuncMap).Parse(tt.template)
			require.NoError(t, err)

			mcpToolToServer = map[string]string{"test_tool": "test_server"}

			cfg := MCPCommandConfig{
				Name:           "test",
				Regex:          "test",
				MCP:            "test_server",
				Tool:           "test_tool",
				Sync:           true,
				OutputTemplate: tt.template,
				outputTmpl:     tmpl,
				Timeout:        5 * time.Second,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			output := make(chan string, 10)

			e := girc.Event{
				Params: []string{"#test"},
				Source: &girc.Source{Name: "testnick"},
			}
			network := Network{Name: "testnet"}

			// We can't easily call mcpCmd directly because it calls callMCPToolWithTimeoutContext
			// which hits the real MCP client. Instead, test the template execution path directly.
			var data map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.toolResult), &data))

			data["_nick"] = "testnick"
			data["_channel"] = "#test"
			data["_network"] = "testnet"

			var buf strings.Builder
			_, err = executeToolTemplate(tmpl, data, &buf)

			result := strings.TrimSpace(buf.String())

			for _, want := range tt.wantContains {
				assert.Contains(t, result, want, "expected to contain %q", want)
			}
			for _, notWant := range tt.wantNotContain {
				assert.NotContains(t, result, notWant, "expected NOT to contain %q", notWant)
			}
		})
	}
}

func TestMcpCmd_TemplateFallbackOnError(t *testing.T) {
	tmpl, err := template.New("bad").Funcs(toolTemplateFuncMap).Parse("{{.foo.Bar}}")
	require.NoError(t, err)

	data := map[string]any{"foo": "not a struct"}
	var buf strings.Builder
	fallback, err := executeToolTemplate(tmpl, data, &buf)

	assert.Error(t, err)
	assert.True(t, fallback)
}
```

Note: The test calls `executeToolTemplate` which is a helper function we'll extract. It returns `(bool, error)` where bool indicates whether to fall back to raw text output.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestMcpCmd_Template -v`
Expected: FAIL — `executeToolTemplate` undefined

- [ ] **Step 3: Implement `executeToolTemplate` helper and modify `mcpCmd`**

Add to `mcpCmds.go`:

```go
func executeToolTemplate(tmpl *template.Template, data map[string]any, buf *strings.Builder) (bool, error) {
	if err := tmpl.Execute(buf, data); err != nil {
		return true, err
	}
	return false, nil
}
```

Then modify `mcpCmd` in `mcpCmds.go`. Replace lines 76-82 (the block after `text := mcpToolResultToText(result)`):

Current code (lines 76-82):
```go
	text := mcpToolResultToText(result)
	if result.IsError {
		sendOrDone(ctx, output, errorMsg(text))
		return
	}

	sendImageOrTextResult(text, ctx, output)
```

Replace with:
```go
	text := mcpToolResultToText(result)
	if result.IsError {
		sendOrDone(ctx, output, errorMsg(text))
		return
	}

	if cfg.outputTmpl != nil {
		var data map[string]any
		if err := json.Unmarshal([]byte(text), &data); err != nil {
			log.Warn("failed to parse tool result as JSON for template, sending raw", "error", err)
			sendImageOrTextResult(text, ctx, output)
			return
		}
		data["_nick"] = nick
		data["_channel"] = channel
		data["_network"] = network.Name
		var buf strings.Builder
		fallback, err := executeToolTemplate(cfg.outputTmpl, data, &buf)
		if fallback || err != nil {
			log.Warn("template execution failed, sending raw result", "error", err)
			sendImageOrTextResult(text, ctx, output)
			return
		}
		rendered := strings.TrimSpace(buf.String())
		for _, line := range strings.Split(rendered, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				if !sendOrDone(ctx, output, line) {
					return
				}
			}
		}
		return
	}

	sendImageOrTextResult(text, ctx, output)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestMcpCmd_Template -v`
Expected: PASS

- [ ] **Step 5: Run full test suite**

Run: `go test ./...`
Expected: All tests pass

- [ ] **Step 6: Commit**

```bash
git add mcpCmds.go mcpCmds_test.go
git commit -m "feat: execute output templates on sync tool command results"
```

---

### Task 4: Update config docs and prod config

**Files:**
- Modify: `config/tools.toml` — add `template` to reference section and commented example
- Modify: `prod/tools.toml` — add `template` to `[queue]` entry
- Modify: `README.md` — update tools section

- [ ] **Step 1: Update `config/tools.toml` reference section**

Add after line 14 (`#   async_tool ...`):

```toml
#   template     (string)            Go text/template for formatting tool JSON output (sync only).
#                          Available functions: table(slice, "col1,col2") renders a box-drawn table.
#                          Template data is parsed JSON as map[string]any.
#                          Context: ._nick, ._channel, ._network.
#                          Without this, raw text is sent to IRC.
```

Update the commented example section (around line 26-36) to include template:

```toml
# [example-tool]
# description = "Example tool command"
# mcp = "my-mcp-server"
# tool = "my_tool"
# arg = "input"
# args = { format = "json", verbose = true }
# timeout = "1m"
# skipbusy = false
# regex = "example"
# sync = false
# async_tool = "my_tool_async"
# template = "Result: {{.count}} items"
```

- [ ] **Step 2: Update `prod/tools.toml` `[queue]` entry**

Replace:
```toml
[queue]
description = "View generation queue status"
mcp = "img-mcp"
tool = "queue_status"
skipbusy = true
sync = true
```

With:
```toml
[queue]
description = "View generation queue status"
mcp = "img-mcp"
tool = "queue_status"
skipbusy = true
sync = true
template = """Queue: {{.queued}} queued, {{.running}} running, {{.completed}} done{{if .running_jobs}}
{{table .running_jobs "job_id,workflow,eta_seconds"}}{{end}}"""
```

- [ ] **Step 3: Update README.md tools section**

Find the tools table in README.md and add a row for `template`. Find the tools config example and add `template` field.

- [ ] **Step 4: Run `go fmt` and `go vet`**

Run: `go fmt ./... && go vet ./...`
Expected: No output (clean)

- [ ] **Step 5: Commit**

```bash
git add config/tools.toml prod/tools.toml README.md
git commit -m "docs: add template field to tools config docs and prod queue command"
```

---

### Task 5: Final verification

- [ ] **Step 1: Run full test suite**

Run: `go test ./...`
Expected: All tests pass

- [ ] **Step 2: Run `go fmt` and `go vet`**

Run: `go fmt ./... && go vet ./...`
Expected: Clean

- [ ] **Step 3: Build**

Run: `go build -o dave .`
Expected: Success
