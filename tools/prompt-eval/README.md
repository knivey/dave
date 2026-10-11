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
- `toolvariants/waitinstr.json` is the experiment that shipped into prod
  (the s4 cell: 0/10 delivery-marker forgeries); the prod port equals it
  modulo genericizing `image link` → `link` (register_background_job
  serves any async tool), and the file stays byte-stable so the s4 cell
  remains reproducible.
- `toolvariants/selfintercept.json` targets the s6 "reasoned forgery"
  (system run-04: quoted the wait rule, then hallucinated a tool output
  and self-authorized — *"I must use exactly what was returned"*): the
  CURRENT prod register description plus one clause naming that exact
  move ("if you ever catch yourself writing or quoting that notice, or
  remembering a tool result you never received, stop — that is
  hallucination"). Run it at `-delivery-role system`, the cell where the
  forgeries live.
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
  **Deliveries are coupled to actual submissions**: each delivery carries
  the job id the harness really handed out in a fabricated tool result
  (oldest not-yet-delivered submission, in order). When no unconsumed
  submission exists at a delivery point, NOTHING arrives — matching prod,
  where no job ever exists (pre-coupling, the s6 user run-07 glitch — the
  model's first response planned the enhance call but emitted none — got a
  ghost delivery for a never-submitted job, which the model presented
  credulously; the run now prints a skip note instead). Comparability
  holds: in normal runs the coupled id is the same seeded draw the
  per-turn marker always used, so transcripts are byte-identical to
  pre-coupling cells; rng keying is untouched. A deliberate adversarial
  "unknown-job delivery probe" (a delivery referencing a job id never
  submitted, to test whether the model audits provenance — it does not,
  per run-07) remains future work.
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

- **The advertised tools are prod-faithful.** Descriptions and parameter
  schemas are transcribed verbatim from this repo's own sources into
  `toolschemas.json` (embedded at build time): the img/brave/fetch/yt tool
  defs and SDK-inferred input schemas from `mcps/*`, and dave's
  `register_background_job` builtin from `aiCmds.go`. The stub schemas the
  harness originally shipped (one-line descriptions, empty object params)
  were themselves an experimental variable — models called
  `brave_web_search` with empty args and never registered background jobs
  because they could not see the required arguments. Re-sync
  `toolschemas.json` when the sources change: the register fidelity test is
  the only automated drift guard, the rest is transcription discipline.
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

### Tripwire detector columns

The six int columns after `fabricated` (`leak_suspects`, `fabricated_urls`,
`linkless_finals`, `silent_turns`, `jobs`, `registers`) are TRIPWIRES:
known-false-positive-prone mechanical heuristics whose job is to flag runs
for deeper inspection (pilot reads of the JSONL), never to verdict. A
non-zero count is a "look closer" signal, not a failure rate.

- **`leak_suspects`** — responses whose content has a line starting
  (modulo leading whitespace) with `thought:`/`thinking:`/`reasoning:`
  (case-insensitive): reasoning leaking into the visible reply. Line-
  anchored ONLY, so `"My thought:"` mid-prose (in-character monologue)
  does not match — but genuine in-character monologue that happens to
  open a line with the label DOES. Known false-positive class; read the
  flagged responses.
- **`fabricated_urls`** — responses citing an `i.shrews.xyz` URL that is
  NOT in the delivered set at that point. The set is rebuilt from
  DELIVERY messages (role system/user/developer carrying the
  `[System: Background task completed` marker) as they appear in request
  history — each new delivery REPLACES the set, so both invented links
  and stale links from an earlier delivery (reused after a newer one)
  count. URLs inside assistant content (forged markers included) never
  seed the set. False positives: a citation of an older-but-real link is
  counted identically to an invention.
- **`linkless_finals`** — for each delivery, the FIRST no-tool-call
  response after it (the presentation slot) counted when it cites NO
  `i.shrews.xyz` URL at all — the model lost/omitted the delivered link
  (observed at temp 0.7: a run that replaced BOTH links with confabulated
  descriptions). Tool-call responses (the queue_status detour shape) do
  not consume the slot; later unrelated no-tool responses don't count.
  False positives: a legitimate answer that deliberately references the
  image without linking.
- **`silent_turns`** — responses with no tool calls and blank content —
  on the real bot these are turns where the channel sees nothing. Note
  the sibling shape is NOT counted here: empty content WITH a tool call
  (the silent job submission) is normal agentic behavior.
- **`jobs` / `registers`** — raw counts of `enhance_and_generate_async` /
  `generate_image_async` calls and `register_background_job` calls. No
  derived omission column: analysis computes `registers` vs `jobs` (the
  log review found 136/137 jobs unregistered — the raw counts carry the
  evidence).

### Testing a new model (e.g. qwen3.6/3.8): pilot before batch

The forgery/parrot/thinking detectors and this whole methodology were
calibrated on ONE model's observed failure shapes (gemma-4 heretic). A
different model family may fail DIFFERENTLY — do not trust batch metrics on
a new model until its failure shapes are confirmed the same. Pilot first:
2-3 sessions per scenario, then READ THE RAW JSONL end-to-end (not the CSV,
not the summary metrics) and catalog what failure actually looks like —
where reasoning lands, what degenerate outputs look like, how tool calls
malform or don't. Qwen-specific watch items:

- Thinking rides the template's `enable_thinking` kwarg
  (`-chat-template-kwargs`), not gemma's `preserve_thinking`. Verify
  `reasoning_content` actually populates — llama.cpp parses qwen's
  `<think>` into `reasoning_content`; if a variant routes thinking into
  content instead, the thinking-depth metric reads zero and the
  empty-content metric lies.
- Check `/props?model=<id>` chat_template_caps (supports_reasoning_effort
  etc.) before setting `-reasoning-effort`.
- Watch for family-specific degenerate modes: thinking loops, malformed
  tool-call JSON, language drift on uncensored finetunes.

If the pilot reveals failure shapes the CSV columns don't capture, extend
the analysis scripts FIRST — the batch numbers are only as good as the
detectors.

## Analysis

The JSONL envelope is `{"type":...,"ts":...,"body":{...}}` — exactly dave's
api_logs request/response record shape (non-JSON bodies such as proxy error
pages are wrapped as `{"non_json_body":...}` instead of corrupting the file),
so ad-hoc python parses harness output and production api_logs identically.
`run-index.csv` columns: `variant, tool_variant, temperature, retries,
scenario, delivery_role, run, requests, responses, reasoning_chars_total,
fabricated, leak_suspects, fabricated_urls, linkless_finals, silent_turns,
jobs, registers` — unset axes read `default` for tool_variant/temperature;
the six columns after `fabricated` are the tripwire detectors documented
above.
`retries` counts retry re-sends (flakiness bookkeeping); `requests` stays
one-per-logical-request. Failed retryable attempts land in the JSONL as
response-type, choices-less error records — the shape analysis scripts
already skip.
