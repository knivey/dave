package main

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Presets (config/presets.toml) are shared base configs commands can
// inherit from with preset = "name". The merge happens at the raw TOML
// layer BEFORE struct decoding so an explicit streaming = false in a
// command is distinguishable from an absent field — the whole point of
// the mechanism. These tests pin that contract end-to-end through
// loadConfigDir.

const presetTestServices = `
[localsvc]
type = "llama"
`

func TestLoadConfigDirPresetsInheritAndOverride(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"presets.toml": `
[base]
service = "localsvc"
model = "gpt-4o"
streaming = true
rendermarkdown = true
temperature = 0.7
timeout = "2m"
maxtokens = 500
aliases = ["base-alt"]
system = "preset system with color \u0003"
`,
		"chats.toml": `
[inheritor]
preset = "base"
description = "inherits everything"

[overrider]
preset = "base"
description = "overrides some"
streaming = false
temperature = 1.2
system = "command system"
aliases = ["cmd-alt"]

[standalone]
service = "localsvc"
description = "no preset involved"
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)

	inh, ok := cfg.Commands.Chats["inheritor"]
	require.True(t, ok, "inheritor command loaded")
	assert.Equal(t, "localsvc", inh.Service, "service inherited from preset")
	assert.Equal(t, "gpt-4o", inh.Model, "model inherited from preset")
	assert.True(t, inh.Streaming, "streaming inherited from preset")
	assert.True(t, inh.RenderMarkdown, "rendermarkdown inherited from preset")
	assert.InDelta(t, 0.7, float64(inh.Temperature), 0.0001, "temperature inherited from preset")
	assert.Equal(t, 2*time.Minute, inh.Timeout, "timeout string survives the raw-layer round trip")
	assert.Equal(t, 500, inh.MaxTokens, "maxtokens inherited from preset")
	assert.Equal(t, []string{"base-alt"}, inh.Aliases, "aliases inherited from preset")
	assert.Contains(t, inh.System, "\x03", "control chars in system strings round-trip the re-encode")
	assert.Equal(t, "inherits everything", inh.Description, "command's own fields land")

	ovr, ok := cfg.Commands.Chats["overrider"]
	require.True(t, ok, "overrider command loaded")
	// THE load-bearing assertion: explicit zero-value overrides must win,
	// which is only possible because merging happens before decoding.
	assert.False(t, ovr.Streaming, "explicit streaming = false overrides preset true")
	assert.InDelta(t, 1.2, float64(ovr.Temperature), 0.0001, "command temperature beats preset")
	assert.Equal(t, "command system", ovr.System, "command system beats preset system")
	assert.Equal(t, []string{"cmd-alt"}, ovr.Aliases, "arrays replace wholesale, never concatenate")
	assert.Equal(t, "gpt-4o", ovr.Model, "unset fields still inherit alongside overrides")

	_, ok = cfg.Commands.Chats["standalone"]
	assert.True(t, ok, "commands without preset load unaffected")

	_, ok = cfg.Commands.Chats["base"]
	assert.False(t, ok, "a preset is never itself a command")
}

func TestLoadConfigDirPresetsLayered(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"presets.toml": `
[base]
service = "localsvc"
streaming = true
timeout = "3m"

[turbo]
preset = "base"
model = "gpt-4o-mini"
temperature = 0.9
`,
		"chats.toml": `
[fast]
preset = "turbo"
temperature = 1.1
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)

	fast, ok := cfg.Commands.Chats["fast"]
	require.True(t, ok, "fast command loaded")
	assert.Equal(t, "localsvc", fast.Service, "base fields inherit through a layered preset")
	assert.True(t, fast.Streaming, "base bool inherits through a layered preset")
	assert.Equal(t, 3*time.Minute, fast.Timeout, "base timeout inherits through a layered preset")
	assert.Equal(t, "gpt-4o-mini", fast.Model, "derived preset overrides base")
	assert.InDelta(t, 1.1, float64(fast.Temperature), 0.0001, "command beats derived preset")
}

func TestLoadConfigDirPresetsUnknownNameRejected(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"chats.toml": `
[orphan]
preset = "nope"
service = "localsvc"
`,
	})
	defer os.RemoveAll(dir)

	_, err := loadConfigDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "preset")
	assert.Contains(t, err.Error(), "nope")
	assert.Contains(t, err.Error(), "orphan", "error names the referencing command")
}

