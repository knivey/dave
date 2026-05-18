# Network-Aware Enhancement for img-mcp

## Problem

Dave needs per-network content policy enforcement for image generation, primarily for Libera compliance. The enhancement system already supports a `refused` field that can reject non-compliant prompts, but there is no mechanism to select or enforce enhancement profiles based on which IRC network a request originates from.

## Solution

Add network-aware enhancement policies to img-mcp. When dave injects the network name via the existing `_dave_inject_network` mechanism, img-mcp looks up a policy for that network and applies it — overriding or injecting the enhancement profile, and optionally forcing enhancement on raw `generate_image` calls.

## Design

### 1. Config: `[network_policy.<network-name>]` section

New reloadable config section in img-mcp TOML:

```toml
[network_policy.libera]
enhancement = "libera-safe"   # enhancement profile to apply
force = true                  # force enhancement even on raw generate_image calls
```

**Struct:**

```go
type NetworkPolicy struct {
    Enhancement string `toml:"enhancement"`
    Force       bool   `toml:"force"`
}
```

Added to `Config` as `NetworkPolicies map[string]NetworkPolicy` with TOML tag `network_policy`.

**Validation:** `enhancement` must name an existing `[enhancement.<name>]` profile. Validated at load time alongside existing enhancement validation.

**Defaults:** Empty map (no policies). Networks not listed pass through unchanged.

**Reloadable:** Yes. `NetworkPolicies` is reloaded on SIGHUP / `POST /admin/reload` alongside enhancements and workflows.

### 2. Inject field on tool inputs

Add `_dave_inject_network` to the 5 tool input structs that involve prompt processing or generation:

| Struct | Tool |
|---|---|
| `EnhancePromptInput` | `enhance_prompt` |
| `GenerateImageInput` | `generate_image` |
| `GenerateImageAsyncInput` | `generate_image_async` |
| `EnhanceAndGenerateInput` | `enhance_and_generate` |
| `EnhanceAndGenerateAsyncInput` | `enhance_and_generate_async` |

Each gets: `Network string \`json:"_dave_inject_network"\``

No inject field on query tools (`queue_status`, `list_jobs`, `wait_for_job`, `job_status`, etc.) — they don't involve content generation.

### 3. Policy application logic

A helper function `applyNetworkPolicy` in `tools.go`:

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
    // Override enhancement
    originalEnhancement := enhancement
    enhancement = policy.Enhancement
    loggerTools.Info("network policy applied: overriding enhancement",
        "network", network,
        "original_enhancement", originalEnhancement,
        "policy_enhancement", policy.Enhancement,
    )
    // Force enhancement on raw generate
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

### 4. Handler changes

Each of the 4 generation handlers calls `applyNetworkPolicy` before submitting the job:

**`handleGenerateImage` / `handleGenerateImageAsync`:**
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

**`handleEnhanceAndGenerate` / `handleEnhanceAndGenerateAsync`:**
```go
enhancement, jobType := h.applyNetworkPolicy(input.Network, input.Enhancement, JobTypeEnhanceGenerate)
job, err := h.queue.Submit(jobType, workflow, JobInput{
    Prompt:       input.Prompt,
    Enhancement:  enhancement,
    OutputFormat: input.OutputFormat,
})
```

**`handleEnhancePrompt`:**
```go
enhancement, _ := h.applyNetworkPolicy(input.Network, input.Enhancement, JobTypeEnhanceGenerate)
result, err := enhancePrompt(ctx, h.getConfig(), enhancement, input.Prompt)
```

### 5. Logging

Policy application is logged at INFO level with:
- The network name
- What changed (enhancement override, force applied)
- Original vs policy values

Example log output:
```
INFO  network policy applied: overriding enhancement  network=libera original_enhancement=default policy_enhancement=libera-safe
INFO  network policy applied: forcing enhancement on generate  network=libera policy_enhancement=libera-safe
```

When no policy matches, nothing is logged (pass-through is silent).

### 6. No dave changes needed

Dave already:
- Injects `_dave_inject_network` via `injectScopeArgs()` in `aiCmds.go:933`
- Strips `_dave_inject_*` from schemas shown to the LLM via `stripInjectFieldsFromSchema()`
- Has test coverage for both behaviors in `mcpClient_test.go`

The only work is in img-mcp.

### 7. Config documentation

Update `example.toml` with reference section for `[network_policy]` and a commented-out example block.

## Files Changed

| File | Change |
|---|---|
| `mcps/img-mcp/config.go` | Add `NetworkPolicy` struct, `NetworkPolicies` field to `Config`, validation in `loadConfig` |
| `mcps/img-mcp/tools.go` | Add `Network` field to 5 input structs, add `applyNetworkPolicy`, update 5 handlers |
| `mcps/img-mcp/logging.go` | Add `loggerTools` logger (or reuse existing) |
| `mcps/img-mcp/example.toml` | Add `[network_policy]` reference and example |
| `mcps/img-mcp/config_test.go` | Tests for policy validation |
| `mcps/img-mcp/tools_test.go` | Tests for `applyNetworkPolicy` |

## Testing

- **Config validation**: `[network_policy.<name>].enhancement` must reference an existing enhancement profile
- **`applyNetworkPolicy`**: table-driven tests covering:
  - Empty network → pass-through
  - Network not in policies → pass-through
  - Network in policies, enhancement override only
  - Network in policies, force=true on generate → type changes to enhance_generate
  - Network in policies, force=false on generate → type stays generate, no enhancement applied
  - enhance_and_generate with network policy → enhancement overridden but type unchanged
  - Empty policy enhancement → pass-through
- **Reload**: verify `NetworkPolicies` is preserved across reload when unchanged, updated when changed
