# Network-Aware Enhancement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-network enhancement policies to img-mcp so that requests from specific IRC networks (e.g., libera) are forced through content-policy-compliant enhancement profiles.

**Architecture:** Dave injects `_dave_inject_network` into MCP tool calls (existing mechanism). img-mcp reads the network name, looks it up in a configurable `[network_policy.<name>]` map, and overrides the enhancement profile or forces enhancement on raw generate calls.

**Tech Stack:** Go, TOML config, existing logxi logging, existing MCP SDK, testify assertions.

**Spec:** `docs/superpowers/specs/2026-05-18-network-aware-enhancement-design.md`

---

## File Structure

| File | Responsibility |
|---|---|
| `mcps/img-mcp/config.go` | `NetworkPolicy` struct, `NetworkPolicies` field on `Config`, validation in `loadConfig` |
| `mcps/img-mcp/tools.go` | `Network` inject field on 5 input structs, `applyNetworkPolicy` helper, updated handlers |
| `mcps/img-mcp/example.toml` | Reference docs for `[network_policy]` section |
| `mcps/img-mcp/config_test.go` | Config validation tests for network policies, reload tests |
| `mcps/img-mcp/tools_test.go` | Table-driven tests for `applyNetworkPolicy` |

No changes needed in the dave main codebase — the `_dave_inject_network` mechanism already exists.

---

### Task 1: Add NetworkPolicy struct and config field

**Files:**
- Modify: `mcps/img-mcp/config.go:12-21` (Config struct)
- Modify: `mcps/img-mcp/config.go:56-63` (after EnhancementConfig)
- Modify: `mcps/img-mcp/config.go:117-137` (enhancement validation section)
- Test: `mcps/img-mcp/config_test.go`

- [ ] **Step 1: Write the failing config test**

Add to `mcps/img-mcp/config_test.go`:

```go
func TestLoadConfig_NetworkPolicyValidation(t *testing.T) {
	t.Run("ValidPolicy", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.default]
baseurl = "https://api.example.com/v1/"
key = "test-key"
model = "test-model"
systemprompt = "enhance this"

[network_policy.libera]
enhancement = "default"
force = true
`
		path := writeTestConfigFile(t, dir, content)
		cfg, err := loadConfig(path)
		require.NoError(t, err)
		require.Contains(t, cfg.NetworkPolicies, "libera")
		assert.Equal(t, "default", cfg.NetworkPolicies["libera"].Enhancement)
		assert.True(t, cfg.NetworkPolicies["libera"].Force)
	})

	t.Run("InvalidEnhancementReference", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		content := baseTestConfigToml("http://localhost:8188") + `
[network_policy.libera]
enhancement = "nonexistent"
force = true
`
		path := writeTestConfigFile(t, dir, content)
		_, err := loadConfig(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network_policy.libera")
		assert.Contains(t, err.Error(), "nonexistent")
	})

	t.Run("EmptyPoliciesAllowed", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))
		cfg, err := loadConfig(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.NetworkPolicies)
	})

	t.Run("ForceDefaultsFalse", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.safe]
baseurl = "https://api.example.com/v1/"
key = "test-key"
model = "test-model"
systemprompt = "safe enhance"

[network_policy.graped]
enhancement = "safe"
`
		path := writeTestConfigFile(t, dir, content)
		cfg, err := loadConfig(path)
		require.NoError(t, err)
		assert.False(t, cfg.NetworkPolicies["graped"].Force)
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/knivey/dave-oc && go test ./mcps/img-mcp/ -run TestLoadConfig_NetworkPolicyValidation -v`
Expected: compile error — `NetworkPolicies` field does not exist on `Config`.

- [ ] **Step 3: Add NetworkPolicy struct and Config field**

In `mcps/img-mcp/config.go`, add the struct after `EnhancementConfig` (after line 63):

```go
type NetworkPolicy struct {
	Enhancement string `toml:"enhancement"`
	Force       bool   `toml:"force"`
}
```

Add the field to the `Config` struct (after `Workflows` line 20):

```go
	NetworkPolicies map[string]NetworkPolicy `toml:"network_policy"`
```

Initialize the map in `loadConfig` after the `Workflows` nil-check block (after line 141), and add validation:

