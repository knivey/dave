package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type EnhancePromptInput struct {
	Prompt      string `json:"prompt" jsonschema:"the raw prompt text to enhance"`
	Enhancement string `json:"enhancement,omitempty" jsonschema:"name of enhancement config to use (default: 'default')"`
}

type EnhancePromptOutput struct {
	EnhancedPrompt string `json:"enhanced_prompt"`
	NegativePrompt string `json:"negative_prompt"`
}

type GenerateImageAsyncInput struct {
	Prompt         string `json:"prompt" jsonschema:"the image generation prompt"`
	NegativePrompt string `json:"negative_prompt,omitempty" jsonschema:"optional negative prompt"`
	Workflow       string `json:"workflow,omitempty" jsonschema:"name of the workflow config to use (empty or 'default' uses the default workflow)"`
	Seed           *int64 `json:"seed,omitempty" jsonschema:"optional fixed seed for reproducibility"`
	OutputFormat   string `json:"output_format,omitempty" jsonschema:"output format: url (default), base64, or both"`
}

type GenerateImageAsyncOutput struct {
	JobID string `json:"job_id"`
}

type GenerateImageInput struct {
	Prompt         string `json:"prompt" jsonschema:"the image generation prompt"`
	NegativePrompt string `json:"negative_prompt,omitempty" jsonschema:"optional negative prompt"`
	Workflow       string `json:"workflow,omitempty" jsonschema:"name of the workflow config to use (empty or 'default' uses the default workflow)"`
	Seed           *int64 `json:"seed,omitempty" jsonschema:"optional fixed seed for reproducibility"`
	OutputFormat   string `json:"output_format,omitempty" jsonschema:"output format: url (default), base64, or both"`
	Timeout        int    `json:"timeout,omitempty" jsonschema:"max seconds to wait for generation (default: 300)"`
}

type GenerateImageOutput struct {
	Images []ImageData `json:"images"`
	Status string      `json:"status"`
	Error  string      `json:"error,omitempty"`
}

type EnhanceAndGenerateAsyncInput struct {
	Prompt       string `json:"prompt" jsonschema:"the raw prompt to enhance and generate"`
	Enhancement  string `json:"enhancement,omitempty" jsonschema:"name of enhancement config (default: from workflow or 'default')"`
	Workflow     string `json:"workflow,omitempty" jsonschema:"name of the workflow config to use (empty or 'default' uses the default workflow)"`
	OutputFormat string `json:"output_format,omitempty" jsonschema:"output format: url (default), base64, or both"`
}

type EnhanceAndGenerateAsyncOutput struct {
	JobID string `json:"job_id"`
}

type EnhanceAndGenerateInput struct {
	Prompt       string `json:"prompt" jsonschema:"the raw prompt to enhance and generate"`
	Enhancement  string `json:"enhancement,omitempty" jsonschema:"name of enhancement config (default: from workflow or 'default')"`
	Workflow     string `json:"workflow,omitempty" jsonschema:"name of the workflow config to use (empty or 'default' uses the default workflow)"`
	OutputFormat string `json:"output_format,omitempty" jsonschema:"output format: url (default), base64, or both"`
	Timeout      int    `json:"timeout,omitempty" jsonschema:"max seconds to wait for generation (default: 300)"`
}

type EnhanceAndGenerateOutput struct {
	Images []ImageData `json:"images"`
	Status string      `json:"status"`
	Error  string      `json:"error,omitempty"`
}

type WaitForJobInput struct {
	JobID   string `json:"job_id" jsonschema:"the job ID to wait for"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"max seconds to wait (default: 300)"`
}

type WaitForJobOutput struct {
	JobID  string     `json:"job_id"`
	Status string     `json:"status"`
	Result *JobResult `json:"result,omitempty"`
	Error  string     `json:"error,omitempty"`
}

type JobStatusInput struct {
	JobID string `json:"job_id" jsonschema:"the job ID to check"`
}

type JobStatusOutput struct {
	JobID       string     `json:"job_id"`
	Status      string     `json:"status"`
	Position    int        `json:"position,omitempty"`
	ETASeconds  *int       `json:"eta_seconds,omitempty"`
	CreatedAt   string     `json:"created_at"`
	StartedAt   string     `json:"started_at,omitempty"`
	CompletedAt string     `json:"completed_at,omitempty"`
	Result      *JobResult `json:"result,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type ListJobsInput struct {
	Status string `json:"status,omitempty" jsonschema:"filter by status: queued, running, completed, failed, cancelled"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max jobs to return (default: 20)"`
}

