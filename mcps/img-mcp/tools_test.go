package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInjectLLMGeneratedNotRequired guards the regression where
// _dave_inject_llm_generated was a REQUIRED schema property: jsonschema-go
// marks every field without `omitempty` as required, and the MCP SDK server
// rejects tool calls that miss required properties. dave's direct tools.toml
// commands never send _dave_inject_* fields, so a required inject field
// breaks every non-LLM caller with
// "validating root: required: missing properties".
// The same applies to _dave_inject_network (pre-existing instance of the
// same bug, caught by the in-memory round-trip test below) and to the
// _dave_inject_channel/_dave_inject_nick pair added with the imgsite
// provenance plumbing — this exact bug has shipped twice already.
func TestInjectLLMGeneratedNotRequired(t *testing.T) {
	schemas := map[string]*jsonschema.Schema{}
	for name, infer := range map[string]func() (*jsonschema.Schema, error){
		"GenerateImageInput":           func() (*jsonschema.Schema, error) { return jsonschema.For[GenerateImageInput](nil) },
		"GenerateImageAsyncInput":      func() (*jsonschema.Schema, error) { return jsonschema.For[GenerateImageAsyncInput](nil) },
		"EnhanceAndGenerateInput":      func() (*jsonschema.Schema, error) { return jsonschema.For[EnhanceAndGenerateInput](nil) },
		"EnhanceAndGenerateAsyncInput": func() (*jsonschema.Schema, error) { return jsonschema.For[EnhanceAndGenerateAsyncInput](nil) },
		"EditImageInput":               func() (*jsonschema.Schema, error) { return jsonschema.For[EditImageInput](nil) },
		"EditImageAsyncInput":          func() (*jsonschema.Schema, error) { return jsonschema.For[EditImageAsyncInput](nil) },
		"EnhancePromptInput":           func() (*jsonschema.Schema, error) { return jsonschema.For[EnhancePromptInput](nil) },
	} {
		s, err := infer()
		require.NoError(t, err, name)
		schemas[name] = s
	}

	expectLLMGenerated := map[string]bool{
		"GenerateImageInput":           true,
		"GenerateImageAsyncInput":      true,
		"EnhanceAndGenerateInput":      true,
		"EnhanceAndGenerateAsyncInput": true,
		"EditImageInput":               true,
		"EditImageAsyncInput":          true,
		"EnhancePromptInput":           false,
	}
	// channel/nick provenance rides the same 4 generation inputs as
	// llm_generated; enhance_prompt only ever had network.
	expectChannelNick := map[string]bool{
		"GenerateImageInput":           true,
		"GenerateImageAsyncInput":      true,
		"EnhanceAndGenerateInput":      true,
		"EnhanceAndGenerateAsyncInput": true,
		"EditImageInput":               true,
		"EditImageAsyncInput":          true,
		"EnhancePromptInput":           false,
	}
	for name, s := range schemas {
		for _, req := range s.Required {
			assert.False(t, strings.HasPrefix(req, "_dave_inject_"),
				"%s: inject fields must be optional (add omitempty to the json tag), found required %q", name, req)
		}
		// The properties must still be advertised so dave can discover and inject them.
		_, hasNet := s.Properties["_dave_inject_network"]
		assert.True(t, hasNet, "%s: _dave_inject_network should still be a declared property", name)
		_, hasGen := s.Properties["_dave_inject_llm_generated"]
		assert.Equal(t, expectLLMGenerated[name], hasGen,
			"%s: unexpected presence of _dave_inject_llm_generated property", name)
		_, hasChan := s.Properties["_dave_inject_channel"]
		assert.Equal(t, expectChannelNick[name], hasChan,
			"%s: unexpected presence of _dave_inject_channel property", name)
		_, hasNick := s.Properties["_dave_inject_nick"]
		assert.Equal(t, expectChannelNick[name], hasNick,
			"%s: unexpected presence of _dave_inject_nick property", name)
	}
}

// TestCallGenerationToolsWithoutInjectFields exercises the SDK server's
// argument validation over a real (in-memory) MCP round trip: a caller that
// sends no _dave_inject_* fields — exactly what dave's direct tools.toml
// commands look like — must be accepted.
//
// Only the async server flavor is exercised: its handlers return immediately
// after queueing, while the sync handlers block in WaitForJob for up to 300s.
// All three server flavors (sync/async/stdio-full) register the same input
// structs and share the same schema inference path, so the schema-level
// assertions above cover the sync tools.
func TestCallGenerationToolsWithoutInjectFields(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	h := NewToolHandlers(cfg, queuedTestQueue(t))
	server := createAsyncServer(cfg, h)

	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx, st)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err, "connect")
	defer cs.Close()

	for _, tool := range []string{"generate_image_async", "enhance_and_generate_async"} {
		t.Run(tool, func(t *testing.T) {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name:      tool,
				Arguments: map[string]any{"prompt": "a cat"},
			})
			require.NoError(t, err, "tool call must pass schema validation without inject fields")
			var texts []string
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					texts = append(texts, tc.Text)
				}
			}
			require.False(t, res.IsError, "tool call should not return an error result: %s", strings.Join(texts, "; "))
		})
	}
}

