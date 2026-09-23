# img-mcp Reasoning Effort + Responses API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `responses_api` and `reasoning_effort` fields to img-mcp's enhancement config, sending reasoning effort on both API paths (Chat Completions top-level `reasoning_effort`, Responses API `reasoning.effort`) and capturing reasoning summaries in logs on the Responses path.

**Architecture:** Branch inside `enhancePrompt()` in `mcps/img-mcp/enhance.go` — shared client/timeout/schema setup stays in `enhancePrompt`, two new private helpers (`enhanceViaChatCompletions`, `enhanceViaResponses`) each make the API call, log path-specific response details, and return raw model text; the existing tail (JSON unmarshal, refusal check, trimming) is shared by both paths. Config fields are plain TOML additions to `EnhancementConfig`, automatically hot-reloadable because `reloadConfigFromFile` swaps `Enhancements` wholesale.

**Tech Stack:** Go 1.25, `github.com/openai/openai-go/v3` v3.33.0 (already in go.mod — NO new dependencies), testify.

**Spec:** `docs/superpowers/specs/2026-05-31-img-mcp-responses-api-design.md` (reviewed + updated 2026-09-23)

## Global Constraints

- No new module dependencies — everything uses `openai-go/v3` v3.33.0 already in `go.mod`.
- Struct fields use TOML snake_case tags: `responses_api`, `reasoning_effort`.
- `reasoning_effort` is passed through **unvalidated** — the provider is the source of truth (spec: "Values xAI accepts today: low, medium, high; Grok 4.7 also xhigh").
- Empty `reasoning_effort` must send **nothing** on either path (SDK fields are `omitzero`; never send `"`)` — non-reasoning models may reject the field.
- Reasoning content is **log-only**: never stored in DB, never in tool output.
- Both new fields are reloadable — do NOT add entries to `compareNonReloadable` in `mcps/img-mcp/config.go`.
- `mcps/img-mcp/example.toml` follows the config documentation convention: live block documents every field with inline comments; refresh retired model slug `grok-4-1-fast-reasoning` → `grok-4.6` everywhere it appears.
- All tests use `github.com/stretchr/testify` `assert`/`require`; `go fmt ./...` and `go vet ./...` must stay clean; `go test ./...` must pass.
- Build check: `go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp` must succeed.

## Review Focus

Five input classes the spec implies but no obvious happy-path test exercises; each pinned to its owning task:

1. **Empty `reasoning_effort` on Chat Completions sends no `reasoning_effort` key** (non-reasoning Grok variants reject unknown effort) → `TestEnhancePromptChatCompletionsOmitsReasoningEffortWhenEmpty` (Task 2).
2. **Empty `reasoning_effort` on Responses API sends no `reasoning` object** → `TestEnhancePromptResponsesAPIOmitsReasoningWhenEmpty` (Task 2).
3. **Configs without the new fields behave exactly as before** (default `responses_api=false` hits `/chat/completions`; defaults are zero values) → `TestLoadConfigEnhancementAPIDefaults` (Task 1) + Task 2's chat-path tests never setting the fields where irrelevant.
4. **Responses output with a reasoning item but no message item** must error clearly, not fail with a confusing JSON-parse error on empty text → `TestEnhancePromptResponsesAPINoMessageOutput` (Task 2).
5. **Changing either field via SIGHUP/`/admin/reload` takes effect with no restart warnings** → `TestReloadConfigFromFile_EnhancementAPIFieldsReloadable` (Task 1).

Additionally (covered, listed for the reviewer): refusal behavior must be identical on both paths → `TestEnhancePromptResponsesAPIRefused` and `TestEnhancePromptChatCompletionsRefused` (Task 2).

---

### Task 1: Config fields + example.toml documentation

**Files:**
- Modify: `mcps/img-mcp/config.go:57-64` (struct `EnhancementConfig`)
- Modify: `mcps/img-mcp/example.toml` (enhancement section, lines ~60-92)
- Test: `mcps/img-mcp/config_test.go` (append new tests)

**Interfaces:**
- Consumes: existing `loadConfig`, `reloadConfigFromFile`, `writeTestConfigFile`, `baseTestConfigToml`, `mustWriteWorkflow` helpers.
- Produces: `EnhancementConfig.ResponsesAPI bool` (`toml:"responses_api"`) and `EnhancementConfig.ReasoningEffort string` (`toml:"reasoning_effort"`) — Task 2 reads these exact names.

- [ ] **Step 1: Write the failing tests**

Append to `mcps/img-mcp/config_test.go`:

```go
func TestLoadConfigEnhancementAPIFields(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "test-key"
model = "grok-4.6"
systemprompt = "enhance"
timeout = 30
responses_api = true
reasoning_effort = "low"
`
	path := writeTestConfigFile(t, dir, content)

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Contains(t, cfg.Enhancements, "default")
	assert.True(t, cfg.Enhancements["default"].ResponsesAPI)
	assert.Equal(t, "low", cfg.Enhancements["default"].ReasoningEffort)
}

func TestLoadConfigEnhancementAPIDefaults(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "test-key"
model = "grok-4.6"
systemprompt = "enhance"
timeout = 30
`
	path := writeTestConfigFile(t, dir, content)

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Contains(t, cfg.Enhancements, "default")
	assert.False(t, cfg.Enhancements["default"].ResponsesAPI, "responses_api must default to false (Chat Completions)")
	assert.Empty(t, cfg.Enhancements["default"].ReasoningEffort, "reasoning_effort must default to empty (not sent)")
}

