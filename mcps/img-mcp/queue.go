package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusCompleted JobStatus = "completed"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

type JobType string

const (
	JobTypeGenerate        JobType = "generate"
	JobTypeEnhanceGenerate JobType = "enhance_generate"
)

type Job struct {
	ID            string
	Type          JobType
	Status        JobStatus
	Workflow      string
	Input         JobInput
	Result        *JobResult
	Error         string
	ComfyPromptID string
	// Safety is the persisted safety verdict as restart recovery loaded
	// it from the jobs row ("safe"/"unsafe"/"unknown"; "" = never
	// resolved — the recovery path re-vets only then). A recovery-time
	// carrier only: processJob keeps its verdict in the local jobSafety
	// (awaited from the vet future) and persists straight to the DB, so
	// nothing outside queue.go reads this field and it deliberately stays
	// out of JobSnapshot.
	Safety      string
	CreatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	QueuedIndex int

	done      chan struct{}
	cancel    chan struct{}
	cancelCtx context.CancelFunc
	closeOnce sync.Once
}

// JobSnapshot is an immutable point-in-time copy of a Job's read-facing
// fields. All Job lifecycle/terminal mutations are synchronized on
// JobQueue.mu (see failJob/Cancel/processJob), so readers outside queue.go
// must never touch a live *Job: Get/WaitForJob/ListJobs hand out snapshots
// copied under the queue lock instead. QueuedIndex is deliberately absent —
// it is written under orderMu (not q.mu) during Submit, so copying it here
// would reintroduce a cross-lock race; nothing outside queue.go reads it.
type JobSnapshot struct {
	ID            string
	Type          JobType
	Status        JobStatus
	Workflow      string
	Input         JobInput
	Result        *JobResult
	Error         string
	ComfyPromptID string
	CreatedAt     time.Time
	StartedAt     *time.Time
	CompletedAt   *time.Time
}

// snapshotJob copies job's read-facing fields. The caller must already hold
// q.mu (or otherwise be synchronized with the last writer — e.g. a fresh
// object built from the DB); this function takes no locks so Get/ListJobs
// can fold the copy into their existing critical section. Result is a shared
// pointer: it is written exactly once before Status flips to completed and
// never mutated afterwards, so sharing it is safe.
func snapshotJob(job *Job) JobSnapshot {
	return JobSnapshot{
		ID:            job.ID,
		Type:          job.Type,
		Status:        job.Status,
		Workflow:      job.Workflow,
		Input:         job.Input,
		Result:        job.Result,
		Error:         job.Error,
		ComfyPromptID: job.ComfyPromptID,
		CreatedAt:     job.CreatedAt,
		StartedAt:     job.StartedAt,
		CompletedAt:   job.CompletedAt,
	}
}

type JobInput struct {
	Prompt         string
	NegativePrompt string
	Enhancement    string
	Seed           *int64
	OutputFormat   string
	// LLMGenerated records whether the prompt was composed by an LLM tool
	// call (dave injects _dave_inject_llm_generated=true on that path)
	// versus typed directly by the IRC user. Persisted so restart-recovered
	// jobs still carry it into the workflow's prompt note node.
	LLMGenerated bool
	// Network/Channel/Nick are dave-injected IRC provenance
	// (_dave_inject_network/_dave_inject_channel/_dave_inject_nick).
	// Network additionally feeds applyNetworkPolicy; all three are persisted
	// (migration 003) so restart-recovered jobs still carry them into the
	// imgsite upload meta.
	Network string
	Channel string
	Nick    string
}

type JobResult struct {
	Images []ImageData `json:"images"`
}

type ImageData struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MIMEType string `json:"mime_type"`
}

// buildUploadMeta assembles the imgsite upload meta payload for a job.
//
// finalPrompt/finalNegative/reasoning are the post-enhancement values from
// processJob's locals: for enhanced jobs finalPrompt is the enhanced prompt
// and reasoning the enhancement LLM's summary; for plain generate jobs
// finalPrompt equals job.Input.Prompt (sent as-is — it IS the prompt the
// image was generated with, and imgsite's EXIF merge is the source of truth
// anyway). The recovery path passes empty enhancement values: it never
// re-runs enhancement, so the enhanced prompt/reasoning live only in the
// image's EXIF and provenance is all meta can honestly contribute.
func buildUploadMeta(job *Job, finalPrompt, finalNegative, reasoning string) UploadMeta {
	return UploadMeta{
		JobID:          job.ID,
		OriginalPrompt: job.Input.Prompt,
		EnhancedPrompt: finalPrompt,
		NegativePrompt: finalNegative,
		Reasoning:      reasoning,
		LLMGenerated:   job.Input.LLMGenerated,
		WorkflowName:   job.Workflow,
		Network:        job.Input.Network,
		Channel:        job.Input.Channel,
		Nick:           job.Input.Nick,
	}
}

