# Image Editing — img-mcp edit tools, ref interception, gen_ refs

Date: 2026-11-06
Status: implemented (2026-11-06) — plan `docs/superpowers/plans/2026-11-06-image-editing.md`, tasks 1-8 landed.
Amended 2026-11-07: dynamic edit inputs (`image_input_node` replaces `image_nodes`), per-workflow `prompt_input`, first-image-is-target semantics — see the "Amendment 2026-11-07" notes in the img-mcp design section. Amended again 2026-11-07: `strength`/`denoise_node` removed entirely — see "Amendment 2026-11-07 — strength/denoise_node removed" at the end.
Builds on: `docs/superpowers/specs/2026-11-06-conversation-images-design.md` (view_image + ref store — shipped)

## Context

The May 2026 image-editing plan (recovered from `todo.md`) is now half-built:
dave mints and stores refs (`usr_` pastes, `web_` views) via the conversation
images feature, and `resolveImageRef` is the deliberately-unused seam. This
spec builds the other half: img-mcp edit workflows (img2img), dave-side ref
interception in tool args, and `gen_` refs so the model can edit what it (or
another user) generated.

Owner rulings from the design conversation (binding):

- The ComfyUI edit workflow JSON is **owner-provided** (like every existing
  workflow); the design adds the config plumbing (`image_input_node`,
  `prompt_input`, optional `denoise_node`) to wire it — with the edit inputs
  wired DYNAMICALLY (amendment 2026-11-07; the workflow file carries no
  LoadImage nodes).
- dave-resolved bytes reach img-mcp as **base64 data URIs rewritten into the
  tool args** at the existing injection seam. No file paths, no dave→ComfyUI
  coupling.
- Generated-image handles are **`gen_<jobid>_<n>` refs carried in tool
  results** (`generate_image`, `wait_for_job`, `job_status`, edit results).
- `output_format` is **dropped from all generation tools** (owner: never once
  used). Results are always `url`. The `jobs.output_format` column stays as
  harmless vestige.
- img-mcp does **zero pixel work**: dave is the single normalization point
  (its pipeline already resizes/re-encodes/bomb-guards everything it stores),
  and `gen_` inputs are first-party ComfyUI outputs at native resolution,
  which the owner confirmed acceptable. No img-mcp-side resize/convert/guard.
  Trust assumption: img-mcp's callers are dave or first-party on a local
  network; revisit if it is ever exposed beyond localhost.
- No enhancement for edit prompts in v1 (the enhancer is text-only and cannot
  see the input image; vision enhancement is future work).

## Mechanism overview

```
model calls edit_image(prompt, input_images: ["web_x1", "gen_abc_0"], strength 0.6)
   │
   ▼ dave: resolveImageRefArgs at the pre-call seam (LLM loop + both direct paths)
      web_x1/usr_* → resolveImageRef → data:<mime>;base64,…   (rewrite in place)
      gen_* and unknown → pass through untouched
      unresolvable usr_/web_ ref → abort the call with a readable error result
   │
   ▼ img-mcp edit_image / edit_image_async
      input entry dispatch: data URI → bytes; gen_<job>_<n> → job_images lookup
        → ComfyUI /view download. Anything else → error tool result teaching
        the model to view_image first and pass the ref.
      bytes → POST /upload/image → {name, subfolder, type}
      → prepareComfyWorkflow(edit): prompt via the per-workflow prompt_input
        key (default "text"); seeds + orphan note as usual; strength →
        denoise_node.inputs.denoise (when set); then the edit inputs are
        wired DYNAMICALLY — one LoadImage node per image (dave_edit_in_<n>)
        linked into image_input_node's images.image_<n> slots, images[0] →
        slot 1 (the edit target; output dimensions follow it)
   │
   ▼ same queue / safety vet / imgsite upload / EXIF-note machinery (JobTypeEdit)
      note payload + upload meta gain edited_from refs + strength
   │
   ▼ result: imgsite URLs + gen_<newjobid>_<n> refs (viewable via view_image,
     re-editable via edit_image)
```

## img-mcp design

### Tools

- `edit_image` (sync server) and `edit_image_async` (async server; both on
  stdio full server). Inputs:
  `prompt` (required), `input_images []string` (required, ≥1, ≤10, ORDERED —
  see the first-image rule below), `negative_prompt`,
  `workflow`, `seed`, `strength` (float 0–1, optional), `timeout` (sync only,
  default 300), + `_dave_inject_network/channel/nick/llm_generated` (omitempty —
  the schema-required bug class is known).
