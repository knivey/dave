package main

import (
	"context"
	"fmt"
	"log"
	"sync"
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
	CreatedAt     time.Time
	StartedAt     *time.Time
	CompletedAt   *time.Time
	QueuedIndex   int

	done      chan struct{}
	cancel    chan struct{}
	cancelCtx context.CancelFunc
	closeOnce sync.Once
}

type JobInput struct {
	Prompt         string
	NegativePrompt string
	Enhancement    string
	Seed           *int64
	OutputFormat   string
}

type JobResult struct {
	Images []ImageData `json:"images"`
}

type ImageData struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MIMEType string `json:"mime_type"`
}

type JobQueue struct {
	cfg Config
	db  *sqlx.DB

	pending chan *Job
	results map[string]*Job
	mu      sync.RWMutex

	queuedOrder []*Job
	orderMu     sync.Mutex

	completedCount int
	failedCount    int
	statsMu        sync.Mutex

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func NewJobQueue(cfg Config, db *sqlx.DB) *JobQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}

	if db != nil {
		q.recoverJobs(ctx)
	}

	for i := 0; i < cfg.Queue.MaxWorkers; i++ {
		q.wg.Add(1)
		go q.worker(ctx, i)
	}

	go q.cleanupLoop(ctx)

	return q
}

func (q *JobQueue) Stop() {
	q.cancel()
	q.wg.Wait()
}

func (q *JobQueue) Submit(jobType JobType, workflow string, input JobInput) (*Job, error) {
	_, ok := q.cfg.Workflows[workflow]
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

		return job, nil
	default:
		return nil, fmt.Errorf("queue is full (%d jobs pending)", q.cfg.Queue.MaxDepth)
	}
}

func (q *JobQueue) Get(jobID string) (*Job, bool) {
	q.mu.RLock()
	job, ok := q.results[jobID]
	q.mu.RUnlock()

	if ok {
		return job, true
	}

	if q.db != nil {
		dbJob, err := dbGetJob(q.db, jobID)
		if err != nil {
			return nil, false
		}
		recovered := jobFromDBJob(dbJob)
		if recovered.Status == StatusCompleted {
			result, comfyImgs, err := buildJobResultFromDB(q.db, jobID)
			if err == nil && result != nil {
				recovered.Result = result
				_ = comfyImgs
			}
		}
		return recovered, true
	}

	return nil, false
}