type JobQueue struct {
	cfgMu sync.RWMutex
	cfg   Config
	db    *sqlx.DB

	pending chan *Job
	results map[string]*Job
	mu      sync.RWMutex

	queuedOrder []*Job
	orderMu     sync.Mutex

	completedCount int
	failedCount    int
	statsMu        sync.Mutex

	wg             sync.WaitGroup
	cancel         context.CancelFunc
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	ready          atomic.Bool
}

func (q *JobQueue) getConfig() Config {
	q.cfgMu.RLock()
	defer q.cfgMu.RUnlock()
	return q.cfg
}

func (q *JobQueue) setConfig(cfg Config) {
	q.cfgMu.Lock()
	defer q.cfgMu.Unlock()
	q.cfg = cfg
}

func NewJobQueue(cfg Config, db *sqlx.DB) *JobQueue {
	ctx, cancel := context.WithCancel(context.Background())
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

	if db != nil {
		q.recoverJobs(ctx)
	} else {
		q.ready.Store(true)
	}

	for i := 0; i < cfg.Queue.MaxWorkers; i++ {
		q.wg.Add(1)
		go q.worker(ctx, i)
	}

	go q.cleanupLoop(ctx)

	return q
}

func (q *JobQueue) Stop() {
	q.shutdownCancel()
	q.cancel()
	q.wg.Wait()
}

func (q *JobQueue) IsReady() bool {
	return q.ready.Load()
}

func (q *JobQueue) Submit(jobType JobType, workflow string, input JobInput) (*Job, error) {
	cfg := q.getConfig()
	_, ok := cfg.Workflows[workflow]
	if !ok {
		return nil, fmt.Errorf("workflow %q not found", workflow)
	}

	job := &Job{
		ID:        uuid.New().String()[:8],
		Type:      jobType,
		Status:    StatusQueued,
		Workflow:  workflow,
		Input:     input,
		CreatedAt: time.Now().UTC(),
		done:      make(chan struct{}),
		cancel:    make(chan struct{}),
	}

	if q.db != nil {
		if err := dbInsertJob(q.db, job); err != nil {
			return nil, fmt.Errorf("persisting job: %w", err)
		}
	}

	select {
	case q.pending <- job:
		q.mu.Lock()
		q.results[job.ID] = job
		q.mu.Unlock()

		q.orderMu.Lock()
		job.QueuedIndex = len(q.queuedOrder)
		q.queuedOrder = append(q.queuedOrder, job)
		q.orderMu.Unlock()

		loggerQueue.Info("job submitted",
			"job_id", job.ID,
			"type", jobType,
			"workflow", workflow,
		)

		return job, nil
	default:
		return nil, fmt.Errorf("queue is full (%d jobs pending)", cfg.Queue.MaxDepth)
	}
}

// Get returns a snapshot of the job's read-facing state, never the live
// *Job — terminal mutations are synchronized on q.mu and tool handlers read
// the returned value without any lock.
func (q *JobQueue) Get(jobID string) (JobSnapshot, bool) {
	q.mu.RLock()
	job, ok := q.results[jobID]
	var snap JobSnapshot
	if ok {
		snap = snapshotJob(job)
	}
	q.mu.RUnlock()

	if ok {
		return snap, true
	}

	if q.db != nil {
		dbJob, err := dbGetJob(q.db, jobID)
		if err != nil {
			return JobSnapshot{}, false
		}
		recovered := jobFromDBJob(dbJob)
		if recovered.Status == StatusCompleted {
			result, comfyImgs, err := buildJobResultFromDB(q.db, jobID)
			if err == nil && result != nil {
				recovered.Result = result
				_ = comfyImgs
			}
		}
		// recovered is a fresh object no other goroutine can reach, so no
		// lock is needed to copy it.
		return snapshotJob(recovered), true
	}

	return JobSnapshot{}, false
}

