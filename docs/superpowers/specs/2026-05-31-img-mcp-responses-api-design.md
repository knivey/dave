# img-mcp Responses API Support for Enhancement

## Goal

Add support for the OpenAI Responses API (`POST /v1/responses`) to img-mcp's prompt enhancement calls, so that reasoning content from models like grok-4-1-fast-reasoning can be captured and logged. The Chat Completions API does not return reasoning content; the Responses API does.

## Scope

- Purely an img-mcp internal change. No changes to dave proper, no DB schema changes, no tool output schema changes.
- Reasoning content is logged only — not stored in the database or returned to the caller.

## Config

Two new fields on `EnhancementConfig`:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `responses_api` | bool | false | Use Responses API instead of Chat Completions |
| `reasoning_effort` | string | "" | "low", "medium", or "high". Sent only when responses_api is true. |

Both fields are reloadable (part of enhancement config, already hot-swapped on SIGHUP/`/admin/reload`).

### Example TOML

```toml
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "YOUR_KEY"
model = "grok-4-1-fast-reasoning"
systemprompt = "You are an expert at writing prompts..."
timeout = 30
responses_api = true
reasoning_effort = "low"
```

## Implementation

### Approach: Branch inside `enhancePrompt()`

When `enhCfg.ResponsesAPI` is true, use the Responses API path instead of Chat Completions within the existing `enhancePrompt()` function. Shared setup (client construction, timeout, JSON schema) stays unified.

### Responses API path

1. Build `responses.ResponseNewParams`:
   - `Model` from config
   - Input: system message + user message (same content as current path)
   - `Text.ResponseFormat` set to the same JSON schema used by Chat Completions
   - `Reasoning.Effort` if `reasoning_effort` is set
2. Call `client.Responses.New(ctx, params)`
3. Parse output items: iterate `resp.Output`, extract `"message"` items for text, log `"reasoning"` items

### Reasoning logging

- Reasoning summary text logged at INFO level:
  ```
  INFO  tools: enhancement reasoning  model=grok-4-1-fast-reasoning  reasoning="The user wants..."
  ```
- Encrypted reasoning content: log receipt at DEBUG (don't log the blob):
  ```
  DEBUG tools: encrypted reasoning received  model=grok-4-1-fast-reasoning  length=1234
  ```

### Chat Completions path (default)

Unchanged from current behavior.

## Files Changed

| File | Change |
|------|--------|
| `mcps/img-mcp/config.go` | Add `ResponsesAPI`, `ReasoningEffort` to `EnhancementConfig` |
| `mcps/img-mcp/enhance.go` | Add Responses API branch, add `responses` import |
| `mcps/img-mcp/example.toml` | Document new fields |
| `mcps/img-mcp/config_test.go` | Test config loading with new fields |
| `mcps/img-mcp/enhance_test.go` (new) | Test both API paths |

## Out of Scope

- DB schema changes (reasoning is log-only)
- Changes to tool output schemas (enhance_prompt, enhance_and_generate responses stay the same)
- Changes to dave's config or code
- Streaming support (enhancement is a simple request/response)
- `encrypted_reasoning` support beyond logging receipt (no passthrough to dave)
- `previous_response_id` chaining (each enhancement call is stateless)