func (q *JobQueue) Cancel(jobID string) bool {
	q.mu.RLock()
	job, ok := q.results[jobID]
	q.mu.RUnlock()

	if !ok {
		return false
	}

	if isTerminalStatus(job.Status) {
		return false
	}

	if job.Status == StatusQueued {
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

	if job.Status == StatusRunning {
		if job.cancelCtx != nil {
			job.cancelCtx()
		}
		if job.ComfyPromptID != "" {
			interruptCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := interruptComfyPrompt(interruptCtx, q.cfg, job.ComfyPromptID); err != nil {
				log.Printf("warning: failed to interrupt comfy prompt %s for job %s: %v", job.ComfyPromptID, jobID, err)
			}
			cancel()
		}
	}

	now := time.Now().UTC()
	job.Status = StatusCancelled
	job.CompletedAt = &now
	job.closeOnce.Do(func() { close(job.done) })

	if q.db != nil {
		if err := dbCancelJob(q.db, jobID); err != nil {
			log.Printf("error cancelling job %s in DB: %v", jobID, err)
		}
	}

	return true
}

func (q *JobQueue) WaitForJob(jobID string, timeout time.Duration) *Job {
	q.mu.RLock()
	job, ok := q.results[jobID]
	q.mu.RUnlock()

	if !ok {
		if q.db != nil {
			dbJob, err := dbGetJob(q.db, jobID)
			if err == nil && isTerminalStatus(JobStatus(dbJob.Status)) {
				recovered := jobFromDBJob(dbJob)
				if JobStatus(dbJob.Status) == StatusCompleted {
					result, _, err := buildJobResultFromDB(q.db, jobID)
					if err == nil {
						recovered.Result = result
					}
				}
				return recovered
			}
		}
		return nil
	}

	if isTerminalStatus(job.Status) {
		return job
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-job.done:
		return job
	case <-timer.C:
		return job
	}
}

func (q *JobQueue) ListJobs(statusFilter string, limit int) []*Job {
	q.mu.RLock()
	defer q.mu.RUnlock()

	jobs := make([]*Job, 0, len(q.results))
	for _, job := range q.results {
		if statusFilter != "" && string(job.Status) != statusFilter {
			continue
		}
		jobs = append(jobs, job)
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

	now := time.Now().UTC()
	result := QueueStatusResult{
		MaxWorkers: q.cfg.Queue.MaxWorkers,
		MaxDepth:   q.cfg.Queue.MaxDepth,
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
	wc := q.cfg.Workflows[job.Workflow]
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
	wc := q.cfg.Workflows[job.Workflow]

	runningRemaining := time.Duration(0)
	for _, rj := range runningJobs {
		rwc := q.cfg.Workflows[rj.Workflow]
		if rwc.TypicalTime > 0 && rj.StartedAt != nil {
			remaining := rwc.TypicalTime - now.Sub(*rj.StartedAt)
			if remaining > 0 {
				runningRemaining += remaining
			}
		}
	}

	aheadTime := time.Duration(0)
	for i := 0; i < position; i++ {
		qwc := q.cfg.Workflows[queuedJobs[i].Workflow]
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
	jobCtx, cancel := context.WithCancel(ctx)
	job.cancelCtx = cancel
	defer cancel()

	now := time.Now().UTC()
	job.Status = StatusRunning
	job.StartedAt = &now

	if q.db != nil {
		if err := dbUpdateJobRunning(q.db, job.ID); err != nil {
			log.Printf("error updating job %s to running in DB: %v", job.ID, err)
		}
	}

	defer func() {
		if job.Status == StatusCancelled {
			return
		}
		job.CompletedAt = ptrTime(time.Now().UTC())
		job.closeOnce.Do(func() { close(job.done) })

		if job.Status == StatusFailed {
			q.statsMu.Lock()
			q.failedCount++
			q.statsMu.Unlock()
		} else if job.Status == StatusCompleted {
			q.statsMu.Lock()
			q.completedCount++
			q.statsMu.Unlock()
		}
	}()

	prompt := job.Input.Prompt
	negativePrompt := job.Input.NegativePrompt

	if job.Type == JobTypeEnhanceGenerate {
		enhancementName := job.Input.Enhancement
		if enhancementName == "" {
			enhancementName = "default"
		}

		result, err := enhancePrompt(jobCtx, q.cfg, enhancementName, job.Input.Prompt)
		if err != nil {
			if jobCtx.Err() != nil {
				return
			}
			q.failJob(job, fmt.Sprintf("prompt enhancement failed: %v", err))
			return
		}
		prompt = result.EnhancedPrompt
		if negativePrompt == "" {
			negativePrompt = result.NegativePrompt
		}
	}

	workflow, err := prepareComfyWorkflow(q.cfg, job.Workflow, prompt, negativePrompt, job.Input.Seed)
	if err != nil {
		q.failJob(job, fmt.Sprintf("workflow preparation failed: %v", err))
		return
	}

	promptID, err := submitComfyPrompt(jobCtx, q.cfg, job.Workflow, workflow)
	if err != nil {
		if jobCtx.Err() != nil {
			return
		}
		q.failJob(job, fmt.Sprintf("prompt submission failed: %v", err))
		return
	}

	job.ComfyPromptID = promptID

	if q.db != nil {
		if err := dbUpdateJobComfyPromptID(q.db, job.ID, promptID); err != nil {
			log.Printf("error saving comfy_prompt_id for job %s: %v", job.ID, err)
		}
	}

	comfyResult, err := monitorComfyGeneration(jobCtx, q.cfg, job.Workflow, promptID)
	if err != nil {
		if job.Status == StatusCancelled {
			return
		}
		q.failJob(job, fmt.Sprintf("generation failed: %v", err))
		return
	}

	outputFormat := job.Input.OutputFormat
	if outputFormat == "" {
		outputFormat = "url"
	}

	jobResult := &JobResult{}
	var comfyImgs []ComfyImage
	for i, img := range comfyResult.Images {
		imgData := ImageData{
			MIMEType: guessMIMEType(img.Filename, "image/png"),
		}

		if i < len(comfyResult.ComfyImages) {
			comfyImgs = append(comfyImgs, comfyResult.ComfyImages[i])
		}

		switch outputFormat {
		case "url":
			url, err := uploadImage(q.cfg, img.Data, img.Filename)
			if err != nil {
				q.failJob(job, fmt.Sprintf("upload failed: %v", err))
				return
			}
			imgData.URL = url
		case "base64":
			imgData.Base64 = encodeBase64(img.Data)
		case "both":
			url, err := uploadImage(q.cfg, img.Data, img.Filename)
			if err != nil {
				q.failJob(job, fmt.Sprintf("upload failed: %v", err))
				return
			}
			imgData.URL = url
			imgData.Base64 = encodeBase64(img.Data)
		}

		jobResult.Images = append(jobResult.Images, imgData)
	}

	job.Result = jobResult
	job.Status = StatusCompleted

	if q.db != nil {
		if err := dbCompleteJob(q.db, job.ID, jobResult, comfyImgs); err != nil {
			log.Printf("error completing job %s in DB: %v", job.ID, err)
		}
	}
}

func (q *JobQueue) failJob(job *Job, errMsg string) {
	job.Status = StatusFailed
	job.Error = errMsg
	if q.db != nil {
		if err := dbFailJob(q.db, job.ID, errMsg); err != nil {
			log.Printf("error failing job %s in DB: %v", job.ID, err)
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

	now := time.Now()
	for id, job := range q.results {
		if job.CompletedAt != nil && now.Sub(*job.CompletedAt) > q.cfg.Queue.ResultTTL {
			delete(q.results, id)
		}
	}

	if q.db != nil {
		if _, err := dbCleanupExpiredJobs(q.db, q.cfg.Queue.ResultTTL); err != nil {
			log.Printf("error cleaning up expired jobs from DB: %v", err)
		}
	}
}

func (q *JobQueue) recoverJobs(ctx context.Context) {
	recoverable, err := dbLoadRecoverableJobs(q.db)
	if err != nil {
		log.Printf("error loading recoverable jobs: %v", err)
		return
	}

	for _, dbj := range recoverable {
		job := jobFromDBJob(&dbj)
		job.done = make(chan struct{})
		job.cancel = make(chan struct{})

		q.mu.Lock()
		q.results[job.ID] = job
		q.mu.Unlock()

		switch job.Status {
		case StatusQueued:
			log.Printf("recovering queued job %s", job.ID)
			select {
			case q.pending <- job:
				q.orderMu.Lock()
				job.QueuedIndex = len(q.queuedOrder)
				q.queuedOrder = append(q.queuedOrder, job)
				q.orderMu.Unlock()
			default:
				log.Printf("queue full during recovery, dropping job %s", job.ID)
				q.mu.Lock()
				q.failJob(job, "queue full during recovery")
				job.CompletedAt = ptrTime(time.Now().UTC())
				close(job.done)
				q.mu.Unlock()
			}

		case StatusRunning:
			if dbj.ComfyPromptID != "" {
				log.Printf("recovering running job %s with comfy_prompt_id %s", job.ID, dbj.ComfyPromptID)
				q.wg.Add(1)
				go q.recoverRunningJob(ctx, job, dbj.ComfyPromptID)
			} else {
				log.Printf("recovering running job %s without comfy_prompt_id, re-queueing", job.ID)
				job.Status = StatusQueued
				if q.db != nil {
					if err := dbUpdateJobStatus(q.db, job.ID, StatusQueued); err != nil {
						log.Printf("error re-queueing job %s in DB: %v", job.ID, err)
					}
				}
				select {
				case q.pending <- job:
				default:
					log.Printf("queue full during recovery, dropping job %s", job.ID)
					q.mu.Lock()
					q.failJob(job, "queue full during recovery")
					job.CompletedAt = ptrTime(time.Now().UTC())
					close(job.done)
					q.mu.Unlock()
				}
				q.orderMu.Lock()
				job.QueuedIndex = len(q.queuedOrder)
				q.queuedOrder = append(q.queuedOrder, job)
				q.orderMu.Unlock()
			}
		}
	}

	completed, err := dbLoadRecentCompletedJobs(q.db, q.cfg.Queue.ResultTTL)
	if err != nil {
		log.Printf("error loading recent completed jobs: %v", err)
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

	log.Printf("recovery complete: %d queued, %d running, %d completed loaded",
		queuedCount, runningCount, len(completed))
}

func (q *JobQueue) recoverRunningJob(ctx context.Context, job *Job, comfyPromptID string) {
	defer q.wg.Done()

	comfyResult, err := resumeComfyGeneration(ctx, q.cfg, job.Workflow, comfyPromptID)
	if err != nil {
		log.Printf("failed to recover job %s: %v", job.ID, err)
		q.mu.Lock()
		q.failJob(job, fmt.Sprintf("recovery failed: %v", err))
		job.CompletedAt = ptrTime(time.Now().UTC())
		job.closeOnce.Do(func() { close(job.done) })
		q.statsMu.Lock()
		q.failedCount++
		q.statsMu.Unlock()
		q.mu.Unlock()
		return
	}

	outputFormat := job.Input.OutputFormat
	if outputFormat == "" {
		outputFormat = "url"
	}

	jobResult := &JobResult{}
	var comfyImgs []ComfyImage
	for i, img := range comfyResult.Images {
		imgData := ImageData{
			MIMEType: guessMIMEType(img.Filename, "image/png"),
		}

		if i < len(comfyResult.ComfyImages) {
			comfyImgs = append(comfyImgs, comfyResult.ComfyImages[i])
		}

		switch outputFormat {
		case "url":
			url, err := uploadImage(q.cfg, img.Data, img.Filename)
			if err != nil {
				q.mu.Lock()
				q.failJob(job, fmt.Sprintf("upload failed during recovery: %v", err))
				q.mu.Unlock()
				return
			}
			imgData.URL = url
		case "base64":
			imgData.Base64 = encodeBase64(img.Data)
		case "both":
			url, err := uploadImage(q.cfg, img.Data, img.Filename)
			if err != nil {
				q.mu.Lock()
				q.failJob(job, fmt.Sprintf("upload failed during recovery: %v", err))
				q.mu.Unlock()
				return
			}
			imgData.URL = url
			imgData.Base64 = encodeBase64(img.Data)
		}

		jobResult.Images = append(jobResult.Images, imgData)
	}

	q.mu.Lock()
	job.Result = jobResult
	job.Status = StatusCompleted
	job.CompletedAt = ptrTime(time.Now().UTC())
	job.closeOnce.Do(func() { close(job.done) })
	q.mu.Unlock()

	q.statsMu.Lock()
	q.completedCount++
	q.statsMu.Unlock()

	if q.db != nil {
		if err := dbCompleteJob(q.db, job.ID, jobResult, comfyImgs); err != nil {
			log.Printf("error completing recovered job %s in DB: %v", job.ID, err)
		}
	}

	log.Printf("successfully recovered job %s", job.ID)
}

func isTerminalStatus(status JobStatus) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
