package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobQueue_Cancel_QueuedJob(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job := submitTestJob(t, q)

	ok := q.Cancel(job.ID)
	require.True(t, ok, "expected Cancel to return true")

	assertJobStatus(t, q, job.ID, StatusCancelled)

	assert.Equal(t, string(StatusCancelled), dbJobStatus(t, q.db, job.ID), "DB status")

	q.mu.RLock()
	_, inResults := q.results[job.ID]
	q.mu.RUnlock()
	assert.True(t, inResults, "cancelled job should still be in results map")
}

func TestJobQueue_Cancel_NonExistentJob(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	ok := q.Cancel("nonexistent")
	require.False(t, ok, "expected Cancel to return false for nonexistent job")
}

func TestJobQueue_Cancel_AlreadyCompleted(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job := submitTestJob(t, q)

	q.mu.Lock()
	job.Status = StatusCompleted
	job.ComfyPromptID = "test-prompt"
	job.closeOnce.Do(func() { close(job.done) })
	q.mu.Unlock()

	ok := q.Cancel(job.ID)
	require.False(t, ok, "expected Cancel to return false for completed job")
}

func TestJobQueue_Cancel_AlreadyCancelled(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job := submitTestJob(t, q)

	q.Cancel(job.ID)

	ok := q.Cancel(job.ID)
	require.False(t, ok, "expected Cancel to return false for already-cancelled job")
}

func TestJobQueue_Cancel_RunningJob_InterruptsComfy(t *testing.T) {
	mockComfy := newMockInterruptServer(t)
	cfg := testConfig(mockComfy.URL())
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job := submitTestJob(t, q)

	q.mu.Lock()
	job.Status = StatusRunning
	job.ComfyPromptID = "comfy-prompt-456"
	_, jobCancel := context.WithCancel(context.Background())
	job.cancelCtx = jobCancel
	q.mu.Unlock()

	ok := q.Cancel(job.ID)
	require.True(t, ok, "expected Cancel to return true for running job")

	q.mu.RLock()
	storedJob := q.results[job.ID]
	q.mu.RUnlock()
	assert.Equal(t, StatusCancelled, storedJob.Status, "job status")

	interrupts := mockComfy.getInterrupts()
	require.Len(t, interrupts, 1, "interrupt calls")
	assert.Equal(t, "comfy-prompt-456", interrupts[0]["prompt_id"], "interrupt prompt_id")

	// The interrupt only lands if the prompt is the one currently executing
	// on the GPU; under max_workers > 1 a cancelled job's prompt may still be
	// PENDING in ComfyUI's internal queue, where a prompt_id-targeted
	// interrupt is a no-op — without the queue delete ComfyUI would execute
	// the cancelled job anyway (orphan output, wasted GPU time).
	deletes := mockComfy.getQueueDeletes()
	require.Len(t, deletes, 1, "queue delete calls")
	assert.Equal(t, []string{"comfy-prompt-456"}, deletes[0], "queue delete prompt_ids")
}

func TestJobQueue_Cancel_RunningJob_WithoutComfyPromptID(t *testing.T) {
	mockComfy := newMockInterruptServer(t)
	cfg := testConfig(mockComfy.URL())
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job := submitTestJob(t, q)

	q.mu.Lock()
	job.Status = StatusRunning
	_, jobCancel := context.WithCancel(context.Background())
	job.cancelCtx = jobCancel
	q.mu.Unlock()

	ok := q.Cancel(job.ID)
	require.True(t, ok, "expected Cancel to return true")

	q.mu.RLock()
	storedJob := q.results[job.ID]
	q.mu.RUnlock()
	assert.Equal(t, StatusCancelled, storedJob.Status, "job status")

	interrupts := mockComfy.getInterrupts()
	assert.Len(t, interrupts, 0, "expected no interrupt calls when no comfy_prompt_id")
}

func TestJobQueue_Cancel_RunningJob_CancelsContext(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job := submitTestJob(t, q)

	jobCtx, jobCancel := context.WithCancel(context.Background())

	q.mu.Lock()
	job.Status = StatusRunning
	job.cancelCtx = jobCancel
	q.mu.Unlock()

	q.Cancel(job.ID)

	select {
	case <-jobCtx.Done():
	default:
		t.Error("expected job context to be cancelled")
	}
}

