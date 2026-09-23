# img-mcp Responses API Support for Enhancement

> **Reviewed 2026-09-23** against openai-go/v3 v3.33.0 and current xAI docs. Changes:
> `reasoning_effort` now also applies to the Chat Completions path (xAI supports
> `reasoning_effort` on both APIs); example model refreshed (`grok-4-1-fast` family
> retired 2026-05-15, redirects to grok-4.3); effort values now include `xhigh` on
> newer Grok models. All SDK type references verified unchanged.

## Goal

Add support for the OpenAI Responses API (`POST /v1/responses`) to img-mcp's prompt enhancement calls, so that reasoning content from reasoning models (e.g. grok-4.6) can be captured and logged, and add a configurable `reasoning_effort` that works on **both** API paths. The Chat Completions API does not return reasoning content; the Responses API does.

## Scope

- Purely an img-mcp internal change. No changes to dave proper, no DB schema changes, no tool output schema changes.
- Reasoning content is logged only — not stored in the database or returned to the caller.

## Config

Two new fields on `EnhancementConfig`:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `responses_api` | bool | false | Use Responses API instead of Chat Completions |
| `reasoning_effort` | string | "" | Sent when non-empty. Chat Completions: top-level `reasoning_effort`. Responses API: `reasoning.effort`. Values xAI accepts today: `low`, `medium`, `high` (Grok 4.7 also `xhigh`). Passed through unvalidated — the provider is the source of truth. |

Both fields are reloadable (part of enhancement config, already hot-swapped on SIGHUP/`/admin/reload`).

### Example TOML

```toml
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "YOUR_KEY"
model = "grok-4.6"
systemprompt = "You are an expert at writing prompts..."
timeout = 30
responses_api = true
reasoning_effort = "low"
```

## Implementation

### Approach: Branch inside `enhancePrompt()`

When `enhCfg.ResponsesAPI` is true, use the Responses API path instead of Chat Completions within the existing `enhancePrompt()` function. Shared setup (client construction, timeout, JSON schema) stays unified. Response parsing (refusal check, JSON unmarshal, trimming) is shared by both paths.

### Chat Completions path (default)

Unchanged except: when `reasoning_effort` is non-empty, set `params.ReasoningEffort = shared.ReasoningEffort(enhCfg.ReasoningEffort)` — the SDK serializes this as the top-level `reasoning_effort` field, which xAI's Chat Completions API accepts (same pattern as dave's `chatCompletion.go`). The field is `omitzero`, so an empty effort sends nothing — non-reasoning models are unaffected.

### Responses API path

1. Build `responses.ResponseNewParams`:
   - `Model` from config
   - Input: `responses.ResponseNewParamsInputUnion{OfInputItemList: []responses.ResponseInputItemUnionParam{...}}` with system message + user message (same content as current path, using `responses.EasyInputMessageParam` with `Role: responses.EasyInputMessageRoleSystem`/`EasyInputMessageRoleUser` and `Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(...)}` — same construction dave's `messagesToResponseInputItems` uses in production against xAI)
   - `Text.Format` set to JSON schema via `responses.ResponseFormatTextConfigUnionParam{OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{...}}` — **not** the Chat Completions `ResponseFormat` field. The Responses API uses `params.Text.Format` for structured output, which is a different mechanism than Chat Completions' top-level `ResponseFormat`.
   - `Reasoning.Effort` via `shared.ReasoningParam{Effort: shared.ReasoningEffort(val)}` if `reasoning_effort` is set
2. Call `client.Responses.New(ctx, params)`
3. Parse `resp.Output []responses.ResponseOutputItemUnion`:
   - `item.Type == "message"` → extract text from `item.Content` where `part.Type == "output_text"`
   - `item.Type == "reasoning"` → concatenate `item.Summary[i].Text` (summary is an array of `{Text, Type}` structs, not a flat string). Log at INFO.

### Reasoning logging

- Reasoning summary text logged at INFO level:
  ```
  INFO  tools: enhancement reasoning  model=grok-4.6  reasoning="The user wants..."
  ```

## Files Changed

| File | Change |
|------|--------|
| `mcps/img-mcp/config.go` | Add `ResponsesAPI`, `ReasoningEffort` to `EnhancementConfig` |
| `mcps/img-mcp/enhance.go` | Add Responses API branch, `ReasoningEffort` on Chat Completions path, add `responses` + `shared` imports from `openai-go/v3` |
| `mcps/img-mcp/example.toml` | Document new fields |
| `mcps/img-mcp/config_test.go` | Test config loading with new fields |
| `mcps/img-mcp/enhance_test.go` (new) | Test both API paths |

## Gotchas (verified from dave's existing Responses API implementation; re-verified 2026-09-23)

- **JSON schema response format is different**: Chat Completions uses top-level `ResponseFormat.OfJSONSchema`. Responses API uses `params.Text.Format.OfJSONSchema` with `responses.ResponseFormatTextJSONSchemaConfigParam`. The schema object itself is the same. xAI's Responses API supports `text.format` json_schema (confirmed in their structured-outputs docs); `enhancementSchema` (string/boolean fields, `required`, `additionalProperties: false`) is within xAI's supported subset.
- **Reasoning summary is an array**: `item.Summary` is `[]ResponseReasoningItemSummary`, each with `.Text` and `.Type`. Must concatenate all entries, not read as a flat string.
- **Reasoning also has a `Content` field**: `ResponseReasoningItem.Content` contains raw reasoning text (different from `Summary`). Dave only reads `Summary`. We should do the same for consistency — log `Summary` text only.
- **Reasoning effort only works on reasoning models**: Setting it on non-reasoning models may cause errors or be silently ignored depending on provider. Empty string sends nothing.
- **Encrypted reasoning**: Grok 4.7 returns `reasoning.encrypted_content` on every Responses API call. We intentionally ignore it — enhancement calls are stateless (no `previous_response_id` chaining), so there is nothing to replay.
- **Model slugs retire**: `grok-4-1-fast` family was retired 2026-05-15 (redirects to grok-4.3 at 4.3 pricing). Use canonical model IDs from xAI's current catalog (e.g. `grok-4.6`, `grok-4.7`).

## Out of Scope

- DB schema changes (reasoning is log-only)
- Changes to tool output schemas (enhance_prompt, enhance_and_generate responses stay the same)
- Changes to dave's config or code
- Streaming support (enhancement is a simple request/response)
- Encrypted reasoning content (not needed)
- `previous_response_id` chaining (each enhancement call is stateless)
