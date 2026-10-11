package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two pinned texts from the experiment brief, verbatim: the current
// prod register_background_job description (message-delivery mode, system
// role) and the waitinstr override that merges the wait-for-the-notice
// instruction into it. Duplicated here on purpose — if either text drifts
// in tools.go or toolvariants/, these literals force a conscious decision.
const wantProdRegisterDescription = `Register a background job for monitoring. When an async tool (e.g. generate_image_async) returns a job_id, call this to have the system monitor the job. You will be notified with the result in a system message when it completes. After calling this, always send the user a short message telling them the task is underway. Do not poll for status — the result arrives automatically.`

const wantWaitinstrRegisterDescription = `Register a background job for monitoring. When an async tool (e.g. generate_image_async) returns a job_id, call this to have the system monitor the job. You will be notified with the result in a system message when it completes. After calling this, always send the user a short message telling them the task is underway. Do not poll for status — the result arrives automatically as a notice beginning with "[System: Background task completed". NEVER write that notice yourself, and never invent an image link: real links only ever arrive inside that notice.`

func TestToolVariantFlagSet(t *testing.T) {
	var v toolVariantList
	require.NoError(t, v.Set("waitinstr=/tmp/waitinstr.json"))
	require.NoError(t, v.Set("other=/tmp/other.json"))
	require.Len(t, v, 2)
	assert.Equal(t, "waitinstr=/tmp/waitinstr.json, other=/tmp/other.json", v.String())

	for _, bad := range []string{"nope", "=x", "x=", "="} {
		assert.ErrorContains(t, v.Set(bad), "name=path", "flag value %q must be rejected", bad)
	}
}

func TestLoadToolVariants(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		return p
	}

	// No flags: the single synthetic default cell.
	tvs, err := loadToolVariants(nil)
	require.NoError(t, err)
	require.Len(t, tvs, 1)
	assert.Equal(t, "", tvs[0].name, "the synthetic default cell carries the zero name")
	assert.Empty(t, tvs[0].overrides)

	// Valid file: overrides round-trip under the sweep name.
	valid := write("valid.json", `{"register_background_job":"override desc","fetch":"other desc"}`)
	tvs, err = loadToolVariants([]toolVariantSpec{{name: "v", path: valid}})
	require.NoError(t, err)
	require.Len(t, tvs, 1)
	assert.Equal(t, "v", tvs[0].name)
	assert.Equal(t, map[string]string{"register_background_job": "override desc", "fetch": "other desc"}, tvs[0].overrides)

	// Unknown tool name: the error names the key.
	unknown := write("unknown.json", `{"fetch":"fine","register_backround_job":"typo"}`)
	_, err = loadToolVariants([]toolVariantSpec{{name: "u", path: unknown}})
	require.ErrorContains(t, err, "unknown tool name")
	assert.ErrorContains(t, err, "register_backround_job", "the error must name the offending tool")

	// Empty (and whitespace-only) descriptions.
	for _, tc := range []struct {
		name, body, wantKey string
	}{
		{"empty", `{"fetch":""}`, "fetch"},
		{"whitespace", `{"fetch":"   "}`, "fetch"},
	} {
		_, err = loadToolVariants([]toolVariantSpec{{name: tc.name, path: write(tc.name+".json", tc.body)}})
		require.ErrorContainsf(t, err, "is empty", "%s description must be rejected", tc.name)
		assert.ErrorContainsf(t, err, tc.wantKey, "%s: the error must name the tool", tc.name)
	}

	// Malformed JSON.
	bad := write("bad.json", `{"fetch": `)
	_, err = loadToolVariants([]toolVariantSpec{{name: "b", path: bad}})
	require.ErrorContains(t, err, "tool variant b", "the error names the variant")

	// Non-object JSON (an array of names, the plausible mistake).
	arr := write("arr.json", `["fetch"]`)
	_, err = loadToolVariants([]toolVariantSpec{{name: "a", path: arr}})
	assert.ErrorContains(t, err, "tool variant a")

	// Empty override object: a no-op variant can only be a mistake.
	none := write("none.json", `{}`)
	_, err = loadToolVariants([]toolVariantSpec{{name: "n", path: none}})
	assert.ErrorContains(t, err, "no overrides")

	// Missing file.
	_, err = loadToolVariants([]toolVariantSpec{{name: "m", path: filepath.Join(dir, "missing.json")}})
	assert.ErrorContains(t, err, "tool variant m")

	// Duplicate names and reserved/unsafe names.
	dup := write("dup.json", `{"fetch":"d"}`)
	_, err = loadToolVariants([]toolVariantSpec{{name: "d", path: dup}, {name: "d", path: dup}})
	assert.ErrorContains(t, err, "duplicate tool variant name")

	for _, name := range []string{"default", "a+b", "a/b", `a\b`} {
		_, err = loadToolVariants([]toolVariantSpec{{name: name, path: dup}})
		assert.ErrorContainsf(t, err, "tool variant name", "name %q must be rejected", name)
	}
}

