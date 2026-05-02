package main

import (
	"context"
	"testing"
	"time"
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
	if !ok {
		t.Fatal("expected Cancel to return true")
	}

	assertJobStatus(t, q, job.ID, StatusCancelled)

	if dbStatus := dbJobStatus(t, q.db, job.ID); dbStatus != string(StatusCancelled) {
		t.Errorf("expected DB status %q, got %q", StatusCancelled, dbStatus)
	}

	q.mu.RLock()
	_, inResults := q.results[job.ID]
	q.mu.RUnlock()
	if !inResults {
		t.Error("cancelled job should still be in results map")
	}
}

func TestJobQueue_Cancel_NonExistentJob(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	q, cleanup := setupTestQueue(t, cfg)
	defer cleanup()

	ok := q.Cancel("nonexistent")
	if ok {
		t.Fatal("expected Cancel to return false for nonexistent job")
	}
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
	if ok {
		t.Fatal("expected Cancel to return false for completed job")
	}
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
	if ok {
		t.Fatal("expected Cancel to return false for already-cancelled job")
	}
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
	if !ok {
		t.Fatal("expected Cancel to return true for running job")
	}

	q.mu.RLock()
	storedJob := q.results[job.ID]
	q.mu.RUnlock()
	if storedJob.Status != StatusCancelled {
		t.Errorf("expected status %s, got %s", StatusCancelled, storedJob.Status)
	}

	interrupts := mockComfy.getInterrupts()
	if len(interrupts) != 1 {
		t.Fatalf("expected 1 interrupt call, got %d", len(interrupts))
	}
	if interrupts[0]["prompt_id"] != "comfy-prompt-456" {
		t.Errorf("expected interrupt for prompt_id 'comfy-prompt-456', got %q", interrupts[0]["prompt_id"])
	}
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
	if !ok {
		t.Fatal("expected Cancel to return true")
	}

	q.mu.RLock()
	storedJob := q.results[job.ID]
	q.mu.RUnlock()
	if storedJob.Status != StatusCancelled {
		t.Errorf("expected status %s, got %s", StatusCancelled, storedJob.Status)
	}

	interrupts := mockComfy.getInterrupts()
	if len(interrupts) != 0 {
		t.Errorf("expected no interrupt calls when no comfy_prompt_id, got %d", len(interrupts))
	}
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
	if beforeLen != 1 {
		t.Fatalf("expected 1 queued job before cancel, got %d", beforeLen)
	}

	q.Cancel(job.ID)

	q.orderMu.Lock()
	afterLen := len(q.queuedOrder)
	q.orderMu.Unlock()
	if afterLen != 0 {
		t.Errorf("expected 0 queued jobs after cancel, got %d", afterLen)
	}
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

	if !q.Cancel(job1.ID) {
		t.Fatal("expected job1 cancel to succeed")
	}
	if !q.Cancel(job2.ID) {
		t.Fatal("expected job2 cancel to succeed")
	}

	assertJobStatus(t, q, job1.ID, StatusCancelled)
	assertJobStatus(t, q, job2.ID, StatusCancelled)
	assertJobStatus(t, q, job3.ID, StatusQueued)

	interrupts := mockComfy.getInterrupts()
	if len(interrupts) != 1 {
		t.Fatalf("expected 1 interrupt, got %d", len(interrupts))
	}
	if interrupts[0]["prompt_id"] != "comfy-2" {
		t.Errorf("expected interrupt for comfy-2, got %q", interrupts[0]["prompt_id"])
	}
}
