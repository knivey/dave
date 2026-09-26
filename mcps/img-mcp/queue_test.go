package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
// the enhanced path carries the LLM's prompt/negative/reasoning, the
// recovery path (empty enhancement args) omits those and lets EXIF supply
// them server-side, and the safety verdict rides along only when
// affirmatively resolved ("" and "unknown" drop out; imgsite's column
// already defaults to unknown and rejects other values).
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
		safety       string
		expect       UploadMeta
		expectJSONEq string
	}{
		{
			name:        "plain generate: final prompt equals original",
			job:         plainJob,
			finalPrompt: "a cat",
			finalNeg:    "",
			reasoning:   "",
			safety:      safetyVerdictSafe,
			expect: UploadMeta{
				JobID:          "plainjob",
				OriginalPrompt: "a cat",
				EnhancedPrompt: "a cat",
				WorkflowName:   "zimage",
				Network:        "libera",
				Channel:        "#dave",
				Nick:           "knivey",
				Safety:         safetyVerdictSafe,
			},
			expectJSONEq: `{"job_id":"plainjob","original_prompt":"a cat","enhanced_prompt":"a cat",
				"workflow_name":"zimage","network":"libera","channel":"#dave","nick":"knivey","safety":"safe"}`,
		},
		{
			name:        "enhanced: LLM prompt, negative, reasoning all carried",
			job:         enhancedJob,
			finalPrompt: "a fluffy cat, cinematic lighting",
			finalNeg:    "blurry, extra limbs",
			reasoning:   "the user asked for a cat",
			safety:      safetyVerdictUnsafe,
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
				Safety:         safetyVerdictUnsafe,
			},
			expectJSONEq: `{"job_id":"enhjob","original_prompt":"a cat","enhanced_prompt":"a fluffy cat, cinematic lighting",
				"negative_prompt":"blurry, extra limbs","reasoning":"the user asked for a cat","llm_generated":true,
				"workflow_name":"zimage","network":"libera","channel":"#dave","nick":"knivey","safety":"unsafe"}`,
		},
		{
			name:        "recovery: empty enhancement args drop out, EXIF supplies them",
			job:         enhancedJob,
			finalPrompt: "",
			finalNeg:    "",
			reasoning:   "",
			safety:      safetyVerdictUnsafe,
			expect: UploadMeta{
				JobID:          "enhjob",
				OriginalPrompt: "a cat",
				LLMGenerated:   true,
				WorkflowName:   "zimage",
				Network:        "libera",
				Channel:        "#dave",
				Nick:           "knivey",
				Safety:         safetyVerdictUnsafe,
			},
			expectJSONEq: `{"job_id":"enhjob","original_prompt":"a cat","llm_generated":true,
				"workflow_name":"zimage","network":"libera","channel":"#dave","nick":"knivey","safety":"unsafe"}`,
		},
		{
			name:        "unknown verdict is omitted, not sent",
			job:         plainJob,
			finalPrompt: "a cat",
			safety:      safetyVerdictUnknown,
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
			name:        "unvetted (skip_networks) verdict is omitted",
			job:         plainJob,
			finalPrompt: "a cat",
			safety:      safetyVerdictUnvetted,
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
			meta := buildUploadMeta(tt.job, tt.finalPrompt, tt.finalNeg, tt.reasoning, tt.safety)
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

// hangingVetServer stands in for the vetting LLM with a slow judgment: each
// /v1/chat/completions request parks until released, then answers with the
// given verdict JSON. Requests are counted and their arrival signalled so a
// test can prove generation proceeded while the vet was still hanging.
type hangingVetServer struct {
	server  *httptest.Server
	calls   atomic.Int32
	arrived chan struct{}
	release chan struct{}
}

func newHangingVetServer(t *testing.T, verdictJSON string) *hangingVetServer {
	t.Helper()
	v := &hangingVetServer{
		arrived: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		v.calls.Add(1)
		v.arrived <- struct{}{}
		<-v.release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatc-1","object":"chat.completion","created":1,"model":"stub",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":` + verdictJSON + `},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	})
	v.server = httptest.NewServer(mux)
	t.Cleanup(v.server.Close)
	return v
}

