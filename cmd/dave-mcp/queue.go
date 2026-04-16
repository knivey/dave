package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
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
	ID          string
	Type        JobType
	Status      JobStatus
	Workflow    string
	Input       JobInput
	Result      *JobResult
	Error       string
	CreatedAt   time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	QueuedIndex int

	done   chan struct{}
	cancel chan struct{}
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

func NewJobQueue(cfg Config) *JobQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &JobQueue{
		cfg:     cfg,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
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
	defer q.mu.RUnlock()
	job, ok := q.results[jobID]
	return job, ok
}

func (q *JobQueue) Cancel(jobID string) bool {
	q.mu.RLock()
	job, ok := q.results[jobID]
	q.mu.RUnlock()

	if !ok {
		return false
	}

	if job.Status != StatusQueued {
		return false
	}

	select {
	case job.cancel <- struct{}{}:
	default:
	}

	job.Status = StatusCancelled
	now := time.Now().UTC()
	job.CompletedAt = &now
	close(job.done)

	q.orderMu.Lock()
	for i, j := range q.queuedOrder {
		if j.ID == jobID {
			q.queuedOrder = append(q.queuedOrder[:i], q.queuedOrder[i+1:]...)
			break
		}
	}
	q.orderMu.Unlock()

	return true
}

func (q *JobQueue) WaitForJob(jobID string, timeout time.Duration) *Job {
	q.mu.RLock()
	job, ok := q.results[jobID]
	q.mu.RUnlock()

	if !ok {
		return nil
	}

	if job.Status == StatusCompleted || job.Status == StatusFailed || job.Status == StatusCancelled {
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

	runningETA := time.Duration(0)
	for _, job := range runningJobs {
		wc := q.cfg.Workflows[job.Workflow]
		if wc.TypicalTime > 0 && job.StartedAt != nil {
			remaining := wc.TypicalTime - now.Sub(*job.StartedAt)
			if remaining > 0 {
				runningETA += remaining
			}
		}
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
	now := time.Now().UTC()
	job.Status = StatusRunning
	job.StartedAt = &now

	defer func() {
		job.CompletedAt = ptrTime(time.Now().UTC())
		close(job.done)

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

		result, err := enhancePrompt(ctx, q.cfg, enhancementName, job.Input.Prompt)
		if err != nil {
			job.Status = StatusFailed
			job.Error = fmt.Sprintf("prompt enhancement failed: %v", err)
			return
		}
		prompt = result.EnhancedPrompt
		if negativePrompt == "" {
			negativePrompt = result.NegativePrompt
		}
	}

	comfyResult, err := submitComfyGeneration(ctx, q.cfg, job.Workflow, prompt, negativePrompt, job.Input.Seed)
	if err != nil {
		job.Status = StatusFailed
		job.Error = fmt.Sprintf("generation failed: %v", err)
		return
	}

	outputFormat := job.Input.OutputFormat
	if outputFormat == "" {
		outputFormat = "url"
	}

	jobResult := &JobResult{}
	for _, img := range comfyResult.Images {
		imgData := ImageData{
			MIMEType: guessMIMEType(img.Filename, "image/png"),
		}

		switch outputFormat {
		case "url":
			url, err := uploadImage(q.cfg, img.Data, img.Filename)
			if err != nil {
				job.Status = StatusFailed
				job.Error = fmt.Sprintf("upload failed: %v", err)
				return
			}
			imgData.URL = url
		case "base64":
			imgData.Base64 = encodeBase64(img.Data)
		case "both":
			url, err := uploadImage(q.cfg, img.Data, img.Filename)
			if err != nil {
				job.Status = StatusFailed
				job.Error = fmt.Sprintf("upload failed: %v", err)
				return
			}
			imgData.URL = url
			imgData.Base64 = encodeBase64(img.Data)
		}

		jobResult.Images = append(jobResult.Images, imgData)
	}

	job.Result = jobResult
	job.Status = StatusCompleted
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
}

func ptrTime(t time.Time) *time.Time {
	return &t
}