func TestReloadConfigFromFile_EnhancementAPIFieldsReloadable(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	base := `
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "k"
model = "grok-4.6"
systemprompt = "enhance"
timeout = 30
`
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188")+base)

	original, err := loadConfig(path)
	require.NoError(t, err)
	assert.False(t, original.Enhancements["default"].ResponsesAPI)

	writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188")+base+`responses_api = true
reasoning_effort = "high"
`)

	newCfg, warnings, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Empty(t, warnings, "enhancement API fields must be reloadable without restart warnings")
	assert.True(t, newCfg.Enhancements["default"].ResponsesAPI)
	assert.Equal(t, "high", newCfg.Enhancements["default"].ReasoningEffort)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./mcps/img-mcp/ -run 'TestLoadConfigEnhancementAPI|TestReloadConfigFromFile_EnhancementAPIFields' -v`
Expected: FAIL — compile error `cfg.Enhancements["default"].ResponsesAPI undefined` (field not defined yet).

- [ ] **Step 3: Add the struct fields**

In `mcps/img-mcp/config.go`, replace:

```go
type EnhancementConfig struct {
	BaseURL      string `toml:"baseurl"`
	Key          string `toml:"key"`
	Model        string `toml:"model"`
	SystemPrompt string `toml:"systemprompt"`
	Timeout      int    `toml:"timeout"`
	Description  string `toml:"description"`
}
```

with:

```go
type EnhancementConfig struct {
	BaseURL      string `toml:"baseurl"`
	Key          string `toml:"key"`
	Model        string `toml:"model"`
	SystemPrompt string `toml:"systemprompt"`
	Timeout      int    `toml:"timeout"`
	Description  string `toml:"description"`
	// ResponsesAPI uses POST /v1/responses instead of Chat Completions.
	// Required to capture reasoning summaries in logs on reasoning models.
	ResponsesAPI bool `toml:"responses_api"`
	// ReasoningEffort is sent when non-empty: top-level reasoning_effort on
	// Chat Completions, reasoning.effort on Responses. Provider is the source
	// of truth for accepted values (xAI: low/medium/high, 4.7 adds xhigh).
	ReasoningEffort string `toml:"reasoning_effort"`
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./mcps/img-mcp/ -run 'TestLoadConfigEnhancementAPI|TestReloadConfigFromFile_EnhancementAPIFields' -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Update example.toml documentation**

In `mcps/img-mcp/example.toml`, update the live `[enhancement.default]` block — replace `model = "grok-4-1-fast-reasoning"` with `model = "grok-4.6"` and add the new fields after the `timeout` line:

```toml
[enhancement.default]
# Prompt enhancement settings (optional)
description = "General purpose image prompt enhancer"
# LLM API endpoint for prompt enhancement
baseurl = "https://api.x.ai/v1/"
# API key for the LLM service
key = "YOUR_API_KEY_HERE"
# Model to use for enhancement (grok-4-1-fast family retired 2026-05-15; use
# canonical slugs like grok-4.6)
model = "grok-4.6"
# System prompt for the enhancement model
systemprompt = "You are an expert at writing prompts for AI image generation..."
# Timeout in seconds for enhancement requests
timeout = 30
# Use the OpenAI Responses API (POST /v1/responses) instead of Chat Completions.
# Captures reasoning summaries in the img-mcp log for reasoning models.
responses_api = false
# Reasoning effort for reasoning models ("low", "medium", "high"; Grok 4.7
# also accepts "xhigh"). Sent on both API paths. Leave unset for non-reasoning
# models — an empty value sends nothing.
# reasoning_effort = "low"
```

Also update the commented `[enhancement.libera-safe]` block: replace its `model = "grok-4-1-fast-reasoning"` line with `# model = "grok-4.6"`.

- [ ] **Step 6: Verify build and full img-mcp suite**

Run: `go build -o /tmp/opencode/img-mcp-build ./mcps/img-mcp && go test ./mcps/img-mcp/`
Expected: build succeeds, all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add mcps/img-mcp/config.go mcps/img-mcp/config_test.go mcps/img-mcp/example.toml
git commit -m "img-mcp: add responses_api and reasoning_effort enhancement config fields"
```

---

### Task 2: Send reasoning effort on both paths + Responses API branch

**Files:**
- Modify: `mcps/img-mcp/enhance.go`
- Create: `mcps/img-mcp/enhance_test.go`

**Interfaces:**
- Consumes: `EnhancementConfig.ResponsesAPI` / `.ReasoningEffort` from Task 1; `enhancementSchema`, `EnhancementResponse`, `EnhanceResult`, `loggerTools` already in `enhance.go`.
- Produces: `enhancePrompt(ctx, cfg, name, rawPrompt) (*EnhanceResult, error)` — signature unchanged; internal helpers `enhanceViaChatCompletions(ctx, client, enhCfg, rawPrompt) (string, error)` and `enhanceViaResponses(ctx, client, enhCfg, rawPrompt) (string, error)` (private, nothing else consumes them).

**SDK notes (verified against openai-go/v3 v3.33.0):**
- `openai.ChatCompletionNewParams.ReasoningEffort shared.ReasoningEffort` serializes as top-level `"reasoning_effort"`.
- Responses input uses `responses.ResponseNewParamsInputUnion{OfInputItemList: []responses.ResponseInputItemUnionParam{{OfMessage: &responses.EasyInputMessageParam{Role: ..., Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(...)}}}}}` — same construction as dave's `messagesToResponseInputItems` (responses.go:13), live-verified against xAI.
- Responses JSON schema goes in `Text.Format` (`responses.ResponseTextConfigParam.Format` → `ResponseFormatTextConfigUnionParam{OfJSONSchema}`), NOT top-level `ResponseFormat`.
- The client posts to `{baseurl}/chat/completions` and `{baseurl}/responses` — baseurl includes the `/v1` suffix (see example.toml `baseurl = "https://api.x.ai/v1/"`). The stub server must therefore serve `/v1/chat/completions` and `/v1/responses`.

- [ ] **Step 1: Write the failing tests**

Create `mcps/img-mcp/enhance_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// enhancementRecorder captures decoded request bodies per API path.
type enhancementRecorder struct {
	mu                sync.Mutex
	chatRequests      []map[string]any
	responsesRequests []map[string]any
}