func TestJobQueue_Cancel_ClosesDone(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job := submitTestJob(t, q)

	go func() {
		time.Sleep(50 * time.Millisecond)
		q.Cancel(job.ID)
	}()

	waitForJobDone(t, job, 2*time.Second)

	assertJobStatus(t, q, job.ID, StatusCancelled)
}

func TestJobQueue_Cancel_QueuedJob_RemovesFromQueue(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:         cfg,
		db:          db,
		pending:     make(chan *Job, cfg.Queue.MaxDepth),
		results:     make(map[string]*Job),
		queuedOrder: []*Job{},
		cancel:      cancel,
	}

	job := submitTestJob(t, q)

	q.orderMu.Lock()
	beforeLen := len(q.queuedOrder)
	q.orderMu.Unlock()
	require.Equal(t, 1, beforeLen, "queued jobs before cancel")

	q.Cancel(job.ID)

	q.orderMu.Lock()
	afterLen := len(q.queuedOrder)
	q.orderMu.Unlock()
	assert.Equal(t, 0, afterLen, "queued jobs after cancel")
}

func TestJobQueue_Cancel_MultipleJobs(t *testing.T) {
	mockComfy := newMockInterruptServer(t)
	cfg := testConfig(mockComfy.URL())
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:         cfg,
		db:          db,
		pending:     make(chan *Job, cfg.Queue.MaxDepth),
		results:     make(map[string]*Job),
		queuedOrder: []*Job{},
		cancel:      cancel,
	}

	job1 := submitTestJob(t, q)
	job2 := submitTestJob(t, q)
	job3 := submitTestJob(t, q)

	q.mu.Lock()
	job2.Status = StatusRunning
	job2.ComfyPromptID = "comfy-2"
	_, cancel2 := context.WithCancel(context.Background())
	job2.cancelCtx = cancel2
	q.mu.Unlock()

	require.True(t, q.Cancel(job1.ID), "expected job1 cancel to succeed")
	require.True(t, q.Cancel(job2.ID), "expected job2 cancel to succeed")

	assertJobStatus(t, q, job1.ID, StatusCancelled)
	assertJobStatus(t, q, job2.ID, StatusCancelled)
	assertJobStatus(t, q, job3.ID, StatusQueued)

	interrupts := mockComfy.getInterrupts()
	require.Len(t, interrupts, 1, "interrupt calls")
	assert.Equal(t, "comfy-2", interrupts[0]["prompt_id"], "interrupt prompt_id")
}

func TestJobQueue_IsReady_NoDB(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	q := NewJobQueue(cfg, nil)
	defer q.Stop()

	require.True(t, q.IsReady(), "expected IsReady true when no DB")
}

func TestJobQueue_LLMGeneratedPersists(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{Prompt: "a cat", LLMGenerated: true})
	require.NoError(t, err, "Submit")

	dbj, err := dbGetJob(db, job.ID)
	require.NoError(t, err, "dbGetJob")
	recovered := jobFromDBJob(dbj)
	assert.True(t, recovered.Input.LLMGenerated, "llm_generated should round-trip through the DB")

	job2, err := q.Submit(JobTypeGenerate, "test", JobInput{Prompt: "a dog"})
	require.NoError(t, err, "Submit")
	dbj2, err := dbGetJob(db, job2.ID)
	require.NoError(t, err, "dbGetJob")
	assert.False(t, jobFromDBJob(dbj2).Input.LLMGenerated,
		"llm_generated should default to false when never set")
}

// TestJobQueue_ProvenancePersists mirrors TestJobQueue_LLMGeneratedPersists
// for the imgsite provenance trio: Submit persists them (dbInsertJob) and a
// recovered job carries them — the same path restart recovery uses.
func TestJobQueue_ProvenancePersists(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt: "a cat", Network: "libera", Channel: "#dave", Nick: "knivey",
	})
	require.NoError(t, err, "Submit")

	dbj, err := dbGetJob(db, job.ID)
	require.NoError(t, err, "dbGetJob")
	recovered := jobFromDBJob(dbj)
	assert.Equal(t, "libera", recovered.Input.Network, "network should persist through Submit")
	assert.Equal(t, "#dave", recovered.Input.Channel, "channel should persist through Submit")
	assert.Equal(t, "knivey", recovered.Input.Nick, "nick should persist through Submit")
}