func (q *JobQueue) Cancel(jobID string) bool {
	// Snapshot the job and its status under the read lock; the rest of this
	// function mutates job terminal state and must be the only writer doing
	// so unlocked-free (processJob's defer and recoverRunningJob synchronize
	// the same fields on q.mu).
	q.mu.RLock()
	job, ok := q.results[jobID]
	status := JobStatus("")
	if ok {
		status = job.Status
	}
	q.mu.RUnlock()

	if !ok {
		return false
	}

	if isTerminalStatus(status) {
		return false
	}

	if status == StatusQueued {
		select {
		case job.cancel <- struct{}{}:
		default:
		}

		q.orderMu.Lock()
		for i, j := range q.queuedOrder {
			if j.ID == jobID {
				q.queuedOrder = append(q.queuedOrder[:i], q.queuedOrder[i+1:]...)
				break
			}
		}
		q.orderMu.Unlock()
	}

	if status == StatusRunning {
		q.mu.RLock()
		cancelFn := job.cancelCtx
		comfyPromptID := job.ComfyPromptID
		q.mu.RUnlock()
		if cancelFn != nil {
			cancelFn()
		}
		if comfyPromptID != "" {
			// Two calls because ComfyUI splits the states: /api/interrupt
			// only fires when this prompt is the one currently executing,
			// and /queue delete only removes pending items. A cancel must
			// cover both — with max_workers > 1 the cancelled job's prompt
			// is routinely still pending behind another worker's prompt.
			interruptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := interruptComfyPrompt(interruptCtx, q.getConfig(), comfyPromptID); err != nil {
				loggerQueue.Warn("failed to interrupt comfy prompt", "prompt_id", comfyPromptID, "job_id", jobID, "error", err)
			}
			cancel()
			deleteCtx, cancelDelete := context.WithTimeout(context.Background(), 5*time.Second)
			if err := deleteComfyQueuedPrompt(deleteCtx, q.getConfig(), comfyPromptID); err != nil {
				loggerQueue.Warn("failed to delete queued comfy prompt", "prompt_id", comfyPromptID, "job_id", jobID, "error", err)
			}
			cancelDelete()
		}
	}

	// First-write-wins against failJob/completeJob: whichever terminal
	// writer grabs the state first (here, via transitionTerminal's
	// re-validation under q.mu) wins in memory AND in the DB — the losers
	// skip their flips and their DB writes entirely.
	if !q.transitionTerminal(job, StatusCancelled, "", nil) {
		// Re-validation lost: the job completed or failed while we were
		// interrupting it — its terminal write won, in memory and the DB.
		return false
	}

	if q.db != nil {
		if err := dbCancelJob(q.db, jobID); err != nil {
			if errors.Is(err, errJobAlreadyTerminal) {
				loggerQueue.Warn("DB cancel skipped: job row already terminal (lost race to another terminal write)",
					"job_id", jobID, "error", err)
			} else {
				loggerQueue.Error("error cancelling job in DB", "job_id", jobID, "error", err)
			}
		}
	}

	return true
}

// WaitForJob blocks until the job reaches a terminal state, the timeout
// elapses, or the job is unknown (nil). It returns a snapshot, not the live
// *Job: on the timeout branch the job may still be mid-flight, and even on
// the done branch a uniform snapshot-under-lock keeps every reader safe.
func (q *JobQueue) WaitForJob(jobID string, timeout time.Duration) *JobSnapshot {
	q.mu.RLock()
	job, ok := q.results[jobID]
	q.mu.RUnlock()

	if !ok {
		loggerQueue.Info("WaitForJob: not in memory", "job_id", jobID)
		if q.db != nil {
			dbJob, err := dbGetJob(q.db, jobID)
			if err != nil {
				loggerQueue.Warn("WaitForJob: not in DB", "job_id", jobID, "error", err)
			} else {
				loggerQueue.Info("WaitForJob: found in DB", "job_id", jobID, "status", dbJob.Status)
				if isTerminalStatus(JobStatus(dbJob.Status)) {
					recovered := jobFromDBJob(dbJob)
					if JobStatus(dbJob.Status) == StatusCompleted {
						result, _, err := buildJobResultFromDB(q.db, jobID)
						if err == nil {
							recovered.Result = result
						}
					}
					// recovered is fresh and unshared — no lock needed.
					snap := snapshotJob(recovered)
					return &snap
				}
			}
		}
		return nil
	}

	q.mu.RLock()
	terminal := isTerminalStatus(job.Status)
	q.mu.RUnlock()
	if terminal {
		return q.snapshot(job)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// The select must watch the LIVE job's done channel (the close is the
	// wakeup signal); only the returned value is a snapshot.
	select {
	case <-job.done:
	case <-timer.C:
	}
	return q.snapshot(job)
}

// snapshot copies job's read-facing fields under q.mu.RLock.
func (q *JobQueue) snapshot(job *Job) *JobSnapshot {
	q.mu.RLock()
	defer q.mu.RUnlock()
	snap := snapshotJob(job)
	return &snap
}

// ListJobs returns snapshots taken under q.mu — callers read the returned
// values without holding any lock.
func (q *JobQueue) ListJobs(statusFilter string, limit int) []JobSnapshot {
	q.mu.RLock()
	defer q.mu.RUnlock()

	jobs := make([]JobSnapshot, 0, len(q.results))
	for _, job := range q.results {
		if statusFilter != "" && string(job.Status) != statusFilter {
			continue
		}
		jobs = append(jobs, snapshotJob(job))
	}

	if len(jobs) > limit {
		jobs = jobs[:limit]
	}

	return jobs
}

type QueueStatusResult struct {
	Queued      int
	Running     int
	Completed   int
	Failed      int
	MaxWorkers  int
	MaxDepth    int
	RunningJobs []QueueJobSummary
	QueuedJobs  []QueueJobSummary
}

type QueueJobSummary struct {
	JobID          string
	Workflow       string
	Position       int
	ElapsedSeconds int
	ETASeconds     *int
}

func (q *JobQueue) Status() QueueStatusResult {
	q.mu.RLock()
	defer q.mu.RUnlock()

	cfg := q.getConfig()
	now := time.Now().UTC()
	result := QueueStatusResult{
		MaxWorkers: cfg.Queue.MaxWorkers,
		MaxDepth:   cfg.Queue.MaxDepth,
	}

	runningJobs := make([]*Job, 0)
	queuedJobs := make([]*Job, 0)

	for _, job := range q.results {
		switch job.Status {
		case StatusQueued:
			result.Queued++
			queuedJobs = append(queuedJobs, job)
		case StatusRunning:
			result.Running++
			runningJobs = append(runningJobs, job)
		case StatusCompleted:
			result.Completed++
		case StatusFailed:
			result.Failed++
		}
	}

	for _, job := range runningJobs {
		elapsed := int(now.Sub(*job.StartedAt).Seconds())
		eta := q.calcRemainingETA(job, now)
		result.RunningJobs = append(result.RunningJobs, QueueJobSummary{
			JobID:          job.ID,
			Workflow:       job.Workflow,
			ElapsedSeconds: elapsed,
			ETASeconds:     eta,
		})
	}

	for i, job := range queuedJobs {
		eta := q.calcQueuedETA(job, i, queuedJobs, runningJobs, now)
		result.QueuedJobs = append(result.QueuedJobs, QueueJobSummary{
			JobID:      job.ID,
			Workflow:   job.Workflow,
			Position:   i + 1,
			ETASeconds: eta,
		})
	}

	return result
}

func (q *JobQueue) calcRemainingETA(job *Job, now time.Time) *int {
	cfg := q.getConfig()
	wc := cfg.Workflows[job.Workflow]
	if wc.TypicalTime == 0 || job.StartedAt == nil {
		return nil
	}
	remaining := wc.TypicalTime - now.Sub(*job.StartedAt)
	if remaining < 0 {
		remaining = 0
	}
	secs := int(remaining.Seconds())
	return &secs
}

func (q *JobQueue) calcQueuedETA(job *Job, position int, queuedJobs []*Job, runningJobs []*Job, now time.Time) *int {
	cfg := q.getConfig()
	wc := cfg.Workflows[job.Workflow]

	runningRemaining := time.Duration(0)
	for _, rj := range runningJobs {
		rwc := cfg.Workflows[rj.Workflow]
		if rwc.TypicalTime > 0 && rj.StartedAt != nil {
			remaining := rwc.TypicalTime - now.Sub(*rj.StartedAt)
			if remaining > 0 {
				runningRemaining += remaining
			}
		}
	}

	aheadTime := time.Duration(0)
	for i := 0; i < position; i++ {
		qwc := cfg.Workflows[queuedJobs[i].Workflow]
		if qwc.TypicalTime > 0 {
			aheadTime += qwc.TypicalTime
		}
	}

	total := runningRemaining + aheadTime
	if total == 0 && wc.TypicalTime == 0 {
		return nil
	}

	secs := int(total.Seconds())
	if secs < 0 {
		secs = 0
	}
	return &secs
}

func (q *JobQueue) worker(ctx context.Context, id int) {
	defer q.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case job := <-q.pending:
			select {
			case <-job.cancel:
				continue
			default:
			}

			q.orderMu.Lock()
			for i, j := range q.queuedOrder {
				if j.ID == job.ID {
					q.queuedOrder = append(q.queuedOrder[:i], q.queuedOrder[i+1:]...)
					break
				}
			}
			q.orderMu.Unlock()

			q.processJob(ctx, job)
		}
	}
}