type ListJobsOutput struct {
	Jobs []JobStatusOutput `json:"jobs"`
}

type CancelJobInput struct {
	JobID string `json:"job_id" jsonschema:"the job ID to cancel"`
}

type CancelJobOutput struct {
	Cancelled bool `json:"cancelled"`
}

type ListEnhancementsInput struct{}

type EnhancementInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Model       string `json:"model"`
}

type ListEnhancementsOutput struct {
	Enhancements []EnhancementInfo `json:"enhancements"`
	Default      string            `json:"default"`
}

type ListWorkflowsInput struct{}

type WorkflowInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	TypicalTime string `json:"typical_time,omitempty"`
}

type ListWorkflowsOutput struct {
	Workflows []WorkflowInfo `json:"workflows"`
	Default   string         `json:"default"`
}

type QueueStatusInput struct{}

type QueueStatusOutput struct {
	Queued      int                  `json:"queued"`
	Running     int                  `json:"running"`
	Completed   int                  `json:"completed"`
	Failed      int                  `json:"failed"`
	MaxWorkers  int                  `json:"max_workers"`
	MaxDepth    int                  `json:"max_depth"`
	RunningJobs []QueueJobSummaryOut `json:"running_jobs,omitempty"`
	QueuedJobs  []QueueJobSummaryOut `json:"queued_jobs,omitempty"`
}

type QueueJobSummaryOut struct {
	JobID          string `json:"job_id"`
	Workflow       string `json:"workflow"`
	Position       int    `json:"position,omitempty"`
	ElapsedSeconds int    `json:"elapsed_seconds,omitempty"`
	ETASeconds     *int   `json:"eta_seconds,omitempty"`
}

type UploadImageInput struct {
	Data     string `json:"data" jsonschema:"base64-encoded image data"`
	Filename string `json:"filename" jsonschema:"filename for the uploaded image"`
	MIMEType string `json:"mime_type,omitempty" jsonschema:"MIME type of the image"`
}

type UploadImageOutput struct {
	URL string `json:"url"`
}

type ServerStatusInput struct{}

type ServerStatusOutput struct {
	Ready bool `json:"ready"`
}

type ToolHandlers struct {
	cfg   Config
	queue *JobQueue
}

func NewToolHandlers(cfg Config, queue *JobQueue) *ToolHandlers {
	return &ToolHandlers{cfg: cfg, queue: queue}
}

func (h *ToolHandlers) resolveWorkflow(name string) (string, error) {
	if name == "" || name == "default" {
		name = h.cfg.Comfy.DefaultWorkflow
	}
	if name == "" {
		return "", fmt.Errorf("no workflow specified and no default_workflow configured")
	}
	if _, ok := h.cfg.Workflows[name]; !ok {
		return "", fmt.Errorf("workflow %q not found", name)
	}
	return name, nil
}

func (h *ToolHandlers) handleEnhancePrompt(ctx context.Context, req *mcp.CallToolRequest, input EnhancePromptInput) (*mcp.CallToolResult, EnhancePromptOutput, error) {
	enhancementName := input.Enhancement
	if enhancementName == "" {
		enhancementName = "default"
	}

	result, err := enhancePrompt(ctx, h.cfg, enhancementName, input.Prompt)
	if err != nil {
		return nil, EnhancePromptOutput{}, err
	}

	return nil, EnhancePromptOutput{
		EnhancedPrompt: result.EnhancedPrompt,
		NegativePrompt: result.NegativePrompt,
	}, nil
}

func (h *ToolHandlers) handleGenerateImageAsync(ctx context.Context, req *mcp.CallToolRequest, input GenerateImageAsyncInput) (*mcp.CallToolResult, GenerateImageAsyncOutput, error) {
	workflow, err := h.resolveWorkflow(input.Workflow)
	if err != nil {
		return nil, GenerateImageAsyncOutput{}, err
	}
	job, err := h.queue.Submit(JobTypeGenerate, workflow, JobInput{
		Prompt:         input.Prompt,
		NegativePrompt: input.NegativePrompt,
		Seed:           input.Seed,
		OutputFormat:   input.OutputFormat,
	})
	if err != nil {
		return nil, GenerateImageAsyncOutput{}, err
	}

	return nil, GenerateImageAsyncOutput{JobID: job.ID}, nil
}