// TestProcessJobVetOverlapsGenerationAndUploadAwaitsIt pins the vet stage's
// concurrency contract: the vet call starts right after enhancement (here:
// a direct-tool job, so straight away) and generation must NOT block on it
// — the ComfyUI submit lands while the vet is still hanging — while the
// upload waits for the resolved verdict (persistence and the EXIF note
// rewrite consume jobSafety after the monitor completes).
func TestProcessJobVetOverlapsGenerationAndUploadAwaitsIt(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	vet := newHangingVetServer(t, `{"safe":true,"reason":"fine"}`)
	up := newBlockingUploadServer(t)

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Upload.URL = up.server.URL
	cfg.Enhancements = map[string]EnhancementConfig{
		"safety-vet": {
			BaseURL:      vet.server.URL + "/v1",
			Key:          "test-key",
			Model:        "stub-model",
			SystemPrompt: "judge the prompts",
			Timeout:      30,
		},
	}

	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt:       "a cat sitting on a mat",
		Network:      "graped",
		OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")

	// The vet call is in flight and hanging...
	select {
	case <-vet.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("vet call never started")
	}

	// ...yet generation must already be under way: the submit must land
	// while the vet is still parked (serializing the vet before the submit
	// would add its full latency to every image).
	require.Eventually(t, func() bool {
		return len(mockComfy.submittedPrompts()) > 0
	}, 5*time.Second, 10*time.Millisecond, "comfy submit must not block on the hanging vet")

	// The monitor (instant on the mock) has finished by now; the upload is
	// gated on the awaited verdict, so it must NOT arrive while the vet
	// hangs. This cannot flake on slowness: with the await in place the
	// upload is unreachable until the release below.
	select {
	case <-up.arrived:
		t.Fatal("upload started before the vet verdict was resolved — the await is missing or misplaced")
	case <-time.After(300 * time.Millisecond):
	}

	// Resolving the verdict unblocks the upload.
	close(vet.release)
	select {
	case <-up.arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("upload never started after the vet resolved")
	}

	close(up.release)
	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)
	assert.Equal(t, int32(1), vet.calls.Load(), "exactly one vet call per job")
}

// TestProcessJobVetFailureStillCompletesJob pins the failure semantics: a
// vet endpoint that 500s degrades the verdict to unknown but never fails
// the job — it completes and uploads normally (the main site shows it
// regardless; the safe site default-denies until an admin marks it).
func TestProcessJobVetFailureStillCompletesJob(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	up := newFakeUploadServer(t)
	vet := newVetStubServer(t, http.StatusInternalServerError, "")

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Upload.URL = up.server.URL
	cfg.Enhancements = map[string]EnhancementConfig{
		"safety-vet": {
			BaseURL:      vet.server.URL + "/v1",
			Key:          "test-key",
			Model:        "stub-model",
			SystemPrompt: "judge the prompts",
			Timeout:      10,
		},
	}

	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt:       "a cat sitting on a mat",
		Network:      "graped",
		OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)
	// The SDK retries 5xx (machinery the vet inherits), so pin "tried"
	// rather than an exact count.
	assert.NotZero(t, vet.calls.Load(), "the vet must have been tried (and failed)")
}

// safetyQueueConfig wires a queue-test config's safety-vet enhancement at
// the given stub and sets skip_networks, keeping the workflow path + queue
// pieces testConfig/mustWriteWorkflow provide.
func safetyQueueConfig(t *testing.T, comfyURL string, vet *vetStubServer, skipNetworks []string) Config {
	t.Helper()
	cfg := testConfig(comfyURL)
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Enhancements = map[string]EnhancementConfig{
		"safety-vet": {
			BaseURL:      vet.server.URL + "/v1",
			Key:          "test-key",
			Model:        "stub-model",
			SystemPrompt: "judge the prompts",
			Timeout:      10,
		},
	}
	cfg.Safety.SkipNetworks = skipNetworks
	return cfg
}