func TestApplyToolVariant(t *testing.T) {
	// No overrides: the baseline advertisement comes back unchanged.
	got := applyToolVariant(advertisedTools, nil)
	assert.Equal(t, advertisedTools, got)

	overrides := map[string]string{"register_background_job": "NEW DESC"}
	got = applyToolVariant(advertisedTools, overrides)
	require.Len(t, got, len(advertisedTools))
	for i := range advertisedTools {
		assert.Equal(t, advertisedTools[i].Function.Name, got[i].Function.Name)
		assert.Equal(t, advertisedTools[i].Type, got[i].Type)
		if advertisedTools[i].Function.Name == "register_background_job" {
			assert.Equal(t, "NEW DESC", got[i].Function.Description)
		} else {
			assert.Equal(t, advertisedTools[i].Function.Description, got[i].Function.Description,
				"only the overridden tool's description may change")
		}
	}
	// The package-global advertisement itself is untouched.
	for _, td := range advertisedTools {
		if td.Function.Name == "register_background_job" {
			assert.Equal(t, wantProdRegisterDescription, td.Function.Description,
				"applyToolVariant must never mutate the shared baseline")
		}
	}
}

// TestToolVariantChangesOnlyToolDescriptions pins the experimental variable
// on the decoded wire: with a register override active, the requests differ
// from baseline in EXACTLY that one tool description — model, kwargs,
// messages, tool count, tool order, names, and every other description stay
// identical.
func TestToolVariantChangesOnlyToolDescriptions(t *testing.T) {
	run := func(tools []toolDef) []decodedRequest {
		sc := &script{responses: []string{
			cannedResp("", "planning", call("call_1", "enhance_and_generate_async", `{"prompt":"group portrait"}`)),
			cannedResp("one moment...", ""),
			cannedResp("behold!", ""),
			cannedResp("a corrected likeness is being prepared", ""),
			cannedResp("done", ""),
		}}
		s, _ := newTestSession(t, sc, 1, "testv", "imageflow")
		s.cfg.tools = tools
		require.NoError(t, s.runImageflow(1, phraseOffset(1, "testv", "imageflow")))
		return decodeRequests(t, sc.requests)
	}
	base := run(advertisedTools)
	over := run(applyToolVariant(advertisedTools, map[string]string{
		"register_background_job": wantWaitinstrRegisterDescription,
	}))
	require.NotEmpty(t, base)
	require.Len(t, over, len(base))

	for i := range base {
		b, o := base[i], over[i]
		assert.Equal(t, b.Model, o.Model, "request %d", i+1)
		assert.Equal(t, b.ToolChoice, o.ToolChoice, "request %d", i+1)
		assert.Equal(t, b.ReasoningEffort, o.ReasoningEffort, "request %d", i+1)
		assert.Equal(t, b.ChatTemplateKwargs, o.ChatTemplateKwargs, "request %d", i+1)
		assert.Equal(t, b.Messages, o.Messages, "request %d: message history must be untouched", i+1)

		require.Len(t, o.Tools, len(b.Tools), "request %d: tool count must not change", i+1)
		for j := range b.Tools {
			assert.Equal(t, b.Tools[j].Function.Name, o.Tools[j].Function.Name, "request %d tool %d", i+1, j)
			if b.Tools[j].Function.Name == "register_background_job" {
				assert.Equal(t, wantWaitinstrRegisterDescription, o.Tools[j].Function.Description,
					"request %d: the override must ride the advertised def", i+1)
				assert.NotEqual(t, b.Tools[j].Function.Description, o.Tools[j].Function.Description)
			} else {
				assert.Equal(t, b.Tools[j].Function.Description, o.Tools[j].Function.Description,
					"request %d tool %d: no other description may change", i+1, j)
			}
		}
	}
}