func (r *enhancementRecorder) chat() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any{}, r.chatRequests...)
}

func (r *enhancementRecorder) responses() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any{}, r.responsesRequests...)
}

// newEnhancementStubServer emulates an OpenAI-compatible API serving both
// /v1/chat/completions and /v1/responses. Every request body is recorded;
// replies embed the canned EnhancementResponse on both paths. The Responses
// reply includes a reasoning item with two summary entries before the
// message item, mirroring real reasoning-model output ordering.
func newEnhancementStubServer(t *testing.T, reply EnhancementResponse) (*httptest.Server, *enhancementRecorder) {
	t.Helper()
	rec := &enhancementRecorder{}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))
		rec.mu.Lock()
		rec.chatRequests = append(rec.chatRequests, decoded)
		rec.mu.Unlock()

		content, _ := json.Marshal(reply)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatc-1","object":"chat.completion","created":1,"model":"stub",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":` + string(content) + `},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	})

	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(body, &decoded))
		rec.mu.Lock()
		rec.responsesRequests = append(rec.responsesRequests, decoded)
		rec.mu.Unlock()

		content, _ := json.Marshal(reply)
		output := `[` +
			`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"step one"},{"type":"summary_text","text":"step two"}]},` +
			`{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + string(content) + `,"annotations":[]}]}` +
			`]`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"stub",` +
			`"output":` + output + `,` +
			`"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"output_tokens_details":{"reasoning_tokens":1}}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, rec
}

func testEnhanceConfig(serverURL string, responsesAPI bool, effort string) Config {
	return Config{
		Enhancements: map[string]EnhancementConfig{
			"default": {
				BaseURL:         serverURL + "/v1",
				Key:             "test-key",
				Model:           "stub-model",
				SystemPrompt:    "enhance the prompt",
				Timeout:         10,
				ResponsesAPI:    responsesAPI,
				ReasoningEffort: effort,
			},
		},
	}
}

func TestEnhancePromptChatCompletionsSendsReasoningEffort(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a majestic cat", NegativePrompt: "blurry"}
	srv, rec := newEnhancementStubServer(t, reply)

	result, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, false, "low"), "default", "a cat")
	require.NoError(t, err)
	assert.Equal(t, "a majestic cat", result.EnhancedPrompt)
	assert.Equal(t, "blurry", result.NegativePrompt)

	requests := rec.chat()
	require.Len(t, requests, 1)
	assert.Equal(t, "low", requests[0]["reasoning_effort"], "effort must be sent as top-level reasoning_effort")
	assert.Equal(t, "stub-model", requests[0]["model"])
	assert.NotContains(t, requests[0], "reasoning", "Chat Completions must not carry a reasoning object")
}

func TestEnhancePromptChatCompletionsOmitsReasoningEffortWhenEmpty(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a cat"}
	srv, rec := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, false, ""), "default", "a cat")
	require.NoError(t, err)

	requests := rec.chat()
	require.Len(t, requests, 1)
	assert.NotContains(t, requests[0], "reasoning_effort", "empty effort must omit reasoning_effort entirely")
}

func TestEnhancePromptResponsesAPIRequestShape(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a majestic cat", NegativePrompt: "blurry"}
	srv, rec := newEnhancementStubServer(t, reply)

	result, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, "high"), "default", "a cat")
	require.NoError(t, err)
	assert.Equal(t, "a majestic cat", result.EnhancedPrompt)
	assert.Empty(t, rec.chat(), "responses_api must not touch chat completions")

	requests := rec.responses()
	require.Len(t, requests, 1)
	body := requests[0]
	assert.Equal(t, "stub-model", body["model"])
	require.Contains(t, body, "reasoning", "non-empty effort must send reasoning")
	assert.Equal(t, "high", body["reasoning"].(map[string]any)["effort"])

	input, ok := body["input"].([]any)
	require.True(t, ok, "input must be an item list")
	require.Len(t, input, 2)
	sysMsg, ok := input[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "system", sysMsg["role"])
	assert.Equal(t, "enhance the prompt", sysMsg["content"])
	userMsg, ok := input[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", userMsg["role"])
	assert.Equal(t, "a cat", userMsg["content"])

	text, ok := body["text"].(map[string]any)
	require.True(t, ok, "structured output must go in text.format")
	format, ok := text["format"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "json_schema", format["type"])
	assert.Equal(t, "prompt_enhancement", format["name"])
	assert.Equal(t, true, format["strict"])
	schema, ok := format["schema"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", schema["type"])
}

func TestEnhancePromptResponsesAPIOmitsReasoningWhenEmpty(t *testing.T) {
	reply := EnhancementResponse{EnhancedPrompt: "a cat"}
	srv, rec := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, ""), "default", "a cat")
	require.NoError(t, err)

	requests := rec.responses()
	require.Len(t, requests, 1)
	assert.NotContains(t, requests[0], "reasoning", "empty effort must omit the reasoning object entirely")
}

func TestEnhancePromptResponsesAPINoMessageOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"stub",` +
			`"output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"hmm"}]}],` +
			`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, ""), "default", "a cat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no message output")
}

func TestEnhancePromptResponsesAPIRefused(t *testing.T) {
	reply := EnhancementResponse{Refused: true, Reason: "policy"}
	srv, _ := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, true, "low"), "default", "a cat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enhancement refused: policy")
}

func TestEnhancePromptChatCompletionsRefused(t *testing.T) {
	reply := EnhancementResponse{Refused: true, Reason: "policy"}
	srv, _ := newEnhancementStubServer(t, reply)

	_, err := enhancePrompt(context.Background(), testEnhanceConfig(srv.URL, false, ""), "default", "a cat")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enhancement refused: policy")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./mcps/img-mcp/ -run TestEnhancePrompt -v`