// toolInputSchemaForTest returns the ADVERTISED input schema of the named
// tool — the wire-level map an MCP client receives from ListTools, not the
// struct-level jsonschema inference. The server is built through the same
// createFullServer path production uses (the one flavor that registers all
// four generation tools) and served over in-memory transports, mirroring
// TestCallGenerationToolsWithoutInjectFields.
func toolInputSchemaForTest(t *testing.T, name string) map[string]any {
	t.Helper()
	cfg := testConfig("http://127.0.0.1:0")
	h := NewToolHandlers(cfg, queuedTestQueue(t))
	server := createFullServer(cfg, h)

	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go server.Run(ctx, st)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err, "connect")
	t.Cleanup(func() { cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	require.NoError(t, err, "ListTools")
	for _, tool := range res.Tools {
		if tool.Name == name {
			schema, ok := tool.InputSchema.(map[string]any)
			require.True(t, ok, "tool %s: advertised input schema must be a map (got %T)", name, tool.InputSchema)
			return schema
		}
	}
	t.Fatalf("tool %q not found in the full server's tool list", name)
	return nil
}

// TestGenerationToolsHaveNoOutputFormat pins the owner ruling: output_format
// (result delivery: url/base64/both) was never once used by any caller, so
// it is dropped from every generation tool's advertised input — results are
// always URLs.
func TestGenerationToolsHaveNoOutputFormat(t *testing.T) {
	for _, name := range []string{"generate_image", "generate_image_async", "enhance_and_generate", "enhance_and_generate_async"} {
		t.Run(name, func(t *testing.T) {
			schema := toolInputSchemaForTest(t, name)
			props, ok := schema["properties"].(map[string]any)
			require.True(t, ok, "tool %s must declare properties", name)
			assert.NotContains(t, props, "output_format",
				"output_format is dropped from generation tools (owner ruling, never used)")
			assert.NotContains(t, props, "base64")
		})
	}
}

// toolDescriptionForTest returns the advertised description of the named
// tool from the full (stdio) server flavor, over in-memory transports —
// the exact text an MCP client sees from ListTools.
func toolDescriptionForTest(t *testing.T, name string) string {
	t.Helper()
	return toolDescriptionFromServer(t, createFullServer, name)
}

// toolDescriptionFromServer is toolDescriptionForTest for an arbitrary
// server flavor, so description pins can cover every registration site
// (sync, async, and full register overlapping tool sets).
func toolDescriptionFromServer(t *testing.T, build func(Config, *ToolHandlers) *mcp.Server, name string) string {
	t.Helper()
	cfg := testConfig("http://127.0.0.1:0")
	h := NewToolHandlers(cfg, queuedTestQueue(t))
	server := build(cfg, h)

	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go server.Run(ctx, st)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err, "connect")
	t.Cleanup(func() { cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	require.NoError(t, err, "ListTools")
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool.Description
		}
	}
	t.Fatalf("tool %q not found in the full server's tool list", name)
	return ""
}

// TestEditToolDescriptionsTeachFirstImage pins the first-image semantic in
// the advertised edit tool descriptions: input_images is ORDERED, the FIRST
// image is the edit target (edit applies to it, output dimensions follow
// it), later images are references. The model has no other place to learn
// the ordering contract.
func TestEditToolDescriptionsTeachFirstImage(t *testing.T) {
	for _, name := range []string{"edit_image", "edit_image_async"} {
		t.Run(name, func(t *testing.T) {
			desc := toolDescriptionForTest(t, name)
			assert.Contains(t, desc, "FIRST image is the edit target",
				"the description must teach the first-image semantic")
			assert.Contains(t, desc, "input_images is ORDERED")
			assert.Contains(t, desc, "usr_/web_", "ref grammar: conversation refs")
			assert.Contains(t, desc, "gen_", "ref grammar: generation refs")
			assert.Contains(t, desc, "strength", "strength must be explained")
			assert.Contains(t, desc, "list_workflows", "pointer to per-workflow grammar")
		})
	}
}

