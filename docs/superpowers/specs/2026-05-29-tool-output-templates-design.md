# Tool Output Templates

## Summary

Add optional Go `text/template` formatting to sync MCP tool command output, so raw JSON results can be rendered as human-readable IRC messages. Includes a `table` template function for rendering arrays as box-drawn tables.

## Motivation

Tool commands (defined in `tools.toml`) currently output raw JSON from the MCP server. For example, `-queue` produces:

```
{"completed":4,"failed":0,"max_depth":100,"max_workers":1,"queued":0,"running":1,"running_jobs":[{"elapsed_seconds":3,"eta_seconds":56,"job_id":"d36035aa","workflow":"qwenHD"}]}
```

This is hard to read on IRC. A template would produce something like:

```
Queue: 0 queued, 1 running, 4 done
┌─────────┬──────────┬──────────────┐
│ job_id  │ workflow │ eta_seconds  │
├─────────┼──────────┼──────────────┤
│ d36035a │ qwenHD   │ 56           │
└─────────┴──────────┴──────────────┘
```

## Scope

- **In scope**: Sync tool command output formatting via Go templates, `table` template function
- **Out of scope**: Async delivery path templates (deferred), custom column headers/alignment, template functions beyond `table`

## Config

Add a `template` field to `MCPCommandConfig` (`config.go`):

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

- Optional field. Tools without `template` keep current raw text behavior (no change).
- Parsed at config load time via `text/template.New().Funcs(funcMap).Parse(...)`.
- Parse errors fail startup/reload (same policy as `SystemTmpl`).
- Stored as unexported `outputTmpl *template.Template` on `MCPCommandConfig`.

### New struct fields

```go
type MCPCommandConfig struct {
    // ... existing fields ...
    OutputTemplate string `toml:"template"`
    outputTmpl     *template.Template // parsed, unexported
}
```

No new data struct is needed — templates receive the parsed JSON `map[string]any` directly, with context keys injected.

### Config docs update

Add to `tools.toml` reference section:
```
#   template     (string)  Go text/template for formatting tool JSON output (sync only).
#                          Template data is the parsed JSON result as map[string]any.
#                          Available functions: table(slice, "col1,col2").
#                          Without this, raw text is sent to IRC.
```

## Template Data

The MCP tool result text is `json.Unmarshal`ed into `map[string]any` and passed directly as the template data. Context fields are injected into the same map with underscore-prefixed keys before template execution:

```go
data["_nick"] = nick
data["_channel"] = channel
data["_network"] = network
```

Templates access tool output at the top level (`{{.queued}}`, `{{.running_jobs}}`). Context fields are available as `{{._nick}}`, `{{._channel}}`, `{{._network}}`. The underscore-prefixed keys are reserved — if the tool JSON contains fields starting with `_`, they may be overwritten.

**JSON number handling**: `json.Unmarshal` into `map[string]any` produces `float64` for all numbers. Go's default `{{.field}}` prints `3` for `3.0` and `3.5` for `3.5`, which is acceptable. No special formatting helpers needed initially.

**IRC codes**: Templates can contain raw IRC formatting bytes (`\x02`, `\x03`, etc.) directly in the template string.

## Custom Template Functions

### `table(slice, columns) string`

Renders a JSON array of objects as a box-drawn table using the existing `MarkdownToIRC/tables` package.

- `slice` (`any`): Expected `[]any` from parsed JSON (array of objects). If nil/empty, returns `""`.
- `columns` (`string`): Comma-separated field names to extract from each object, e.g. `"job_id,workflow,eta_seconds"`.

**Behavior**:
1. Parses column names from the comma-separated string.
2. First row of `TableData.Rows` is the header row (field names used as headers), with `HeaderRowCount: 1`.
3. For each object in the slice, extracts each named field via type assertion on `map[string]any`, formats via `fmt.Sprintf("%v", val)`.
4. Renders via `tables.RenderTable(data)` with default max width (100).
5. Returns the rendered string (including leading newline from `RenderTable`).

**Error handling**: If `slice` is not `[]any` or individual items are not `map[string]any`, returns an error string like `(table: expected array of objects)`. Does not panic.

## Execution Flow

### Sync path (`mcpCmd` in `mcpCmds.go`)

After getting the MCP tool result:

1. `text := mcpToolResultToText(result)` — existing behavior.
2. If `cfg.outputTmpl != nil`:
   a. `json.Unmarshal([]byte(text), &data)` — parse into `map[string]any`.
   b. Inject context: `data["_nick"]`, `data["_channel"]`, `data["_network"]`.
   c. Execute template into `strings.Builder`.
   d. On execution error: log warning, fall back to raw text via `sendImageOrTextResult`.
   e. On success: `strings.TrimSpace` the output (handles leading/trailing newlines from TOML triple-quoted templates), then split by newlines and send each non-empty line to `output` channel.
3. If `cfg.outputTmpl == nil`: existing `sendImageOrTextResult(text, ...)` behavior (unchanged).

### Async path

No changes. Async job results continue using `sendImageOrTextResult` / `deliverToolAsyncResult`. Future work may extend templates to the async delivery path.

## Validation

At config load time, in the same location where tools are validated (inside `registerCommandsLocked` or a dedicated validation function):

1. If `OutputTemplate != ""`: parse with `text/template.New(name + "_output").Funcs(toolFuncMap).Parse(...)`.
2. Validate by executing against dummy data: `map[string]any{"example": "test", "items": []any{map[string]any{"field": "val"}}, "_nick": "test", "_channel": "#test", "_network": "test"}`.
3. Return error on parse or validation failure (fails startup/reload).

## Testing

- **Config validation**: Test that valid templates parse, invalid templates return errors. Table-driven tests with cases for missing fields, bad syntax, unknown functions.
- **Template execution**: Test rendering with various JSON shapes (scalars, arrays, nested objects). Verify IRC codes pass through. Verify context keys (`_nick`, `_channel`, `_network`) are available.
- **`table` function**: Test with empty slice, single row, multiple rows, missing fields, non-object items. Verify box-drawing output matches expected format.
- **Integration**: Test `mcpCmd` with a mock MCP server returning JSON, verify template output vs raw fallback.
- **No template**: Verify existing behavior unchanged when `template` field is absent.

## Files Changed

| File | Change |
|------|--------|
| `config.go` | Add `OutputTemplate`/`outputTmpl` fields, validation, template parsing |
| `mcpCmds.go` | Add template execution in sync path of `mcpCmd` |
| `mcpCmds_test.go` | Tests for template execution, fallback, table function |
| `config_test.go` | Tests for template validation |
| `config/tools.toml` | Add `template` to reference docs and commented example |
| `prod/tools.toml` | Add `template` to `[queue]` entry |
| `README.md` | Update tools section with `template` field docs |