// TestBuildUploadMeta covers the meta payload img-mcp hands imgsite per
// upload: the plain path sends the final generation prompt as
// enhanced_prompt (equal to the original — it IS what the image ran with),
// the enhanced path carries the LLM's prompt/negative/reasoning, and the
// recovery path (empty enhancement args) omits those and lets EXIF supply
// them server-side.
func TestBuildUploadMeta(t *testing.T) {
	plainJob := &Job{
		ID:       "plainjob",
		Type:     JobTypeGenerate,
		Workflow: "zimage",
		Input: JobInput{
			Prompt:       "a cat",
			LLMGenerated: false,
			Network:      "libera",
			Channel:      "#dave",
			Nick:         "knivey",
		},
	}
	enhancedJob := &Job{
		ID:       "enhjob",
		Type:     JobTypeEnhanceGenerate,
		Workflow: "zimage",
		Input: JobInput{
			Prompt:       "a cat",
			LLMGenerated: true,
			Network:      "libera",
			Channel:      "#dave",
			Nick:         "knivey",
		},
	}

	tests := []struct {
		name string
		job  *Job
		// processJob locals at upload time
		finalPrompt  string
		finalNeg     string
		reasoning    string
		expect       UploadMeta
		expectJSONEq string
	}{
		{
			name:        "plain generate: final prompt equals original",
			job:         plainJob,
			finalPrompt: "a cat",
			finalNeg:    "",
			reasoning:   "",
			expect: UploadMeta{
				JobID:          "plainjob",
				OriginalPrompt: "a cat",
				EnhancedPrompt: "a cat",
				WorkflowName:   "zimage",
				Network:        "libera",
				Channel:        "#dave",
				Nick:           "knivey",
			},
			expectJSONEq: `{"job_id":"plainjob","original_prompt":"a cat","enhanced_prompt":"a cat",
				"workflow_name":"zimage","network":"libera","channel":"#dave","nick":"knivey"}`,
		},
		{
			name:        "enhanced: LLM prompt, negative, reasoning all carried",
			job:         enhancedJob,
			finalPrompt: "a fluffy cat, cinematic lighting",
			finalNeg:    "blurry, extra limbs",
			reasoning:   "the user asked for a cat",
			expect: UploadMeta{
				JobID:          "enhjob",
				OriginalPrompt: "a cat",
				EnhancedPrompt: "a fluffy cat, cinematic lighting",
				NegativePrompt: "blurry, extra limbs",
				Reasoning:      "the user asked for a cat",
				LLMGenerated:   true,
				WorkflowName:   "zimage",
				Network:        "libera",
				Channel:        "#dave",
				Nick:           "knivey",
			},
			expectJSONEq: `{"job_id":"enhjob","original_prompt":"a cat","enhanced_prompt":"a fluffy cat, cinematic lighting",
				"negative_prompt":"blurry, extra limbs","reasoning":"the user asked for a cat","llm_generated":true,
				"workflow_name":"zimage","network":"libera","channel":"#dave","nick":"knivey"}`,
		},
		{
			name:        "recovery: empty enhancement args drop out, EXIF supplies them",
			job:         enhancedJob,
			finalPrompt: "",
			finalNeg:    "",
			reasoning:   "",
			expect: UploadMeta{
				JobID:          "enhjob",
				OriginalPrompt: "a cat",
				LLMGenerated:   true,
				WorkflowName:   "zimage",
				Network:        "libera",
				Channel:        "#dave",
				Nick:           "knivey",
			},
			expectJSONEq: `{"job_id":"enhjob","original_prompt":"a cat","llm_generated":true,
				"workflow_name":"zimage","network":"libera","channel":"#dave","nick":"knivey"}`,
		},
		{
			name:        "no provenance: direct tool command without inject fields",
			job:         &Job{ID: "barejob", Type: JobTypeGenerate, Workflow: "zimage", Input: JobInput{Prompt: "a cat"}},
			finalPrompt: "a cat",
			expect: UploadMeta{
				JobID:          "barejob",
				OriginalPrompt: "a cat",
				EnhancedPrompt: "a cat",
				WorkflowName:   "zimage",
			},
			expectJSONEq: `{"job_id":"barejob","original_prompt":"a cat","enhanced_prompt":"a cat","workflow_name":"zimage"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := buildUploadMeta(tt.job, tt.finalPrompt, tt.finalNeg, tt.reasoning)
			assert.Equal(t, tt.expect, meta)

			data, err := json.Marshal(meta)
			require.NoError(t, err)
			assert.JSONEq(t, tt.expectJSONEq, string(data))
		})
	}
}

func TestJobQueue_IsReady_WithDB(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	q := NewJobQueue(cfg, db)
	defer q.Stop()

	require.True(t, q.IsReady(), "expected IsReady true after NewJobQueue with DB (recovery is synchronous)")
}

func TestJobQueue_ConfigSwap(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	cfg.Queue.ResultTTL = 1 * time.Hour

	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	got := q.getConfig()
	assert.Equal(t, 1*time.Hour, got.Queue.ResultTTL)
	assert.Contains(t, got.Workflows, "test")

	newCfg := cfg
	newCfg.Queue.ResultTTL = 30 * time.Minute
	newCfg.Workflows = map[string]WorkflowConfig{
		"other": {ClientID: "other", OutputNode: "out", PromptNode: "in", Timeout: 60},
	}

	q.setConfig(newCfg)

	got = q.getConfig()
	assert.Equal(t, 30*time.Minute, got.Queue.ResultTTL)
	assert.NotContains(t, got.Workflows, "test")
	assert.Contains(t, got.Workflows, "other")

	assert.Equal(t, cfg.Queue.MaxWorkers, got.Queue.MaxWorkers, "non-reloadable fields preserved")
	assert.Equal(t, cfg.Queue.MaxDepth, got.Queue.MaxDepth, "non-reloadable fields preserved")
}

// TestCancelDuringMonitorKeepsJobCancelled pins the interaction between
// Cancel() and a monitorComfyGeneration that returns promptly on context
// cancellation. Cancel() cancels the job context BEFORE it sets
// StatusCancelled (the interrupt HTTP call sits in between), so a cancelled
// job must not be marked failed with a "generation failed: context canceled"
// error in that window — it should end up cleanly cancelled.
func TestCancelDuringMonitorKeepsJobCancelled(t *testing.T) {
	// Never-ready history and a silent websocket keep the monitor polling
	// until the cancel lands. The interrupt call is parked so Cancel() is
	// deterministically still between cancelCtx() and the status update when
	// the monitor returns.
	silent := newSilentComfyServer(t, 0)
	silent.interruptDelay = 500 * time.Millisecond

	cfg := testConfig(silent.server.URL)
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc

	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{Prompt: "a cat"})
	require.NoError(t, err, "Submit")

	select {
	case <-silent.promptAccepted:
	case <-time.After(10 * time.Second):
		t.Fatal("job never submitted its prompt to comfy")
	}
	select {
	case <-silent.wsConnected:
	case <-time.After(10 * time.Second):
		t.Fatal("monitor never connected to websocket")
	}

	require.True(t, q.Cancel(job.ID), "Cancel")
	waitForJobDone(t, job, 15*time.Second)

	assert.Equal(t, StatusCancelled, job.Status, "final status should be cancelled")
	assert.Empty(t, job.Error, "cancelled job must not carry a generation-failure error")
	assert.Equal(t, 0, q.Status().Failed, "cancelled job must not count as failed")
}

// manualQueueLiteral builds a zero-worker JobQueue for tests that drive
// processJob/recoverRunningJob directly. The real worker would race the
// manual invocation for the same job.
func manualQueueLiteral(t *testing.T, cfg Config) *JobQueue {
	t.Helper()
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	q := &JobQueue{
		cfg:            cfg,
		db:             db,
		pending:        make(chan *Job, cfg.Queue.MaxDepth),
		results:        make(map[string]*Job),
		cancel:         cancel,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}
	t.Cleanup(func() { q.Stop() })
	return q
}

// markRunningInMemoryAndDB flips a freshly submitted job to the running
// state in memory and in the DB, mimicking what recovery finds on restart.
func markRunningInMemoryAndDB(t *testing.T, q *JobQueue, job *Job, comfyPromptID string) {
	t.Helper()
	require.NoError(t, dbUpdateJobRunning(q.db, job.ID), "dbUpdateJobRunning")
	q.mu.Lock()
	now := time.Now().UTC()
	job.Status = StatusRunning
	job.StartedAt = &now
	job.ComfyPromptID = comfyPromptID
	q.mu.Unlock()
}

// TestFailJobDoesNotOverwriteCancelled pins memory-side first-write-wins
// for failures: Cancel() flips the job to cancelled under q.mu, and a
// failJob that lost the race (e.g. the monitor errored out just as the
// user cancelled) must leave both the in-memory state and the DB row
// untouched. Pre-fix, memory said failed while the fenced DB row said
// cancelled — exactly the divergence a restart would surface.
func TestFailJobDoesNotOverwriteCancelled(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job := submitTestJob(t, q)
	require.True(t, q.Cancel(job.ID), "Cancel")

	q.failJob(job, "boom")

	assertJobStatus(t, q, job.ID, StatusCancelled)
	q.mu.RLock()
	jobErr := job.Error
	q.mu.RUnlock()
	assert.Empty(t, jobErr, "losing failJob must not stamp its error on a cancelled job")
	assert.Equal(t, string(StatusCancelled), dbJobStatus(t, q.db, job.ID), "DB status")
}

// TestProcessJobAbortsWhenCancelledBeforeStart covers the dequeue race:
// Cancel()'s channel send is non-blocking and is dropped unless the worker
// is parked exactly at its check, so a job can be cancelled in memory and
// the DB while the worker is already inside processJob. The queued->running
// flip must re-validate and abort instead of resurrecting the cancel (in
// memory via the unconditional flip, in the DB via the unconditional
// dbUpdateJobRunning + a subsequent dbCompleteJob that would then win the
// terminal fence against a re-opened row).
func TestProcessJobAbortsWhenCancelledBeforeStart(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job := submitTestJob(t, q)
	require.True(t, q.Cancel(job.ID), "Cancel")

	// The worker already pulled the job past its cancel-channel check when
	// Cancel landed — run the processing path directly.
	q.processJob(context.Background(), job)

	assertJobStatus(t, q, job.ID, StatusCancelled)
	assert.Equal(t, string(StatusCancelled), dbJobStatus(t, q.db, job.ID), "DB status")
	assert.Empty(t, mockComfy.submittedPrompts(), "cancelled job must never reach comfy")
	waitForJobDone(t, job, time.Second) // Cancel must have closed done already
}

// blockingUploadServer mimics the imgsite upload wire protocol but parks
// each /updo request until released, giving tests a deterministic window
// where processJob sits between monitor completion and its terminal
// transition.
type blockingUploadServer struct {
	server  *httptest.Server
	arrived chan struct{}
	release chan struct{}
}

func newBlockingUploadServer(t *testing.T) *blockingUploadServer {
	t.Helper()
	b := &blockingUploadServer{
		arrived: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/updo", func(w http.ResponseWriter, r *http.Request) {
		b.arrived <- struct{}{}
		<-b.release
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{
			"id":       "aQ3f9xK",
			"url":      b.server.URL + "/aQ3f9xK/orig/test-image.png",
			"page":     b.server.URL + "/aQ3f9xK",
			"filename": "test-image.png",
		})
	})
	b.server = httptest.NewServer(mux)
	t.Cleanup(b.server.Close)
	return b
}

// TestCancelDuringUploadKeepsJobCancelled pins memory-side first-write-wins
// for the success path: cancel lands while processJob is uploading the
// finished image (uploadImage takes no job context). The completion must
// not overwrite the cancel in memory or count as completed.
func TestCancelDuringUploadKeepsJobCancelled(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	up := newBlockingUploadServer(t)

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Upload.URL = up.server.URL

	q, cleanup := setupTestQueue(t, cfg) // single real worker
	defer cleanup()

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{Prompt: "a cat", OutputFormat: "url"})
	require.NoError(t, err, "Submit")

	select {
	case <-up.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("upload never started — monitor did not complete")
	}

	require.True(t, q.Cancel(job.ID), "Cancel while the worker is mid-upload")

	// Drain the parked upload, then prove the worker finished job's
	// processing by completing a canary behind it (one worker ⇒ serial).
	close(up.release)
	canary, err := q.Submit(JobTypeGenerate, "test", JobInput{Prompt: "a dog", OutputFormat: "base64"})
	require.NoError(t, err, "Submit canary")
	waitForJobDone(t, canary, 15*time.Second)
	assertJobStatus(t, q, canary.ID, StatusCompleted)

	assertJobStatus(t, q, job.ID, StatusCancelled)
	q.mu.RLock()
	result := job.Result
	jobErr := job.Error
	q.mu.RUnlock()
	assert.Nil(t, result, "cancelled job must not gain a result from the losing completion")
	assert.Empty(t, jobErr, "cancelled job must not carry the losing completion's state")
	assert.Equal(t, string(StatusCancelled), dbJobStatus(t, q.db, job.ID), "DB status")
	assert.Equal(t, 0, jobImageCount(t, q.db, job.ID), "no images may be persisted for the cancelled job")
	assert.Equal(t, 1, q.Status().Completed, "only the canary may count as completed")
	assert.Equal(t, 0, q.Status().Failed, "cancelled job must not count as failed")
}

// TestRecoverRunningJobUploadFailureClosesDone pins the waiter hang on the
// recovery upload-failure path: it called failJob and returned without the
// terminal bookkeeping, so done stayed open and wait_for_job / sync tools
// blocked until their full timeout.
func TestRecoverRunningJobUploadFailureClosesDone(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	failingUpload := newFakeUploadServer(t)
	failingUpload.updoStatus = http.StatusInternalServerError

	cfg := testConfig(mockComfy.URL())
	cfg.Upload.URL = failingUpload.server.URL
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job := submitTestJob(t, q) // OutputFormat "" -> url -> upload fails
	markRunningInMemoryAndDB(t, q, job, "test-prompt-1")

	q.wg.Add(1)
	go q.recoverRunningJob(context.Background(), job, "test-prompt-1")

	waitForJobDone(t, job, 10*time.Second)

	assertJobStatus(t, q, job.ID, StatusFailed)
	q.mu.RLock()
	jobErr := job.Error
	q.mu.RUnlock()
	assert.Contains(t, jobErr, "upload failed during recovery")
	assert.Equal(t, string(StatusFailed), dbJobStatus(t, q.db, job.ID), "DB status")
}

// TestCancelAbortsRecoveryPromptly covers Cancel() racing a
// recoverRunningJob: recovery never wired job.cancelCtx, so a cancel could
// not stop the resume monitor — the recovery ground on for the full
// Comfy.Timeout and its failJob then overwrote the cancel. The recovery
// context must be wired so Cancel aborts the monitor promptly, and the
// failure must not overwrite the cancelled state.
func TestCancelAbortsRecoveryPromptly(t *testing.T) {
	silent := newSilentComfyServer(t, 0) // history never becomes ready
	cfg := testConfig(silent.server.URL)
	cfg.Comfy.Timeout = 5
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job := submitTestJob(t, q)
	markRunningInMemoryAndDB(t, q, job, "test-prompt-1")

	finished := make(chan struct{})
	q.wg.Add(1)
	go func() {
		defer close(finished)
		q.recoverRunningJob(context.Background(), job, "test-prompt-1")
	}()

	select {
	case <-silent.wsConnected:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery never connected to the monitor websocket")
	}

	require.True(t, q.Cancel(job.ID), "Cancel")

	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("recovery must abandon promptly once the job is cancelled (cancelCtx must be wired)")
	}

	assertJobStatus(t, q, job.ID, StatusCancelled)
	q.mu.RLock()
	jobErr := job.Error
	q.mu.RUnlock()
	assert.Empty(t, jobErr, "cancelled job must not be overwritten by the recovery failure")
	assert.Equal(t, string(StatusCancelled), dbJobStatus(t, q.db, job.ID), "DB status")
}