func (q *JobQueue) processJob(ctx context.Context, job *Job) {
	jobCtx, cancel := context.WithCancel(q.shutdownCtx)
	q.mu.Lock()
	job.cancelCtx = cancel
	q.mu.Unlock()
	defer cancel()

	cfg := q.getConfig()

	now := time.Now().UTC()
	q.mu.Lock()
	if job.Status != StatusQueued {
		// Cancelled between the worker's dequeue check and here — Cancel()'s
		// channel send is non-blocking and is dropped unless the worker is
		// parked exactly at its check. Cancel already performed the full
		// terminal transition; flipping to running would resurrect the
		// user's cancel in memory, and the (now fenced) dbUpdateJobRunning
		// below would resurrect it in the DB too.
		q.mu.Unlock()
		loggerQueue.Info("job cancelled before start, aborting", "job_id", job.ID)
		return
	}
	job.Status = StatusRunning
	job.StartedAt = &now
	q.mu.Unlock()

	if q.db != nil {
		if err := dbUpdateJobRunning(q.db, job.ID); err != nil {
			if errors.Is(err, errJobAlreadyTerminal) {
				// Cancel's DB write won even though memory still said queued
				// when we checked (the memory flip and the DB write are only
				// loosely ordered). Memory is cancelled by now — abort.
				loggerQueue.Warn("job cancelled concurrently in DB before start, aborting",
					"job_id", job.ID)
				return
			}
			loggerQueue.Error("error updating job to running in DB", "job_id", job.ID, "error", err)
		}
	}

	defer func() {
		// Terminal bookkeeping (CompletedAt, done close, stats) happens
		// inside transitionTerminal — reached via failJob/completeJob on the
		// error/success paths, or by Cancel winning a race. What remains
		// here is the shutdown case: the job is still running because jobCtx
		// was cancelled by q.Stop() rather than by a terminal writer — close
		// done so waiters wake; recovery re-runs the job on the next start.
		q.mu.Lock()
		defer q.mu.Unlock()

		if isTerminalStatus(job.Status) {
			return
		}
		job.CompletedAt = ptrTime(time.Now().UTC())
		job.closeOnce.Do(func() { close(job.done) })
	}()

	prompt := job.Input.Prompt
	negativePrompt := job.Input.NegativePrompt
	// enhancementReasoning carries the enhancement LLM's reasoning summary
	// (Responses API path) into the workflow's prompt note node. Plumbed
	// from the enhancePrompt result below; "" for plain generate jobs.
	enhancementReasoning := ""
	// firstPassNSFW is the safety first pass riding the enhancement call:
	// true means sexual content was flagged (→ safety unsafe, vet call
	// skipped); false/absent is NO signal — it never asserts safety. Plain
	// generate jobs have no first pass and go straight to the vet.
	firstPassNSFW := false

	loggerQueue.Info("processing job",
		"job_id", job.ID,
		"type", job.Type,
		"workflow", job.Workflow,
	)

	if job.Type == JobTypeEnhanceGenerate {
		enhancementName := job.Input.Enhancement
		if enhancementName == "" {
			enhancementName = "default"
		}

		// Per-workflow enhancement instructions live in the workflow file
		// (dave_enhancement_instructions node) so steering travels with the
		// workflow itself. Restart recovery re-runs enhancement from scratch
		// and re-reads the file, so nothing needs persisting;
		// prepareComfyWorkflow strips the node before submit.
		workflowInstructions := workflowEnhancementInstructions(cfg, job.Workflow)

		result, err := enhancePrompt(jobCtx, cfg, enhancementName, job.Input.Prompt, workflowInstructions)
		if err != nil {
			if jobCtx.Err() != nil {
				return
			}
			q.failJob(job, fmt.Sprintf("prompt enhancement failed: %v", err))
			return
		}
		loggerQueue.Debug("prompt enhanced",
			"job_id", job.ID,
			"enhanced_prompt", result.EnhancedPrompt,
			"negative_prompt", result.NegativePrompt,
		)
		prompt = result.EnhancedPrompt
		enhancementReasoning = result.Reasoning
		firstPassNSFW = result.NSFW
		if negativePrompt == "" {
			negativePrompt = result.NegativePrompt
		}
	}

	// Safety classification (safe-site split), started BEFORE the workflow
	// build/submit so the vet's LLM latency overlaps the 10-60s generation
	// instead of delaying it: nsfw:true short-circuits to unsafe with no
	// vet call, skip_networks jobs are not classified at all, and the vet
	// itself never blocks generation — its verdict is awaited after the
	// monitor completes. A job that fails or is cancelled earlier simply
	// never waits: the goroutine terminates with the job context below.
	safetyVet := startSafetyVet(jobCtx, cfg, job.Input.Network, firstPassNSFW, job.Input.Prompt, prompt)

	promptNote, err := buildPromptNote(job, enhancementReasoning)
	if err != nil {
		q.failJob(job, fmt.Sprintf("workflow preparation failed: %v", err))
		return
	}

	workflow, err := prepareComfyWorkflow(cfg, job.Workflow, prompt, negativePrompt, job.Input.Seed, promptNote)
	if err != nil {
		q.failJob(job, fmt.Sprintf("workflow preparation failed: %v", err))
		return
	}

	loggerQueue.Info("submitting to comfyui",
		"job_id", job.ID,
		"workflow", job.Workflow,
		"prompt", prompt,
		"negative_prompt", negativePrompt,
	)

	promptID, err := submitComfyPrompt(jobCtx, cfg, job.Workflow, workflow, job.ID)
	if err != nil {
		if jobCtx.Err() != nil {
			return
		}
		q.failJob(job, fmt.Sprintf("prompt submission failed: %v", err))
		return
	}

	q.mu.Lock()
	job.ComfyPromptID = promptID
	q.mu.Unlock()
	loggerQueue.Info("comfyui prompt accepted",
		"job_id", job.ID,
		"prompt_id", promptID,
	)

	if q.db != nil {
		if err := dbUpdateJobComfyPromptID(q.db, job.ID, promptID); err != nil {
			loggerQueue.Error("error saving comfy_prompt_id", "job_id", job.ID, "error", err)
		}
	}

	comfyResult, err := monitorComfyGeneration(jobCtx, cfg, job.Workflow, promptID, job.ID)
	if err != nil {
		// jobCtx cancellation means Cancel() is flipping this job to
		// cancelled (it cancels the context before setting the status);
		// reporting a failure in that window would leave the job marked
		// failed with a bogus error.
		q.mu.RLock()
		cancelled := job.Status == StatusCancelled
		q.mu.RUnlock()
		if cancelled || errors.Is(err, context.Canceled) {
			return
		}
		q.failJob(job, fmt.Sprintf("generation failed: %v", err))
		return
	}

	outputFormat := job.Input.OutputFormat
	if outputFormat == "" {
		outputFormat = "url"
	}

	loggerQueue.Info("generation complete",
		"job_id", job.ID,
		"prompt_id", promptID,
		"images", len(comfyResult.Images),
		"output_format", outputFormat,
	)

	jobResult := &JobResult{}
	var comfyImgs []ComfyImage
	// jobSafety is the resolved safety verdict, awaited now that generation
	// has finished — by upload time it is always computed, so persistence
	// (jobs.safety) and the EXIF note rewrite can consume it downstream.
	// Normally the vet finished long ago (it overlapped generation); the
	// worst case is its remainder. Failures already degraded to "unknown"
	// inside runSafetyVet, and skipped networks yield the empty unvetted
	// marker.
	jobSafety := safetyVet.wait()
	loggerQueue.Info("safety verdict resolved", "job_id", job.ID, "safety", jobSafety)
	// Persist the moment it resolves — before the upload loop — so a crash
	// mid-upload still leaves recovery with the verdict in hand.
	q.persistJobSafety(job.ID, jobSafety)
	uploadMeta := buildUploadMeta(job, prompt, negativePrompt, enhancementReasoning)
	for i, img := range comfyResult.Images {
		imgData := ImageData{
			MIMEType: guessMIMEType(img.Filename, "image/png"),
		}

		if i < len(comfyResult.ComfyImages) {
			comfyImgs = append(comfyImgs, comfyResult.ComfyImages[i])
		}

		switch outputFormat {
		case "url":
			url, err := uploadImage(cfg, img.Data, img.Filename, uploadMeta)
			if err != nil {
				q.failJob(job, fmt.Sprintf("upload failed: %v", err))
				return
			}
			imgData.URL = url
		case "base64":
			imgData.Base64 = encodeBase64(img.Data)
		case "both":
			url, err := uploadImage(cfg, img.Data, img.Filename, uploadMeta)
			if err != nil {
				q.failJob(job, fmt.Sprintf("upload failed: %v", err))
				return
			}
			imgData.URL = url
			imgData.Base64 = encodeBase64(img.Data)
		}

		jobResult.Images = append(jobResult.Images, imgData)
	}

	q.completeJob(job, jobResult, comfyImgs)
}

