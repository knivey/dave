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
   - Input: `[]responses.ResponseInputItemUnionParam` with system message + user message (same content as current path, but using `responses.EasyInputMessageParam` instead of `openai.SystemMessage`/`openai.UserMessage`)
   - `Text.Format` set to JSON schema via `responses.ResponseFormatTextConfigUnionParam{OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{...}}` — **not** the Chat Completions `ResponseFormat` field. The Responses API uses `params.Text.Format` for structured output, which is a different mechanism than Chat Completions' top-level `ResponseFormat`.
   - `Reasoning.Effort` via `shared.ReasoningParam{Effort: shared.ReasoningEffort(val)}` if `reasoning_effort` is set
2. Call `client.Responses.New(ctx, params)`
3. Parse `resp.Output []responses.ResponseOutputItemUnion`:
   - `item.Type == "message"` → extract text from `item.Content` where `part.Type == "output_text"`
   - `item.Type == "reasoning"` → concatenate `item.Summary[i].Text` (summary is an array of `{Text, Type}` structs, not a flat string). Log at INFO.

### Reasoning logging

- Reasoning summary text logged at INFO level:
  ```
  INFO  tools: enhancement reasoning  model=grok-4-1-fast-reasoning  reasoning="The user wants..."
  ```

### Chat Completions path (default)

Unchanged from current behavior.

## Files Changed

| File | Change |
|------|--------|
| `mcps/img-mcp/config.go` | Add `ResponsesAPI`, `ReasoningEffort` to `EnhancementConfig` |
| `mcps/img-mcp/enhance.go` | Add Responses API branch, add `responses` + `shared` imports from `openai-go/v3` |
| `mcps/img-mcp/example.toml` | Document new fields |
| `mcps/img-mcp/config_test.go` | Test config loading with new fields |
| `mcps/img-mcp/enhance_test.go` (new) | Test both API paths |

## Gotchas (verified from dave's existing Responses API implementation)

- **JSON schema response format is different**: Chat Completions uses top-level `ResponseFormat.OfJSONSchema`. Responses API uses `params.Text.Format.OfJSONSchema` with `responses.ResponseFormatTextJSONSchemaConfigParam`. The schema object itself is the same.
- **Reasoning summary is an array**: `item.Summary` is `[]ResponseReasoningItemSummary`, each with `.Text` and `.Type`. Must concatenate all entries, not read as a flat string.
- **Reasoning also has a `Content` field**: `ResponseReasoningItem.Content` contains raw reasoning text (different from `Summary`). Dave only reads `Summary`. We should do the same for consistency — log `Summary` text only.
- **Reasoning effort only works on reasoning models**: Setting it on non-reasoning models may cause errors or be silently ignored depending on provider.

## Out of Scope

- DB schema changes (reasoning is log-only)
- Changes to tool output schemas (enhance_prompt, enhance_and_generate responses stay the same)
- Changes to dave's config or code
- Streaming support (enhancement is a simple request/response)
- Encrypted reasoning content (not needed)
- `previous_response_id` chaining (each enhancement call is stateless)