// TestEditToolDescriptionsTeachDefaultEditWorkflow pins the
// default-edit-workflow sentence on ALL FOUR edit registrations (sync
// edit_image, async edit_image_async, and both on the full stdio server):
// the model has no other way to know an empty or "default" workflow
// argument is legal for edits. The sentence was dropped in the
// dynamic-inputs round and reinstated with default_edit_workflow.
func TestEditToolDescriptionsTeachDefaultEditWorkflow(t *testing.T) {
	combos := []struct {
		serverFlavor string
		build        func(Config, *ToolHandlers) *mcp.Server
		tool         string
	}{
		{"sync", createSyncServer, "edit_image"},
		{"async", createAsyncServer, "edit_image_async"},
		{"full", createFullServer, "edit_image"},
		{"full", createFullServer, "edit_image_async"},
	}
	for _, c := range combos {
		t.Run(c.serverFlavor+"/"+c.tool, func(t *testing.T) {
			desc := toolDescriptionFromServer(t, c.build, c.tool)
			assert.Contains(t, desc, "Uses the default edit workflow if workflow is empty or 'default'.")
		})
	}
}

// TestEditToolDescriptionPolicy pins the [safety] edit_policy suffix: the
// configured content rules ride the edit tool descriptions verbatim after
// the marker sentence, past the flavor tail; empty policy = no suffix at
// all. This is the instruction-layer guard for the safeguard-free Qwen
// Image Edit model — every model that can call the edit tools sees the
// rules, and the safety vet stays text-only.
func TestEditToolDescriptionPolicy(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")

	t.Run("empty policy: base text only", func(t *testing.T) {
		cfg.Safety.EditPolicy = ""
		syncDesc := editToolDescription(cfg, false)
		asyncDesc := editToolDescription(cfg, true)
		for name, d := range map[string]string{"sync": syncDesc, "async": asyncDesc} {
			assert.Contains(t, d, "FIRST image is the edit target", name+": base teaching text")
			assert.Contains(t, d, "Uses the default edit workflow if workflow is empty or 'default'.", name)
			assert.NotContains(t, d, "Content rules", name+": empty policy must add no suffix")
		}
		assert.Contains(t, syncDesc, "Blocks until the edit completes and returns the image result directly.")
		assert.NotContains(t, syncDesc, "Queues the edit job")
		assert.Contains(t, asyncDesc, "Queues the edit job and returns a job_id immediately without waiting.")
		assert.NotContains(t, asyncDesc, "Blocks until the edit completes")
	})

	t.Run("policy appended verbatim after the marker, at the end", func(t *testing.T) {
		cfg.Safety.EditPolicy = "No ducks. Ever."
		for _, async := range []bool{false, true} {
			d := editToolDescription(cfg, async)
			assert.Contains(t, d, "\n\nContent rules you MUST follow when editing: No ducks. Ever.")
			assert.True(t, strings.HasSuffix(d, "No ducks. Ever."),
				"suffix must land at the end, after the flavor tail")
		}
	})
}

// TestRegisterEditToolsCarryPolicy pins the full wire path: a server built
// with a policy advertises it in the listed edit tool descriptions, and the
// liveServer refresh closure swaps it for a new one (RemoveTools + AddTool
// → tools/list_changed) so the next ListTools shows the new text exactly
// once. This is exactly what a SIGHUP-driven edit_policy change does in
// production (doReload → every liveServer's refresh).
func TestRegisterEditToolsCarryPolicy(t *testing.T) {
	cfgA := testConfig("http://127.0.0.1:0")
	cfgA.Safety.EditPolicy = "policy A: be kind"
	h := NewToolHandlers(cfgA, queuedTestQueue(t))
	server := createFullServer(cfgA, h)
	ls := newLiveServer(server, h, true, true)

	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go server.Run(ctx, st)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err, "connect")
	t.Cleanup(func() { cs.Close() })

	// editDescs lists the tools and returns both edit descriptions, also
	// pinning that the refresh leaves exactly ONE registration per name
	// (RemoveTools before AddTool — no duplicates).
	editDescs := func(t *testing.T) (string, string) {
		t.Helper()
		res, err := cs.ListTools(ctx, nil)
		require.NoError(t, err, "ListTools")
		var syncDesc, asyncDesc string
		var syncCount, asyncCount int
		for _, tool := range res.Tools {
			switch tool.Name {
			case "edit_image":
				syncDesc, syncCount = tool.Description, syncCount+1
			case "edit_image_async":
				asyncDesc, asyncCount = tool.Description, asyncCount+1
			}
		}
		require.Equal(t, 1, syncCount, "exactly one edit_image registration")
		require.Equal(t, 1, asyncCount, "exactly one edit_image_async registration")
		require.NotEmpty(t, syncDesc)
		require.NotEmpty(t, asyncDesc)
		return syncDesc, asyncDesc
	}

	syncDesc, asyncDesc := editDescs(t)
	assert.Contains(t, syncDesc, "policy A: be kind")
	assert.Contains(t, asyncDesc, "policy A: be kind")

	// The refresh closure is what doReload invokes on a policy change:
	// re-register under cfgB, then the next ListTools must show B, not A.
	cfgB := cfgA
	cfgB.Safety.EditPolicy = "policy B: be cruel"
	ls.refresh(cfgB)

	syncDesc, asyncDesc = editDescs(t)
	assert.Contains(t, syncDesc, "policy B: be cruel")
	assert.Contains(t, asyncDesc, "policy B: be cruel")
	assert.NotContains(t, syncDesc, "policy A: be kind")
	assert.NotContains(t, asyncDesc, "policy A: be kind")
}