// transitionTerminal flips a job to the given terminal state with
// first-write-wins semantics: if another writer already reached a terminal
// state, nothing changes and it returns false. On a win it performs the
// complete terminal bookkeeping exactly once — status, error/result
// payloads, CompletedAt, and the done-channel close that wakes waiters —
// all under q.mu, then bumps the queue stats. DB persistence stays with
// the callers (failJob/completeJob/Cancel), which skip it when they lose,
// so the same transition wins in memory and in the DB.
func (q *JobQueue) transitionTerminal(job *Job, status JobStatus, errMsg string, result *JobResult) bool {
	q.mu.Lock()
	if isTerminalStatus(job.Status) {
		q.mu.Unlock()
		return false
	}
	now := time.Now().UTC()
	job.Status = status
	if errMsg != "" {
		job.Error = errMsg
	}
	if result != nil {
		job.Result = result
	}
	job.CompletedAt = &now
	job.closeOnce.Do(func() { close(job.done) })
	q.mu.Unlock()

	switch status {
	case StatusCompleted:
		q.statsMu.Lock()
		q.completedCount++
		q.statsMu.Unlock()
	case StatusFailed:
		q.statsMu.Lock()
		q.failedCount++
		q.statsMu.Unlock()
	}
	return true
}

// persistJobSafety stamps a resolved verdict onto the jobs row so restart
// recovery never re-vets. The unvetted marker ("") is deliberately NOT
// written: the column's empty default IS that marker, kept distinct from
// "unknown" (vetted but unresolved), which recovery must not re-vet either.
// Failures only log — a lost verdict degrades to a re-vet on recovery,
// never to a failed job.
func (q *JobQueue) persistJobSafety(jobID, safety string) {
	if safety == safetyVerdictUnvetted || q.db == nil {
		return
	}
	if err := dbUpdateJobSafety(q.db, jobID, safety); err != nil {
		loggerQueue.Error("error persisting job safety verdict", "job_id", jobID, "error", err)
	}
}