func (h *ToolHandlers) handleEnhanceAndGenerateAsync(ctx context.Context, req *mcp.CallToolRequest, input EnhanceAndGenerateAsyncInput) (*mcp.CallToolResult, EnhanceAndGenerateAsyncOutput, error) {
	workflow, err := h.resolveWorkflow(input.Workflow)
	if err != nil {
		return nil, EnhanceAndGenerateAsyncOutput{}, err
	}
	job, err := h.queue.Submit(JobTypeEnhanceGenerate, workflow, JobInput{
		Prompt:       input.Prompt,
		Enhancement:  input.Enhancement,
		OutputFormat: input.OutputFormat,
	})
	if err != nil {
		return nil, EnhanceAndGenerateAsyncOutput{}, err
	}

	return nil, EnhanceAndGenerateAsyncOutput{JobID: job.ID}, nil
}

func (h *ToolHandlers) handleGenerateImage(ctx context.Context, req *mcp.CallToolRequest, input GenerateImageInput) (*mcp.CallToolResult, GenerateImageOutput, error) {
	workflow, err := h.resolveWorkflow(input.Workflow)
	if err != nil {
		return nil, GenerateImageOutput{}, err
	}
	job, err := h.queue.Submit(JobTypeGenerate, workflow, JobInput{
		Prompt:         input.Prompt,
		NegativePrompt: input.NegativePrompt,
		Seed:           input.Seed,
		OutputFormat:   input.OutputFormat,
	})
	if err != nil {
		return nil, GenerateImageOutput{}, err
	}

	timeout := input.Timeout
	if timeout == 0 {
		timeout = 300
	}
	result := h.queue.WaitForJob(job.ID, time.Duration(timeout)*time.Second)
	if result == nil {
		return nil, GenerateImageOutput{}, fmt.Errorf("job %q not found", job.ID)
	}

	out := GenerateImageOutput{
		Status: string(result.Status),
		Error:  result.Error,
	}
	if result.Result != nil {
		out.Images = result.Result.Images
	}
	return nil, out, nil
}

func (h *ToolHandlers) handleEnhanceAndGenerate(ctx context.Context, req *mcp.CallToolRequest, input EnhanceAndGenerateInput) (*mcp.CallToolResult, EnhanceAndGenerateOutput, error) {
	workflow, err := h.resolveWorkflow(input.Workflow)
	if err != nil {
		return nil, EnhanceAndGenerateOutput{}, err
	}
	job, err := h.queue.Submit(JobTypeEnhanceGenerate, workflow, JobInput{
		Prompt:       input.Prompt,
		Enhancement:  input.Enhancement,
		OutputFormat: input.OutputFormat,
	})
	if err != nil {
		return nil, EnhanceAndGenerateOutput{}, err
	}

	timeout := input.Timeout
	if timeout == 0 {
		timeout = 300
	}
	result := h.queue.WaitForJob(job.ID, time.Duration(timeout)*time.Second)
	if result == nil {
		return nil, EnhanceAndGenerateOutput{}, fmt.Errorf("job %q not found", job.ID)
	}

	out := EnhanceAndGenerateOutput{
		Status: string(result.Status),
		Error:  result.Error,
	}
	if result.Result != nil {
		out.Images = result.Result.Images
	}
	return nil, out, nil
}

func (h *ToolHandlers) handleWaitForJob(ctx context.Context, req *mcp.CallToolRequest, input WaitForJobInput) (*mcp.CallToolResult, WaitForJobOutput, error) {
	timeout := input.Timeout
	if timeout == 0 {
		timeout = 300
	}

	job := h.queue.WaitForJob(input.JobID, time.Duration(timeout)*time.Second)
	if job == nil {
		return nil, WaitForJobOutput{}, fmt.Errorf("job %q not found", input.JobID)
	}

	status := string(job.Status)
	if status == string(StatusQueued) || status == string(StatusRunning) {
		status = "timeout"
	}

	return nil, WaitForJobOutput{
		JobID:  job.ID,
		Status: status,
		Result: job.Result,
		Error:  job.Error,
	}, nil
}

func (h *ToolHandlers) handleJobStatus(ctx context.Context, req *mcp.CallToolRequest, input JobStatusInput) (*mcp.CallToolResult, JobStatusOutput, error) {
	job, ok := h.queue.Get(input.JobID)
	if !ok {
		return nil, JobStatusOutput{}, fmt.Errorf("job %q not found", input.JobID)
	}

	out := jobToStatusOutput(job)

	if job.Status == StatusQueued {
		qs := h.queue.Status()
		for _, qj := range qs.QueuedJobs {
			if qj.JobID == job.ID {
				out.Position = qj.Position
				out.ETASeconds = qj.ETASeconds
				break
			}
		}
	}

	return nil, out, nil
}