Expected: FAIL — `TestEnhancePromptResponsesAPI*` fail because `enhancePrompt` ignores `ResponsesAPI` and posts to `/v1/chat/completions` (stub returns 404 for the chat route → "enhancement API call" error); `TestEnhancePromptChatCompletionsSendsReasoningEffort` fails on `reasoning_effort` assertion. Compile succeeds (Task 1 added the fields).

- [ ] **Step 3: Implement both paths in enhance.go**

3a. Replace the import block:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)
```

3b. Replace the body of `enhancePrompt` from the `resp, err := client.Chat...` line through the `enhanced := strings.TrimSpace(...)` line (lines 66-86 in the current file). New body after the `defer cancel()` line:

```go
	var enhanced string
	var err error
	if enhCfg.ResponsesAPI {
		enhanced, err = enhanceViaResponses(enhanceCtx, client, enhCfg, rawPrompt)
	} else {
		enhanced, err = enhanceViaChatCompletions(enhanceCtx, client, enhCfg, rawPrompt)
	}
	if err != nil {
		return nil, fmt.Errorf("enhancement API call: %w", err)
	}
	enhanced = strings.TrimSpace(enhanced)
```

Keep everything after (the `enhancement LLM response` log line is replaced — see 3c) and the existing JSON unmarshal / refusal / empty-prompt / `enhancement complete` tail unchanged.

3c. Delete the old inline `loggerTools.Info("enhancement LLM response", ...)` block (its usage fields only exist on the Chat Completions response; each helper now logs its own) and add the two helpers at the end of the file:

```go
// enhanceViaChatCompletions calls the Chat Completions API and returns the raw
// assistant message content. Reasoning effort, when set, is sent as the
// top-level reasoning_effort field (the format xAI's Chat Completions API
// expects; same as dave's chatCompletion.go).
func enhanceViaChatCompletions(ctx context.Context, client *openai.Client, enhCfg EnhancementConfig, rawPrompt string) (string, error) {
	params := openai.ChatCompletionNewParams{
		Model: enhCfg.Model,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(enhCfg.SystemPrompt),
			openai.UserMessage(rawPrompt),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "prompt_enhancement",
					Schema: enhancementSchema,
					Strict: openai.Bool(true),
				},
			},
		},
	}
	if enhCfg.ReasoningEffort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(enhCfg.ReasoningEffort)
	}

	resp, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		return "", err
	}

	loggerTools.Info("enhancement LLM response",
		"path", "chat_completions",
		"model", resp.Model,
		"finish_reason", resp.Choices[0].FinishReason,
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
		"total_tokens", resp.Usage.TotalTokens,
	)
	return resp.Choices[0].Message.Content, nil
}

