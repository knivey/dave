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

func TestLoadVariants(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "one.txt")
	p2 := filepath.Join(dir, "two.txt")
	require.NoError(t, os.WriteFile(p1, []byte("prompt one"), 0o644))
	require.NoError(t, os.WriteFile(p2, []byte("prompt two"), 0o644))

	vs, err := loadVariants([]variantSpec{{name: "one", path: p1}, {name: "two", path: p2}})
	require.NoError(t, err)
	require.Len(t, vs, 2)
	assert.Equal(t, loadedVariant{name: "one", text: "prompt one"}, vs[0])
	assert.Equal(t, loadedVariant{name: "two", text: "prompt two"}, vs[1])

	_, err = loadVariants([]variantSpec{{name: "one", path: p1}, {name: "one", path: p2}})
	assert.ErrorContains(t, err, "duplicate variant name")

	_, err = loadVariants([]variantSpec{{name: "x", path: filepath.Join(dir, "missing.txt")}})
	assert.ErrorContains(t, err, "variant x")

	empty := filepath.Join(dir, "empty.txt")
	require.NoError(t, os.WriteFile(empty, []byte("   \n"), 0o644))
	_, err = loadVariants([]variantSpec{{name: "e", path: empty}})
	assert.ErrorContains(t, err, "is empty")
}

func TestVariantFlagSet(t *testing.T) {
	var v variantList
	require.NoError(t, v.Set("none=/tmp/none.txt"))
	require.NoError(t, v.Set("broken=/tmp/broken.txt"))
	require.Len(t, v, 2)
	assert.Equal(t, "none=/tmp/none.txt, broken=/tmp/broken.txt", v.String())

	assert.ErrorContains(t, v.Set("nope"), "name=path")
	assert.ErrorContains(t, v.Set("=x"), "name=path")
	assert.ErrorContains(t, v.Set("x="), "name=path")
}

func TestShippedVariants(t *testing.T) {
	texts := map[string]string{}
	for _, n := range []string{"none", "dial", "broken"} {
		b, err := os.ReadFile(filepath.Join("variants", n+".txt"))
		require.NoError(t, err, "shipped variant %s.txt must exist", n)
		texts[n] = string(b)
		assert.NotEmpty(t, texts[n])
	}
	assert.NotEqual(t, texts["none"], texts["dial"])
	assert.NotEqual(t, texts["none"], texts["broken"])
	assert.NotEqual(t, texts["dial"], texts["broken"])

	// Every variant carries the shared base scaffolding.
	for _, n := range []string{"none", "dial", "broken"} {
		assert.Contains(t, texts[n], "You are davebird, one of the user's scheming eunuchs.")
		assert.Contains(t, texts[n], "You can use /me <action text>")
		assert.Contains(t, texts[n], "/め")
	}
	// The variants differ exactly in the guidance slot.
	assert.Contains(t, texts["dial"], "Think briefly and efficiently — keep internal reasoning short")
	assert.Contains(t, texts["broken"], "### COGNITIVE ROUTING & TOOL RULES")
	assert.NotContains(t, texts["none"], "COGNITIVE ROUTING")
	assert.NotContains(t, texts["none"], "Think briefly")
}

func TestParseScenarios(t *testing.T) {
	got, err := parseScenarios("imageflow, prompts,research")
	require.NoError(t, err)
	assert.Equal(t, []string{"imageflow", "prompts", "research"}, got)

	got, err = parseScenarios("prompts,prompts,imageflow")
	require.NoError(t, err)
	assert.Equal(t, []string{"prompts", "imageflow"}, got, "duplicates collapse")

	_, err = parseScenarios("imageflow,nonsense")
	assert.ErrorContains(t, err, "unknown scenario")

	_, err = parseScenarios(" , ")
	assert.ErrorContains(t, err, "selected nothing")
}

func TestValidateDeliveryRole(t *testing.T) {
	for _, role := range []string{"system", "user", "developer"} {
		assert.NoErrorf(t, validateDeliveryRole(role), "role %q must be accepted", role)
	}
	for _, role := range []string{"", "System", "SYSTEM", "supervisor", "tool", " assistant"} {
		assert.ErrorContainsf(t, validateDeliveryRole(role), "-delivery-role", "role %q must be rejected", role)
	}
}

// TestRunRejectsInvalidDeliveryRole pins the startup wiring: an invalid
// -delivery-role fails run() before any directory is created or any
// request is made. flag state is swapped for a fresh FlagSet because run()
// registers on the package CommandLine.
func TestRunRejectsInvalidDeliveryRole(t *testing.T) {
	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	oldUsage := flag.Usage
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
		flag.Usage = oldUsage
	})
	outdir := filepath.Join(t.TempDir(), "out")
	flag.CommandLine = flag.NewFlagSet(t.Name(), flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	os.Args = []string{"prompt-eval", "-delivery-role", "supervisor", "-outdir", outdir, "-runs", "1"}

	err := run()
	require.ErrorContains(t, err, "-delivery-role")
	require.ErrorContains(t, err, "supervisor")
	assert.NoDirExists(t, outdir, "validation must fail before the outdir is created")
}

// TestRunOneScenarioDirIncludesDeliveryRole pins the output path shape:
// the scenario dir is ALWAYS suffixed with the delivery role (default
// included) so separate invocations cannot collide on run-NN.jsonl, and
// the row the index receives carries the bare scenario plus the role.
func TestRunOneScenarioDirIncludesDeliveryRole(t *testing.T) {
	for _, tc := range []struct {
		role    string
		wantDir string
	}{
		{role: "system", wantDir: "imageflow-system"}, // the flag default
		{role: "user", wantDir: "imageflow-user"},
		{role: "developer", wantDir: "imageflow-developer"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			outdir := t.TempDir()
			sc := &script{responses: []string{ // minimal no-tool imageflow shape
				cannedResp("i shall simply describe it...", ""),
				cannedResp("as delivered", ""),
				cannedResp("hmm, no tool again", ""),
				cannedResp("very well", ""),
			}}
			cfg := runConfig{
				model:        "test-model",
				tools:        advertisedTools,
				seed:         1,
				deliveryRole: tc.role,
			}
			row, err := runOne(loadedVariant{name: "testv", text: "SYS PROMPT"}, scenarioImageflow, 1, cfg, sc.responder(), outdir)
			require.NoError(t, err)
			require.NotNil(t, row)

			assert.FileExists(t, filepath.Join(outdir, "testv", tc.wantDir, "run-01.jsonl"),
				"the scenario dir must be suffixed with the delivery role")
			assert.NoDirExists(t, filepath.Join(outdir, "testv", scenarioImageflow),
				"the unsuffixed scenario dir must never be created")
			assert.Equal(t, scenarioImageflow, row.Scenario, "the CSV scenario column stays the bare name")
			assert.Equal(t, tc.role, row.DeliveryRole)
		})
	}
}
