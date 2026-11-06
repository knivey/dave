# Image Editing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** img-mcp `edit_image`/`edit_image_async` tools (owner-provided img2img workflows) fed by dave-side ref interception (`usr_`/`web_` → base64 data URIs) and img-mcp `gen_<jobid>_<n>` refs, with `output_format` dropped from all generation tools.

**Architecture:** dave rewrites `input_images` entries at the existing three pre-call injection sites (a field-name convention like `_dave_inject_`); img-mcp resolves `gen_` refs from its own `job_images` table (ComfyUI `/view` re-download, imgsite URL fallback), uploads input bytes to ComfyUI `/upload/image`, wires uploaded names into the workflow's configured `image_nodes` (+ optional `denoise_node` for strength), and rides the unchanged queue/safety/upload machinery as `JobTypeEdit`. Resolved upload coordinates persist on the job so restart recovery re-submits without re-upload. img-mcp does ZERO pixel work — dave is the single normalization point.

**Tech Stack:** Go 1.25, modelcontextprotocol-go SDK (img-mcp), sqlx + goose migrations (img-mcp SQLite), testify. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-11-06-image-editing-design.md` — the plan argues from the spec; read both.

## Global Constraints

- Two modules: root package (dave, `github.com/knivey/dave`) and `mcps/img-mcp` (own module). Build both: `go build -o dave .` and `go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp`.
- All `_dave_inject_*` tool input fields MUST use `omitempty` (jsonschema marks non-omitempty fields required).
- img-mcp jobs DB: goose migrations under `mcps/img-mcp/migrations/` (next number: 005). Terminal-state DB writes stay fenced; `input_images`/`strength` are insert/recovery-only columns no terminal UPDATE touches.
- Every logxi logger: `SetLevel(logxi.LevelAll)`.
- Tests: testify table-driven; fake ComfyUI via `newMockComfyFlowServer` (comfy_test.go:675) + `testConfig`/`setupTestQueue`/`mustWriteWorkflow`/`submitTestJob` (testhelpers_test.go).
- dave test harness: `setupTestDB(t)`, ref store via `config.ImagesDir = t.TempDir()` + `t.Cleanup` resets (pristine output).
- Config docs convention: new img-mcp TOML fields documented in `mcps/img-mcp/example.toml` (reference + commented example).
- Never `git add -f`. `go fmt ./...` + `go vet ./...` before every commit; module-scoped `go test` after every task.

## Review Focus

1. **Edit called with a workflow lacking `image_nodes`** → readable error naming the workflow, no job submitted. Pinned in Task 5 (`TestHandleEditImageRequiresImageNodes`).
2. **Model passes a raw URL (or garbage) in `input_images`** → img-mcp error tool result teaching "view the image first (view_image) and pass its ref". Pinned in Task 4 (`TestResolveEditInputsRejectsUnknownEntry`).
3. **Restart recovery of a running edit job** re-submits using persisted upload names (no re-upload; no bytes in DB); if ComfyUI's input dir was cleaned the failure is a readable error. Pinned in Task 5 (`TestEditJobRecoveryReusesPersistedUploadNames`).
4. **`strength` out of range** (`≤ 0`, `> 1`) → readable error at the handler, no job. Pinned in Task 5 (`TestHandleEditImageValidatesStrength`).
5. **dave aborts an unresolvable `usr_`/`web_` ref without calling the MCP** → error tool result naming the ref (LLM path) / error notice (direct path). Pinned in Task 7 (`TestExecuteToolCallsAbortsUnresolvableRef` + `TestResolveImageRefArgs`).

---

### Task 1: img-mcp — drop `output_format` from all generation tools

**Files:**
- Modify: `mcps/img-mcp/tools.go` (4 input structs, 4 handlers)
- Modify: `mcps/img-mcp/queue.go` (`JobInput`)
- Modify: `mcps/img-mcp/comfy.go` (result/base64 branches, if the OutputFormat consumer lives here)
- Modify: `mcps/img-mcp/db.go` (`dbInsertJob`, `jobFromDBJob`; `dbJob` scan stays)
- Test: `mcps/img-mcp/tools_test.go`, `queue_test.go` (update references)

**Interfaces:**
- Consumes: nothing new.
- Produces: generation tool inputs without `OutputFormat`; `JobInput` without `OutputFormat`; results always URL. `jobs.output_format` column untouched (new rows get the `'url'` DEFAULT because the column leaves the INSERT list). `ImageData.Base64` field REMOVED.

- [ ] **Step 1: Survey every site**

Run: `grep -rn 'OutputFormat\|output_format\|Base64\|"base64"\|"both"' mcps/img-mcp/*.go mcps/img-mcp/*_test.go`
Expected hits to change: `GenerateImageInput`/`GenerateImageAsyncInput`/`EnhanceAndGenerateInput`/`EnhanceAndGenerateAsyncInput` (tools.go:27-115 — delete the `OutputFormat` field from each), the four handlers' `Submit` literals (`OutputFormat: input.OutputFormat,` — delete), `JobInput.OutputFormat` (queue.go:108 — delete field + its doc), `dbInsertJob` (db.go:112 — remove `output_format` from column list AND the value `job.Input.OutputFormat`), `jobFromDBJob` (db.go:322 — remove `OutputFormat: dbj.OutputFormat,`), `ImageData.Base64` (tools.go:128-132 — delete field), and every `if input.OutputFormat == "base64" || ... "both"` result branch (locate by grep; delete the base64 encoding arm, keep the URL arm unconditionally). `dbJob` struct + its SELECT/scan KEPT (old rows still carry values; add a one-line comment "vestige: column no longer written").

- [ ] **Step 2: Write/adjust the failing tests**

In `tools_test.go` add:

```go
func TestGenerationToolsHaveNoOutputFormat(t *testing.T) {
	for _, name := range []string{"generate_image", "generate_image_async", "enhance_and_generate", "enhance_and_generate_async"} {
		t.Run(name, func(t *testing.T) {
			schema := toolInputSchemaForTest(t, name) // helper below
			props, ok := schema["properties"].(map[string]any)
			require.True(t, ok, "tool %s must declare properties", name)
			assert.NotContains(t, props, "output_format",
				"output_format is dropped from generation tools (owner ruling, never used)")
			assert.NotContains(t, props, "base64")
		})
	}
}
```

If no existing helper returns a tool's advertised input schema, write `toolInputSchemaForTest` by registering the tool through the same `mcp.AddTool` path `createFullServer` uses against a test server, then reading the tool list (mirror how existing tools_test.go tests reach tool metadata; if none does, build the server via `createFullServer(&ToolHandlers{...})` with a nil-free test config and list tools through the SDK). Update any existing test that sets `OutputFormat` or asserts `Base64` (grep from Step 1 lists them).

- [ ] **Step 3: Run to verify the new test fails, then implement**

Run: `go test -run TestGenerationToolsHaveNoOutputFormat ./mcps/img-mcp` → FAIL (field present).
Apply every Step-1 change. `go build ./...` must pass with zero references left: `grep -rn 'OutputFormat' mcps/img-mcp/*.go` returns only the `dbJob` vestige comment/scan.

- [ ] **Step 4: Full module tests green**

Run: `go test ./mcps/img-mcp`
Expected: PASS (updated suite; no base64-format rows remain).

- [ ] **Step 5: Commit**

```bash
git add mcps/img-mcp
git commit -m "feat!: drop output_format from generation tools — results always url"
```

---

### Task 2: img-mcp — WorkflowConfig image/denoise nodes + edit workflow preparation

**Files:**
- Modify: `mcps/img-mcp/config.go` (`WorkflowConfig` ~102)
- Modify: `mcps/img-mcp/comfy.go` (new `prepareComfyEditWorkflow`, `comfyImageInputName`)
- Modify: `mcps/img-mcp/example.toml` (docs)
- Test: `mcps/img-mcp/comfy_test.go`

**Interfaces:**
- Consumes: `prepareComfyWorkflow(cfg, workflowName, prompt, negativePrompt string, seedOverride *int64, promptNote string) (ComfyWorkflow, error)` (comfy.go:276), `ComfyImage{Filename, Subfolder, Type}` (comfy.go:68).
- Produces:
  - `WorkflowConfig.ImageNodes []string` (`toml:"image_nodes"`), `WorkflowConfig.DenoiseNode string` (`toml:"denoise_node"`)
  - `comfyImageInputName(name, subfolder string) string`
  - `prepareComfyEditWorkflow(cfg Config, workflowName, prompt, negativePrompt string, seedOverride *int64, promptNote string, images []ComfyImage, strength *float64) (ComfyWorkflow, error)`

- [ ] **Step 1: Write the failing tests**

Add to `comfy_test.go` (workflow fixtures: a minimal map with a prompt node "6", a `LoadImage`-classed node "40", a denoise-bearing node "3" — write with `mustWriteWorkflow` after marshaling, or extend the map literal pattern existing tests use):

```go
func editTestWorkflowJSON(t *testing.T) string {
	t.Helper()
	wf := ComfyWorkflow{
		"6":  {Inputs: map[string]interface{}{"text": "orig"}, Class: "CLIPTextEncode"},
		"40": {Inputs: map[string]interface{}{"image": "placeholder.png"}, Class: "LoadImage"},
		"3":  {Inputs: map[string]interface{}{"denoise": 1.0, "seed": 1}, Class: "KSampler"},
	}
	b, err := json.Marshal(wf)
	require.NoError(t, err)
	return string(b)
}

func editTestConfig(t *testing.T) Config {
	cfg := testConfig("")
	cfg.Workflows["edit"] = WorkflowConfig{
		WorkflowPath: mustWriteWorkflowString(t, editTestWorkflowJSON(t)),
		ClientID:     "test-client", OutputNode: "output-node", PromptNode: "6",
		ImageNodes:  []string{"40"},
		DenoiseNode: "3",
		Timeout:     60,
	}
	return cfg
}

func TestPrepareComfyEditWorkflowWiresImagesAndDenoise(t *testing.T) {
	cfg := editTestConfig(t)
	images := []ComfyImage{
		{Filename: "edit_1_0.jpg"},
		{Filename: "in.png", Subfolder: "sub"},
	}
	strength := 0.6
	wf, err := prepareComfyEditWorkflow(cfg, "edit", "make it blue", "", nil, "note", images, &strength)
	require.NoError(t, err)
	assert.Equal(t, "edit_1_0.jpg", wf["40"].Inputs["image"], "no subfolder -> bare name")
	wf2, err := prepareComfyEditWorkflow(cfg, "edit", "p", "", nil, "n", []ComfyImage{{Filename: "x.png", Subfolder: "sub"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "sub/x.png", wf2["40"].Inputs["image"], "subfolder -> sub/name")
	assert.Equal(t, 0.6, wf["3"].Inputs["denoise"], "strength wired to denoise_node")
	assert.Equal(t, 1.0, wf2["3"].Inputs["denoise"], "nil strength leaves the workflow default")
	assert.Equal(t, "make it blue", wf["6"].Inputs["text"], "prompt wiring inherited from prepareComfyWorkflow")
}

func TestPrepareComfyEditWorkflowMissingNodeErrors(t *testing.T) {
	cfg := editTestConfig(t)
	cfg.Workflows["edit"].ImageNodes = []string{"999"}
	_, err := prepareComfyEditWorkflow(cfg, "edit", "p", "", nil, "n", []ComfyImage{{Filename: "x.jpg"}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "999")
	assert.Contains(t, err.Error(), "edit", "error names the node and workflow")
}
```

(If `mustWriteWorkflow` takes a path template rather than raw JSON, add `mustWriteWorkflowString(t, jsonStr)` next to it writing the string to a temp file — mirror the existing helper's body.)

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestPrepareComfyEditWorkflow' ./mcps/img-mcp` → FAIL (`undefined: prepareComfyEditWorkflow` / unknown fields).

- [ ] **Step 3: Implement**

`config.go` — add to `WorkflowConfig` after `SeedNodes`:

```go
	// ImageNodes names the LoadImage node ids an EDIT workflow wires uploaded
	// input images into (inputs.image). Only edit workflows configure these;
	// calling edit_image against a workflow without them is a readable error.
	ImageNodes []string `toml:"image_nodes"`
	// DenoiseNode optionally names the node whose inputs.denoise receives the
	// edit strength (0-1). Unset = the workflow file's own default rides.
	DenoiseNode string `toml:"denoise_node"`
```

(No new required-field validation — generation workflows must keep loading unchanged.)

`comfy.go` — after `prepareComfyWorkflow`:

```go
// comfyImageInputName renders a ComfyUI LoadImage inputs.image value:
// bare filename at the input root, "subfolder/name" otherwise.
func comfyImageInputName(name, subfolder string) string {
	if subfolder == "" {
		return name
	}
	return subfolder + "/" + name
}

// prepareComfyEditWorkflow builds an edit (img2img) prompt: it reuses
// prepareComfyWorkflow for everything a generation gets (prompt/negative
// text, seeds, orphan prompt note), then wires the uploaded input images
// into the configured image_nodes (in order) and the optional strength into
// denoise_node. Missing image nodes are errors, not silent skips — the same
// guard style as the prompt-node lookup.
func prepareComfyEditWorkflow(cfg Config, workflowName, prompt, negativePrompt string, seedOverride *int64, promptNote string, images []ComfyImage, strength *float64) (ComfyWorkflow, error) {
	workflow, err := prepareComfyWorkflow(cfg, workflowName, prompt, negativePrompt, seedOverride, promptNote)
	if err != nil {
		return nil, err
	}
	wc := cfg.Workflows[workflowName]
	if len(wc.ImageNodes) == 0 {
		return nil, fmt.Errorf("workflow %q has no image_nodes configured — not an edit workflow", workflowName)
	}
	if len(images) > len(wc.ImageNodes) {
		return nil, fmt.Errorf("workflow %q accepts %d input image(s), got %d", workflowName, len(wc.ImageNodes), len(images))
	}
	for i, nodeID := range wc.ImageNodes {
		node, ok := workflow[nodeID]
		if !ok || node.Inputs == nil {
			return nil, fmt.Errorf("image node %q not found in workflow %q", nodeID, wc.WorkflowPath)
		}
		if i < len(images) {
			node.Inputs["image"] = comfyImageInputName(images[i].Filename, images[i].Subfolder)
		}
	}
	if strength != nil && wc.DenoiseNode != "" {
		node, ok := workflow[wc.DenoiseNode]
		if !ok || node.Inputs == nil {
			return nil, fmt.Errorf("denoise node %q not found in workflow %q", wc.DenoiseNode, wc.WorkflowPath)
		}
		node.Inputs["denoise"] = *strength
	}
	return workflow, nil
}
```

`example.toml` — document both fields per the config docs convention:

```toml
# image_nodes (string list, default []) — LoadImage node ids an EDIT workflow
#   wires uploaded input images into (edit_image/edit_image_async only).
# denoise_node (string, default "") — node whose inputs.denoise receives the
#   edit strength; unset keeps the workflow file's own default.
```

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestPrepareComfyEditWorkflow' ./mcps/img-mcp && go test ./mcps/img-mcp`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mcps/img-mcp/config.go mcps/img-mcp/comfy.go mcps/img-mcp/comfy_test.go mcps/img-mcp/example.toml
git commit -m "feat: edit workflow plumbing — image_nodes, denoise_node, prepareComfyEditWorkflow"
```

---

### Task 3: img-mcp — ComfyUI /upload/image client + gen_ ref resolution

**Files:**
- Modify: `mcps/img-mcp/comfy.go` (`uploadComfyImage`, `parseGenRef`)
- Modify: `mcps/img-mcp/db.go` (`dbGetJobImage`)
- Create: `mcps/img-mcp/editinputs.go` (`fetchGenRefBytes`)
- Test: `mcps/img-mcp/comfy_test.go`, new `mcps/img-mcp/editinputs_test.go`

**Interfaces:**
- Consumes: `downloadComfyImage(ctx, baseURL string, img ComfyImage) ([]byte, ImageDLStats, error)` (comfy.go:811), `setupTestDB(t)` (testhelpers).
- Produces:
  - `uploadComfyImage(ctx context.Context, baseURL string, data []byte, filename string) (ComfyImage, error)`
  - `parseGenRef(s string) (jobID string, ordinal int, ok bool)` — `gen_<jobid>` → ordinal 0; `gen_<jobid>_<n>` → n; anything else → ok=false
  - `dbGetJobImage(db *sqlx.DB, jobID string, ordinal int) (img ComfyImage, url string, err error)`
  - `fetchGenRefBytes(ctx context.Context, db *sqlx.DB, comfyURL string, ref string) ([]byte, error)` — DB lookup → `/view` download → imgsite `url` fallback with WARN

- [ ] **Step 1: Write the failing tests**

`comfy_test.go` additions:

```go
func TestUploadComfyImage(t *testing.T) {
	var gotName, gotOverwrite string
	mux := http.NewServeMux()
	mux.HandleFunc("/upload/image", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(8<<20))
		f, hdr, err := r.FormFile("image")
		require.NoError(t, err)
		defer f.Close()
		gotName = hdr.Filename
		gotOverwrite = r.FormValue("overwrite")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"stored.png","subfolder":"dave","type":"input"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	img, err := uploadComfyImage(context.Background(), srv.URL, []byte("pngbytes"), "edit_1_0.jpg")
	require.NoError(t, err)
	assert.Equal(t, "stored.png", img.Filename)
	assert.Equal(t, "dave", img.Subfolder)
	assert.Equal(t, "input", img.Type)
	assert.Equal(t, "edit_1_0.jpg", gotName, "multipart file part carries the filename")
	assert.Equal(t, "true", gotOverwrite, "overwrite=true so re-edits replace cleanly")
}

func TestParseGenRef(t *testing.T) {
	job, ord, ok := parseGenRef("gen_abc12345")
	require.True(t, ok)
	assert.Equal(t, "abc12345", job)
	assert.Equal(t, 0, ord)
	job, ord, ok = parseGenRef("gen_abc12345_2")
	require.True(t, ok)
	assert.Equal(t, 2, ord)
	_, _, ok = parseGenRef("usr_abc")
	assert.False(t, ok)
	_, _, ok = parseGenRef("gen_")
	assert.False(t, ok)
	_, _, ok = parseGenRef("https://img.example.com/x")
	assert.False(t, ok)
}
```

New `editinputs_test.go`:

```go
func TestDbGetJobImage(t *testing.T) {
	db := setupTestDB(t)
	// Seed a completed-style job + two job_images rows the way dbCompleteJob does.
	// Reuse the existing seed helpers in queue_test.go/db_test.go if present;
	// otherwise insert directly:
	require.NoError(t, db.Exec(`INSERT INTO jobs (job_id, type, status, workflow, prompt) VALUES ('j1','generate','completed','w','p')`))
	require.NoError(t, db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('j1','https://img/1','f1.png','s','output','image/png',0)`))
	require.NoError(t, db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('j1','https://img/2','f2.png','s','output','image/png',1)`))

	img, url, err := dbGetJobImage(db, "j1", 1)
	require.NoError(t, err)
	assert.Equal(t, "f2.png", img.Filename)
	assert.Equal(t, "https://img/2", url)

	_, _, err = dbGetJobImage(db, "j1", 5)
	require.Error(t, err, "ordinal out of range errors")
	_, _, err = dbGetJobImage(db, "nope", 0)
	require.Error(t, err, "unknown job errors")
}

func TestFetchGenRefBytesViewPrimaryImgsiteFallback(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.Exec(`INSERT INTO jobs (job_id, type, status, workflow, prompt) VALUES ('j1','generate','completed','w','p')`))
	require.NoError(t, db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('j1','IMGURL','f1.png','','output','image/png',0)`))

	// /view serves the bytes -> used, no imgsite hit.
	var imgsiteHits int
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/view" {
			fmt.Fprint(w, "VIEWBYTES")
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer comfy.Close()
	imgsite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { imgsiteHits++ }))
	defer imgsite.Close()
	// point the row's url at the imgsite stub:
	require.NoError(t, db.Exec(`UPDATE job_images SET url = ? WHERE job_id='j1' AND ordinal=0`, imgsite.URL+"/i/1"))

	b, err := fetchGenRefBytes(context.Background(), db, comfy.URL, "gen_j1")
	require.NoError(t, err)
	assert.Equal(t, []byte("VIEWBYTES"), b)
	assert.Zero(t, imgsiteHits)

	// /view fails (500) -> imgsite fallback serves the bytes.
	comfy2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer comfy2.Close()
	b, err = fetchGenRefBytes(context.Background(), db, comfy2.URL, "gen_j1")
	require.NoError(t, err)
	assert.Equal(t, int(1), imgsiteHits)
	assert.NotEmpty(t, b, "fallback bytes come from the imgsite url")
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestUploadComfyImage|TestParseGenRef|TestDbGetJobImage|TestFetchGenRefBytes' ./mcps/img-mcp` → FAIL (undefined symbols).

- [ ] **Step 3: Implement**

`comfy.go`:

```go
// uploadComfyImage POSTs input bytes to ComfyUI's /upload/image (multipart
// "image" file part + overwrite=true so repeated edits of the same ref
// replace cleanly) and returns the stored coordinates for a LoadImage node.
func uploadComfyImage(ctx context.Context, baseURL string, data []byte, filename string) (ComfyImage, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="%s"`, filename))
	h.Set("Content-Type", "application/octet-stream")
	fw, err := mw.CreatePart(h)
	if err != nil {
		return ComfyImage{}, fmt.Errorf("building multipart: %w", err)
	}
	if _, err := fw.Write(data); err != nil {
		return ComfyImage{}, fmt.Errorf("writing multipart: %w", err)
	}
	if err := mw.WriteField("overwrite", "true"); err != nil {
		return ComfyImage{}, fmt.Errorf("building multipart: %w", err)
	}
	if err := mw.Close(); err != nil {
		return ComfyImage{}, fmt.Errorf("closing multipart: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/upload/image", &buf)
	if err != nil {
		return ComfyImage{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := httpClient.Do(req)
	if err != nil {
		return ComfyImage{}, fmt.Errorf("uploading image to comfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return ComfyImage{}, fmt.Errorf("comfy /upload/image returned %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Name     string `json:"name"`
		Subfolder string `json:"subfolder"`
		Type     string `json:"type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ComfyImage{}, fmt.Errorf("parsing /upload/image response: %w", err)
	}
	if out.Name == "" {
		return ComfyImage{}, fmt.Errorf("comfy /upload/image returned empty name")
	}
	if out.Type == "" {
		out.Type = "input"
	}
	return ComfyImage{Filename: out.Name, Subfolder: out.Subfolder, Type: out.Type}, nil
}

// parseGenRef parses gen_<jobid> (ordinal 0) and gen_<jobid>_<n>.
func parseGenRef(s string) (string, int, bool) {
	rest, ok := strings.CutPrefix(s, "gen_")
	if !ok || rest == "" {
		return "", 0, false
	}
	job, n, hasN := strings.Cut(rest, "_")
	if job == "" {
		return "", 0, false
	}
	if !hasN {
		return job, 0, true
	}
	ord, err := strconv.Atoi(n)
	if err != nil || ord < 0 {
		return "", 0, false
	}
	return job, ord, true
}
```

(Use the package's existing HTTP client — check what `downloadComfyImage`/`submitComfyPrompt` use and reuse the same one rather than inventing `httpClient`; imports: `mime/multipart`, `net/textproto`, `strconv`.)

`db.go`:

```go
// dbGetJobImage returns the nth (0-based) job_images row for a job — the
// ComfyUI /view coordinates plus the imgsite fallback URL.
func dbGetJobImage(db *sqlx.DB, jobID string, ordinal int) (ComfyImage, string, error) {
	var row struct {
		Filename  string `db:"filename"`
		Subfolder string `db:"subfolder"`
		ImgType   string `db:"img_type"`
		URL       string `db:"url"`
	}
	err := db.Get(&row, `SELECT filename, subfolder, img_type, url FROM job_images WHERE job_id = ? AND ordinal = ?`, jobID, ordinal)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ComfyImage{}, "", fmt.Errorf("no image %d for job %q", ordinal, jobID)
		}
		return ComfyImage{}, "", err
	}
	return ComfyImage{Filename: row.Filename, Subfolder: row.Subfolder, Type: row.ImgType}, row.URL, nil
}
```

`editinputs.go` (new):

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// fetchGenRefBytes resolves a gen_<jobid>_<n> ref to bytes: the job_images
// row's ComfyUI /view coordinates first, the imgsite URL as fallback (WARN)
// when ComfyUI's output dir no longer has the file. First-party bytes only —
// img-mcp never downloads arbitrary URLs (that is dave's pipeline's job).
func fetchGenRefBytes(ctx context.Context, db *sqlx.DB, comfyURL string, ref string) ([]byte, error) {
	jobID, ordinal, ok := parseGenRef(ref)
	if !ok {
		return nil, fmt.Errorf("not a gen_ ref: %q", ref)
	}
	img, fallbackURL, err := dbGetJobImage(db, jobID, ordinal)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", ref, err)
	}
	data, _, err := downloadComfyImage(ctx, comfyURL, img)
	if err == nil && len(data) > 0 {
		return data, nil
	}
	loggerEditInputs.Warn("gen ref /view download failed, falling back to imgsite url", "ref", ref, "error", fmt.Sprintf("%v", err))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fallbackURL, nil)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", ref, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resolving %q (view and imgsite both failed): %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolving %q: imgsite fallback returned %d", ref, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}
```

(Declare `loggerEditInputs` with the package logger idiom; import `github.com/jmoiron/sqlx`.)

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestUploadComfyImage|TestParseGenRef|TestDbGetJobImage|TestFetchGenRefBytes' ./mcps/img-mcp && go test ./mcps/img-mcp`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add mcps/img-mcp/comfy.go mcps/img-mcp/db.go mcps/img-mcp/editinputs.go mcps/img-mcp/comfy_test.go mcps/img-mcp/editinputs_test.go
git commit -m "feat: comfy /upload/image client + gen_ ref resolution"
```

---

### Task 4: img-mcp — edit input resolution + persistence (migration 005)

**Files:**
- Create: `mcps/img-mcp/migrations/005_add_jobs_input_images.sql`
- Modify: `mcps/img-mcp/queue.go` (`JobInput` gains `InputImages`/`Strength`), `mcps/img-mcp/db.go` (insert/select/rebuild)
- Modify: `mcps/img-mcp/editinputs.go` (`InputImageRef`, `resolveEditInputs`)
- Test: `mcps/img-mcp/editinputs_test.go`, `mcps/img-mcp/db_test.go`

**Interfaces:**
- Consumes: Task 3's `uploadComfyImage`/`fetchGenRefBytes`/`parseGenRef`.
- Produces:
  - `type InputImageRef struct { Ref string; Comfy ComfyImage }`
  - `JobInput.InputImages []InputImageRef`, `JobInput.Strength *float64`
  - `resolveEditInputs(ctx, db *sqlx.DB, cfg Config, jobID string, entries []string) ([]InputImageRef, error)` — entries: data URIs decode+upload; `gen_*` fetch+upload; anything else → error teaching view_image
  - `dbUpdateJobInputImages(db, jobID string, imgs []InputImageRef) error`
  - migration 005: `jobs.input_images TEXT DEFAULT ''`, `jobs.strength REAL NULL`

- [ ] **Step 1: Write the failing tests**

Add to `editinputs_test.go`:

```go
func TestResolveEditInputsDispatch(t *testing.T) {
	db := setupTestDB(t)
	// gen_ source: seed a job + image whose /view bytes are servable.
	require.NoError(t, db.Exec(`INSERT INTO jobs (job_id, type, status, workflow, prompt) VALUES ('g1','generate','completed','w','p')`))
	require.NoError(t, db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('g1','','gen1.png','','output','image/png',0)`))

	var mu sync.Mutex
	uploads := map[string]string{} // uploaded filename -> decoded bytes
	mux := http.NewServeMux()
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "GENBYTES")
	})
	mux.HandleFunc("/upload/image", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(8<<20))
		f, hdr, err := r.FormFile("image")
		require.NoError(t, err)
		defer f.Close()
		b, _ := io.ReadAll(f)
		mu.Lock()
		uploads[hdr.Filename] = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"subfolder":"","type":"input"}`, hdr.Filename)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	cfg := testConfig(srv.URL)

	refs, err := resolveEditInputs(context.Background(), db, cfg, "jj1", []string{
		"data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("USERBYTES")),
		"gen_g1",
	})
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, "data:image/jpeg;base64,VVNFUkJZVEVT", refs[0].Ref, "data-URI entry keeps its full original string for provenance")
	assert.Equal(t, "gen_g1", refs[1].Ref)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "USERBYTES", uploads["edit_jj1_0.jpg"], "data-URI bytes uploaded with per-entry filename + mime-derived ext")
	assert.Equal(t, "GENBYTES", uploads["edit_jj1_1.png"], "gen_ bytes fetched from /view then uploaded")
	assert.Equal(t, "edit_jj1_0.jpg", refs[0].Comfy.Filename, "echoed upload name becomes the Comfy coordinate")
	assert.Equal(t, "edit_jj1_1.png", refs[1].Comfy.Filename)
}

func TestResolveEditInputsRejectsUnknownEntry(t *testing.T) {
	db := setupTestDB(t)
	cfg := testConfig("")
	_, err := resolveEditInputs(context.Background(), db, cfg, "jj1", []string{"https://img.example.com/some.png"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "view_image")
	assert.Contains(t, err.Error(), "https://img.example.com/some.png", "error names the offending entry")
}
```

Add to `db_test.go` (round-trip, mirroring the file's existing insert/reload test style):

```go
func TestJobInputImagesRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	Strength := 0.55
	job := &Job{ID: "rt1", Type: JobTypeEdit, Status: StatusQueued, Workflow: "w",
		Input: JobInput{Prompt: "p", InputImages: []InputImageRef{
			{Ref: "web_aaa", Comfy: ComfyImage{Filename: "f.jpg"}},
			{Ref: "gen_bbb_0", Comfy: ComfyImage{Filename: "g.png", Subfolder: "s"}},
		}, Strength: &Strength}}
	require.NoError(t, dbInsertJob(db, job))
	require.NoError(t, dbUpdateJobInputImages(db, job.ID, job.Input.InputImages))

	dbj := loadDBJobForTest(t, db, "rt1") // existing reload helper in db_test.go, or dbGetJob equivalent
	got := jobFromDBJob(dbj)
	require.Len(t, got.Input.InputImages, 2)
	assert.Equal(t, "web_aaa", got.Input.InputImages[0].Ref)
	assert.Equal(t, "g.png", got.Input.InputImages[1].Comfy.Filename)
	require.NotNil(t, got.Input.Strength)
	assert.Equal(t, 0.55, *got.Input.Strength)
}
```

(If `loadDBJobForTest` does not exist, load via the same SELECT `jobFromDBJob` consumes — the file's existing tests show how.)

- [ ] **Step 2: Run to verify failure** — `go test -run 'TestResolveEditInputs|TestJobInputImagesRoundTrip' ./mcps/img-mcp` → FAIL (undefined).

- [ ] **Step 3: Implement**

`migrations/005_add_jobs_input_images.sql`:

```sql
-- +goose Up
-- Edit jobs persist their resolved ComfyUI input-image upload coordinates
-- (JSON of [{ref, comfy filename/subfolder/type}]) plus the edit strength so
-- restart recovery re-submits without re-uploading. Insert/recovery-only
-- columns: no terminal-status UPDATE touches them.
ALTER TABLE jobs ADD COLUMN input_images TEXT DEFAULT '';
ALTER TABLE jobs ADD COLUMN strength REAL NULL;

-- +goose Down
ALTER TABLE jobs DROP COLUMN strength;
ALTER TABLE jobs DROP COLUMN input_images;
```

`queue.go` `JobInput` — add:

```go
	// InputImages carries the RESOLVED edit inputs (uploaded ComfyUI
	// coordinates + the original ref strings for provenance) and Strength the
	// edit denoise. Persisted (migration 005) so recovery re-submits with the
	// same uploaded names without re-uploading.
	InputImages []InputImageRef
	Strength    *float64
```

`editinputs.go` — add:

```go
// InputImageRef pairs the caller's original reference string (usr_/web_/gen_
// or the data URI — provenance for the note + upload meta) with the ComfyUI
// upload coordinates the bytes landed at.
type InputImageRef struct {
	Ref   string     `json:"ref"`
	Comfy ComfyImage `json:"comfy"`
}

// extForDataMIME maps a data-URI mime to a filename extension ComfyUI's
// picker recognizes.
func extForDataMIME(mime string) string {
	switch strings.TrimPrefix(mime, "data:") {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	default:
		return ".jpg"
	}
}

// resolveEditInputs turns edit tool input_images entries into uploaded
// ComfyUI coordinates: data URIs (dave-resolved usr_/web_ refs) decode and
// upload; gen_ refs fetch from ComfyUI/imgsite and upload; anything else is
// an error teaching the model the ref loop. Entries are resolved in order.
func resolveEditInputs(ctx context.Context, db *sqlx.DB, cfg Config, jobID string, entries []string) ([]InputImageRef, error) {
	var out []InputImageRef
	for i, entry := range entries {
		var (
			data []byte
			ext  = ".jpg"
			ref  = entry
		)
		if strings.HasPrefix(entry, "data:") {
			comma := strings.Index(entry, ",")
			if comma < 0 {
				return nil, fmt.Errorf("input_images[%d]: malformed data URI", i)
			}
			b64 := entry[comma+1:]
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("input_images[%d]: decoding data URI: %v", i, err)
			}
			data = raw
			ext = extForDataMIME(entry[:comma])
		} else if _, _, ok := parseGenRef(entry); ok {
			raw, err := fetchGenRefBytes(ctx, db, cfg.Comfy.BaseURL, entry)
			if err != nil {
				return nil, fmt.Errorf("input_images[%d]: %v", i, err)
			}
			data = raw
			ext = ".png"
		} else {
			return nil, fmt.Errorf("input_images[%d]: %q is not an image ref — view the image first (view_image) and pass its ref (usr_/web_) or a gen_ ref from a generation result", i, entry)
		}
		comfy, err := uploadComfyImage(ctx, cfg.Comfy.BaseURL, data, fmt.Sprintf("edit_%s_%d%s", jobID, i, ext))
		if err != nil {
			return nil, fmt.Errorf("input_images[%d]: %v", i, err)
		}
		out = append(out, InputImageRef{Ref: ref, Comfy: comfy})
	}
	return out, nil
}
```

`db.go`: extend `dbInsertJob`'s INSERT with `input_images, strength` (empty string / nil when unset — marshal with `json.Marshal` when non-empty; strength as `job.Input.Strength` which sqlx writes as NULL for nil), extend the `dbJob` struct + its SELECT with `InputImages string \`db:"input_images"\`` and `Strength *float64 \`db:"strength"\`` (both with a vestige-style comment), and add:

```go
// dbUpdateJobInputImages persists resolved edit-input upload coordinates
// after processJob uploads them — the recovery re-submit seam. Deliberately
// unfenced like jobs.safety: it touches only these columns and can never
// contend with a terminal status write.
func dbUpdateJobInputImages(db *sqlx.DB, jobID string, imgs []InputImageRef) error {
	b, err := json.Marshal(imgs)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE jobs SET input_images = ? WHERE job_id = ?`, string(b), jobID)
	return err
}
```

`jobFromDBJob` — reconstruct: unmarshal `InputImages` when non-empty; copy `Strength`.

- [ ] **Step 4: Run tests** — `go test -run 'TestResolveEditInputs|TestJobInputImagesRoundTrip|TestRecover|TestTerminalDB' ./mcps/img-mcp && go test ./mcps/img-mcp` (recovery + fencing suites must stay green). PASS expected.

- [ ] **Step 5: Commit**

```bash
git add mcps/img-mcp/migrations mcps/img-mcp/queue.go mcps/img-mcp/db.go mcps/img-mcp/editinputs.go mcps/img-mcp/editinputs_test.go mcps/img-mcp/db_test.go
git commit -m "feat: edit input resolution + persisted upload coordinates (migration 005)"
```

---

### Task 5: img-mcp — edit job path, handlers, tool registration

**Files:**
- Modify: `mcps/img-mcp/queue.go` (`JobTypeEdit`, processJob edit branch, note/meta)
- Modify: `mcps/img-mcp/comfy.go` (`promptNotePayload` + `buildPromptNote*` pass-through)
- Modify: `mcps/img-mcp/tools.go` (inputs, handlers, `ImageData.Ref` population hook)
- Modify: `mcps/img-mcp/server.go` (registration)
- Test: `mcps/img-mcp/queue_test.go`, `mcps/img-mcp/tools_test.go`

**Interfaces:**
- Consumes: Tasks 2-4 (`prepareComfyEditWorkflow`, `resolveEditInputs`, `dbUpdateJobInputImages`, persisted `InputImages`/`Strength`).
- Produces: `edit_image` + `edit_image_async` tools; `JobTypeEdit JobType = "edit"`; `EditImageInput`/`EditImageAsyncInput`; `promptNotePayload.EditedFrom []string` + `.Strength *float64` (omitempty); `UploadMeta.EditedFrom []string` + `.Strength *float64` (omitempty).

- [ ] **Step 1: Write the failing tests**

`tools_test.go`:

```go
func TestHandleEditImageValidates(t *testing.T) {
	h := newTestToolHandlers(t) // existing harness in tools_test.go, or mirror how other handler tests build ToolHandlers+queue
	cases := []struct {
		name string
		args EditImageInput
		want string
	}{
		{"no inputs", EditImageInput{Prompt: "p"}, "input_images"},
		{"bad strength low", EditImageInput{Prompt: "p", InputImages: []string{"gen_a"}, Strength: floatPtr(0)}, "strength"},
		{"bad strength high", EditImageInput{Prompt: "p", InputImages: []string{"gen_a"}, Strength: floatPtr(1.5)}, "strength"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := h.handleEditImageForTest(context.Background(), tc.args) // thin wrapper or call the handler with a zero req like sibling tests do
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestHandleEditImageRequiresImageNodes(t *testing.T) {
	h := newTestToolHandlers(t) // config whose only workflow lacks image_nodes
	_, _, err := h.handleEditImageForTest(context.Background(), EditImageInput{Prompt: "p", InputImages: []string{"gen_a"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_nodes")
}
```

(Add `floatPtr` helper if absent. If handlers can't be called without an `*mcp.CallToolRequest`, build a zero `&mcp.CallToolRequest{}` the way existing handler tests in the file do — grep for `handleGenerateImage(` in tools_test.go and copy the invocation pattern.)

`queue_test.go`:

```go
func editQueueConfig(t *testing.T, comfyURL string) Config {
	cfg := testConfig(comfyURL)
	cfg.Workflows["edit"] = WorkflowConfig{
		WorkflowPath: mustWriteWorkflowString(t, editTestWorkflowJSON(t)),
		ClientID:     "test-client", OutputNode: "output-node", PromptNode: "6",
		ImageNodes:  []string{"40"},
		DenoiseNode: "3",
		Timeout:     60,
	}
	return cfg
}

func TestProcessJobEditSubmitsUploadedInputs(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	cfg := editQueueConfig(t, mockComfy.URL())
	q, stop := setupTestQueue(t, cfg)
	defer stop()
	Strength := 0.65
	inputs := []InputImageRef{{Ref: "web_aa", Comfy: ComfyImage{Filename: "in0.jpg"}}}
	job, err := q.Submit(JobTypeEdit, "edit", JobInput{Prompt: "edit it", InputImages: inputs, Strength: &Strength})
	require.NoError(t, err)
	result := q.WaitForJob(job.ID, 30*time.Second)
	require.NotNil(t, result)
	require.Empty(t, result.Error, "job must succeed: %s", result.Error)

	prompts := mockComfy.submittedPrompts()
	require.Len(t, prompts, 1)
	wf := prompts[0].Prompt // ComfyPromptRequest.Prompt is ComfyWorkflow — assert on it:
	assert.Equal(t, "in0.jpg", wf["40"].Inputs["image"], "uploaded name wired into image node")
	assert.Equal(t, 0.65, wf["3"].Inputs["denoise"], "strength wired into denoise node")
	assert.Equal(t, "edit it", wf["6"].Inputs["text"])
}

func TestEditJobRecoveryReusesPersistedUploadNames(t *testing.T) {
	// A counting front-proxy over the flow mock: recovery must re-submit the
	// SAME uploaded name without a second /upload/image hit.
	var uploads int32
	mockComfy := newMockComfyFlowServer(t)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upload/image" {
			atomic.AddInt32(&uploads, 1)
		}
		mockComfy.ServeHTTP(w, r) // shim added to mockComfyFlowServer in this task if not already exposed
	}))
	defer proxy.Close()
	cfg := editQueueConfig(t, proxy.URL)
	q, stop := setupTestQueue(t, cfg)
	defer stop()

	// Fresh edit job (RawInputImages set, no resolved coords): resolution
	// uploads once (via a data-URI entry pointing at the proxy's /upload).
	Strength := 0.5
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNG"))
	job, err := q.Submit(JobTypeEdit, "edit", JobInput{Prompt: "p", RawInputImages: []string{dataURI}, Strength: &Strength})
	require.NoError(t, err)
	res := q.WaitForJob(job.ID, 30*time.Second)
	require.NotNil(t, res)
	require.Empty(t, res.Error)
	assert.Equal(t, int32(1), atomic.LoadInt32(&uploads), "fresh run uploads exactly once")
	first := mockComfy.submittedPrompts()[0].Prompt["40"].Inputs["image"]

	// Recovery shape: a NEW queue (simulated restart) processing a job whose
	// InputImages were already persisted (jobFromDBJob reconstruction).
	// Re-submit the completed job's resolved coords through a queued Job:
	recJob, err := q.Submit(JobTypeEdit, "edit", JobInput{
		Prompt: "p", Strength: &Strength,
		InputImages: []InputImageRef{{Ref: dataURI, Comfy: ComfyImage{Filename: filepathFromInputName(t, first)}}},
	})
	require.NoError(t, err)
	res2 := q.WaitForJob(recJob.ID, 30*time.Second)
	require.NotNil(t, res2)
	require.Empty(t, res2.Error)
	assert.Equal(t, int32(1), atomic.LoadInt32(&uploads), "recovered job re-submits WITHOUT re-uploading")
	submitted := mockComfy.submittedPrompts()
	assert.Equal(t, first, submitted[len(submitted)-1].Prompt["40"].Inputs["image"], "same uploaded name re-used")
}

// filepathFromInputName strips a possible subfolder prefix for the Comfy
// coordinate used in the recovery assertion.
func filepathFromInputName(t *testing.T, in string) string {
	t.Helper()
	if i := strings.LastIndex(in, "/"); i >= 0 {
		return in[i+1:]
	}
	return in
}
```

(If `mockComfyFlowServer` does not already expose its handler, add a two-line `ServeHTTP` method to it in comfy_test.go — `func (m *mockComfyFlowServer) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.server.Handler.ServeHTTP(w, r) }` — the minimal shim this test needs.)

- [ ] **Step 2: Run to verify failure** → FAIL (no edit tools/types).

- [ ] **Step 3: Implement**

`queue.go`: `JobTypeEdit JobType = "edit"` in the constants block. In `processJob`, after the enhance-generate block (queue.go ~738-770) and BEFORE the existing `prepareComfyWorkflow` call (~790), add the edit branch:

```go
	var editImages []ComfyImage
	if job.Type == JobTypeEdit {
		// Recovery re-submit: InputImages already carry uploaded coordinates
		// persisted by the first run (dbUpdateJobInputImages) — skip
		// resolution/upload entirely. If ComfyUI's input dir was cleaned
		// since, the submit/monitor fails with a readable error (accepted).
		if len(job.Input.InputImages) == 0 {
			refs, err := resolveEditInputs(jobCtx, q.db, cfg, job.ID, job.Input.RawInputImages)
			if err != nil {
				q.failJob(job, fmt.Sprintf("edit inputs failed: %v", err))
				return
			}
			job.Input.InputImages = refs
			if q.db != nil {
				if uerr := dbUpdateJobInputImages(q.db, job.ID, refs); uerr != nil {
					loggerQueue.Warn("persisting edit input coords failed (recovery will re-resolve)", "job_id", job.ID, "error", uerr.Error())
				}
			}
		}
		for _, r := range job.Input.InputImages {
			editImages = append(editImages, r.Comfy)
		}
	}
```

and replace the workflow preparation call with a branch:

```go
	var workflow ComfyWorkflow
	var werr error
	if job.Type == JobTypeEdit {
		workflow, werr = prepareComfyEditWorkflow(cfg, job.Workflow, prompt, negativePrompt, job.Input.Seed, promptNote, editImages, job.Input.Strength)
	} else {
		workflow, werr = prepareComfyWorkflow(cfg, job.Workflow, prompt, negativePrompt, job.Input.Seed, promptNote)
	}
	if werr != nil {
		q.failJob(job, fmt.Sprintf("workflow preparation failed: %v", werr))
		return
	}
```

`JobInput` gains `RawInputImages []string` (the model's entries — NOT persisted; resolution happens at processJob time and the RESOLVED refs persist instead). On the resolve-fresh path, seed `RawInputImages` from the handler.

`comfy.go` `promptNotePayload` — add:

```go
	EditedFrom []string `json:"edited_from,omitempty"`
	Strength   *float64 `json:"strength,omitempty"`
```

`buildPromptNoteWithSafety` — populate from `job.Input` (`EditedFrom`: each `InputImages[i].Ref`; `Strength: job.Input.Strength`).

`queue.go` `buildUploadMeta` — add the same two fields to `UploadMeta` (upload.go) and populate identically.

`tools.go`:

```go
type EditImageInput struct {
	Prompt         string   `json:"prompt" jsonschema:"the edit to apply — describe the change, not the whole scene"`
	InputImages    []string `json:"input_images" jsonschema:"image references to edit — ref ids from image labels in the conversation (usr_/web_) or gen_ refs from generation results"`
	NegativePrompt string   `json:"negative_prompt,omitempty" jsonschema:"optional negative prompt"`
	Workflow       string   `json:"workflow,omitempty" jsonschema:"name of the edit workflow config to use (empty or 'default' uses the default workflow)"`
	Seed           *int64   `json:"seed,omitempty" jsonschema:"optional fixed seed for reproducibility"`
	Strength       *float64 `json:"strength,omitempty" jsonschema:"edit strength 0-1 (how much to change; workflow default when omitted)"`
	Timeout        int      `json:"timeout,omitempty" jsonschema:"max seconds to wait (default: 300)"`
	Network        string   `json:"_dave_inject_network,omitempty"`
	Channel        string   `json:"_dave_inject_channel,omitempty"`
	Nick           string   `json:"_dave_inject_nick,omitempty"`
	LLMGenerated   bool     `json:"_dave_inject_llm_generated,omitempty"`
}

type EditImageAsyncInput struct { // same minus Timeout
```

Handlers mirror the generate pair verbatim (`handleEditImage` / `handleEditImageAsync`), with pre-Submit validation:

```go
	if len(input.InputImages) == 0 {
		return nil, EditImageOutput{}, fmt.Errorf("input_images is required (at least one image reference)")
	}
	if input.Strength != nil && (*input.Strength <= 0 || *input.Strength > 1) {
		return nil, EditImageOutput{}, fmt.Errorf("strength must be between 0 and 1")
	}
	workflow, err := h.resolveWorkflow(input.Workflow)
	if err != nil {
		return nil, EditImageOutput{}, err
	}
	if len(h.getConfig().Workflows[workflow].ImageNodes) == 0 {
		return nil, EditImageOutput{}, fmt.Errorf("workflow %q has no image_nodes configured — not an edit workflow", workflow)
	}
	job, err := h.queue.Submit(JobTypeEdit, workflow, JobInput{
		Prompt: input.Prompt, NegativePrompt: input.NegativePrompt,
		RawInputImages: input.InputImages, Seed: input.Seed, Strength: input.Strength,
		LLMGenerated: input.LLMGenerated,
		Network: input.Network, Channel: input.Channel, Nick: input.Nick,
	})
```

(async returns `GenerateImageAsyncOutput{JobID: job.ID}`; sync waits + returns `GenerateImageOutput`-shaped output — reuse `GenerateImageOutput`).

`server.go`: register `edit_image` on sync + full (description: "Edit one or more images using an img2img workflow. Pass image references in input_images — the ref ids shown next to images in the conversation (usr_/web_) or gen_ refs from generation results. strength controls how much to change (0-1).") and `edit_image_async` on async + full (same + non-blocking note), copying the existing registration shape.

- [ ] **Step 4: Run tests** — `go test -run 'TestHandleEditImage|TestProcessJobEdit|TestEditJobRecovery' ./mcps/img-mcp && go test ./mcps/img-mcp` → PASS.

- [ ] **Step 5: Commit**

```bash
git add mcps/img-mcp/queue.go mcps/img-mcp/comfy.go mcps/img-mcp/tools.go mcps/img-mcp/server.go mcps/img-mcp/upload.go mcps/img-mcp/queue_test.go mcps/img-mcp/tools_test.go
git commit -m "feat: edit_image/edit_image_async — JobTypeEdit end to end"
```

---

### Task 6: img-mcp — results carry `gen_` refs

**Files:**
- Modify: `mcps/img-mcp/tools.go` (`ImageData.Ref`)
- Modify: `mcps/img-mcp/queue.go` (population after upload)
- Modify: `mcps/img-mcp/db.go` (`buildJobResultFromDB`)
- Test: `mcps/img-mcp/queue_test.go`, `db_test.go`

**Interfaces:**
- Consumes: Task 5.
- Produces: `ImageData.Ref string \`json:"ref,omitempty"\`` populated as `gen_<jobid>_<ordinal>` on live results AND DB-rebuilt results. This is how the sync `generate_image` path finally exposes its job identity.

- [ ] **Step 1: Write the failing tests**

`queue_test.go`:

```go
func TestResultsCarryGenRefs(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	mockComfy.serveHistoryImages(2)
	// imgsite upload target: newBlockingUploadServer exists for parking — use
	// a minimal /updo stub returning {"url":..., "page":...} per upload_test.go's pattern.
	cfg := testConfig(mockComfy.URL())
	// ... run a generate job to completion (mirror TestProcessJobSubmitsPromptNote's flow)
	result := q.WaitForJob(job.ID, 30*time.Second)
	require.NotNil(t, result)
	require.Empty(t, result.Error)
	require.Len(t, result.Result.Images, 2)
	assert.Equal(t, "gen_"+job.ID+"_0", result.Result.Images[0].Ref)
	assert.Equal(t, "gen_"+job.ID+"_1", result.Result.Images[1].Ref)
}
```

`db_test.go`:

```go
func TestBuildJobResultFromDBIncludesRefs(t *testing.T) {
	db := setupTestDB(t)
	require.NoError(t, db.Exec(`INSERT INTO jobs (job_id, type, status, workflow, prompt) VALUES ('r1','generate','completed','w','p')`))
	require.NoError(t, db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('r1','https://img/a','a.png','','output','image/png',0)`))
	res := buildJobResultFromDB(db, "r1")
	require.NotNil(t, res)
	require.Len(t, res.Images, 1)
	assert.Equal(t, "gen_r1_0", res.Images[0].Ref)
}
```

- [ ] **Step 2: Run to verify failure** → FAIL (no Ref field).

- [ ] **Step 3: Implement**

`tools.go` `ImageData` — add `Ref string \`json:"ref,omitempty"\`` with a comment ("gen_<jobid>_<n> — pass back in edit_image input_images").

`queue.go`: at the site where completed images' imgsite URLs are set into `result.Result.Images` (grep `ImageData{` / the upload loop in processJob), set `img.Ref = fmt.Sprintf("gen_%s_%d", job.ID, i)` per ordinal. `db.go` `buildJobResultFromDB` (db.go:359-385): while iterating `job_images` rows it already knows the ordinal — set `Ref: fmt.Sprintf("gen_%s_%d", jobID, ordinal)`.

- [ ] **Step 4: Run tests** — focused + `go test ./mcps/img-mcp` → PASS.

- [ ] **Step 5: Commit**

```bash
git add mcps/img-mcp/tools.go mcps/img-mcp/queue.go mcps/img-mcp/db.go mcps/img-mcp/queue_test.go mcps/img-mcp/db_test.go
git commit -m "feat: results carry gen_<jobid>_<n> refs"
```

---

### Task 7: dave — `input_images` ref interception

**Files:**
- Modify: `imageref.go` (`resolveImageRefArgs`)
- Modify: `mcpClient.go` (var seam beside the inject helpers)
- Modify: `aiCmds.go` (executeToolCalls branch), `mcpCmds.go` (sync + async paths)
- Test: `imageref_test.go`, `aiCmds_test.go`

**Interfaces:**
- Consumes: `resolveImageRef(ref string) (*ImageRef, error)` (imageref.go:156), `daveInjectPrefix` convention.
- Produces: `resolveImageRefArgs(toolArgs map[string]any) error` — rewrites `input_images` entries (`usr_*`/`web_*` → `data:<mime>;base64,…`; everything else untouched); non-nil error = abort the call naming the ref. Var seam `resolveImageRefArgsFn` for wiring tests (repo precedent: `connectMCPServerImpl`, `claimNickForFn`).

- [ ] **Step 1: Write the failing tests**

`imageref_test.go`:

```go
func TestResolveImageRefArgsRewritesUsrWebPassesGen(t *testing.T) {
	setupTestDB(t)
	usrRef, err := registerImageRef(1, "user", "https://x/a.jpg", "image/jpeg", 10, 10, []byte("USRB"))
	require.NoError(t, err)
	webRef, err := registerImageRef(1, "view", "https://x/b.png", "image/png", 10, 10, []byte("WEBB"))
	require.NoError(t, err)

	entries := []string{usrRef, "gen_abc_0", webRef, "data:image/png;base64,QQ=="}

	// Shape 1: []string (programmatic callers)
	args := map[string]any{"input_images": append([]string(nil), entries...)}
	require.NoError(t, resolveImageRefArgs(args))
	got := args["input_images"].([]string)
	assert.Equal(t, "data:image/jpeg;base64,VVNSUg==", got[0], "usr_ ref rewritten from the stored file's bytes")
	assert.Equal(t, "gen_abc_0", got[1], "gen_ passthrough")
	assert.Equal(t, "data:image/png;base64,V0VCQg==", got[2], "web_ ref rewritten")
	assert.Equal(t, "data:image/png;base64,QQ==", got[3], "existing data URI untouched")

	// Shape 2: []any (what json.Unmarshal of model args produces)
	anyEntries := make([]any, len(entries))
	for i, e := range entries {
		anyEntries[i] = e
	}
	args2 := map[string]any{"input_images": anyEntries}
	require.NoError(t, resolveImageRefArgs(args2))
	got2 := args2["input_images"].([]any)
	assert.Equal(t, "data:image/jpeg;base64,VVNSUg==", got2[0])
	assert.Equal(t, "gen_abc_0", got2[1])
	assert.Equal(t, "data:image/png;base64,V0VCQg==", got2[2])

	// No input_images -> no-op, no error
	plain := map[string]any{"prompt": "hi"}
	require.NoError(t, resolveImageRefArgs(plain))
}

func TestResolveImageRefArgsUnresolvableRef(t *testing.T) {
	setupTestDB(t)
	args := map[string]any{"input_images": []any{"usr_deadbeefcafe"}}
	err := resolveImageRefArgs(args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "usr_deadbeefcafe")
}
```

`aiCmds_test.go`:

```go
func TestExecuteToolCallsAbortsUnresolvableRef(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)
	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 5}, 9) // Task-5-of-conversation-images helper, image_detect_test.go
	turn := newTurnContext(9, nil)
	turn.Add(ChatMessage{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1"}}})

	orig := resolveImageRefArgsFn
	resolveImageRefArgsFn = func(map[string]any) error { return fmt.Errorf("image ref %q could not be resolved: missing", "usr_x") }
	t.Cleanup(func() { resolveImageRefArgsFn = orig })

	// A tool name that resolves as MCP is required to reach the args stage:
	// register one via the test MCP registration path used by
	// TestCallMCPToolWakesPendingRetry's in-process variant, or stub the
	// resolution by naming a tool present in the mcpTools map fixture —
	// mirror whichever pattern the existing executeToolCalls tests use.
	args, _ := json.Marshal(map[string]any{"input_images": []string{"usr_x"}})
	cr.executeToolCalls(turn, []ToolCall{{ID: "c1", Type: "function",
		Function: FunctionCall{Name: registeredTestMCPToolName(t), Arguments: string(args)}}})

	last := turn.Messages()[len(turn.Messages())-1]
	assert.Equal(t, RoleTool, last.Role)
	assert.Contains(t, last.Content, "usr_x", "call aborted with an error result naming the ref")
	assert.Empty(t, cr.pendingInjections)
}
```

(The test needs ONE registered MCP tool name so dispatch reaches the args stage — look at how existing root tests register in-memory MCP tools (`getMCPTools`/`registerMCPServer` fixtures in mcpClient_test.go) and reuse; if the only harness is the heavyweight binary one, an acceptable lighter path is calling the branch's helper directly with a registered name via the in-memory map the wake tests use.)

- [ ] **Step 2: Run to verify failure** → FAIL (`undefined: resolveImageRefArgs`).

- [ ] **Step 3: Implement**

`imageref.go` — add (with a design comment naming the `input_images` field-name convention alongside `_dave_inject_`):

```go
// resolveImageRefArgs rewrites input_images entries in MCP tool args from
// usr_/web_ refs to data URIs the MCP server can consume (base64 of the
// stored ref file). The FIELD NAME is a dave-MCP convention — the same idea
// as the _dave_inject_ prefix: any tool declaring an input_images string
// array gets its ref entries expanded here. gen_ refs, data URIs, and
// anything else pass through untouched (img-mcp owns gen_ resolution).
// A non-nil error aborts the call: the message names the dead ref.
var resolveImageRefArgsFn = resolveImageRefArgs

func resolveImageRefArgs(toolArgs map[string]any) error {
	raw, ok := toolArgs["input_images"]
	if !ok {
		return nil
	}
	rewrite := func(s string) (string, error) {
		if !strings.HasPrefix(s, "usr_") && !strings.HasPrefix(s, "web_") {
			return s, nil
		}
		row, err := resolveImageRef(s)
		if err != nil {
			return "", fmt.Errorf("image ref %q could not be resolved: %v", s, err)
		}
		data, err := os.ReadFile(row.FilePath)
		if err != nil {
			return "", fmt.Errorf("image ref %q could not be read: %v", s, err)
		}
		return "data:" + row.MIME + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	}
	switch entries := raw.(type) {
	case []string:
		for i, s := range entries {
			nv, err := rewrite(s)
			if err != nil {
				return err
			}
			entries[i] = nv
		}
	case []any:
		for i, v := range entries {
			s, ok := v.(string)
			if !ok {
				continue
			}
			nv, err := rewrite(s)
			if err != nil {
				return err
			}
			entries[i] = nv
		}
	}
	return nil
}
```

Wire at the three sites (each immediately after the existing `injectScopeArgs` call):

`aiCmds.go` executeToolCalls (after `injectScopeArgsFromRunner(toolArgs, tc.Function.Name, cr)`, before `callMCPToolWithContext`):

```go
		if err := resolveImageRefArgsFn(toolArgs); err != nil {
			toolMsg := toolResultMsg(tc.ID, "error: "+err.Error())
			turn.Add(toolMsg)
			continue
		}
```

`mcpCmds.go` sync path (after its `injectScopeArgs`) and `mcpCmdAsync` (after its `injectScopeArgs`) — same shape:

```go
	if err := resolveImageRefArgsFn(toolArgs); err != nil {
		sendOrDone(ctx, output, errorNotice(n.Tools.Failed, map[string]string{"error": err.Error()}))
		log.Error("MCP tool call failed", "error", err.Error())
		return
	}
```

Update the `resolveImageRef` doc comment in imageref.go — it is no longer "deliberately unused in production".

- [ ] **Step 4: Run tests** — `go test -run 'TestResolveImageRefArgs|TestExecuteToolCallsAborts' . && go test .` → PASS.

- [ ] **Step 5: Commit**

```bash
git add imageref.go imageref_test.go mcpClient.go aiCmds.go aiCmds_test.go mcpCmds.go
git commit -m "feat: dave intercepts input_images refs — usr_/web_ expand to data URIs"
```

---

### Task 8: docs + AGENTS.md + full verification

**Files:**
- Modify: `AGENTS.md` (img-mcp bullet + dave bullet extension)
- Modify: `mcps/img-mcp/example.toml` (edit tools + edit workflow example block)
- Modify: `docs/superpowers/specs/2026-11-06-image-editing-design.md` (status line → implemented)

**Interfaces:** Consumes Tasks 1-7. Produces: docs.

- [ ] **Step 1: Docs**

`AGENTS.md` — extend the conversation-images bullet (or add a sibling) with:

```markdown
- **Image editing** (img-mcp + dave, Nov 2026; spec `docs/superpowers/specs/2026-11-06-image-editing-design.md`): `edit_image`/`edit_image_async` (img2img, owner-provided workflows with `image_nodes` + optional `denoise_node`). The field name `input_images` is a dave-MCP convention (like `_dave_inject_`): `resolveImageRefArgs` rewrites `usr_`/`web_` entries to base64 data URIs at all three pre-call sites (LLM loop + both direct tools.toml paths; unresolvable ref aborts the call with a readable error). img-mcp resolves its own `gen_<jobid>_<n>` refs from `job_images` (ComfyUI `/view` primary, imgsite URL fallback), uploads input bytes via ComfyUI `/upload/image` (overwrite=true), persists resolved upload coordinates on the job (`jobs.input_images`/`strength`, migration 005) so recovery re-submits without re-upload, and rides the unchanged queue/safety/upload machinery as `JobTypeEdit`. EXIF note + imgsite upload meta carry `edited_from` refs + `strength`. Results carry `gen_` refs on every surface (`generate_image` sync included — its first job identity). img-mcp does ZERO pixel work — dave is the single normalization point (trust assumption: local callers only). `output_format` was dropped from all generation tools (owner ruling; results always url; `jobs.output_format` column is a vestige). Guarded by `TestPrepareComfyEditWorkflow*` + `TestUploadComfyImage`/`TestParseGenRef`/`TestDbGetJobImage`/`TestFetchGenRefBytes*` + `TestResolveEditInputs*` + `TestJobInputImagesRoundTrip` + `TestProcessJobEdit*`/`TestEditJobRecovery*` + `TestHandleEditImage*` + `TestResultsCarryGenRefs`/`TestBuildJobResultFromDBIncludesRefs` + `TestResolveImageRefArgs*`/`TestExecuteToolCallsAbortsUnresolvableRef` + `TestGenerationToolsHaveNoOutputFormat`.
```

`example.toml` — add a commented edit-workflow block:

```toml
# [workflow.zimage-edit]
# workflow_path = "workflows/z_image_turbo-edit.json"
# clientid = "img-mcp-edit"
# output_node = "28"
# prompt_node = "6"
# negative_prompt_node = "7"
# seed_nodes = ["3"]
# image_nodes = ["40"]      # LoadImage node ids receiving uploaded inputs
# denoise_node = "3"        # node whose inputs.denoise = strength (optional)
# timeout = 300
```

- [ ] **Step 2: Full verification**

```bash
go fmt ./...
go vet ./...
go test ./...
go build -o dave .
go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp
go build -o mcps/yt-mcp/yt-mcp ./mcps/yt-mcp
go build -o imgsite/imgsite ./imgsite
go build -o tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql ./tools/migrate-sqlite-to-pgsql
go build -o tools/prompt-eval/prompt-eval ./tools/prompt-eval
```

Expected: all green; delete any build artifacts created in-tree (gitignored — never `git add -f`).

- [ ] **Step 3: Commit**

```bash
git add AGENTS.md mcps/img-mcp/example.toml docs/superpowers/specs/2026-11-06-image-editing-design.md
git commit -m "docs: image editing reference + spec status"
```

---

## Self-Review notes (already applied)

- Spec coverage: edit tools (5), workflow plumbing (2), upload client + gen resolution (3), input resolution + persistence/migration (4), gen refs in results (6), dave interception (7), output_format drop (1), docs (8). Owner-ruling coverage: zero pixel work in img-mcp (no resize/decode tasks — confirmed no task adds image libs to img-mcp), gen native res accepted, base64 transport (Task 4 data URIs), error-and-teach (Task 4), no enhancement for edits (edit branch skips the enhance block).
- Known plan-vs-code seams flagged for implementers: Task 5's `RawInputImages` is a plan-introduced JobInput field (not persisted) — the spec's "persist resolved coords" is what recovery consumes; Task 6's population site located by grep (`ImageData{`) because the upload loop's exact line was not transcribed; Task 7's abort test reuses whichever in-memory MCP registration fixture exists — the brief names the candidate patterns rather than inventing one.
- Type consistency: `ComfyImage{Filename, Subfolder, Type}`, `InputImageRef{Ref, Comfy}`, `prepareComfyEditWorkflow(..., images []ComfyImage, strength *float64)`, `resolveImageRefArgs(map[string]any) error` checked across tasks.
