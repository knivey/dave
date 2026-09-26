# Safe-site host split with LLM safety classification

Date: 2026-09-26
Status: design approved by owner; spec for implementation planning

## Problem

All IRC networks dave serves except Libera run with loose content
restrictions; Libera uses a stricter enhancement prompt. Today both
audiences browse the same gallery, so Libera users are exposed to
graphic content generated on the other networks. Requirements:

- Libera-facing viewers must only see Libera-safe content.
- Everyone else keeps seeing everything, including Libera
  generations (asymmetric visibility on origin).
- One imgsite deployment (owner decision: no second site/instance).
- Existing Libera-generation history must appear on the Libera-facing
  view at launch (backfill).
- Safety classification for non-Libera content is two-stage LLM:
  a cheap NSFW first pass riding the enhancement call that already
  happens, then a stricter second-pass vetting call for anything the
  first pass doesn't mark NSFW. Failures degrade to unknown and never
  elevate. The vet runs CONCURRENTLY with generation (LLM calls cost
  ~5s; serializing them before submit would visibly delay every
  image) — the verdict is baked into the image's EXIF afterward by
  rewriting the workflow note, and persisted in the jobs table so
  recovery never re-vets.

## Non-goals

- Second imgsite instance / separate databases (explicitly rejected).
- Batch re-vetting tool for the accumulated `unknown` pile (later
  follow-up if needed; admin CLI covers manual marking).
- Admin web UI for moderation (CLI only).
- Hiding `unsafe` content from the main site (main site shows
  everything; `safety` only gates the safe site).
- Image (pixel) classification — judgments are prompt-text only.

## Architecture overview

imgsite serves two logical sites from one process, selected by the
request Host header:

- **Default site** (today's behavior, `server.base_url`): all
  non-hidden images.
- **Safe site** (optional `[safe_site]` config): images that are
  non-hidden AND (origin network in `allowed_networks` OR
  `safety = 'safe'`). Default-deny: `unknown` is invisible here.

img-mcp classifies every non-Libera generation: enhancement responses
gain an `nsfw` flag (first pass), and a reserved enhancement-config
entry (`safety-vet`) performs the strict second-pass judgment
concurrently with generation. The verdict travels three places for
durability: the jobs table (recovery), the image's EXIF note (baked
in post-generation by chunk surgery, alongside provenance), and the
upload metadata into the `images.safety` column. dave changes
nothing — it pastes the upload response verbatim, and imgsite builds
that response with the safe site's base URL for Libera-origin
uploads.

## imgsite

### Config (hot-reloadable via SIGHUP, like site title)

```toml
# Optional. Absent entirely = single-site behavior identical to today.
[safe_site]
hosts = ["safe.example.com"]      # matched case-insensitively against
                                  # request Host (port stripped)
base_url = "https://safe.example.com"
allowed_networks = ["libera"]     # compared case-insensitively against
                                  # the stored provenance network
```

Request Host matching no configured safe host → default site. The
default site's base URL remains `server.base_url`.

### Data model — migration 003

```sql
ALTER TABLE images ADD COLUMN safety TEXT NOT NULL DEFAULT 'unknown';
```

Values: `unknown` | `safe` | `unsafe`. NULL never occurs. No index at
current scale (revisit past ~50k rows). Re-extract and
`dbUpdateImageMetadata` must preserve `safety` exactly like they
preserve visibility/provenance — never reset it.

### Visibility predicate

Safe-site surfaces add to their WHERE:

```sql
AND (LOWER(i.network) IN (/* lowercased allowed_networks */)
     OR i.safety = 'safe')
```

NULL network is only visible via `safety = 'safe'` (default-deny for
imported/legacy rows and direct uploads without provenance).

### Surfaces that become site-aware

1. Gallery page + fragment (`dbGetGalleryPage`).
2. Search page + fragment: the predicate is applied as an outer AND
   on the images row in all three tiers (FTS tier 1, FTS tier 2, LIKE
   tier 3 including the trigram prefilter subquery set). MATCH
   expressions themselves are untouched — the trigram-superset
   argument is preserved because the predicate intersects results
   after retrieval.
3. Image details page and its prev/next neighbor summaries (a "next"
   on the safe site must never navigate into an invisible image).
4. Asset routes — original, thumbs, download target: per-site
   visibility check; non-visible ids 404 on the safe host only (the
   same ids serve normally on the default host). og:image and og:url
   use the current site's base_url.