func TestLoadConfigDirPresetsCycleRejected(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"presets.toml": `
[a]
preset = "b"
service = "localsvc"

[b]
preset = "a"
`,
		"chats.toml": `
[user-of-a]
preset = "a"
`,
	})
	defer os.RemoveAll(dir)

	_, err := loadConfigDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
	assert.Contains(t, err.Error(), "a")
	assert.Contains(t, err.Error(), "b")
}

// TestLoadConfigDirPresetsCycleRejectedWhenUnreferenced pins the eager
// recursion-loop guard: a preset cycle is a load error even when no
// command references it — broken presets must not sit silently in the
// config waiting for someone to opt in.
func TestLoadConfigDirPresetsCycleRejectedWhenUnreferenced(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"presets.toml": `
[a]
preset = "b"

[b]
preset = "a"
`,
		"chats.toml": `
[plain]
service = "localsvc"
`,
	})
	defer os.RemoveAll(dir)

	_, err := loadConfigDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
}

// TestLoadConfigDirPresetsSelfCycleRejected pins the degenerate one-node
// loop: preset = "self" inside its own table.
func TestLoadConfigDirPresetsSelfCycleRejected(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"presets.toml": `
[ouroboros]
preset = "ouroboros"
`,
		"chats.toml": `
[plain]
service = "localsvc"
`,
	})
	defer os.RemoveAll(dir)

	_, err := loadConfigDir(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
}

func TestLoadConfigDirPresetsDeepMergeMaps(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"presets.toml": `
[base]
service = "localsvc"
extra_body = {reasoning = {effort = "high", budget = 1000}, top_k = 20}
`,
		"chats.toml": `
[mrg]
preset = "base"
extra_body = {reasoning = {effort = "low"}, max_tokens = 99}
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)

	mrg, ok := cfg.Commands.Chats["mrg"]
	require.True(t, ok, "mrg command loaded")
	reasoning, ok := mrg.ExtraBody["reasoning"].(map[string]any)
	require.True(t, ok, "extra_body.reasoning stays a nested map")
	assert.Equal(t, "low", reasoning["effort"], "command's nested key beats preset's")
	assert.Equal(t, int64(1000), reasoning["budget"], "preset's nested sibling key inherits")
	assert.Equal(t, int64(20), mrg.ExtraBody["top_k"], "preset's top-level extra_body key inherits")
	assert.Equal(t, int64(99), mrg.ExtraBody["max_tokens"], "command's top-level extra_body key lands")
}

func TestLoadConfigDirPresetsGeneratorsAndCompletions(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"presets.toml": `
[genbase]
service = "localsvc"
model = "qwen3-32b"
streaming = true
`,
		"completions.toml": `
[comp]
preset = "genbase"
description = "completion using a preset"
`,
		"generators.toml": `
[summary]
preset = "genbase"
prompt = "summarize the channel"
respond_tool = true

[summary.log]
window = "1h"
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)

	comp, ok := cfg.Commands.Completions["comp"]
	require.True(t, ok, "completion command loaded")
	assert.Equal(t, "localsvc", comp.Service, "preset fields land on completions")
	assert.True(t, comp.Streaming, "preset bools land on completions")

	gen, ok := cfg.Commands.Generators["summary"]
	require.True(t, ok, "generator command loaded")
	assert.Equal(t, "qwen3-32b", gen.Model, "preset model lands on generators")
	assert.True(t, gen.Streaming, "preset bools land on generators")
	assert.Equal(t, "summarize the channel", gen.Prompt, "generator's own fields land")
	assert.True(t, gen.RespondTool, "generator's own bools land")
	require.NotNil(t, gen.Log, "nested [name.log] table survives the round trip")
	assert.Equal(t, "1h", gen.Log.Window, "nested log window survives the round trip")
}

func TestLoadConfigDirPresetsMissingFileNoop(t *testing.T) {
	dir := createTestConfigDir(t, "", map[string]string{
		"services.toml": presetTestServices,
		"chats.toml": `
[plain]
service = "localsvc"
system = "no presets file at all"
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	_, ok := cfg.Commands.Chats["plain"]
	assert.True(t, ok, "loads fine without presets.toml")
}
