# Streaming markdown rendering — investigation & fix (Oct 2026)

Why `streaming = true` + `rendermarkdown = true` was disabled, with hard
evidence for every bug class, the tooling that found them, and the rewrite
that fixed them. The harness stays as the regression net; the fixture
corpus was promoted to golden tests.

**Status: FIXED.** 20,000-document survey run: 0 parity failures, 0
chunk-invariance failures. See "Resolution" at the bottom.

## Background

Streaming markdown goes through `StreamingRenderer`
(`MarkdownToIRC/MarkdownToIRC.go`): a hand-rolled **line state machine** that
buffers deltas and only emits at "safe" boundaries, plus per-chunk
`MarkdownToIRCStream` (goldmark, streaming mode: fixed 80-col code padding,
no tables, no Chroma) for everything else. The two paths disagree about
markdown structure whenever constructs nest — that disagreement is what this
investigation chased.

Live config (`config/chats.toml`) never combines the two flags today:
`[chat]` has `renderMarkdown = true` (no streaming), `[yo]` has
`streaming = true` (no markdown, prompt says "Do not use markdown
formatting").

## The harness (`MarkdownToIRC/streamfuzz_test.go`)

Structure-aware generator (paragraphs with inline styles, ATX/setext
headings, fenced code with info strings / `~~~` / 4+ backtick fences /
indented fences / blank lines inside / unclosed, indented code, nested tight
& loose lists, nested blockquotes with lazy continuations, tables, HRs, HTML
blocks, task lists; blocks joined with random 0–2 blank-line separators —
zero-width adjacency is deliberate bug food) checked against two oracles:

- **P1 chunk invariance** — output must be identical across chunkings
  (one-shot, bytes(1/3/5/13), line-ish, word-ish). Always-on
  (`TestStreamingChunkInvariance`). *The current renderer PASSES this* —
  the state machine only ever acts on complete lines. This is the safety
  net any future rewrite must keep green.
- **P2 parity** — incremental output must equal `MarkdownToIRCStream(whole
  document)`. Env-gated survey (`DAVE_STREAM_SURVEY=1`, `…_ITERS`,
  `…_MAXBUGS`, `DAVE_STREAM_SEED`) shrinks every violation to a minimal
  reproducer and records it to `testdata/streaming/known_bugs/` — the
  "record the markdown that broke it" mechanism that was missing.
  `TestStreamingKnownBugs` pins the corpus as known failures and flips
  loudly when a fix lands.

Survey result at HEAD: **~29% of random documents diverge** (4000 docs →
1156 failures, 40 distinct recorded), 0 invariance failures.

## Bug taxonomy

### A. Renderer: fences are only recognized as bare ` ``` ` lines

`Process()` treats a line as a fence iff `strings.TrimSpace(line) == "```"`.
Everything else falls through to the goldmark chunk path, which is correct
per-chunk but wrong across chunk boundaries:

1. **Info strings** (`​```go`) — the opener is never recognized. With a
   blank line inside the code, the chunk splits there and code after the
   blank renders as plain prose (`004`-class; recorded fixture:
   `"Example:\n```go\nA\n\nB\n```\n\nAfter."` renders `B` unstyled).
2. **Closing fence misread as opener** — a bare ` ``` ` closing a
   language-tagged block (after a blank-line split) flips the machine into
   code mode; all following prose is swallowed and finally **dropped at
   flush** (`got: ""` for `"```python\nx\n\n```\nDone. This trailing
   sentence should appear."` — the sentence never reaches IRC).
3. **`~~~` fences and 4-backtick fences** — not recognized at all (same
   failure shape as 1/2). Longer closing fences (`​```` ` closing `​``` `)
   are legal CommonMark and also missed.
4. **Fences inside lists/quotes** (`​> ``` `, `  ``` ` under a list item) —
   the state machine hijacks them at top level; goldmark would render them
   inside the container. Recorded: `"   ````\n\n> > 1) brownx"`.

### B. Renderer: data loss at flush

- **Partial last line inside a code block is dropped**: flush emits
  `codeLines` and discards `s.buffer` (`"```\n| baz | overx"` → the whole
  content vanishes; recorded 029).
- **Same class for unclosed indented fences**: prose after an unrecognized
  fence that never closes is dropped or misclassified (recorded 057:
  `"dolor\n\n   ```\nqux lorem"` loses `qux lorem`).

### C. Renderer: architectural limitations (chunk-split on `\n\n`)

- **Indented code blocks spanning blank lines** — the splitter eats the
  blank line; two chunks render as two code blocks and the reference (one
  block, blank preserved) differs (recorded 004). Cosmetic but real.
- **Loose list continuation paragraphs** — chunked render separates item
  text and continuation with a newline; the whole-doc renderer glues them
  (`"…lazy`xlorem"`). The whole-doc behavior is itself questionable (the
  non-streaming renderer has the same gluing) — fixing the renderer should
  decide which is correct and make both agree.
- **HTML blocks / `<br>`** — newline placement differs between chunked and
  whole-doc render because HTML blocks are `TrimSpace`d with no separating
  newline. Cosmetic.
- **Tables** — `MarkdownToIRCStream` omits the Table extension entirely
  (pipes render as prose). The `renderNode` streaming-table branch
  (`[table omitted in streaming mode]`) is dead code — no streaming
  configuration ever registers the Table extension.

### D. aiCmds.go wiring bugs (proven by a scratch SSE test, since removed)

1. **Premature flush on empty deltas**: `HandleDelta("")` funnels into
   `renderer.Process("")`, which the renderer treats as END-OF-STREAM and
   flushes its buffer. Any wire chunk with an empty content delta —
   tool-call chunks, finish chunks, keepalives, interleaved
   `reasoning_content` chunks — fires it mid-stream. Observed as a
   paragraph emitted early, then continued text rendering as a separate
   paragraph. (`Flush` must be an explicit signal, not an empty delta.)
2. **Double-send on tool-call turns**: in renderer mode `streamOutput.buffer`
   holds the FULL raw text (never cleared after live sends), and
   `runTurnStream`'s tool-call branch re-renders + re-sends it — everything
   already streamed to IRC appears twice. Proven: stream "para one
   here\n\npara two" + tool call → IRC gets 4 lines (live para-one,
   premature-flushed para-two, then para-one + para-two again).
   (`runTurnResponsesStream`/`callResponsesStream` do NOT have this bug —
   they flush once inside `callResponsesStream`.)
3. **No flush on idle-timeout/error paths** — the renderer's held tail is
   silently dropped when a stream errors or times out (minor, but the user
   loses the tail they were watching build up).

### E. Non-findings worth recording

- **Chunk invariance holds** (P1: 0 failures across ~26k renderings) — the
  state machine's decisions are a pure function of the byte stream.
- `normalizeForStreaming` (`MarkdownToIRC.go:499`) is dead code — never
  called.
- The always-on P1 test found a harness bug during development (a strategy
  mutated the shared doc and "produced" empty output) — worth remembering
  when reading survey results: verify oracles before believing failures.

## Recommended fix direction (for the next session)

The state machine cannot be patched into correctness — rules 1–4 above are
CommonMark block-structure, and reimplementing that is how the bug got here.
Two viable architectures:

1. **Settled-prefix re-render**: accumulate the full raw text; each time a
   blank-line boundary arrives, re-render the settled prefix with goldmark
   and emit only complete output lines that can no longer change. Needs a
   correct "is this prefix settled" test (fence state incl. info strings,
   container context) — a smaller, honest version of the same problem, but
   backed by goldmark for the actual rendering.
2. **goldmark-based incremental parse**: parse the whole buffer each time
   (small docs, cheap), diff the render against what was already emitted,
   and emit only additions — never emitting into the last possibly-open
   block. Handles nesting by construction; the engineering is in the
   "possibly-open" determination (AST position vs. last blank line).

Either way: fix D1/D2/D3 in `aiCmds.go` first (they're independent of the
renderer rewrite), keep P1 green throughout, and promote the
`known_bugs` fixtures into `testdata/streaming/*.md` regression fixtures as
each class gets fixed.

## Resolution (same month — implemented)

**aiCmds.go wiring (D1–D3), guarded by `aiStreamOutput_test.go`:**
`HandleDelta` ignores empty deltas (they were firing the renderer's
end-of-stream flush — tool-call/finish/keepalive chunks all carry
`content:""`); the tool-call branch calls `sOut.Flush` instead of
re-rendering the never-cleared full-text buffer (the double-send); stream
error, idle-timeout, and responses-stream terminal paths flush the held
tail before the error notice (the user-stop path deliberately does not).

**Renderer rewrite (architecture 1, settled-prefix):** `StreamingRenderer`
no longer detects structure — it only decides where the stream is SAFE TO
CUT, and goldmark renders each settled prefix (`MarkdownToIRCStream`).
A prefix settles at a blank-line run that is not interior to a
blank-spanning block:

- fenced code — real CommonMark fence rules (info strings, `~~~`,
  longer/different-char closing fences, backtick-info rejection);
- indented-code and list continuation — a blank line does not settle when
  the next line is 2+-space indented or a list marker (conservative hold);
- `<pre>/<script>/<style>/<textarea>` HTML spans — held until the closing
  tag line.

Emission is prefix-monotonic: each settled render must extend the previous
output byte-for-byte. A settled block is released IMMEDIATELY at the settle
point — the completed block does not wait for a following newline or a
second blank line. Every block kind renders with a LEADING newline
(`KindHTMLBlock` included — it used to glue onto the previous line, which
was itself a rendering wart), so the entry boundary always coincides with
the render's own newline; the boundary newline is trimmed from the entry
because each entry is its own IRC message. Latency semantics (pinned by
`streaming_latency_test.go`): a paragraph is emitted as soon as the FIRST
LINE of the next block completes (that line is what classifies the blank
as a boundary rather than list/code continuation); a top-level fenced code
block is emitted at its CLOSING fence line; a tight list is held until the
list ends (interior blank lines are list continuation, not boundaries);
the final block is emitted by the flush. An invariant break would stop
incremental emission and fail over to a raw flush — never dropping data;
the oracles watch for it and it has never fired. Flush renders the WHOLE
buffer, so unclosed fences and trailing partial lines render exactly like
the non-streaming path instead of being dropped.

**Validation:** the 40 recorded `known_bugs` fixtures all render correctly
and were promoted to golden regression pairs (`testdata/streaming/bug-*.md`
+ `.irc`, discovered by `TestStreamingFromTestData`); fresh surveys of
20,000 random documents found 0 divergences (previously ~29%). Known
accepted divergence: a link-reference definition arriving after a
paragraph that referenced it (goldmark resolves references per document;
we render per prefix — LLM output essentially never does this; the
degraded-mode fallback resyncs to a line boundary so the user gets a
coherent re-sent line rather than mid-token splicing). Code review (Oct
2026) additionally found and fixed: a fence opening ON a list-marker line
(`- ```py`) whose real closer was misread as a new top-level opener —
poisoning fence state and holding every later block until flush (silent
batch degradation, byte-correct so no oracle caught it; fixed by
detecting openers through the marker prefix, pinned by
`TestStreamingLatencyMarkerLineFence`, and the generator now produces the
shape); and `sOut.buffer`'s dead O(n²) raw accumulation in renderer mode.
Note for production: streamed output does NOT strip `<think>…</think>`
blocks (final-text paths do, via ExtractFinalText) — pre-existing
behavior, previously masked by the double-send's duplicate copy.

**Enabling in production:** set `streaming = true` alongside
`rendermarkdown = true` on a command in `chats.toml`. Tables still render
as pipe-text in streaming mode (the streaming parser deliberately omits
the Table extension) — same as before, now documented.