// TestProcessJobPersistsSafetyVerdict pins the resolution→row write: by the
// time the job completes, jobs.safety holds the awaited verdict. The
// empty-string distinction is load-bearing — a skip_networks job (never
// classified) must leave the column at its unvetted default so recovery can
// tell "nothing to write" apart from a persisted unknown.
func TestProcessJobPersistsSafetyVerdict(t *testing.T) {
	tests := []struct {
		name         string
		network      string
		skipNetworks []string
		vetStatus    int
		vetContent   string
		wantSafety   string
	}{
		{
			name:       "vet safe:true persists safe",
			network:    "graped",
			vetStatus:  http.StatusOK,
			vetContent: `{"safe":true,"reason":"fine"}`,
			wantSafety: safetyVerdictSafe,
		},
		{
			name:       "vet safe:false persists unsafe",
			network:    "graped",
			vetStatus:  http.StatusOK,
			vetContent: `{"safe":false,"reason":"sexual content"}`,
			wantSafety: safetyVerdictUnsafe,
		},
		{
			name:       "vet failure persists unknown",
			network:    "graped",
			vetStatus:  http.StatusInternalServerError,
			wantSafety: safetyVerdictUnknown,
		},
		{
			name:         "skip network leaves the column unvetted",
			network:      "Libera",
			skipNetworks: []string{"libera"},
			vetStatus:    http.StatusOK,
			vetContent:   `{"safe":true,"reason":"fine"}`,
			wantSafety:   safetyVerdictUnvetted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockComfy := newMockComfyFlowServer(t)
			vet := newVetStubServer(t, tt.vetStatus, tt.vetContent)
			cfg := safetyQueueConfig(t, mockComfy.URL(), vet, tt.skipNetworks)

			q, cleanup := setupTestQueue(t, cfg)
			defer cleanup()

			job, err := q.Submit(JobTypeGenerate, "test", JobInput{
				Prompt:       "a cat sitting on a mat",
				Network:      tt.network,
				OutputFormat: "base64", // upload is not under test here
			})
			require.NoError(t, err, "Submit")

			waitForJobDone(t, job, 15*time.Second)
			assertJobStatus(t, q, job.ID, StatusCompleted)
			assert.Equal(t, tt.wantSafety, dbJobSafety(t, q.db, job.ID),
				"resolved verdict must be persisted on the jobs row")
		})
	}
}

// restartRecoverJob mimics what recoverJobs does for a running job at
// startup: reload the row from the DB into a fresh Job (the persisted
// safety verdict rides along on Job.Safety) and rewire its channels.
func restartRecoverJob(t *testing.T, q *JobQueue, jobID string) *Job {
	t.Helper()
	dbj, err := dbGetJob(q.db, jobID)
	require.NoError(t, err, "dbGetJob")
	recovered := jobFromDBJob(dbj)
	recovered.done = make(chan struct{})
	recovered.cancel = make(chan struct{})
	q.mu.Lock()
	q.results[recovered.ID] = recovered
	q.mu.Unlock()
	return recovered
}

// TestRecoverRunningJobUsesPersistedSafety pins the never-re-vet rule: a
// crash AFTER the verdict resolved left jobs.safety populated, so the
// restart's resume monitor reuses it — the vet endpoint sees zero calls —
// and the row keeps the original verdict (an unknown verdict is equally
// final: "vetted but unresolved" must not be re-classified either).
func TestRecoverRunningJobUsesPersistedSafety(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`) // would say safe...
	cfg := safetyQueueConfig(t, mockComfy.URL(), vet, nil)
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt: "a cat", Network: "graped", OutputFormat: "base64",
	})
	require.NoError(t, err, "Submit")
	markRunningInMemoryAndDB(t, q, job, "test-prompt-1")

	// ...but the verdict resolved (to unsafe) before the crash.
	require.NoError(t, dbUpdateJobSafety(q.db, job.ID, safetyVerdictUnsafe))
	recovered := restartRecoverJob(t, q, job.ID)
	require.Equal(t, safetyVerdictUnsafe, recovered.Safety,
		"the recovered job must carry the persisted verdict")

	q.wg.Add(1)
	go q.recoverRunningJob(context.Background(), recovered, "test-prompt-1")

	waitForJobDone(t, recovered, 15*time.Second)
	assertJobStatus(t, q, recovered.ID, StatusCompleted)
	assert.Zero(t, vet.calls.Load(), "a persisted verdict must never be re-vetted on recovery")
	assert.Equal(t, safetyVerdictUnsafe, dbJobSafety(t, q.db, job.ID),
		"row must keep the pre-crash verdict, not a fresh one")
}

// TestRecoverRunningJobReVetsWhenUnresolved pins the crash-before-resolution
// path: an empty jobs.safety means the vet never resolved, so recovery
// re-vets during the resume monitor — original prompt from the job input,
// enhanced prompt recovered from the completed image's embedded workflow
// (the only surviving copy — recovery never re-runs enhancement) — and
// persists the fresh verdict.
func TestRecoverRunningJobReVetsWhenUnresolved(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	const enhancedPrompt = "an enhanced majestic cat, studio lighting"
	mockComfy.serveViewData(exifWebPWithPrompt(t, enhancedPrompt))
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`)
	cfg := safetyQueueConfig(t, mockComfy.URL(), vet, nil)
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt: "a cat", Network: "graped", OutputFormat: "base64",
	})
	require.NoError(t, err, "Submit")
	markRunningInMemoryAndDB(t, q, job, "test-prompt-1")

	recovered := restartRecoverJob(t, q, job.ID)
	require.Empty(t, recovered.Safety, "no verdict survived the crash")

	q.wg.Add(1)
	go q.recoverRunningJob(context.Background(), recovered, "test-prompt-1")

	waitForJobDone(t, recovered, 15*time.Second)
	assertJobStatus(t, q, recovered.ID, StatusCompleted)

	assert.Equal(t, int32(1), vet.calls.Load(), "recovery must re-vet exactly once")
	contents := vet.userContents()
	require.Len(t, contents, 1, "one vet call, one captured user message")
	assert.Contains(t, contents[0], "a cat",
		"re-vet input must carry the original prompt from the job input")
	assert.Contains(t, contents[0], enhancedPrompt,
		"re-vet input must carry the enhanced prompt recovered from the image's EXIF")

	assert.Equal(t, safetyVerdictSafe, dbJobSafety(t, q.db, job.ID),
		"the re-vetted verdict must persist so a second restart never re-vets")
}