```go
	if cfg.NetworkPolicies == nil {
		cfg.NetworkPolicies = make(map[string]NetworkPolicy)
	}
	for name, np := range cfg.NetworkPolicies {
		if np.Enhancement == "" {
			return cfg, fmt.Errorf("network_policy.%s enhancement is required", name)
		}
		if _, ok := cfg.Enhancements[np.Enhancement]; !ok {
			return cfg, fmt.Errorf("network_policy.%s enhancement %q is not defined in [enhancement]", name, np.Enhancement)
		}
		cfg.NetworkPolicies[name] = np
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /home/knivey/dave-oc && go test ./mcps/img-mcp/ -run TestLoadConfig_NetworkPolicyValidation -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add mcps/img-mcp/config.go mcps/img-mcp/config_test.go
git commit -m "img-mcp: add NetworkPolicy config struct and validation"
```

---

### Task 2: Test that NetworkPolicies reload correctly

**Files:**
- Modify: `mcps/img-mcp/config_test.go`

- [ ] **Step 1: Write the reload test**

Add to `mcps/img-mcp/config_test.go`:

```go
func TestReloadConfigFromFile_NetworkPolicies(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)

	enhancementToml := `
[enhancement.default]
baseurl = "https://api.example.com/v1/"
key = "test-key"
model = "test-model"
systemprompt = "enhance this"
`
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188")+enhancementToml)
	original, err := loadConfig(path)
	require.NoError(t, err)
	assert.Empty(t, original.NetworkPolicies)

	updated := baseTestConfigToml("http://localhost:8188") + enhancementToml + `
[network_policy.libera]
enhancement = "default"
force = true
`
	writeTestConfigFile(t, dir, updated)

	newCfg, warnings, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	require.Contains(t, newCfg.NetworkPolicies, "libera")
	assert.Equal(t, "default", newCfg.NetworkPolicies["libera"].Enhancement)
	assert.True(t, newCfg.NetworkPolicies["libera"].Force)
}

func TestReloadConfigFromFile_NetworkPolicyValidationFails(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	original, err := loadConfig(path)
	require.NoError(t, err)

	badConfig := baseTestConfigToml("http://localhost:8188") + `