// TestJobReadersRaceFreeWhileCompleting pins the fix for unlocked job-field
// readers in the tool handlers: job_status / list_jobs / wait_for_job must
// observe the job through a snapshot taken under JobQueue.mu, never through
// the live *Job that Get/WaitForJob/ListJobs used to hand out. The writer
// below mimics processJob's lifecycle writes (same fields, same q.mu
// discipline) while readers poll the tool handlers; under -race the pre-fix
// code is flagged because jobToStatusOutput / handleWaitForJob read
// Status/Result/StartedAt/CompletedAt with no lock at all.
func TestJobReadersRaceFreeWhileCompleting(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	q := queuedTestQueue(t) // no workers: the job stays put while we flip its state
	h := NewToolHandlers(cfg, q)

	job := submitTestJob(t, q)

	ctx := context.Background()
	stop := make(chan struct{})
	var readers sync.WaitGroup

	poll := func(f func()) {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				f()
			}
		}()
	}

	poll(func() { _, _, _ = h.handleJobStatus(ctx, nil, JobStatusInput{JobID: job.ID}) })
	poll(func() { _, _, _ = h.handleListJobs(ctx, nil, ListJobsInput{}) })
	// Timeout is in whole seconds; during non-terminal phases this poller
	// blocks in WaitForJob, but every completed phase returns immediately
	// through the terminal fast path — which is exactly the read that used
	// to race (live *Job returned to the handler).
	poll(func() { _, _, _ = h.handleWaitForJob(ctx, nil, WaitForJobInput{JobID: job.ID, Timeout: 1}) })

	// Writer: cycle queued→running→completed exactly like processJob does —
	// every mutation under q.mu, terminal writes last. done is never closed
	// on purpose: readers must be safe regardless of waiter wakeups.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		now := time.Now().UTC()

		q.mu.Lock()
		job.Status = StatusRunning
		job.StartedAt = &now
		q.mu.Unlock()

		q.mu.Lock()
		job.Result = &JobResult{Images: []ImageData{{MIMEType: "image/png", URL: "https://img.example.com/img.png"}}}
		job.Status = StatusCompleted
		job.CompletedAt = &now
		q.mu.Unlock()

		// Back to a non-terminal state so the next iteration exercises the
		// queued/running reader paths (including WaitForJob's timeout
		// branch) as well.
		q.mu.Lock()
		job.Status = StatusQueued
		job.Result = nil
		job.CompletedAt = nil
		q.mu.Unlock()
	}

	// Leave the job terminally completed; after the readers stop, a final
	// read must agree with the in-memory state.
	q.mu.Lock()
	now := time.Now().UTC()
	job.Status = StatusCompleted
	job.CompletedAt = &now
	job.Result = &JobResult{}
	q.mu.Unlock()

	close(stop)
	readers.Wait()

	_, out, err := h.handleJobStatus(ctx, nil, JobStatusInput{JobID: job.ID})
	require.NoError(t, err)
	assert.Equal(t, string(StatusCompleted), out.Status)
	assert.Equal(t, job.ID, out.JobID)
}