// TestRecoverRunningJobSkipNetworksDoesNotReVet pins the skip_networks rule
// on the recovery path: restart recovery of a Libera job must not classify
// even when the verdict never resolved — its visibility comes from imgsite's
// allowed_networks rule, not a verdict.
func TestRecoverRunningJobSkipNetworksDoesNotReVet(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`)
	cfg := safetyQueueConfig(t, mockComfy.URL(), vet, []string{"libera"})
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt: "a cat", Network: "Libera", OutputFormat: "base64",
	})
	require.NoError(t, err, "Submit")
	markRunningInMemoryAndDB(t, q, job, "test-prompt-1")

	recovered := restartRecoverJob(t, q, job.ID)
	require.Empty(t, recovered.Safety)

	q.wg.Add(1)
	go q.recoverRunningJob(context.Background(), recovered, "test-prompt-1")

	waitForJobDone(t, recovered, 15*time.Second)
	assertJobStatus(t, q, recovered.ID, StatusCompleted)
	assert.Zero(t, vet.calls.Load(), "skip_networks applies on recovery too — no vet call")
	assert.Empty(t, dbJobSafety(t, q.db, job.ID),
		"an unvetted job must stay unvetted — empty column, not unknown")
}

// TestProcessJobRewritesNoteBeforeUpload pins the load-bearing ordering of
// the EXIF note rewrite: after the monitor completes and the verdict is
// awaited, each image's note is rebuilt wholesale (verdict + provenance join
// the submit-time fields) and baked into the bytes BEFORE the upload — the
// bytes imgsite receives, stores, and sha256-dedups are the rewritten bytes.
// The submit-time note carries provenance but never a safety verdict (it
// does not exist yet at submit).
func TestProcessJobRewritesNoteBeforeUpload(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	const enhancedPrompt = "an enhanced majestic cat, studio lighting"
	fixture := exifWebPWithNote(t,
		`{"prompt":"a cat","llm_generated":true,"job_id":"stale"}`, enhancedPrompt)
	mockComfy.serveViewData(fixture)
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`)
	up := newFakeUploadServer(t)

	cfg := safetyQueueConfig(t, mockComfy.URL(), vet, nil)
	cfg.Upload.URL = up.server.URL

	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt:       "a cat",
		LLMGenerated: true,
		Network:      "graped",
		Channel:      "#test",
		Nick:         "user1",
		OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	// Submit-time note: provenance rides from submit; the verdict does not.
	submitted := mockComfy.submittedPrompts()
	require.Len(t, submitted, 1, "exactly one prompt submission")
	submitNote, ok := submitted[0].Prompt[davePromptNoteNodeID].Inputs["text"].(string)
	require.True(t, ok, "submitted note text should be a string")
	assert.Contains(t, submitNote, `"network":"graped"`)
	assert.Contains(t, submitNote, `"channel":"#test"`)
	assert.Contains(t, submitNote, `"nick":"user1"`)
	assert.NotContains(t, submitNote, "safety",
		"the submit-time note must not carry a safety verdict — it has not resolved yet")

	// The uploaded bytes must hash-match the rewritten artifact: reconstruct
	// the expected result (note + surgery) and compare against what the fake
	// site received. Had the rewrite landed after the upload — or not at all
	// — the received hash would match the fixture instead.
	expectedNote, err := buildPromptNoteWithSafety(job, "", safetyVerdictSafe, false)
	require.NoError(t, err, "buildPromptNoteWithSafety")
	expectedBytes, err := rewriteWebpNoteData(fixture, expectedNote)
	require.NoError(t, err, "rewriteWebpNoteData")

	received := up.gotFileContent
	require.NotEmpty(t, received, "upload must have received the image")
	assert.NotEqual(t, sha256.Sum256(fixture), sha256.Sum256(received),
		"the uploaded bytes must not be the un-rewritten fixture")
	assert.Equal(t, sha256.Sum256(expectedBytes), sha256.Sum256(received),
		"uploaded bytes must hash-match the post-rewrite image (rewrite → sha256 → upload)")
	assert.True(t, bytes.Equal(received, expectedBytes),
		"uploaded bytes must be exactly the rewritten image")

	// The verdict + provenance are recoverable from the uploaded bytes via
	// the production reader (imgsite's extraction twin).
	note, ok := embeddedPromptNote(received)
	require.True(t, ok, "rewritten note must parse back out of the uploaded bytes")
	assert.Equal(t, "a cat", note.Prompt)
	assert.True(t, note.LLMGenerated)
	assert.Equal(t, job.ID, note.JobID)
	assert.Equal(t, "graped", note.Network)
	assert.Equal(t, "#test", note.Channel)
	assert.Equal(t, "user1", note.Nick)
	assert.Equal(t, safetyVerdictSafe, note.Safety)

	// The enhanced prompt the image ran with survives the surgery.
	api, ok := embeddedWorkflowJSON(received)
	require.True(t, ok, "uploaded bytes must still embed a workflow")
	var wf ComfyWorkflow
	require.NoError(t, json.Unmarshal([]byte(api), &wf))
	assert.Equal(t, enhancedPrompt, wf["prompt-node"].Inputs["text"])

	// Meta carries the verdict alongside the baked copy.
	var meta UploadMeta
	require.NoError(t, json.Unmarshal([]byte(up.gotMetaRaw), &meta))
	assert.Equal(t, safetyVerdictSafe, meta.Safety)
	assert.Equal(t, "graped", meta.Network)
}

