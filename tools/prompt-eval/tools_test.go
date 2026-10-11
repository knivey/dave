package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadEmbeddedSchemas decodes toolschemas.json independently of the loader,
// so wire/content assertions compare two unmarshal passes over the same
// embedded bytes rather than a value against itself.
func loadEmbeddedSchemas(t *testing.T) map[string]toolSchemaEntry {
	t.Helper()
	var entries map[string]toolSchemaEntry
	require.NoError(t, json.Unmarshal(toolschemasJSON, &entries))
	return entries
}

// requiredOf extracts a schema's required list as []string (nil when the
// key is absent — brave_place_search declares none).
func requiredOf(t *testing.T, params map[string]any) []string {
	t.Helper()
	raw, ok := params["required"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	require.Truef(t, ok, "required must be an array, got %T", raw)
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		require.Truef(t, ok, "required entries must be strings, got %T", v)
		out = append(out, s)
	}
	return out
}

func propertiesOf(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	props, ok := params["properties"].(map[string]any)
	require.Truef(t, ok, "parameters must carry a properties object, got %T", params["properties"])
	return props
}

// TestAdvertisedToolsAreProdFaithful is the table-wide guarantee: every
// advertised tool resolves to an entry with a non-empty description and an
// object parameter schema, in the harness's historic order, and every tool
// is type "function" (the wire shape dave sends).
func TestAdvertisedToolsAreProdFaithful(t *testing.T) {
	require.Len(t, advertisedTools, len(advertisedToolOrder))
	entries := loadEmbeddedSchemas(t)

	for i, name := range advertisedToolOrder {
		td := advertisedTools[i]
		assert.Equalf(t, name, td.Function.Name, "position %d must keep the advertised order", i)
		assert.Equal(t, "function", td.Type)

		entry, ok := entries[name]
		require.Truef(t, ok, "tool %q must have an embedded entry", name)
		assert.Equalf(t, entry.Description, td.Function.Description,
			"tool %q: the advertisement must carry the embedded description verbatim", name)
		assert.NotEmptyf(t, td.Function.Description, "tool %q: description must not be empty", name)

		require.NotNilf(t, td.Function.Parameters, "tool %q: parameters must not be nil", name)
		typ, _ := td.Function.Parameters["type"].(string)
		assert.Equalf(t, "object", typ, "tool %q: parameters must be an object schema", name)
	}
}

// TestRegisterSchemaSpotPin pins the register_background_job transcription
// against aiCmds.go's builtinTools def: the three declared properties with
// their types, and the required list naming job_id and tool_name (the
// diagnosed 1/137 artifact was the model being unable to see these).
func TestRegisterSchemaSpotPin(t *testing.T) {
	var params map[string]any
	found := false
	for _, td := range advertisedTools {
		if td.Function.Name == "register_background_job" {
			found = true
			params = td.Function.Parameters
		}
	}
	require.True(t, found)

	props := propertiesOf(t, params)
	for _, tc := range []struct{ name, typ string }{
		{"job_id", "string"},
		{"tool_name", "string"},
		{"server_name", "string"},
	} {
		prop, ok := props[tc.name].(map[string]any)
		require.Truef(t, ok, "property %q must be declared", tc.name)
		assert.Equalf(t, tc.typ, prop["type"], "property %q type", tc.name)
		assert.NotEmptyf(t, prop["description"], "property %q description", tc.name)
	}

	assert.Equal(t, []string{"job_id", "tool_name"}, requiredOf(t, params),
		"required must be exactly job_id+tool_name, in the prod def's order")
}

// TestEnhanceAndGenerateAsyncSpotPin pins the img-mcp async transcription:
// the prompt fields the model needs, and the _dave_inject_* properties
// ABSENT — dave's getMCPTools strips them from the model-facing schema
// (stripInjectFieldsFromSchema), so the harness matches the POST-STRIP
// advertisement exactly (controller ruling on the schema-fidelity report's
// concern 1: prod models never see these properties).
func TestEnhanceAndGenerateAsyncSpotPin(t *testing.T) {
	var params map[string]any
	found := false
	for _, td := range advertisedTools {
		if td.Function.Name == "enhance_and_generate_async" {
			found = true
			params = td.Function.Parameters
		}
	}
	require.True(t, found)

	props := propertiesOf(t, params)
	for _, name := range []string{"prompt", "enhancement", "workflow", "output_format"} {
		assert.Containsf(t, props, name, "property %q must be declared", name)
	}
	for _, name := range []string{
		"_dave_inject_network", "_dave_inject_channel", "_dave_inject_nick", "_dave_inject_llm_generated",
	} {
		assert.NotContainsf(t, props, name,
			"property %q must be absent — dave strips the inject fields from the advertised schema", name)
	}
	assert.NotContains(t, params, "_dave_inject", "no inject-prefixed key may appear anywhere in the schema")
	assert.Equal(t, []string{"prompt"}, requiredOf(t, params),
		"only prompt is required")
}

// TestBraveWebSearchSpotPin pins the diagnosed brave artifact's fix: the
// schema the MCP server actually declares requires query (13/21 empty-args
// calls happened under the permissive stub).
func TestBraveWebSearchSpotPin(t *testing.T) {
	var params map[string]any
	found := false
	for _, td := range advertisedTools {
		if td.Function.Name == "brave_web_search" {
			found = true
			params = td.Function.Parameters
		}
	}
	require.True(t, found)

	props := propertiesOf(t, params)
	for _, name := range []string{"query", "count", "offset", "safesearch", "freshness"} {
		assert.Containsf(t, props, name, "property %q must be declared", name)
	}
	assert.Contains(t, requiredOf(t, params), "query", "query must be required")
	assert.Equal(t, false, params["additionalProperties"],
		"the MCP-declared schema carries additionalProperties:false")
}