// enhanceViaResponses calls the OpenAI Responses API (POST /v1/responses) and
// returns the concatenated output_text of the assistant message. Reasoning
// summaries are logged at INFO — the Responses API is the only way to get
// them back from reasoning models. Structured output goes in Text.Format,
// NOT a top-level ResponseFormat (different mechanism than Chat Completions).
func enhanceViaResponses(ctx context.Context, client *openai.Client, enhCfg EnhancementConfig, rawPrompt string) (string, error) {
	params := responses.ResponseNewParams{
		Model: enhCfg.Model,
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: []responses.ResponseInputItemUnionParam{
				{OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleSystem,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: openai.String(enhCfg.SystemPrompt),
					},
				}},
				{OfMessage: &responses.EasyInputMessageParam{
					Role: responses.EasyInputMessageRoleUser,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: openai.String(rawPrompt),
					},
				}},
			},
		},
		Text: responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:   "prompt_enhancement",
					Schema: enhancementSchema,
					Strict: openai.Bool(true),
				},
			},
		},
	}
	if enhCfg.ReasoningEffort != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort: shared.ReasoningEffort(enhCfg.ReasoningEffort),
		}
	}

	resp, err := client.Responses.New(ctx, params)
	if err != nil {
		return "", err
	}

	// Same output-walk as dave's parseSDKResponseOutput (responses.go): message
	// items yield output_text, reasoning items yield summary text. Summary is
	// an array of {Text, Type} entries — concatenate all of them. We log
	// Summary only (never Content, the raw reasoning) for consistency with dave.
	var text, reasoning string
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text += part.Text
				}
			}
		case "reasoning":
			for _, s := range item.Summary {
				reasoning += s.Text
			}
		}
	}
	if reasoning != "" {
		loggerTools.Info("enhancement reasoning",
			"path", "responses",
			"model", resp.Model,
			"reasoning", reasoning,
		)
	}
	loggerTools.Info("enhancement LLM response",
		"path", "responses",
		"model", resp.Model,
		"status", resp.Status,
		"prompt_tokens", resp.Usage.InputTokens,
		"completion_tokens", resp.Usage.OutputTokens,
		"total_tokens", resp.Usage.TotalTokens,
	)
	if text == "" {
		return "", fmt.Errorf("responses API returned no message output")
	}
	return text, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./mcps/img-mcp/ -run TestEnhancePrompt -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Verify build and full img-mcp suite**