// TestProcessJobNoteRewriteSkipsNonWebp pins the WARN-and-skip path:
// production output is webp, but a non-webp output must not fail the job —
// the original bytes upload unchanged and the verdict still travels in the
// upload meta.
func TestProcessJobNoteRewriteSkipsNonWebp(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t) // /view serves "fakedata" — not a webp
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`)
	up := newFakeUploadServer(t)

	cfg := safetyQueueConfig(t, mockComfy.URL(), vet, nil)
	cfg.Upload.URL = up.server.URL

	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt: "a cat", Network: "graped", OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	assert.Equal(t, []byte("fakedata"), up.gotFileContent,
		"non-webp output must upload the original bytes (rewrite skipped)")
	var meta UploadMeta
	require.NoError(t, json.Unmarshal([]byte(up.gotMetaRaw), &meta))
	assert.Equal(t, safetyVerdictSafe, meta.Safety,
		"the verdict still travels in the upload meta when the rewrite is skipped")
}

// TestProcessJobNoteRewriteCarriesNSFWFirstPass pins the first-pass flag's
// full journey into the EXIF note: an enhancement reply carrying
// "nsfw":true bakes nsfw:true into the SUBMIT-time note (so the flag
// survives a crash exactly like the enhancement reasoning), and the
// rewrite-time note rebuilt after the short-circuited verdict carries BOTH
// nsfw:true and safety:"unsafe" in the uploaded bytes. The vet stub would
// say safe — zero calls prove the flag short-circuits classification
// rather than riding alongside it.
func TestProcessJobNoteRewriteCarriesNSFWFirstPass(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	const enhancedPrompt = "an enhanced majestic cat, studio lighting"
	fixture := exifWebPWithNote(t,
		`{"prompt":"a cat","llm_generated":false,"job_id":"stale"}`, enhancedPrompt)
	mockComfy.serveViewData(fixture)
	enhServer, _ := newEnhancementStubServer(t, EnhancementResponse{
		EnhancedPrompt: enhancedPrompt,
		NegativePrompt: "blurry",
		NSFW:           true,
	})
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`)
	up := newFakeUploadServer(t)

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Upload.URL = up.server.URL
	cfg.Enhancements = map[string]EnhancementConfig{
		"default": {
			BaseURL:      enhServer.URL + "/v1",
			Key:          "test-key",
			Model:        "stub-model",
			SystemPrompt: "enhance",
			Timeout:      10,
		},
		"safety-vet": {
			BaseURL:      vet.server.URL + "/v1",
			Key:          "test-key",
			Model:        "stub-model",
			SystemPrompt: "judge the prompts",
			Timeout:      10,
		},
	}

	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeEnhanceGenerate, "test", JobInput{
		Prompt:       "a cat",
		Network:      "graped",
		OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	// Submit-time note: the flag rides from the moment enhancement
	// resolves, so a crash before the rewrite leaves it in the image.
	submitted := mockComfy.submittedPrompts()
	require.Len(t, submitted, 1, "exactly one prompt submission")
	submitNote, ok := submitted[0].Prompt[davePromptNoteNodeID].Inputs["text"].(string)
	require.True(t, ok, "submitted note text should be a string")
	assert.Contains(t, submitNote, `"nsfw":true`,
		"the submit-time note must bake the flagged first pass")
	assert.NotContains(t, submitNote, "safety",
		"the submit-time note still carries no verdict — it has not resolved yet")

	// nsfw:true short-circuits to unsafe with NO vet call.
	assert.Zero(t, vet.calls.Load(), "nsfw:true must skip the vet call entirely")
	assert.Equal(t, safetyVerdictUnsafe, dbJobSafety(t, q.db, job.ID),
		"the short-circuit verdict must be the persisted one")

	// Uploaded bytes carry nsfw:true ALONGSIDE safety unsafe.
	received := up.gotFileContent
	require.NotEmpty(t, received, "upload must have received the image")
	note, ok := embeddedPromptNote(received)
	require.True(t, ok, "rewritten note must parse back out of the uploaded bytes")
	assert.True(t, note.NSFW, "the first-pass flag must ride the rewrite into the uploaded bytes")
	assert.Equal(t, safetyVerdictUnsafe, note.Safety,
		"the baked verdict is the short-circuited unsafe")
}

