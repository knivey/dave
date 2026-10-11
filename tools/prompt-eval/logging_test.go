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
		Variant: "none", ToolVariant: "default", Temperature: "default", Scenario: "imageflow", DeliveryRole: "system", Run: 1,
		Requests: 7, Responses: 7, ReasoningChars: 4321, Fabricated: false,
		LeakSuspects: 1, Jobs: 2, Registers: 1,
	}))
	require.NoError(t, idx.add(indexRow{
		Variant: "broken", ToolVariant: "default", Temperature: "default", Scenario: "imageflow", DeliveryRole: "user", Run: 2,
		Requests: 7, Responses: 7, ReasoningChars: 2100, Fabricated: true,
		FabricatedURLs: 2, SilentTurns: 1,
	}))
	require.NoError(t, idx.add(indexRow{
		Variant: "dial", ToolVariant: "waitinstr", Temperature: "0.7", Retries: 2, Scenario: "prompts", DeliveryRole: "developer", Run: 12,
		Requests: 6, Responses: 6, ReasoningChars: 987, Fabricated: true,
		LeakSuspects: 3, LinklessFinals: 1, Jobs: 5, Registers: 2,
	}))
	require.NoError(t, idx.close())

	want := "variant,tool_variant,temperature,retries,scenario,delivery_role,run,requests,responses,reasoning_chars_total,fabricated,leak_suspects,fabricated_urls,linkless_finals,silent_turns,jobs,registers\n" +
		"none,default,default,0,imageflow,system,01,7,7,4321,false,1,0,0,0,2,1\n" +
		"broken,default,default,0,imageflow,user,02,7,7,2100,true,0,2,0,1,0,0\n" +
		"dial,waitinstr,0.7,2,prompts,developer,12,6,6,987,true,3,0,1,0,5,2\n"
	assert.Equal(t, want, buf.String())
}