// TestGenerateToolsPassProvenance mirrors TestGenerateToolsPassLLMGeneratedFlag
// for the imgsite provenance trio: network/channel/nick from the dave-injected
// tool fields must reach JobInput (and from there the DB and the upload meta).
func TestGenerateToolsPassProvenance(t *testing.T) {
	tests := []struct {
		name   string
		submit func(t *testing.T, h *ToolHandlers) (string, error)
	}{
		{
			name: "generate_image_async",
			submit: func(t *testing.T, h *ToolHandlers) (string, error) {
				_, out, err := h.handleGenerateImageAsync(context.Background(), nil,
					GenerateImageAsyncInput{Prompt: "a cat", Network: "libera", Channel: "#dave", Nick: "knivey"})
				return out.JobID, err
			},
		},
		{
			name: "enhance_and_generate_async",
			submit: func(t *testing.T, h *ToolHandlers) (string, error) {
				_, out, err := h.handleEnhanceAndGenerateAsync(context.Background(), nil,
					EnhanceAndGenerateAsyncInput{Prompt: "a cat", Network: "libera", Channel: "#dave", Nick: "knivey"})
				return out.JobID, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewToolHandlers(testConfig("http://127.0.0.1:0"), queuedTestQueue(t))

			jobID, err := tt.submit(t, h)
			require.NoError(t, err, "tool handler")

			job, ok := h.queue.Get(jobID)
			require.True(t, ok, "job should be findable in queue")
			assert.Equal(t, "libera", job.Input.Network, "Network from tool input should reach JobInput")
			assert.Equal(t, "#dave", job.Input.Channel, "Channel from tool input should reach JobInput")
			assert.Equal(t, "knivey", job.Input.Nick, "Nick from tool input should reach JobInput")
		})
	}
}

// TestGenerateToolsSyncPassProvenance runs the blocking tool handlers
// end-to-end against a mock ComfyUI and asserts provenance survives the
// full Submit → processJob path (including the DB write on Submit).
func TestGenerateToolsSyncPassProvenance(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)

	tests := []struct {
		name string
		call func(t *testing.T, h *ToolHandlers) error
	}{
		{
			name: "generate_image",
			call: func(t *testing.T, h *ToolHandlers) error {
				_, out, err := h.handleGenerateImage(context.Background(), nil,
					GenerateImageInput{Prompt: "a cat", Network: "libera", Channel: "#dave", Nick: "knivey"})
				assert.Equal(t, "completed", out.Status)
				return err
			},
		},
		{
			name: "enhance_and_generate",
			call: func(t *testing.T, h *ToolHandlers) error {
				_, out, err := h.handleEnhanceAndGenerate(context.Background(), nil,
					EnhanceAndGenerateInput{Prompt: "a cat", Network: "libera", Channel: "#dave", Nick: "knivey"})
				assert.Equal(t, "completed", out.Status)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(mockComfy.URL())
			wc := cfg.Workflows["test"]
			wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
			cfg.Workflows["test"] = wc
			cfg.Upload.URL = newFakeUploadServer(t).server.URL
			cfg.Enhancements = map[string]EnhancementConfig{
				"default": {
					BaseURL:      newMockEnhanceServer(t).URL,
					Key:          "test-key",
					Model:        "enhancer",
					SystemPrompt: "enhance",
				},
			}
			q, cleanup := setupTestQueue(t, cfg)
			defer cleanup()
			h := NewToolHandlers(cfg, q)

			require.NoError(t, tt.call(t, h), "tool handler")

			jobs := q.ListJobs("", 10)
			require.NotEmpty(t, jobs, "job should be in queue")
			assert.Equal(t, "libera", jobs[0].Input.Network, "Network should survive the sync path")
			assert.Equal(t, "#dave", jobs[0].Input.Channel, "Channel should survive the sync path")
			assert.Equal(t, "knivey", jobs[0].Input.Nick, "Nick should survive the sync path")
		})
	}
}

func TestToolHandlersConfigSwap(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Comfy.Timeout = 60
	cfg.Upload.URL = "https://upload.example.com"

	queue, cleanup := setupTestQueue(t, Config{})
	defer cleanup()

	h := NewToolHandlers(cfg, queue)

	got := h.getConfig()
	assert.Equal(t, 60, got.Comfy.Timeout)
	assert.Equal(t, "https://upload.example.com", got.Upload.URL)
	assert.Equal(t, "http://localhost:8188", got.Comfy.BaseURL)

	newCfg := testConfig("http://localhost:9999")
	newCfg.Comfy.Timeout = 120
	newCfg.Upload.URL = "https://new-upload.example.com"

	h.setConfig(newCfg)

	got = h.getConfig()
	assert.Equal(t, 120, got.Comfy.Timeout)
	assert.Equal(t, "https://new-upload.example.com", got.Upload.URL)
	assert.Equal(t, "http://localhost:9999", got.Comfy.BaseURL)

	require.Same(t, queue, h.queue, "queue reference should be unchanged")
}