// TestQueueStatusSpotPin pins the queue_status transcription: the async-path
// img-mcp server declares an empty input struct, so the schema is a bare
// object with additionalProperties:false — no properties, no required.
func TestQueueStatusSpotPin(t *testing.T) {
	var params map[string]any
	found := false
	for _, td := range advertisedTools {
		if td.Function.Name == "queue_status" {
			found = true
			params = td.Function.Parameters
		}
	}
	require.True(t, found)

	assert.Equal(t, "object", params["type"])
	assert.Equal(t, false, params["additionalProperties"])
	assert.NotContains(t, params, "properties", "the declared input struct is empty")
	assert.NotContains(t, params, "required")
}

// TestNoAdvertisedToolCarriesInjectProperties pins the post-strip invariant
// across the WHOLE advertisement: dave's getMCPTools strips every
// _dave_inject_* property from every MCP tool's model-facing schema, so no
// advertised tool may carry one — not just the two img tools that declare
// them server-side.
func TestNoAdvertisedToolCarriesInjectProperties(t *testing.T) {
	for _, td := range advertisedTools {
		props, ok := td.Function.Parameters["properties"].(map[string]any)
		if !ok {
			continue
		}
		for name := range props {
			assert.NotContainsf(t, name, "_dave_inject_",
				"tool %q: inject properties are host-side only and must be stripped from the advertisement", td.Function.Name)
		}
	}
}

// TestWireCarriesEmbeddedSchemas pins the wire: every request the harness
// sends carries the embedded parameter schemas unchanged — same tools, same
// order, and each tool's parameters deep-equal the embedded entry (decoded
// through the request JSON, the existing decoded-request pattern, so a
// dropped required list or renamed property cannot slip through).
func TestWireCarriesEmbeddedSchemas(t *testing.T) {
	sc := &script{responses: []string{
		cannedResp("one moment...", ""),
	}}
	s, _ := newTestSession(t, sc, 1, "schemas", "imageflow")
	require.NoError(t, s.userTurn("draw something"))
	reqs := decodeRequests(t, sc.requests)
	require.NotEmpty(t, reqs)

	entries := loadEmbeddedSchemas(t)
	for i, r := range reqs {
		require.Lenf(t, r.Tools, len(advertisedToolOrder), "request %d must advertise every tool", i+1)
		for j, tool := range r.Tools {
			assert.Equalf(t, advertisedToolOrder[j], tool.Function.Name,
				"request %d tool %d: order must match the advertisement", i+1, j)
			entry, ok := entries[tool.Function.Name]
			require.Truef(t, ok, "request %d tool %d: unknown tool on the wire", i+1, j)
			assert.Equalf(t, entry.Description, tool.Function.Description,
				"request %d tool %s: description", i+1, tool.Function.Name)
			assert.Equalf(t, entry.Parameters, tool.Function.Parameters,
				"request %d tool %s: parameters must ride the wire unchanged", i+1, tool.Function.Name)
		}
	}
}

// TestLoadAdvertisedTools is the loader's error discipline: a table that is
// missing a tool, empty-described, non-object, or carrying an unadvertised
// extra (job_status!) must fail loudly — never degrade toward a stub.
func TestLoadAdvertisedTools(t *testing.T) {
	order := []string{"fetch", "register_background_job"}
	valid := `{
		"fetch": {
			"description": "Fetch a URL.",
			"parameters": {"type": "object", "properties": {"url": {"type": "string"}}, "required": ["url"]}
		},
		"register_background_job": {
			"description": "Register a background job.",
			"parameters": {"type": "object", "properties": {"job_id": {"type": "string"}}, "required": ["job_id"]}
		}
	}`

	tools, err := loadAdvertisedTools(order, []byte(valid))
	require.NoError(t, err)
	require.Len(t, tools, 2)
	assert.Equal(t, "fetch", tools[0].Function.Name)
	assert.Equal(t, "Fetch a URL.", tools[0].Function.Description)
	assert.Equal(t, []any{"url"}, tools[0].Function.Parameters["required"])

	for _, tc := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{"malformed json", `{"fetch": `, "parsing tool schema table"},
		{"missing entry", `{"fetch": {"description": "d", "parameters": {"type": "object"}}}`, "no entry for advertised tool \"register_background_job\""},
		{"empty description", `{"fetch": {"description": "  ", "parameters": {"type": "object"}}, "register_background_job": {"description": "d", "parameters": {"type": "object"}}}`, "empty description"},
		{"missing parameters", `{"fetch": {"description": "d"}, "register_background_job": {"description": "d", "parameters": {"type": "object"}}}`, "no parameters"},
		{"non-object parameters", `{"fetch": {"description": "d", "parameters": {"type": "string"}}, "register_background_job": {"description": "d", "parameters": {"type": "object"}}}`, "not an object schema"},
		{
			// job_status must stay absent (hidden in prod since session
			// 1898); an entry for it is dead data, not an advertisement.
			"unadvertised extra (job_status tripwire)",
			`{"fetch": {"description": "d", "parameters": {"type": "object"}}, "register_background_job": {"description": "d", "parameters": {"type": "object"}}, "job_status": {"description": "d", "parameters": {"type": "object"}}}`,
			"\"job_status\", which is not an advertised tool",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAdvertisedTools(order, []byte(tc.body))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestJobStatusStaysHidden pins the brief's absence rule end-to-end: the
// embedded table itself must not carry a job_status entry (the 1910
// reproduction must not teach the model that call exists).
func TestJobStatusStaysHidden(t *testing.T) {
	assert.NotContains(t, loadEmbeddedSchemas(t), "job_status")
	assert.NotContains(t, advertisedToolOrder, "job_status")
}