5. SSE: `/events` resolves the site from the request Host and tags
   the subscriber. Each publish computes the event's per-site
   visibility once (at publish time the row's network+safety are at
   hand) and stores it on the ring entry so replay filters per site
   without DB reads. `image-new` and `thumb-ready` are filtered;
   `image-hidden` is sent to all sites (dropping an absent card is a
   client no-op). hello/reset unchanged.
6. Upload response: when the upload meta's `network` (case-folded)
   is in the safe site's `allowed_networks`, the response `page` and
   `url` are built from the safe site's `base_url` — so dave pastes
   safe links into Libera channels with zero img-mcp/dave changes.
   The upload `meta.safety` value (when present and one of
   `safe`/`unsafe`) is merged into the column; invalid values are
   rejected as a bad request (img-mcp is the only writer; this is a
   tripwire, not a defense).

### Admin CLI

`imgsite -safety <id>[,<id>…] safe|unsafe|unknown [config]` —
mirrors `-delete` conventions exactly: same flag/usage/exit-code
discipline, per-id resolve + prompt snippet + continue-on-miss,
summary with counts and DB path, offline like `-import`/`-delete`.

## img-mcp

### First pass — nsfw flag on the enhancement call

`EnhancementResponse` (the JSON the enhancement prompts already
return, parsed at enhance.go:99) gains `NSFW bool` (`json:"nsfw"`).
The loose enhancement PROMPTS (owner-edited config text; an example
line ships in example.toml) instruct the model to include
`"nsfw": true` when the request or the enhanced result contains
sexual content — one judgment from rules the model already knows; no
Libera policy text is added to these prompts (owner requirement:
avoid confusion/refusals). Absent or false means only "no first-pass
signal" — it never asserts safety. `EnhanceResult` carries NSFW.

### Second pass — strict vetting, concurrent with generation

New pipeline stage, ordered:

  enhance → build workflow (note carries everything known at submit:
  prompt, llm_generated, job_id, reasoning, network/channel/nick) →
  submit → [vet runs concurrently with ComfyUI] → monitor completes →
  await vet → rewrite EXIF note (adds safety) → sha256 → upload

- The vet call starts right after enhancement and overlaps the
  10–60s generation; wall-clock cost is normally zero, worst case
  the vet's remainder at completion.
- Runs when the job's network is NOT in the new `[safety]`
  `skip_networks` list (default `["libera"]`; correspondence with
  imgsite's `allowed_networks` is by convention, like `upload.api_key`
  vs imgsite's `auth.api_key` already is — documented, separately
  configured).
- The vetter is a reserved enhancement config entry, default name
  `safety-vet` (`[safety] vet_enhancement = "safety-vet"`), reusing
  the entire existing enhancement machinery: Chat Completions and
  Responses API paths, `reasoning_effort`, timeouts, retry/backoff,
  SIGHUP hot-reload. Its prompt (owner-authored; example ships) sees
  the original and enhanced prompts and returns JSON
  `{"safe": true|false, "reason": "…"}`.
- Mapping: enhancement `nsfw:true` → safety `unsafe` and the vet
  call is SKIPPED (the call-saving). Otherwise vet: `safe:true` →
  `safe`; `safe:false` → `unsafe`. Vet call failure, unparseable
  verdict, or the named enhancement config missing → `unknown` +
  WARN; the job completes and uploads normally (main site shows it
  regardless; safe site default-denies until an admin marks it).
- Direct-tool generations (no enhancement) skip the first pass and
  go straight to the vet.

### Persistence and recovery (img-mcp migration: jobs.safety)

When the vet (or the nsfw first pass) resolves, the verdict is
written to a new `jobs.safety` column (insert/recovery-read only, like
the provenance columns). Restart recovery reads it and never re-vets;
if the crash happened before the verdict existed, recovery re-vets
during the resume monitor (its input — original prompt from job
input, enhanced prompt from the image's EXIF — is available on that
path) and the value persists once resolved.

### EXIF note rewrite (post-generation, pre-upload)

The `dave_original_prompt` note node's JSON payload is rebuilt at
upload-prep time with ALL fields — `prompt`, `llm_generated`,
`job_id`, `enhancement_reasoning` (omitempty), `network`, `channel`,
`nick` (omitempty provenance — new), `safety` (omitempty — new) —
and written into the completed image by replacing the EXIF chunk
that carries the embedded workflow (RIFF container surgery: parse
chunks, replace the EXIF chunk, fix container size; image data and
all other chunks untouched; never re-encoded). Production output is
webp (`Image Saver Simple`, embed_workflow=true); a non-webp output
logs a WARN and skips the rewrite (safety still travels in upload
meta). Ordering is load-bearing: rewrite → sha256 → upload, so the
stored hash and dedup match the bytes on disk.