func TestToolHandlersResolveWorkflow_AfterConfigSwap(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Comfy.DefaultWorkflow = "test"

	queue, cleanup := setupTestQueue(t, Config{})
	defer cleanup()

	h := NewToolHandlers(cfg, queue)

	name, err := h.resolveWorkflow("")
	require.NoError(t, err)
	assert.Equal(t, "test", name)

	newCfg := testConfig("http://localhost:8188")
	newCfg.Comfy.DefaultWorkflow = "other"
	newCfg.Workflows["other"] = WorkflowConfig{
		ClientID: "other-client", OutputNode: "out", PromptNode: "in", Timeout: 60,
	}
	h.setConfig(newCfg)

	name, err = h.resolveWorkflow("")
	require.NoError(t, err)
	assert.Equal(t, "other", name)
}

// queuedTestQueue builds a JobQueue with no workers so submitted jobs stay
// queued and inspectable.
func queuedTestQueue(t *testing.T) *JobQueue {
	t.Helper()
	return queuedTestQueueWithConfig(t, testConfig("http://127.0.0.1:0"))
}

// queuedTestQueueWithConfig is queuedTestQueue for tests whose handlers
// resolve workflows the default test config does not carry (e.g. the edit
// workflow): Submit validates the workflow name against the QUEUE's config,
// so handler and queue must agree.
func queuedTestQueueWithConfig(t *testing.T, cfg Config) *JobQueue {
	t.Helper()
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}
	return q
}

func TestGenerateToolsPassLLMGeneratedFlag(t *testing.T) {
	tests := []struct {
		name   string
		submit func(t *testing.T, h *ToolHandlers) (string, error)
	}{
		{
			name: "generate_image_async",
			submit: func(t *testing.T, h *ToolHandlers) (string, error) {
				_, out, err := h.handleGenerateImageAsync(context.Background(), nil,
					GenerateImageAsyncInput{Prompt: "a cat", LLMGenerated: true})
				return out.JobID, err
			},
		},
		{
			name: "enhance_and_generate_async",
			submit: func(t *testing.T, h *ToolHandlers) (string, error) {
				_, out, err := h.handleEnhanceAndGenerateAsync(context.Background(), nil,
					EnhanceAndGenerateAsyncInput{Prompt: "a cat", LLMGenerated: true})
				return out.JobID, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewToolHandlers(testConfig("http://127.0.0.1:0"), queuedTestQueue(t))

			jobID, err := tt.submit(t, h)
			require.NoError(t, err, "tool handler")

			job, ok := h.queue.Get(jobID)
			require.True(t, ok, "job should be findable in queue")
			assert.True(t, job.Input.LLMGenerated,
				"LLMGenerated from tool input should reach JobInput")
		})
	}
}

// TestGenerateToolsSyncPassLLMGeneratedFlag runs the blocking tool handlers
// end-to-end against a mock ComfyUI and asserts the flag reaches the job.
func TestGenerateToolsSyncPassLLMGeneratedFlag(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)

	tests := []struct {
		name string
		call func(t *testing.T, h *ToolHandlers) error
	}{
		{
			name: "generate_image",
			call: func(t *testing.T, h *ToolHandlers) error {
				_, out, err := h.handleGenerateImage(context.Background(), nil,
					GenerateImageInput{Prompt: "a cat", LLMGenerated: true})
				assert.Equal(t, "completed", out.Status)
				return err
			},
		},
		{
			name: "enhance_and_generate",
			call: func(t *testing.T, h *ToolHandlers) error {
				_, out, err := h.handleEnhanceAndGenerate(context.Background(), nil,
					EnhanceAndGenerateInput{Prompt: "a cat", LLMGenerated: true})
				assert.Equal(t, "completed", out.Status)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(mockComfy.URL())
			wc := cfg.Workflows["test"]
			wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
			cfg.Workflows["test"] = wc
			cfg.Upload.URL = newFakeUploadServer(t).server.URL
			cfg.Enhancements = map[string]EnhancementConfig{
				"default": {
					BaseURL:      newMockEnhanceServer(t).URL,
					Key:          "test-key",
					Model:        "enhancer",
					SystemPrompt: "enhance",
				},
			}
			q, cleanup := setupTestQueue(t, cfg)
			defer cleanup()
			h := NewToolHandlers(cfg, q)

			require.NoError(t, tt.call(t, h), "tool handler")

			submitted := mockComfy.submittedPrompts()
			require.NotEmpty(t, submitted, "job should have been submitted to comfy")
			node, ok := submitted[len(submitted)-1].Prompt[davePromptNoteNodeID]
			require.True(t, ok, "submitted workflow should contain the prompt note node")
			var payload promptNotePayload
			require.NoError(t, json.Unmarshal([]byte(node.Inputs["text"].(string)), &payload))
			assert.True(t, payload.LLMGenerated, "note payload llm_generated")
		})
	}
}