func (h *ToolHandlers) handleListWorkflows(ctx context.Context, req *mcp.CallToolRequest, input ListWorkflowsInput) (*mcp.CallToolResult, ListWorkflowsOutput, error) {
	out := ListWorkflowsOutput{
		Default: h.cfg.Comfy.DefaultWorkflow,
	}
	for name, wc := range h.cfg.Workflows {
		info := WorkflowInfo{
			Name:        name,
			Description: wc.Description,
		}
		if wc.TypicalTime > 0 {
			info.TypicalTime = wc.TypicalTime.String()
		}
		out.Workflows = append(out.Workflows, info)
	}
	return nil, out, nil
}

func (h *ToolHandlers) handleListJobs(ctx context.Context, req *mcp.CallToolRequest, input ListJobsInput) (*mcp.CallToolResult, ListJobsOutput, error) {
	limit := input.Limit
	if limit == 0 {
		limit = 20
	}

	jobs := h.queue.ListJobs(input.Status, limit)
	out := ListJobsOutput{}
	for _, job := range jobs {
		out.Jobs = append(out.Jobs, jobToStatusOutput(job))
	}

	return nil, out, nil
}

func (h *ToolHandlers) handleCancelJob(ctx context.Context, req *mcp.CallToolRequest, input CancelJobInput) (*mcp.CallToolResult, CancelJobOutput, error) {
	cancelled := h.queue.Cancel(input.JobID)
	return nil, CancelJobOutput{Cancelled: cancelled}, nil
}

func (h *ToolHandlers) handleListEnhancements(ctx context.Context, req *mcp.CallToolRequest, input ListEnhancementsInput) (*mcp.CallToolResult, ListEnhancementsOutput, error) {
	out := ListEnhancementsOutput{
		Default: "default",
	}
	for name, ec := range h.cfg.Enhancements {
		out.Enhancements = append(out.Enhancements, EnhancementInfo{
			Name:        name,
			Description: ec.Description,
			Model:       ec.Model,
		})
	}
	return nil, out, nil
}

func (h *ToolHandlers) handleQueueStatus(ctx context.Context, req *mcp.CallToolRequest, input QueueStatusInput) (*mcp.CallToolResult, QueueStatusOutput, error) {
	qs := h.queue.Status()

	out := QueueStatusOutput{
		Queued:     qs.Queued,
		Running:    qs.Running,
		Completed:  qs.Completed,
		Failed:     qs.Failed,
		MaxWorkers: qs.MaxWorkers,
		MaxDepth:   qs.MaxDepth,
	}

	for _, rj := range qs.RunningJobs {
		out.RunningJobs = append(out.RunningJobs, QueueJobSummaryOut{
			JobID:          rj.JobID,
			Workflow:       rj.Workflow,
			ElapsedSeconds: rj.ElapsedSeconds,
			ETASeconds:     rj.ETASeconds,
		})
	}

	for _, qj := range qs.QueuedJobs {
		out.QueuedJobs = append(out.QueuedJobs, QueueJobSummaryOut{
			JobID:      qj.JobID,
			Workflow:   qj.Workflow,
			Position:   qj.Position,
			ETASeconds: qj.ETASeconds,
		})
	}

	return nil, out, nil
}

func (h *ToolHandlers) handleUploadImage(ctx context.Context, req *mcp.CallToolRequest, input UploadImageInput) (*mcp.CallToolResult, UploadImageOutput, error) {
	data, err := base64.StdEncoding.DecodeString(input.Data)
	if err != nil {
		return nil, UploadImageOutput{}, fmt.Errorf("decoding base64: %w", err)
	}

	url, err := uploadImage(h.cfg, data, input.Filename)
	if err != nil {
		return nil, UploadImageOutput{}, err
	}

	return nil, UploadImageOutput{URL: url}, nil
}

func (h *ToolHandlers) handleServerStatus(ctx context.Context, req *mcp.CallToolRequest, input ServerStatusInput) (*mcp.CallToolResult, ServerStatusOutput, error) {
	ready := h.queue.IsReady()
	loggerTools.Info("server_status tool called", "ready", ready)
	return nil, ServerStatusOutput{Ready: ready}, nil
}

func jobToStatusOutput(job *Job) JobStatusOutput {
	out := JobStatusOutput{
		JobID:     job.ID,
		Status:    string(job.Status),
		CreatedAt: job.CreatedAt.Format(time.RFC3339),
		Result:    job.Result,
		Error:     job.Error,
	}
	if job.StartedAt != nil {
		out.StartedAt = job.StartedAt.Format(time.RFC3339)
	}
	if job.CompletedAt != nil {
		out.CompletedAt = job.CompletedAt.Format(time.RFC3339)
	}
	return out
}