### Plumbing

`UploadMeta` gains `Safety string` — **`omitempty`** (AGENTS schema
rule: non-omitempty tool-input fields become required in the
advertised schema and break callers). The direct `upload_image` tool
sends empty meta → column stays `unknown` (default-deny; admins can
mark).

### imgsite reextract healing

`/admin/reextract` (and the shared `applyMergedMetadata` path) learns
the enriched note payload: when the stored row has EMPTY provenance
(network/channel/nick) or EMPTY safety, re-extract backfills them
from the note; non-empty values are never overwritten (same
preserve-and-merge policy as visibility today). This heals rows whose
upload meta was incomplete and makes the EXIF note the durable
recovery source the rewrite above produces.

## dave

No changes. Links are pasted verbatim from the upload response; imgsite
already points Libera-origin uploads at the safe base URL.

## Testing

imgsite (per-surface × two-site matrix):
- Gallery/fragment, search tiers 1-3 (incl. trigram-accelerated and
  fallback shapes), details + neighbors: safe host shows only
  libera-origin ∪ safety='safe'; unknown and unsafe and NULL-network
  rows excluded; default host unchanged.
- Asset routes 404 on safe host for invisible ids, 200 on default.
- og tags per-site base_url.
- SSE: safe-host subscriber does not receive image-new/thumb-ready
  for invisible images (live AND replay); default-host subscriber
  receives everything; image-hidden unfiltered.
- Upload response base_url switches on meta.network casing variants.
- meta.safety merge: valid values accepted, invalid rejected, absent
  leaves unknown; re-extract preserves safety.
- Migration 003 + default value; `-safety` CLI (set each value,
  matrix of per-id misses, exit codes).
img-mcp: nsfw flag parse (true/false/absent) and skip-vet mapping;
vet verdict mapping; vet failure → unknown + job completes; missing
config → unknown + WARN-once; direct-tool path vets; vet overlaps
generation (processJob-level test asserting submit is not blocked on
the vet and upload awaits it); jobs.safety persistence + recovery
read/re-vet; EXIF chunk surgery (payload rewritten with safety +
provenance, other chunks/image bytes preserved, sha256 computed
post-rewrite; non-webp WARN-skip); UploadMeta omitempty schema.
imgsite reextract: backfills empty provenance/safety from the note,
never overwrites non-empty.

## Rollout

1. Deploy both rebuilt binaries; migration 003 runs at imgsite start.
2. Add `[safe_site]` to imgsite config (SIGHUP after edit).
3. DNS + proxy for the safe hostname → same imgsite (Host must pass
   through; nginx default).
4. img-mcp config: add the nsfw instruction line to the loose
   enhancement prompts, add `[enhancement.safety-vet]` (owner writes
   the judging prompt; example provided), add the `[safety]` block.
5. Backfill is automatic: existing rows with libera provenance become
   safe-host-visible by the origin rule the moment the config lands.

## Deferred

Backfill tooling for pre-existing images (owner-named follow-ups):
write provenance/safety into EXISTING site files' EXIF notes; batch
vetting of old images via the vetting LLM; admin tooling for marking
old files for sexual content (`-safety` CLI exists; a batch/marking
helper would build on it). Also: admin web UI; any notion of hiding
unsafe content from the default site; image-pixel classification.

## Addendum (2026-09-26): nsfw first-pass flag persisted into the EXIF note

Shipped follow-up to the first pass, recorded here without rewriting
the design above: the note payload's field list gains `nsfw`
(omitempty). The flag is baked at submit time — enhancement has
resolved by then, so `EnhanceResult.NSFW` is known — and the
rewrite-time rebuild and restart recovery carry it forward (recovery
reads it from the old note, exactly the enhancement_reasoning
surviving-copy pattern). Semantics are tri-state by composition:
`nsfw:true` = enhanced and flagged; `enhancement_reasoning` present
without `nsfw` = enhanced and unflagged; neither = never enhanced
(direct tools). skip_networks jobs never carry the flag (the whole
classification machinery is skipped there). Owner intent: the flag
"can be used later to improve the site" — imgsite parses it (`*bool`,
nil = absent) onto the extracted metadata struct for reextract/future
tooling, with no column and no merge consequence today.