// TestProcessJobNoteRewriteSkipNetworksOmitsNSFW pins the skip gate inside
// noteNSFW (firstPassNSFW && !safetyNetworkSkipped): an enhancement reply
// carrying "nsfw":true on a skip_networks job must bake NO nsfw key at
// submit or in the rewritten upload. Deleting the gate clause (leaving
// noteNSFW := firstPassNSFW) fails this test — the plain-generate skip case
// in TestProcessJobNoteRewriteOmitsUnresolvedSafety cannot catch it, because
// its absence follows from having no enhancement at all, not from the gate.
// The vet stub would say safe and is never called: skip_networks
// short-circuits classification before the flag is even consulted.
func TestProcessJobNoteRewriteSkipNetworksOmitsNSFW(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	const enhancedPrompt = "an enhanced majestic cat, studio lighting"
	fixture := exifWebPWithNote(t,
		`{"prompt":"a cat","llm_generated":false,"job_id":"stale"}`, enhancedPrompt)
	mockComfy.serveViewData(fixture)
	enhServer, _ := newEnhancementStubServer(t, EnhancementResponse{
		EnhancedPrompt: enhancedPrompt,
		NegativePrompt: "blurry",
		NSFW:           true,
	})
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`)
	up := newFakeUploadServer(t)

	cfg := testConfig(mockComfy.URL())
	wc := cfg.Workflows["test"]
	wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
	cfg.Workflows["test"] = wc
	cfg.Upload.URL = up.server.URL
	cfg.Enhancements = map[string]EnhancementConfig{
		"default": {
			BaseURL:      enhServer.URL + "/v1",
			Key:          "test-key",
			Model:        "stub-model",
			SystemPrompt: "enhance",
			Timeout:      10,
		},
		"safety-vet": {
			BaseURL:      vet.server.URL + "/v1",
			Key:          "test-key",
			Model:        "stub-model",
			SystemPrompt: "judge the prompts",
			Timeout:      10,
		},
	}
	cfg.Safety.SkipNetworks = []string{"libera"}

	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	job, err := q.Submit(JobTypeEnhanceGenerate, "test", JobInput{
		Prompt:       "a cat",
		Network:      "Libera",
		OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")

	waitForJobDone(t, job, 15*time.Second)
	assertJobStatus(t, q, job.ID, StatusCompleted)

	// skip_networks short-circuits classification before the flag is
	// consulted: no vet call, unvetted column.
	assert.Zero(t, vet.calls.Load(), "skip_networks must skip the vet call even with nsfw:true")
	assert.Empty(t, dbJobSafety(t, q.db, job.ID), "an unvetted job must stay unvetted — empty column")

	// Submit-time note: the gate — not the absence of enhancement — keeps
	// the flagged first pass out.
	submitted := mockComfy.submittedPrompts()
	require.Len(t, submitted, 1, "exactly one prompt submission")
	submitNote, ok := submitted[0].Prompt[davePromptNoteNodeID].Inputs["text"].(string)
	require.True(t, ok, "submitted note text should be a string")
	assert.NotContains(t, submitNote, "nsfw",
		"the skip gate must keep the flagged first pass out of the submit-time note")

	// Rewritten upload: same absence through the rewrite (and no verdict —
	// a skipped network is unvetted). Raw-text pins because struct decoding
	// cannot tell an absent key from a false one.
	received := up.gotFileContent
	require.NotEmpty(t, received, "upload must have received the image")
	note, ok := embeddedPromptNote(received)
	require.True(t, ok, "rewritten note must parse back out of the uploaded bytes")
	assert.False(t, note.NSFW)
	wfJSON, ok := embeddedWorkflowJSON(received)
	require.True(t, ok)
	var wf ComfyWorkflow
	require.NoError(t, json.Unmarshal([]byte(wfJSON), &wf))
	noteText, ok := wf[davePromptNoteNodeID].Inputs["text"].(string)
	require.True(t, ok, "note text should be a string")
	assert.NotContains(t, noteText, "nsfw",
		"the skip gate must hold through the rewrite-time rebuild too")
	assert.NotContains(t, noteText, "safety",
		"a skipped network is unvetted: no verdict baked either")
}

// TestRecoverRunningJobCarriesNSFWFlagAcrossRewrite pins the recovery half
// of the first-pass flag's persistence: the nsfw flag baked into the
// submit-time note is carried across the wholesale rewrite from the note
// already embedded in the image — the same surviving-copy pattern the
// enhancement reasoning uses — alongside the persisted verdict.
func TestRecoverRunningJobCarriesNSFWFlagAcrossRewrite(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	const enhancedPrompt = "an enhanced majestic cat, studio lighting"
	fixture := exifWebPWithNote(t,
		`{"prompt":"a cat","llm_generated":true,"job_id":"stale","enhancement_reasoning":"the user asked for a cat","nsfw":true}`,
		enhancedPrompt)
	mockComfy.serveViewData(fixture)
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`) // would say safe...
	up := newFakeUploadServer(t)

	cfg := safetyQueueConfig(t, mockComfy.URL(), vet, nil)
	cfg.Upload.URL = up.server.URL
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt:       "a cat",
		LLMGenerated: true,
		Network:      "graped",
		Channel:      "#test",
		Nick:         "user1",
		OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")
	markRunningInMemoryAndDB(t, q, job, "test-prompt-1")

	// ...but the verdict resolved (unsafe) before the crash, and the note
	// in the completed image carries the submit-time nsfw flag.
	require.NoError(t, dbUpdateJobSafety(q.db, job.ID, safetyVerdictUnsafe))
	recovered := restartRecoverJob(t, q, job.ID)

	q.wg.Add(1)
	go q.recoverRunningJob(context.Background(), recovered, "test-prompt-1")

	waitForJobDone(t, recovered, 15*time.Second)
	assertJobStatus(t, q, recovered.ID, StatusCompleted)
	assert.Zero(t, vet.calls.Load(), "a persisted verdict must never be re-vetted")

	received := up.gotFileContent
	require.NotEmpty(t, received, "upload must have received the image")
	note, ok := embeddedPromptNote(received)
	require.True(t, ok, "recovery-rewritten note must parse back out")
	assert.True(t, note.NSFW,
		"the first-pass flag must survive the recovery rewrite via the old note")
	assert.Equal(t, "the user asked for a cat", note.EnhancementReasoning,
		"the reasoning must keep riding the same carry-across path")
	assert.Equal(t, safetyVerdictUnsafe, note.Safety,
		"the persisted verdict must be baked on the recovery path")
	assert.Equal(t, job.ID, note.JobID, "the rebuild is wholesale: stale fields do not survive")
}