// TestRunOneToolVariantPath pins the output path rule and the CSV values:
// the variant dir is the bare <prompt-variant> when no tool variant is
// active and <prompt-variant>+<tool-variant> when one is, with no sibling
// cells created; the row records the prompt variant and the tool-variant
// label ("default" when the axis is absent).
func TestRunOneToolVariantPath(t *testing.T) {
	for _, tc := range []struct {
		toolVariant   string
		wantVariant   string
		wantToolLabel string
	}{
		{toolVariant: "", wantVariant: "testv", wantToolLabel: "default"}, // the axis absent
		{toolVariant: "waitinstr", wantVariant: "testv+waitinstr", wantToolLabel: "waitinstr"},
	} {
		t.Run(tc.wantToolLabel, func(t *testing.T) {
			outdir := t.TempDir()
			sc := &script{responses: []string{ // minimal no-tool imageflow shape
				cannedResp("i shall simply describe it...", ""),
				cannedResp("as delivered", ""),
				cannedResp("hmm, no tool again", ""),
				cannedResp("very well", ""),
			}}
			cfg := runConfig{
				model:           "test-model",
				tools:           advertisedTools,
				seed:            1,
				deliveryRole:    "system",
				toolVariantName: tc.toolVariant,
			}
			row, err := runOne(loadedVariant{name: "testv", text: "SYS PROMPT"}, scenarioImageflow, 1, cfg, sc.responder(), outdir)
			require.NoError(t, err)
			require.NotNil(t, row)

			assert.FileExists(t, filepath.Join(outdir, tc.wantVariant, "imageflow-system", "run-01.jsonl"),
				"the variant dir follows the <prompt>[+<tool>] rule")
			entries, err := os.ReadDir(outdir)
			require.NoError(t, err)
			require.Len(t, entries, 1, "exactly one variant directory per cell")
			assert.Equal(t, tc.wantVariant, entries[0].Name())

			assert.Equal(t, "testv", row.Variant, "the CSV variant column stays the bare prompt variant")
			assert.Equal(t, tc.wantToolLabel, row.ToolVariant, "the CSV tool_variant column")
		})
	}
}

// TestRunRejectsInvalidToolVariant pins the startup wiring: an unknown tool
// name inside a -tool-variant file fails run() before any directory is
// created or any request is made. flag state is swapped for a fresh FlagSet
// because run() registers on the package CommandLine.
func TestRunRejectsInvalidToolVariant(t *testing.T) {
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	oldUsage := flag.Usage
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
		flag.Usage = oldUsage
	})
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"register_backround_job":"typo"}`), 0o644))
	outdir := filepath.Join(t.TempDir(), "out")
	flag.CommandLine = flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = []string{"prompt-eval", "-tool-variant", "waitinstr=" + bad, "-outdir", outdir, "-runs", "1"}

	err := run()
	require.ErrorContains(t, err, "unknown tool name")
	require.ErrorContains(t, err, "register_backround_job", "the error must name the typo'd tool")
	assert.NoDirExists(t, outdir, "validation must fail before the outdir is created")
}

// TestRegisterBaselineIsProdDescription pins the advertised baseline: the
// harness must sell the model the CURRENT prod register text, not a stub.
func TestRegisterBaselineIsProdDescription(t *testing.T) {
	found := false
	for _, td := range advertisedTools {
		if td.Function.Name == "register_background_job" {
			found = true
			assert.Equal(t, wantProdRegisterDescription, td.Function.Description,
				"the baseline register description must be the prod text verbatim")
		}
	}
	require.True(t, found, "register_background_job must stay advertised")
}

// TestShippedWaitinstrToolVariant pins the shipped experiment file: it must
// load cleanly through the real loader and carry the exact brief text.
func TestShippedWaitinstrToolVariant(t *testing.T) {
	p := filepath.Join("toolvariants", "waitinstr.json")
	require.FileExists(t, p)
	tvs, err := loadToolVariants([]toolVariantSpec{{name: "waitinstr", path: p}})
	require.NoError(t, err, "the shipped experiment file must load cleanly")
	require.Len(t, tvs, 1)
	assert.Equal(t, map[string]string{"register_background_job": wantWaitinstrRegisterDescription},
		tvs[0].overrides, "the shipped file must carry the exact brief text")
	assert.NotEqual(t, wantProdRegisterDescription, wantWaitinstrRegisterDescription,
		"the experiment must actually differ from the baseline")
}