- The tool descriptions teach the ordering contract: `input_images` is
  ORDERED, the FIRST image is the edit target — the edit applies to it and
  the output dimensions follow it; any additional images are references
  (mention them in the prompt as `<image1>`, `<image2>`, … when the workflow
  expects it — see the workflow's `description` via `list_workflows`).
- Registered alongside the generation tools; `list_workflows` continues to
  advertise everything (edit-capable workflows noted via config description).

### Dropped: output_format

Removed from `GenerateImageInput`, `GenerateImageAsyncInput`,
`EnhanceAndGenerateInput`, `EnhanceAndGenerateAsyncInput`. The base64 result
plumbing that only served it is removed (`ImageData.Base64`, format branches).
`upload_image` (base64 *input*) is unaffected. `jobs.output_format` column
untouched (vestige; new rows get the `'url'` default).

### ComfyUI input upload (new client surface)

`uploadComfyImage(baseURL, client, data []byte, filename) (name, subfolder string, err)`:
`POST {base}/upload/image` multipart (`image` file part, `overwrite=true`),
parse `{name, subfolder, type}`. Used by the edit path only.

### WorkflowConfig additions

```go
PromptInput    string `toml:"prompt_input"`    // prompt node's input key (default "text")
ImageInputNode string `toml:"image_input_node"` // node receiving images.image_N (edit workflows)
DenoiseNode    string `toml:"denoise_node"`    // optional; inputs.denoise = strength
```

Validation: edit call against a workflow without `image_input_node` →
readable error naming the workflow; >10 input images → readable error
(pre-Submit, `maxEditInputImages`). None of the fields affect generation
workflows (`prompt_input` defaults to `"text"`, the key every existing
workflow uses).

**Amendment 2026-11-07 — dynamic edit inputs.** The original design wired a
fixed `image_nodes` list (one LoadImage node id per input slot, the workflow
file carrying that many nodes). The owner reviewed their real Qwen Image 2.1
edit workflow and ruled the change:

- The workflow file carries ZERO LoadImage nodes.
  `prepareComfyEditWorkflow` adds one LoadImage per supplied image
  (`dave_edit_in_<n>`, the same node-adding precedent as
  `dave_original_prompt`; LoadImage is a core backend class, satisfying
  `validate_prompt`'s registration check) and links it into the configured
  `image_input_node`'s `images.image_<n>` slot. A fixed node count would
  leak placeholder images into unused slots.
- Any pre-existing `images.image_*` links on the target are CLEARED before
  wiring, so a workflow file that still carries LoadImage nodes can never
  feed placeholders alongside the dynamic wiring.
- Cap: 10 input images (`maxEditInputImages`), enforced both pre-Submit
  (`validateEditInput`) and at prepare.
- Reserved ids: a workflow file containing a node literally named
  `dave_edit_in_<n>` is a readable error.
- **FIRST image is the edit target**: `images[0]` lands in `images.image_1` —
  the slot the workflow treats as the edit subject and (resolution=0) the
  output canvas. Later images are references. Order is preserved end to end
  from the tool's `input_images`, including restart recovery (persisted
  upload coordinates keep their array order).
- `prompt_input` (default `"text"`): the prompt node's input key
  `prepareComfyWorkflow` writes — `TextEncodeQwenImage21` names its prompt
  input `"prompt"`. The negative prompt stays hardcoded `"text"` (no known
  workflow needs otherwise).
- `negative_prompt_node` stays optional per workflow (the qwen-edit workflow
  configures none — `TextEncodeQwenImage21` folds the negative into its own
  inputs).

### gen_ ref resolution

`parseGenRef("gen_<jobid>_<n>")` → job lookup → `job_images` row n → ComfyUI
`/view?filename=&subfolder=&type=` download (the existing coordinates are
persisted there). Fallback: the imgsite `url` column (WARN on use). Missing
job/image → readable error.

### Jobs

- `JobTypeEdit`.
- `JobInput.InputImages`: the RESOLVED upload coordinates + the original ref
  strings — `[{ref, name, subfolder, type}]` — persisted in a new
  `jobs.input_images` TEXT column (JSON, migration 005). Recovery re-submits
  with the same uploaded names: no re-upload, no bytes in the DB. If ComfyUI's
  input dir was cleaned, recovery fails with a readable error (accepted).
- `processJob` edit path: resolve refs → upload → prepare → submit → monitor →
  safety vet (unchanged pipeline) → rewrite EXIF note → upload to imgsite.
- EXIF note payload (`dave_original_prompt`) + `UploadMeta` gain
  `edited_from []string` (the original ref strings) and `strength float`
  (omitempty).

### Results

`ImageData.Ref` (`gen_<jobid>_<n>`, omitempty) added; populated on
`generate_image` (sync — this is also how the sync path finally exposes its
job identity), `wait_for_job`, `job_status`, and edit results. dave's
`mcpImageResult` ignores it (URL parsing unchanged); the MODEL reads it in the
result text and can pass it back.

## dave design

### Ref interception (`resolveImageRefArgs`)

Convention: **the field name `input_images` is a dave-MCP contract** (like the
`_dave_inject_` prefix). Where a tool call's args contain `input_images` as a
`[]string` (or `[]any` of strings), dave rewrites each entry:

- `usr_*` / `web_*` → `resolveImageRef` → read `<images_dir>/<ref>.<ext>` →
  `data:<mime>;base64,…`. Unresolvable (unknown ref, missing file) → the whole
  call is aborted with a readable error tool result (LLM path: same shape as
  the disabled/unknown-tool branches, no MCP call made; direct tools.toml
  paths: user error notice).
- everything else (`gen_*`, already-a-data-URI) → passed through untouched.

Called at the three pre-call sites immediately before the existing
`injectScopeArgs` calls: the LLM loop (`aiCmds.go`, `executeToolCalls`), and
both direct paths in `mcpCmds.go`. `view_image` and the ref store are
unchanged.

### Result labels

No change: edit results teach refs by example (`gen_…` in `ImageData.Ref`);
the model learns the loop and the ordering contract from the tool
descriptions (amended 2026-11-07: `input_images` is ORDERED, the FIRST image
is the edit target, refs are the conversation `usr_`/`web_` ids or `gen_`
refs from generation results).

## Error handling summary

| Failure | Behavior |
|---|---|
| Unknown `input_images` entry (not usr_/web_/gen_/data:) | img-mcp error tool result: "view the image first (view_image) and pass its ref" |
| Unresolvable usr_/web_ ref (dave side) | call aborted, readable error result naming the ref |
| Edit called with workflow lacking image_input_node | readable error naming the workflow |
| Edit called with >10 input_images | readable error ("at most 10 input images") before any job is queued |
| gen_ ref: job/image missing from img-mcp DB | readable error naming the ref |
| ComfyUI /upload/image failure | readable error |
| Recovery after ComfyUI input-dir cleanup | job fails with readable error (accepted) |

## Testing

Both modules' existing patterns (table-driven testify; fake ComfyUI httptest
servers; dave's ref-store tests):

- img-mcp: uploadComfyImage (multipart shape, response parse), gen ref parse +
  resolution (DB → /view, imgsite fallback), edit processJob end-to-end
  (inputs uploaded, dynamic dave_edit_in wiring + prompt_input/denoise
  wired, note carries edited_from,
  safety rides), recovery with persisted upload names, output_format removal
  (tools no longer declare it; results always url; base64 field gone),
  edit_image_async path, error rows above.
- dave: resolveImageRefArgs (rewrite shapes, gen passthrough, unresolvable
  abort at the LLM seam + direct-path notice), integration: an executeToolCalls
  round where edit_image args get rewritten before the (mocked) MCP call.
- Workflows: an owner-supplied edit workflow JSON lands as
  `workflows/<name>.json` (not part of the automated tests beyond config
  validation; the owner verifies against live ComfyUI).

## Future work (explicit seams, not built)

- Vision enhancement: enhancement LLM sees the input images (multi-part
  content on both enhance.go paths).
- Edit chaining beyond gen_ (e.g. imgsite ids as refs).
- img-mcp pixel-side normalization if it's ever exposed beyond localhost.

## Related history

- Original plan: `todo.md` (gitignored, main checkout), May 2026 session.
- Foundation: conversation-images spec + implementation (view_image, ref
  store, `resolveImageRef` seam) — merged to master 2026-11-06.

## Amendment 2026-11-07 — strength/denoise_node removed

The `strength` tool input (both edit tools), its (0, 1] range validation,
the `denoise_node` workflow-config field, and every downstream carrier
(`JobInput.Strength`, the `jobs.strength` insert, `prepareComfyEditWorkflow`'s
denoise override, the prompt-note payload field, the imgsite upload-meta
field, and the tool-description sentence teaching it) were removed entirely,
per an owner ruling: Qwen Image 2.1 edit only works at denoise 1.0 — any
other value makes a bad image, so the knob was a footgun. The workflow
file's own KSampler `denoise` value is the fixed authoritative setting;
img-mcp never overrides it. The `jobs.strength` column (migration 005)
remains as a vestige, exactly like `jobs.output_format`: no new migration,
the column simply stops being written (new rows take the NULL default) and
old rows keep their values.
