package main

import (
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
