# prompt-eval — model-behavior A/B harness

`prompt-eval` is a scripted model-behavior A/B harness: it replays fixed bot
sessions against a live llama-server (behind the router) and measures how the
model's behavior changes when exactly one input axis changes. Every tool call
is answered from a built-in fabrication table — no MCP servers are contacted,
and the tool never touches anything outside `-outdir`. Output is api-log-shaped
JSONL plus a `run-index.csv` summary, so thinking depth, delivery-marker
fabrication, and stability can be compared offline.

```sh
go build -o tools/prompt-eval/prompt-eval ./tools/prompt-eval
tools/prompt-eval/prompt-eval -h        # full flag list with defaults
```

## Experiment axes

Four orthogonal axes, each encoded in the output path and the CSV so cells
never collide:

| Axis                | Flag                                            | Encoding            |
|---------------------|-------------------------------------------------|---------------------|
| System prompt       | `-variant name=path` (repeatable)               | `<variant>/`        |
| Tool descriptions   | `-tool-variant name=path` (repeatable)          | `+<name>`           |
| Delivery role       | `-delivery-role system\|user\|developer`        | `<scenario>-<role>/`|
| Temperature         | `-temperature F`                                | `@<temp>` (when set)|

Full shape:
`<outdir>/<variant>[+<tool-variant>][@<temp>]/<scenario>-<delivery-role>/run-NN.jsonl`
plus `<outdir>/run-index.csv`. `-runs` (default 10) sessions run per scenario
per cell.

- `-variant` points at a FINAL rendered system prompt. Default: the three
  bundled `variants/` files (`none`, `dial`, `broken` — searched next to the
  binary, then `tools/prompt-eval/variants/`, then `variants/` in the cwd).
- `-tool-variant` points at a JSON object of advertised tool name →
  replacement description (`toolvariants/waitinstr.json` ships one).
  Validated loudly at startup: unknown tool names, empty descriptions,
  malformed JSON, the reserved name `default`, `/`/`+`/`\` in names.
- `-delivery-role` is the wire role of the imageflow delivery-marker
  injections (dave's `injection_role` knob); the marker TEXT is the prod
  format regardless of role.
- `-temperature`: 0 (default) omits the field and samples at the server
  default — dave's convention; only values > 0 are sent.

**Determinism**: the fabrication rng keys on `-seed` (default 1), the BARE
variant, the scenario, AND the run number (runs within a sweep fabricate
different job ids/urls), while the phrase rotation keys on seed, bare
variant, and scenario only (phrasing steps through the pool per run from a
per-sweep offset) — the delivery-role, tool-variant, and temperature axes
are deliberately excluded from both keys. Two invocations with the same
`-seed` and different axis values fabricate identical job ids, urls, and
phrasings run-for-run, so the axis is the only variable between cells.

## Scenarios

- `imageflow` (default) — replicates production session 1910's
  delivery-forgery window: ornate image request → fabricated async tool
  rounds → final; delivery for that job → final; shrew correction → new
  fabricated job → **final with no delivery preceding it** (the window where
  1910's model forged the `[System:` marker itself); then the real delivery
  → final.
- `prompts` (default) — six one-shot thinking-depth probes (casual chat,
  transformation, research question, small multi-step puzzle, creative
  writing, tool-worthy ask), fresh history each, tools advertised but never
  called.
- `research` (opt-in via `-scenarios`) — adversarial ask for a fact the
  canned search/fetch data never contains; the JSONL is the measurement
  surface — count search/fetch rounds until the model stops or the 8-round
  cap forces a final.

## Running on other models

The model-agnostic knobs are `-baseurl`, `-model`, `-reasoning-effort`, and
`-chat-template-kwargs`. Two flag DEFAULTS are gemma-shaped and must be
overridden or emptied per model:

- `-reasoning-effort` defaults to `"low"` (empty string omits the field).
- `-chat-template-kwargs` defaults to `{"preserve_thinking":true}` (empty
  string omits).

Other models use different thinking controls — e.g. the qwen3.6 moe / qwen3.8
entries on the same router — so an empty-kwargs invocation is the usual
starting point:

```sh
prompt-eval -baseurl http://192.168.100.45:8000/v1 -model <router-entry-id> \
  -reasoning-effort "" -chat-template-kwargs "" \
  -variant none=variants/none.txt -scenarios imageflow,prompts
```

Reading a child server's true defaults through the router: `GET
/props?model=<id>` (works with the built-in router; plain `/props` is
degenerate) — shows n_ctx, the chat template, template caps, and default
sampling params. Per-model sampling defaults matter: record them per
experiment. Production incident: prod and harness were both silently
sampling at temperature 1.0 because nobody had checked.

## Methodology notes (hard-won)

- **The CSV `fabricated` column is PREFIX-based and UNDERCOUNTS.** Detection
  is a leading `"[System:"` on response content — a forgery wrapped in
  markdown emphasis (`**[System:` …) escapes it. Always re-scan the JSONL
  response contents with contains-matching for the honest rate.
- **Exclude cap-forced finals from thinking-depth comparisons.** The 8-round
  forced final drops `tools` from the request entirely (the universal
  "answer now" shape that also works without `tool_choice:"none"` support) —
  a different request than the model would otherwise have seen.
- **Retries** (`-retries`, default 3; 0 disables) recover the router's
  transient 500s — "proxy error: Failed to read connection", which killed
  ~5.5% of sessions in the 70-session sweep. The identical body is re-sent
  with exponential backoff (`-retry-wait`, default 2s, doubled per attempt,
  capped at 5m). Client timeouts deliberately never retry: a >5-minute
  generation is a degenerate sample, and re-sending queues duplicate
  generations behind the server's parallel=1.
- **Sequential + `-delay` (default 750ms) between HTTP requests** — the
  server is shared with a live bot. Retry re-sends pay the delay too.

## Analysis

The JSONL envelope is `{"type":...,"ts":...,"body":{...}}` — exactly dave's
api_logs request/response record shape (non-JSON bodies such as proxy error
pages are wrapped as `{"non_json_body":...}` instead of corrupting the file),
so ad-hoc python parses harness output and production api_logs identically.
`run-index.csv` columns: `variant, tool_variant, temperature, retries,
scenario, delivery_role, run, requests, responses, reasoning_chars_total,
fabricated` — unset axes read `default` for tool_variant/temperature.
`retries` counts retry re-sends (flakiness bookkeeping); `requests` stays
one-per-logical-request. Failed retryable attempts land in the JSONL as
response-type, choices-less error records — the shape analysis scripts
already skip.