func TestApplyNetworkPolicy(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Enhancements = map[string]EnhancementConfig{
		"safe":    {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
		"liberal": {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
	}
	cfg.NetworkPolicies = map[string]NetworkPolicy{
		"libera":     {Enhancement: "safe", Force: true},
		"graped":     {Enhancement: "liberal", Force: false},
		"empty-test": {Enhancement: "", Force: true},
	}

	queue, cleanup := setupTestQueue(t, Config{})
	defer cleanup()

	tests := []struct {
		name              string
		network           string
		inputEnhancement  string
		inputJobType      JobType
		expectEnhancement string
		expectJobType     JobType
	}{
		{
			name:              "empty network passes through",
			network:           "",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "network not in policies passes through",
			network:           "unknown",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy overrides enhancement on enhance_generate",
			network:           "libera",
			inputEnhancement:  "liberal",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy with force changes generate to enhance_generate",
			network:           "libera",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy without force sets enhancement but keeps generate as generate",
			network:           "graped",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "liberal",
			expectJobType:     JobTypeGenerate,
		},
		{
			name:              "policy without force overrides enhancement on enhance_generate",
			network:           "graped",
			inputEnhancement:  "",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "liberal",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "generate with no matching policy passes through",
			network:           "unknown",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "",
			expectJobType:     JobTypeGenerate,
		},
		{
			name:              "policy with empty enhancement passes through",
			network:           "empty-test",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeGenerate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewToolHandlers(cfg, queue)
			enhancement, jobType := h.applyNetworkPolicy(tt.network, tt.inputEnhancement, tt.inputJobType)
			assert.Equal(t, tt.expectEnhancement, enhancement, "enhancement")
			assert.Equal(t, tt.expectJobType, jobType, "jobType")
		})
	}
}

// floatPtr is the *float64 test helper for edit strength (nil pointer =
// "workflow default" is part of the tool contract, so the field is a
// pointer everywhere).
func floatPtr(f float64) *float64 { return &f }

// TestHandleEditImageValidates pins the edit tool's pre-Submit validation:
// input_images is required and strength must sit in (0, 1] — errors surface
// as tool errors BEFORE any job is queued, so the model can correct itself.
func TestHandleEditImageValidates(t *testing.T) {
	h := NewToolHandlers(testConfig("http://127.0.0.1:0"), queuedTestQueue(t))
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
			_, _, err := h.handleEditImage(context.Background(), nil, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestHandleEditImageRequiresImageInputNode pins the workflow gate: an edit
// must land on a workflow with image_input_node configured. With
// default_edit_workflow, an explicitly named generation workflow is the way
// to reach the gate (an empty argument now resolves to the edit default or
// errors naming the knob — never the generation default_workflow, the
// production wasted-round-trip this resolver closes).
func TestHandleEditImageRequiresImageInputNode(t *testing.T) {
	// testConfig's only workflow ("test") has no image_input_node.
	h := NewToolHandlers(testConfig("http://127.0.0.1:0"), queuedTestQueue(t))

	t.Run("explicit generation workflow fails the gate", func(t *testing.T) {
		_, _, err := h.handleEditImage(context.Background(), nil, EditImageInput{Prompt: "p", Workflow: "test", InputImages: []string{"gen_a"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image_input_node")
		assert.Contains(t, err.Error(), `"test"`, "error names the workflow")
	})

	t.Run("empty workflow with no edit default names the knob", func(t *testing.T) {
		_, _, err := h.handleEditImage(context.Background(), nil, EditImageInput{Prompt: "p", InputImages: []string{"gen_a"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default_edit_workflow")
		assert.NotContains(t, err.Error(), "image_input_node",
			"the generation default must not be aimed at the edit gate")
	})
}

// TestHandleEditImageCapsInputImages pins the pre-Submit cap: the dynamic
// wiring builds images.image_1..image_10, so an over-supply is rejected
// before any job is queued.
func TestHandleEditImageCapsInputImages(t *testing.T) {
	cfg := editTestConfig(t)
	h := NewToolHandlers(cfg, queuedTestQueueWithConfig(t, cfg))
	refs := make([]string, maxEditInputImages+1)
	for i := range refs {
		refs[i] = fmt.Sprintf("gen_job_%d", i)
	}
	_, _, err := h.handleEditImage(context.Background(), nil, EditImageInput{Prompt: "p", Workflow: "edit", InputImages: refs})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at most 10 input images")
}

// TestEditToolsPassRawInputs pins the handler wiring the processJob tests
// cannot see (they build JobInput directly): the model's input_images
// entries ride JobInput.RawInputImages untouched for processJob-time
// resolution, the strength pointer survives, and the job lands under
// JobTypeEdit on the named workflow.
func TestEditToolsPassRawInputs(t *testing.T) {
	cfg := editTestConfig(t)
	h := NewToolHandlers(cfg, queuedTestQueueWithConfig(t, cfg))
	strength := 0.6
	_, out, err := h.handleEditImageAsync(context.Background(), nil, EditImageAsyncInput{
		Prompt:      "make it blue",
		Workflow:    "edit",
		InputImages: []string{"gen_a", "usr_1"},
		Strength:    &strength,
	})
	require.NoError(t, err, "handler")

	job, ok := h.queue.Get(out.JobID)
	require.True(t, ok, "job should be findable in queue")
	assert.Equal(t, JobTypeEdit, job.Type)
	assert.Equal(t, "edit", job.Workflow)
	assert.Equal(t, []string{"gen_a", "usr_1"}, job.Input.RawInputImages,
		"the model's entries pass through verbatim — resolution is processJob's job")
	assert.Equal(t, &strength, job.Input.Strength)
	assert.Empty(t, job.Input.InputImages, "nothing is resolved at handler time")
}

// TestResolveEditWorkflow pins the edit resolver's defaulting contract: an
// explicit name is looked up like resolveWorkflow, but empty/"default"
// resolves to [comfy] default_edit_workflow — NEVER DefaultWorkflow, which
// is a generation workflow and would die at the edit gate after a wasted
// round-trip (the production incident this knob exists for).
func TestResolveEditWorkflow(t *testing.T) {
	cfg := editTestConfig(t)
	cfg.Comfy.DefaultEditWorkflow = "edit"
	h := NewToolHandlers(cfg, queuedTestQueueWithConfig(t, cfg))

	// The discriminating setup: the generation default ("test", a txt2img
	// workflow with no image_input_node) is set alongside the edit default —
	// an empty argument must pick "edit", never "test".
	require.Equal(t, "test", cfg.Comfy.DefaultWorkflow)

	cases := []struct {
		name    string
		arg     string
		want    string
		wantErr string
	}{
		{"explicit existing name", "edit", "edit", ""},
		{"empty resolves to edit default", "", "edit", ""},
		{"default keyword resolves to edit default", "default", "edit", ""},
		{"unknown explicit name", "nope", "", `workflow "nope" not found`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, err := h.resolveEditWorkflow(tc.arg)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, name)
		})
	}

	// No edit default configured: empty must error naming the knob — not
	// fall through to the generation default.
	noDefault := editTestConfig(t)
	noDefault.Comfy.DefaultEditWorkflow = ""
	h2 := NewToolHandlers(noDefault, queuedTestQueue(t))
	_, err := h2.resolveEditWorkflow("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_edit_workflow")
}

// TestEditHandlersUseEditDefault pins the handler wiring of the edit
// default through the shared validateEditInput seam: an edit call with an
// empty workflow argument submits its job on the configured
// default_edit_workflow, not the generation default (which would die at
// the image_input_node gate — the wasted round-trip the production model
// kept paying).
func TestEditHandlersUseEditDefault(t *testing.T) {
	newHandlers := func(t *testing.T) *ToolHandlers {
		t.Helper()
		cfg := editTestConfig(t)
		cfg.Comfy.DefaultEditWorkflow = "edit"
		return NewToolHandlers(cfg, queuedTestQueueWithConfig(t, cfg))
	}

	t.Run("async", func(t *testing.T) {
		h := newHandlers(t)
		_, out, err := h.handleEditImageAsync(context.Background(), nil, EditImageAsyncInput{
			Prompt:      "make it blue",
			InputImages: []string{"gen_a"},
		})
		require.NoError(t, err, "handler")
		job, ok := h.queue.Get(out.JobID)
		require.True(t, ok, "job should be findable in queue")
		assert.Equal(t, JobTypeEdit, job.Type)
		assert.Equal(t, "edit", job.Workflow, "empty workflow arg must land on the edit default")
	})

	t.Run("sync", func(t *testing.T) {
		h := newHandlers(t)
		// No workers: WaitForJob times out after 1s; the handler returns
		// cleanly and the submitted job stays queued for inspection.
		_, _, err := h.handleEditImage(context.Background(), nil, EditImageInput{
			Prompt:      "make it blue",
			InputImages: []string{"gen_a"},
			Timeout:     1,
		})
		require.NoError(t, err, "handler")
		jobs := h.queue.ListJobs("", 10)
		require.Len(t, jobs, 1, "exactly the one submitted job")
		assert.Equal(t, JobTypeEdit, jobs[0].Type)
		assert.Equal(t, "edit", jobs[0].Workflow, "empty workflow arg must land on the edit default")
	})
}