// TestProcessJobNoteRewriteOmitsUnresolvedSafety pins the bake discipline for
// non-affirmative verdicts: unknown (vet failed) and unvetted
// (skip_networks) bake nothing into the note and send nothing in the meta —
// imgsite's column already defaults to unknown and the site rejects any
// other value.
func TestProcessJobNoteRewriteOmitsUnresolvedSafety(t *testing.T) {
	tests := []struct {
		name         string
		network      string
		skipNetworks []string
		vetStatus    int
	}{
		{
			name:      "vet failure degrades to unknown: nothing baked",
			network:   "graped",
			vetStatus: http.StatusInternalServerError,
		},
		{
			name:         "skip networks is unvetted: nothing baked",
			network:      "Libera",
			skipNetworks: []string{"libera"},
			vetStatus:    http.StatusOK,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockComfy := newMockComfyFlowServer(t)
			mockComfy.serveViewData(exifWebPWithNote(t,
				`{"prompt":"a cat","llm_generated":false,"job_id":"stale"}`, "enhanced cat"))
			vet := newVetStubServer(t, tt.vetStatus, `{"safe":true,"reason":"fine"}`)
			up := newFakeUploadServer(t)

			cfg := safetyQueueConfig(t, mockComfy.URL(), vet, tt.skipNetworks)
			cfg.Upload.URL = up.server.URL

			q, cleanup := setupTestQueue(t, cfg)
			defer cleanup()

			job, err := q.Submit(JobTypeGenerate, "test", JobInput{
				Prompt: "a cat", Network: tt.network, OutputFormat: "url",
			})
			require.NoError(t, err, "Submit")

			waitForJobDone(t, job, 15*time.Second)
			assertJobStatus(t, q, job.ID, StatusCompleted)

			note, ok := embeddedPromptNote(up.gotFileContent)
			require.True(t, ok, "the rewrite itself must still happen (webp served)")
			assert.Empty(t, note.Safety, "non-affirmative verdicts must bake nothing")
			// Struct decoding can't tell an absent key from ""; pin the raw
			// note text (the note JSON rides escaped inside the workflow JSON,
			// so the workflow-level string would not show it verbatim).
			wfJSON, ok := embeddedWorkflowJSON(up.gotFileContent)
			require.True(t, ok)
			var wf ComfyWorkflow
			require.NoError(t, json.Unmarshal([]byte(wfJSON), &wf))
			noteText, ok := wf[davePromptNoteNodeID].Inputs["text"].(string)
			require.True(t, ok, "note text should be a string")
			assert.NotContains(t, noteText, "safety",
				"the safety key must be absent from the embedded note, not empty")
			assert.NotContains(t, noteText, "nsfw",
				"these paths ran no first pass (direct tool, no enhancement): the nsfw key must be absent too")

			assert.NotContains(t, up.gotMetaRaw, "safety",
				"non-affirmative verdicts must be omitted from the upload meta")
		})
	}
}