// completeJob marks a job terminally completed and persists the result.
// Like failJob it is first-write-wins: a job cancelled (or failed)
// concurrently stays in that state, in memory and in the DB.
func (q *JobQueue) completeJob(job *Job, result *JobResult, comfyImages []ComfyImage) bool {
	if !q.transitionTerminal(job, StatusCompleted, "", result) {
		loggerQueue.Info("complete skipped: job already terminal (first terminal write wins)",
			"job_id", job.ID)
		return false
	}
	if q.db != nil {
		if err := dbCompleteJob(q.db, job.ID, result, comfyImages); err != nil {
			if errors.Is(err, errJobAlreadyTerminal) {
				loggerQueue.Warn("DB complete skipped: job row already terminal (lost race to another terminal write)",
					"job_id", job.ID, "error", err)
			} else {
				loggerQueue.Error("error completing job in DB", "job_id", job.ID, "error", err)
			}
		}
	}
	return true
}

// failJob marks a job terminally failed. Callers must NOT hold q.mu —
// transitionTerminal takes the lock itself. First-write-wins: a job that
// was cancelled (or completed) concurrently keeps that state — the losing
// failure is dropped entirely, including its DB write, so memory and the
// DB row can never disagree about which transition won. (Pre-fix this
// overwrote memory to failed while the fenced DB write lost — a restart
// would resurface the job with the wrong terminal status.)
func (q *JobQueue) failJob(job *Job, errMsg string) {
	if !q.transitionTerminal(job, StatusFailed, errMsg, nil) {
		loggerQueue.Info("fail skipped: job already terminal (first terminal write wins)",
			"job_id", job.ID, "intended_error", errMsg)
		return
	}
	if q.db != nil {
		if err := dbFailJob(q.db, job.ID, errMsg); err != nil {
			if errors.Is(err, errJobAlreadyTerminal) {
				loggerQueue.Warn("DB fail skipped: job row already terminal (lost race to another terminal write)",
					"job_id", job.ID, "error", err)
			} else {
				loggerQueue.Error("error failing job in DB", "job_id", job.ID, "error", err)
			}
		}
	}
}

