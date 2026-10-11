package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONLWriterShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	w, err := newJSONLWriter(path)
	require.NoError(t, err)
	require.NoError(t, w.logExchange("request", []byte(`{"model":"m","messages":[]}`)))
	require.NoError(t, w.logExchange("response", []byte(`{"choices":[]}`)))
	require.NoError(t, w.Close())

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	require.Len(t, lines, 2)

	var e apiLogEntry
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &e))
	assert.Equal(t, "request", e.Type)
	assert.JSONEq(t, `{"model":"m","messages":[]}`, string(e.Body), "body must round-trip verbatim")
	_, err = time.Parse(time.RFC3339Nano, e.Ts)
	assert.NoError(t, err, "ts must be RFC3339Nano like dave's api logs")

	require.NoError(t, json.Unmarshal([]byte(lines[1]), &e))
	assert.Equal(t, "response", e.Type)
	assert.JSONEq(t, `{"choices":[]}`, string(e.Body))
}

func TestJSONLWriterWrapsNonJSONBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.jsonl")
	w, err := newJSONLWriter(path)
	require.NoError(t, err)
	require.NoError(t, w.logExchange("response", []byte("<html>bad gateway</html>")))
	require.NoError(t, w.Close())

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	require.Len(t, lines, 1)

	var e apiLogEntry
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &e))
	assert.Equal(t, "response", e.Type)
	assert.JSONEq(t, `{"non_json_body":"<html>bad gateway</html>"}`, string(e.Body),
		"a non-JSON body must be wrapped, never corrupt the file")
}

func TestRunIndexCSV(t *testing.T) {
	var buf bytes.Buffer
	idx := newRunIndex(&buf)
	require.NoError(t, idx.add(indexRow{
		Variant: "none", ToolVariant: "default", Scenario: "imageflow", DeliveryRole: "system", Run: 1,
		Requests: 7, Responses: 7, ReasoningChars: 4321, Fabricated: false,
	}))
	require.NoError(t, idx.add(indexRow{
		Variant: "broken", ToolVariant: "default", Scenario: "imageflow", DeliveryRole: "user", Run: 2,
		Requests: 7, Responses: 7, ReasoningChars: 2100, Fabricated: true,
	}))
	require.NoError(t, idx.add(indexRow{
		Variant: "dial", ToolVariant: "waitinstr", Scenario: "prompts", DeliveryRole: "developer", Run: 12,
		Requests: 6, Responses: 6, ReasoningChars: 987, Fabricated: true,
	}))
	require.NoError(t, idx.close())

	want := "variant,tool_variant,scenario,delivery_role,run,requests,responses,reasoning_chars_total,fabricated\n" +
		"none,default,imageflow,system,01,7,7,4321,false\n" +
		"broken,default,imageflow,user,02,7,7,2100,true\n" +
		"dial,waitinstr,prompts,developer,12,6,6,987,true\n"
	assert.Equal(t, want, buf.String())
}