// TestRecoverRunningJobRewritesNoteWithPersistedSafety pins the recovery
// half of the rewrite: the persisted verdict (jobSafety) is baked into the
// bytes AND sent in the meta, and the enhancement reasoning — which recovery
// cannot rebuild — is carried across the wholesale note replacement from the
// note already embedded in the image, its only surviving copy.
func TestRecoverRunningJobRewritesNoteWithPersistedSafety(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)
	const enhancedPrompt = "an enhanced majestic cat, studio lighting"
	fixture := exifWebPWithNote(t,
		`{"prompt":"a cat","llm_generated":true,"job_id":"stale","enhancement_reasoning":"the user asked for a cat"}`,
		enhancedPrompt)
	mockComfy.serveViewData(fixture)
	vet := newVetStubServer(t, http.StatusOK, `{"safe":true,"reason":"fine"}`) // would say safe...
	up := newFakeUploadServer(t)

	cfg := safetyQueueConfig(t, mockComfy.URL(), vet, nil)
	cfg.Upload.URL = up.server.URL
	cfg.Queue.MaxWorkers = 0
	q := manualQueueLiteral(t, cfg)

	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt:       "a cat",
		LLMGenerated: true,
		Network:      "graped",
		Channel:      "#test",
		Nick:         "user1",
		OutputFormat: "url",
	})
	require.NoError(t, err, "Submit")
	markRunningInMemoryAndDB(t, q, job, "test-prompt-1")

	// ...but the verdict resolved (unsafe) before the crash.
	require.NoError(t, dbUpdateJobSafety(q.db, job.ID, safetyVerdictUnsafe))
	recovered := restartRecoverJob(t, q, job.ID)

	q.wg.Add(1)
	go q.recoverRunningJob(context.Background(), recovered, "test-prompt-1")

	waitForJobDone(t, recovered, 15*time.Second)
	assertJobStatus(t, q, recovered.ID, StatusCompleted)
	assert.Zero(t, vet.calls.Load(), "a persisted verdict must never be re-vetted")

	received := up.gotFileContent
	require.NotEmpty(t, received, "upload must have received the image")
	note, ok := embeddedPromptNote(received)
	require.True(t, ok, "recovery-rewritten note must parse back out")
	assert.Equal(t, safetyVerdictUnsafe, note.Safety,
		"the persisted verdict must be baked on the recovery path")
	assert.Equal(t, job.ID, note.JobID, "the rebuild is wholesale: stale fields do not survive")
	assert.Equal(t, "graped", note.Network)
	assert.Equal(t, "#test", note.Channel)
	assert.Equal(t, "user1", note.Nick)
	assert.Equal(t, "the user asked for a cat", note.EnhancementReasoning,
		"the embedded reasoning must be carried across the rewrite — recovery cannot rebuild it")

	var meta UploadMeta
	require.NoError(t, json.Unmarshal([]byte(up.gotMetaRaw), &meta))
	assert.Equal(t, safetyVerdictUnsafe, meta.Safety,
		"the recovery path's buildUploadMeta must consume the jobSafety verdict too")
}