Run: `go build -o /tmp/opencode/img-mcp-build ./mcps/img-mcp && go test ./mcps/img-mcp/`
Expected: build succeeds, all tests PASS.

- [ ] **Step 6: Commit**

```bash
git add mcps/img-mcp/enhance.go mcps/img-mcp/enhance_test.go
git commit -m "img-mcp: send reasoning effort on both enhancement paths, add Responses API support"
```

---

### Task 3: AGENTS.md + full-repo verification + review

**Files:**
- Modify: `AGENTS.md` (img-mcp section, one line)

**Interfaces:**
- Consumes: completed Tasks 1-2.
- Produces: documentation only.

- [ ] **Step 1: Update AGENTS.md img-mcp section**

In the `mcps/img-mcp/` bullet list, after the **Prompt provenance node** bullet and before the `server.go` bullet, insert:

```markdown
    - **Enhancement LLM paths** (`enhance.go`): `responses_api = true` per `[enhancement.<name>]` switches prompt enhancement from Chat Completions to the Responses API (`POST /v1/responses`) to capture reasoning summaries in logs. `reasoning_effort` (string, e.g. `"low"`/`"high"`) is sent on BOTH paths when non-empty — top-level `reasoning_effort` on Chat Completions, `reasoning.effort` on Responses — passed through unvalidated (provider is source of truth). Both fields hot-reload. Reasoning summaries are log-only, never persisted.
```

- [ ] **Step 2: Run full verification suite**

Run: `go fmt ./... && go vet ./... && go build -o /tmp/opencode/img-mcp-build ./mcps/img-mcp && go test ./...`
Expected: fmt produces no diffs, vet clean, build succeeds, ALL tests pass.

- [ ] **Step 3: Commit**

```bash
git add AGENTS.md
git commit -m "docs: document img-mcp enhancement reasoning effort and Responses API support"
```

- [ ] **Step 4: Request code review (MANDATORY)**

REQUIRED SUB-SKILL: Use superpowers:requesting-code-review — spec compliance + code quality review of the whole branch. Fix all findings before declaring done.