func (q *JobQueue) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.cleanup()
		}
	}
}

func (q *JobQueue) cleanup() {
	q.mu.Lock()
	defer q.mu.Unlock()

	cfg := q.getConfig()
	now := time.Now()
	for id, job := range q.results {
		if job.CompletedAt != nil && now.Sub(*job.CompletedAt) > cfg.Queue.ResultTTL {
			delete(q.results, id)
		}
	}

	if q.db != nil {
		if _, err := dbCleanupExpiredJobs(q.db, cfg.Queue.ResultTTL); err != nil {
			loggerQueue.Error("error cleaning up expired jobs from DB", "error", err)
		}
	}
}

func (q *JobQueue) recoverJobs(ctx context.Context) {
	defer func() {
		q.ready.Store(true)
		loggerQueue.Info("server recovery finished, ready=true")
	}()

	if q.db == nil {
		loggerQueue.Info("no database configured, skipping job recovery")
		return
	}

	recoverable, err := dbLoadRecoverableJobs(q.db)
	if err != nil {
		loggerQueue.Error("error loading recoverable jobs", "error", err)
		return
	}
	loggerQueue.Info("found recoverable jobs in database", "count", len(recoverable))

	for _, dbj := range recoverable {
		comfyID := ptrStr(dbj.ComfyPromptID)
		loggerQueue.Info("recovering job", "job_id", dbj.JobID, "status", dbj.Status, "comfy_prompt_id", comfyID)
		job := jobFromDBJob(&dbj)
		job.done = make(chan struct{})
		job.cancel = make(chan struct{})

		q.mu.Lock()
		q.results[job.ID] = job
		status := job.Status
		q.mu.Unlock()

		switch status {
		case StatusQueued:
			loggerQueue.Info("recovering queued job", "job_id", job.ID)
			select {
			case q.pending <- job:
				q.orderMu.Lock()
				job.QueuedIndex = len(q.queuedOrder)
				q.queuedOrder = append(q.queuedOrder, job)
				q.orderMu.Unlock()
			default:
				loggerQueue.Warn("queue full during recovery, dropping job", "job_id", job.ID)
				q.failJob(job, "queue full during recovery")
			}

		case StatusRunning:
			if comfyID != "" {
				loggerQueue.Info("recovering running job with comfy_prompt_id", "job_id", job.ID, "comfy_prompt_id", comfyID)
				q.wg.Add(1)
				go q.recoverRunningJob(ctx, job, comfyID)
			} else {
				loggerQueue.Info("recovering running job without comfy_prompt_id, re-queueing", "job_id", job.ID)
				q.mu.Lock()
				job.Status = StatusQueued
				q.mu.Unlock()
				if q.db != nil {
					if err := dbUpdateJobStatus(q.db, job.ID, StatusQueued); err != nil {
						loggerQueue.Error("error re-queueing job in DB", "job_id", job.ID, "error", err)
					}
				}
				select {
				case q.pending <- job:
				default:
					loggerQueue.Warn("queue full during recovery, dropping job", "job_id", job.ID)
					q.failJob(job, "queue full during recovery")
				}
				q.orderMu.Lock()
				job.QueuedIndex = len(q.queuedOrder)
				q.queuedOrder = append(q.queuedOrder, job)
				q.orderMu.Unlock()
			}
		}
	}

	completed, err := dbLoadRecentCompletedJobs(q.db, q.getConfig().Queue.ResultTTL)
	if err != nil {
		loggerQueue.Error("error loading recent completed jobs", "error", err)
		return
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	for _, dbj := range completed {
		if _, exists := q.results[dbj.JobID]; exists {
			continue
		}
		job := jobFromDBJob(&dbj)
		result, _, err := buildJobResultFromDB(q.db, dbj.JobID)
		if err == nil {
			job.Result = result
		}
		q.results[job.ID] = job
	}

	queuedCount := 0
	runningCount := 0
	for _, dbj := range recoverable {
		if JobStatus(dbj.Status) == StatusQueued {
			queuedCount++
		} else if JobStatus(dbj.Status) == StatusRunning {
			runningCount++
		}
	}

	loggerQueue.Info("recovery complete", "queued", queuedCount, "running", runningCount, "completed", len(completed))
}

func (q *JobQueue) recoverRunningJob(_ context.Context, job *Job, comfyPromptID string) {
	defer q.wg.Done()

	cfg := q.getConfig()
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), time.Duration(cfg.Comfy.Timeout)*time.Second)
	defer recoverCancel()

	// Wire the recovery context into the job so Cancel() can abort the
	// resume monitor promptly — without this, a cancelled job's recovery
	// ground on for the full Comfy.Timeout before failing (and its failJob
	// then fought the cancel). Set under q.mu like processJob does.
	q.mu.Lock()
	job.cancelCtx = recoverCancel
	q.mu.Unlock()

	comfyResult, err := resumeComfyGeneration(recoverCtx, cfg, job.Workflow, comfyPromptID, job.ID)
	if err != nil {
		// A Cancel() that raced us cancels recoverCtx (wired above) — the
		// error is a symptom of the cancellation, not a real failure, and
		// failJob would fight Cancel's own terminal write (mirrors the
		// cancelled-check in processJob's monitor path). Either check alone
		// can lose the race window, so check both.
		q.mu.RLock()
		cancelled := job.Status == StatusCancelled
		q.mu.RUnlock()
		if cancelled || errors.Is(err, context.Canceled) {
			loggerQueue.Info("recovery aborted by cancel", "job_id", job.ID, "error", err)
			return
		}
		loggerQueue.Error("failed to recover job", "job_id", job.ID, "error", err)
		q.failJob(job, fmt.Sprintf("recovery failed: %v", err))
		return
	}

	outputFormat := job.Input.OutputFormat
	if outputFormat == "" {
		outputFormat = "url"
	}

	// Safety verdict on the recovery path (same jobSafety shape processJob
	// produces for the EXIF rewrite / upload meta): a verdict persisted
	// before the crash is reused as-is — never re-vetted, an "unknown" as
	// final as a "safe". An empty column means the crash happened before
	// resolution, so the vet re-runs here: original prompt from the job
	// input, enhanced prompt recovered from the completed image's embedded
	// workflow (the only surviving copy — recovery never re-runs
	// enhancement). skip_networks applies exactly as on the live path (a
	// Libera job must not be classified by recovery either), and the
	// resolved verdict persists so a second restart never re-vets. The vet
	// rides the recovery context, so a Cancel aborts it too.
	jobSafety := job.Safety
	if jobSafety == safetyVerdictUnvetted {
		enhanced := job.Input.Prompt
		if len(comfyResult.Images) > 0 {
			if fromEXIF, ok := enhancedPromptFromImage(cfg, job.Workflow, comfyResult.Images[0].Data); ok {
				enhanced = fromEXIF
			} else {
				loggerQueue.Warn("recovery could not extract the enhanced prompt from the image; vetting the original prompt only",
					"job_id", job.ID)
			}
		}
		jobSafety = startSafetyVet(recoverCtx, cfg, job.Input.Network, false, job.Input.Prompt, enhanced).wait()
		q.persistJobSafety(job.ID, jobSafety)
		loggerQueue.Info("safety verdict resolved during recovery", "job_id", job.ID, "safety", jobSafety)
	}

	jobResult := &JobResult{}
	var comfyImgs []ComfyImage
	// Thinner meta than processJob: recovery never re-runs enhancement, so
	// the enhanced prompt/negative/reasoning exist only inside the image's
	// EXIF (imgsite's merge policy prefers EXIF for those fields anyway —
	// see docs/image-site.md). Meta contributes provenance + the original
	// prompt; the empty enhancement args drop out via omitempty.
	uploadMeta := buildUploadMeta(job, "", "", "")
	for i, img := range comfyResult.Images {
		imgData := ImageData{
			MIMEType: guessMIMEType(img.Filename, "image/png"),
		}

		if i < len(comfyResult.ComfyImages) {
			comfyImgs = append(comfyImgs, comfyResult.ComfyImages[i])
		}

		switch outputFormat {
		case "url":
			url, err := uploadImage(cfg, img.Data, img.Filename, uploadMeta)
			if err != nil {
				q.failJob(job, fmt.Sprintf("upload failed during recovery: %v", err))
				return
			}
			imgData.URL = url
		case "base64":
			imgData.Base64 = encodeBase64(img.Data)
		case "both":
			url, err := uploadImage(cfg, img.Data, img.Filename, uploadMeta)
			if err != nil {
				q.failJob(job, fmt.Sprintf("upload failed during recovery: %v", err))
				return
			}
			imgData.URL = url
			imgData.Base64 = encodeBase64(img.Data)
		}

		jobResult.Images = append(jobResult.Images, imgData)
	}

	if q.completeJob(job, jobResult, comfyImgs) {
		loggerQueue.Info("successfully recovered job", "job_id", job.ID)
	}
}

func isTerminalStatus(status JobStatus) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