[network_policy.libera]
enhancement = "nonexistent"
`
	writeTestConfigFile(t, dir, badConfig)

	_, _, err = reloadConfigFromFile(path, original)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network_policy.libera")
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `cd /home/knivey/dave-oc && go test ./mcps/img-mcp/ -run "TestReloadConfigFromFile_NetworkPolic" -v`
Expected: PASS (reload reuses `loadConfig` which now validates `NetworkPolicies`)

- [ ] **Step 3: Commit**

```bash
git add mcps/img-mcp/config_test.go
git commit -m "img-mcp: add reload tests for NetworkPolicies"
```

---

### Task 3: Add applyNetworkPolicy helper and Network inject field to input structs

**Files:**
- Modify: `mcps/img-mcp/tools.go:13-67` (input structs)
- Modify: `mcps/img-mcp/tools.go:183-217` (ToolHandlers, after resolveWorkflow)
- Test: `mcps/img-mcp/tools_test.go`

- [ ] **Step 1: Write the failing test**

Add to `mcps/img-mcp/tools_test.go`:

```go
func TestApplyNetworkPolicy(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Enhancements = map[string]EnhancementConfig{
		"safe":    {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
		"liberal": {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
	}
	cfg.NetworkPolicies = map[string]NetworkPolicy{
		"libera": {Enhancement: "safe", Force: true},
		"graped": {Enhancement: "liberal", Force: false},
	}

	queue, cleanup := setupTestQueue(t, Config{})
	defer cleanup()

	tests := []struct {
		name              string
		network           string
		inputEnhancement  string
		inputJobType      JobType
		expectEnhancement string
		expectJobType     JobType
	}{
		{
			name:              "empty network passes through",
			network:           "",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "network not in policies passes through",
			network:           "unknown",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy overrides enhancement on enhance_generate",
			network:           "libera",
			inputEnhancement:  "liberal",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy with force changes generate to enhance_generate",
			network:           "libera",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy without force keeps generate as generate",
			network:           "graped",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "",
			expectJobType:     JobTypeGenerate,
		},
		{
			name:              "policy without force overrides enhancement on enhance_generate",
			network:           "graped",
			inputEnhancement:  "",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "liberal",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "generate with no matching policy passes through",
			network:           "unknown",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "",
			expectJobType:     JobTypeGenerate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewToolHandlers(cfg, queue)
			enhancement, jobType := h.applyNetworkPolicy(tt.network, tt.inputEnhancement, tt.inputJobType)
			assert.Equal(t, tt.expectEnhancement, enhancement, "enhancement")
			assert.Equal(t, tt.expectJobType, jobType, "jobType")
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /home/knivey/dave-oc && go test ./mcps/img-mcp/ -run TestApplyNetworkPolicy -v`
Expected: compile error — `applyNetworkPolicy` method does not exist.

- [ ] **Step 3: Add Network field to input structs**

In `mcps/img-mcp/tools.go`, add `Network` to each of the 5 input structs:

`EnhancePromptInput` — add after line 15:
```go
	Network string `json:"_dave_inject_network"`
```

`GenerateImageAsyncInput` — add after line 28:
```go
	Network string `json:"_dave_inject_network"`
```

`GenerateImageInput` — add after line 41:
```go
	Network string `json:"_dave_inject_network"`
```

`EnhanceAndGenerateAsyncInput` — add after line 54:
```go
	Network string `json:"_dave_inject_network"`
```

`EnhanceAndGenerateInput` — add after line 66:
```go
	Network string `json:"_dave_inject_network"`
```

- [ ] **Step 4: Add applyNetworkPolicy method**

In `mcps/img-mcp/tools.go`, add after the `resolveWorkflow` method (after line 217):

```go
func (h *ToolHandlers) applyNetworkPolicy(network string, enhancement string, jobType JobType) (string, JobType) {
	if network == "" {
		return enhancement, jobType
	}
	cfg := h.getConfig()
	policy, ok := cfg.NetworkPolicies[network]
	if !ok {
		return enhancement, jobType
	}
	if policy.Enhancement == "" {
		return enhancement, jobType
	}
	originalEnhancement := enhancement
	enhancement = policy.Enhancement
	loggerTools.Info("network policy applied: overriding enhancement",
		"network", network,
		"original_enhancement", originalEnhancement,
		"policy_enhancement", policy.Enhancement,
	)
	if policy.Force && jobType == JobTypeGenerate {
		jobType = JobTypeEnhanceGenerate
		loggerTools.Info("network policy applied: forcing enhancement on generate",
			"network", network,
			"policy_enhancement", policy.Enhancement,
		)
	}
	return enhancement, jobType
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd /home/knivey/dave-oc && go test ./mcps/img-mcp/ -run TestApplyNetworkPolicy -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add mcps/img-mcp/tools.go mcps/img-mcp/tools_test.go
git commit -m "img-mcp: add applyNetworkPolicy helper and inject field on input structs"
```

---

### Task 4: Wire applyNetworkPolicy into tool handlers

**Files:**
- Modify: `mcps/img-mcp/tools.go:219-336` (5 handlers)

- [ ] **Step 1: Update handleEnhancePrompt**

In `handleEnhancePrompt` (line 219), replace the enhancement selection block:

Before:
```go
	enhancementName := input.Enhancement
	if enhancementName == "" {
		enhancementName = "default"
	}
```

After:
```go
	enhancementName := input.Enhancement
	if enhancementName == "" {
		enhancementName = "default"
	}
	enhancementName, _ = h.applyNetworkPolicy(input.Network, enhancementName, JobTypeEnhanceGenerate)
```

- [ ] **Step 2: Update handleGenerateImageAsync**

In `handleGenerateImageAsync` (line 236), replace the Submit call:

Before:
```go
	job, err := h.queue.Submit(JobTypeGenerate, workflow, JobInput{
		Prompt:         input.Prompt,
		NegativePrompt: input.NegativePrompt,
		Seed:           input.Seed,
		OutputFormat:   input.OutputFormat,
	})
```

After:
```go
	enhancement, jobType := h.applyNetworkPolicy(input.Network, "", JobTypeGenerate)
	job, err := h.queue.Submit(jobType, workflow, JobInput{
		Prompt:         input.Prompt,
		NegativePrompt: input.NegativePrompt,
		Enhancement:    enhancement,
		Seed:           input.Seed,
		OutputFormat:   input.OutputFormat,
	})
```

- [ ] **Step 3: Update handleEnhanceAndGenerateAsync**

In `handleEnhanceAndGenerateAsync` (line 254), replace the Submit call:

Before:
```go
	job, err := h.queue.Submit(JobTypeEnhanceGenerate, workflow, JobInput{
		Prompt:       input.Prompt,
		Enhancement:  input.Enhancement,
		OutputFormat: input.OutputFormat,
	})
```

After:
```go
	enhancement, jobType := h.applyNetworkPolicy(input.Network, input.Enhancement, JobTypeEnhanceGenerate)
	job, err := h.queue.Submit(jobType, workflow, JobInput{
		Prompt:       input.Prompt,
		Enhancement:  enhancement,
		OutputFormat: input.OutputFormat,
	})
```

- [ ] **Step 4: Update handleGenerateImage**

In `handleGenerateImage` (line 271), replace the Submit call:

Before:
```go
	job, err := h.queue.Submit(JobTypeGenerate, workflow, JobInput{
		Prompt:         input.Prompt,
		NegativePrompt: input.NegativePrompt,
		Seed:           input.Seed,
		OutputFormat:   input.OutputFormat,
	})
```

After:
```go
	enhancement, jobType := h.applyNetworkPolicy(input.Network, "", JobTypeGenerate)
	job, err := h.queue.Submit(jobType, workflow, JobInput{
		Prompt:         input.Prompt,
		NegativePrompt: input.NegativePrompt,
		Enhancement:    enhancement,
		Seed:           input.Seed,
		OutputFormat:   input.OutputFormat,
	})
```

- [ ] **Step 5: Update handleEnhanceAndGenerate**

In `handleEnhanceAndGenerate` (line 305), replace the Submit call:

Before:
```go
	job, err := h.queue.Submit(JobTypeEnhanceGenerate, workflow, JobInput{
		Prompt:       input.Prompt,
		Enhancement:  input.Enhancement,
		OutputFormat: input.OutputFormat,
	})
```

After:
```go
	enhancement, jobType := h.applyNetworkPolicy(input.Network, input.Enhancement, JobTypeEnhanceGenerate)
	job, err := h.queue.Submit(jobType, workflow, JobInput{
		Prompt:       input.Prompt,
		Enhancement:  enhancement,
		OutputFormat: input.OutputFormat,
	})
```

- [ ] **Step 6: Build and run all tests**

Run: `cd /home/knivey/dave-oc && go build ./mcps/img-mcp/ && go test ./mcps/img-mcp/ -v`
Expected: build succeeds, all tests pass.

- [ ] **Step 7: Commit**

```bash
git add mcps/img-mcp/tools.go
git commit -m "img-mcp: wire applyNetworkPolicy into generation handlers"
```

---

### Task 5: Update example.toml with network_policy docs

**Files:**
- Modify: `mcps/img-mcp/example.toml`

- [ ] **Step 1: Add network_policy reference section**

Add before the `[enhancement.default]` section in `mcps/img-mcp/example.toml` (before line 45):

```toml
# Network policies map IRC network names to enhancement profiles.
# When dave sends a request with a known network, the policy overrides
# which enhancement profile is used. If force=true, even raw generate_image
# calls are run through enhancement first.
#
# [network_policy.<network-name>]
# enhancement = "name"     # Required. Name of an [enhancement.*] profile to use.
# force = false            # If true, force enhancement on generate_image calls too.

# Example: enforce content policy on Libera
# [network_policy.libera]
# enhancement = "default"
# force = true

```

- [ ] **Step 2: Build and verify no regressions**

Run: `cd /home/knivey/dave-oc && go build ./mcps/img-mcp/ && go test ./mcps/img-mcp/ -v`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add mcps/img-mcp/example.toml
git commit -m "img-mcp: add network_policy docs to example config"
```

---

### Task 6: Final verification

- [ ] **Step 1: Run full test suite**

Run: `cd /home/knivey/dave-oc && go test ./... -count=1`
Expected: all tests pass

- [ ] **Step 2: Run go fmt and go vet**

Run: `cd /home/knivey/dave-oc && go fmt ./... && go vet ./...`
Expected: no output (clean)

- [ ] **Step 3: Verify build**

Run: `cd /home/knivey/dave-oc && go build ./mcps/img-mcp/`
Expected: succeeds
