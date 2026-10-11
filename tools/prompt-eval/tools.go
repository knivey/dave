package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// toolDef / toolFuncDef are the wire shape of one advertised function tool.
type toolDef struct {
	Type     string      `json:"type"`
	Function toolFuncDef `json:"function"`
}

type toolFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// toolschemasJSON is the embedded prod-faithful tool table: every advertised
// tool's description and parameter JSON Schema, keyed by tool name.
//
// TRANSCRIPTION SOURCES (verbatim — descriptions and schemas are the
// model-facing contract; a wrong property name is a silent fidelity bug):
//
//   - enhance_and_generate_async, generate_image_async, list_workflows,
//     queue_status — mcps/img-mcp: the async-path tool defs in server.go's
//     createAsyncServer, with parameter schemas as the MCP SDK infers them
//     from the *Input structs in tools.go (jsonschema-go reflection over the
//     json/jsonschema struct tags: omitempty fields are NOT required,
//     additionalProperties:false, pointer fields become ["null", <type>]
//     unions). The _dave_inject_* properties are kept exactly as declared —
//     host-injected provenance fields the MCP server accepts (omitempty, so
//     never in required; that omission is load-bearing, see the AGENTS.md
//     img-mcp gotcha).
//   - brave_* — mcps/brave-mcp/tools.go (registerTools defs + *Input
//     structs). Note brave_place_search declares NO required property
//     (query is omitempty there).
//   - fetch — mcps/fetch-mcp (main.go createServer def + FetchInput).
//   - get_transcript, get_video_info — mcps/yt-mcp (main.go createServer
//     defs + GetTranscriptInput/GetVideoInfoInput).
//   - register_background_job — aiCmds.go's builtinTools def. The
//     description is the CURRENT prod string: the message-delivery
//     getBuiltinToolDefs rendering with the default guidance role ("You
//     will be notified with the result in a system message..."), carrying
//     the wait instruction shipped Oct 2026. The static base def in
//     builtinTools ("...notified with the result when it completes...") is
//     unreachable in prod and NOT what is transcribed.
//
// job_status is deliberately absent — it has been hidden in prod since the
// polling incident (session 1898), and the harness must not teach the model
// that call exists. loadAdvertisedTools enforces absence structurally: every
// key in this file must be in advertisedToolOrder and vice versa, so a
// stray job_status entry fails startup instead of lurking as dead data.
//
// Re-sync when the sources change: the register fidelity test
// (TestRegisterBaselineIsProdDescription) is the only automated drift guard
// — the rest is transcription discipline.
//
//go:embed toolschemas.json
var toolschemasJSON []byte

// toolSchemaEntry is one advertised tool's prod-faithful description and
// parameter schema from toolschemas.json.
type toolSchemaEntry struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// advertisedToolOrder is the advertisement order (the stub table's historic
// order, preserved so sweeps stay comparable across the stub→schema switch).
var advertisedToolOrder = []string{
	"enhance_and_generate_async",
	"generate_image_async",
	"list_workflows",
	"queue_status",
	"brave_answers",
	"brave_image_search",
	"brave_llm_context",
	"brave_local_search",
	"brave_news_search",
	"brave_place_search",
	"brave_video_search",
	"brave_web_search",
	"fetch",
	"get_transcript",
	"get_video_info",
	"register_background_job",
}

// loadAdvertisedTools builds the tool advertisement from a schema table:
// one toolDef per name in order, each carrying its entry's description and
// parameters. Everything is validated loudly — an embedded table that is
// missing a tool, carries an unknown one (job_status!), has an empty
// description, or a non-object parameter schema is a transcription error
// that must fail, never silently degrade back toward a stub.
func loadAdvertisedTools(order []string, data []byte) ([]toolDef, error) {
	var entries map[string]toolSchemaEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing tool schema table: %w", err)
	}

	seen := make(map[string]bool, len(order))
	for _, name := range order {
		seen[name] = true
		entry, ok := entries[name]
		if !ok {
			return nil, fmt.Errorf("tool schema table has no entry for advertised tool %q", name)
		}
		if strings.TrimSpace(entry.Description) == "" {
			return nil, fmt.Errorf("tool schema entry %q has an empty description", name)
		}
		if entry.Parameters == nil {
			return nil, fmt.Errorf("tool schema entry %q has no parameters", name)
		}
		if typ, _ := entry.Parameters["type"].(string); typ != "object" {
			return nil, fmt.Errorf("tool schema entry %q parameters are not an object schema (type %q)", name, typ)
		}
	}

	// No extras: the order list IS the advertisement; an entry outside it
	// (job_status, say) is unadvertised dead data that belongs to no cell.
	for name := range entries {
		if !seen[name] {
			return nil, fmt.Errorf("tool schema table carries %q, which is not an advertised tool", name)
		}
	}

	tools := make([]toolDef, 0, len(order))
	for _, name := range order {
		entry := entries[name]
		tools = append(tools, toolDef{
			Type: "function",
			Function: toolFuncDef{
				Name:        name,
				Description: entry.Description,
				Parameters:  entry.Parameters,
			},
		})
	}
	return tools, nil
}

// advertisedTools is the prod-faithful advertisement: real descriptions and
// real parameter schemas for everything the prod bot offers the model
// (job_status stays hidden — see above). Loaded from the embedded table at
// init; a malformed table is a build-time programming error, so the panic
// (exercised through loadAdvertisedTools's error paths in tests) is the
// loudest possible failure.
var advertisedTools = mustLoadAdvertisedTools()

func mustLoadAdvertisedTools() []toolDef {
	tools, err := loadAdvertisedTools(advertisedToolOrder, toolschemasJSON)
	if err != nil {
		panic("prompt-eval: " + err.Error())
	}
	return tools
}
