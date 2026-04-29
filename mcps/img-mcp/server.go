package main

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newServer(cfg Config, name string, instructions string) *mcp.Server {
	return mcp.NewServer(
		&mcp.Implementation{
			Name:    name,
			Version: cfg.Server.Version,
		},
		&mcp.ServerOptions{
			Instructions: instructions,
		},
	)
}

func createSyncServer(cfg Config, handlers *ToolHandlers) *mcp.Server {
	server := newServer(cfg, cfg.Server.Name,
		"MCP server for ComfyUI image generation (sync). Use generate_image or enhance_and_generate for one-step image generation (blocks until done).")

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_prompt",
		Description: "Enhance a raw image generation prompt using an LLM. Returns an enhanced positive prompt and a negative prompt. Uses the 'default' enhancement profile unless a different one is specified.",
	}, handlers.handleEnhancePrompt)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "generate_image",
		Description: "Generate an image using ComfyUI. Blocks until generation completes and returns the image result directly. This is the simplest way to generate an image. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleGenerateImage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_and_generate",
		Description: "Enhance a prompt via LLM and then generate an image using ComfyUI in one step. Blocks until both enhancement and generation complete and returns the image result directly. This is the simplest way to enhance and generate. Uses the 'default' enhancement profile unless a different one is specified. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleEnhanceAndGenerate)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload_image",
		Description: "Upload base64-encoded image data to the configured file host and return the URL.",
	}, handlers.handleUploadImage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_enhancements",
		Description: "List available prompt enhancement profiles with descriptions. Use the enhancement name in enhance_prompt or enhance_and_generate.",
	}, handlers.handleListEnhancements)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_workflows",
		Description: "List available ComfyUI workflows with descriptions and typical generation times. Use the workflow name in generate_image or enhance_and_generate.",
	}, handlers.handleListWorkflows)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List recent jobs with optional status filter.",
	}, handlers.handleListJobs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "queue_status",
		Description: "Get overview of the job queue: counts of queued, running, completed, failed jobs, plus per-job ETA estimates.",
	}, handlers.handleQueueStatus)

	return server
}

func createAsyncServer(cfg Config, handlers *ToolHandlers) *mcp.Server {
	server := newServer(cfg, cfg.Server.Name+"-async",
		"MCP server for ComfyUI image generation (async). Use generate_image_async or enhance_and_generate_async to queue jobs and get a job_id immediately, then check with job_status or wait_for_job.")

	mcp.AddTool(server, &mcp.Tool{
		Name:        "generate_image_async",
		Description: "Queue a ComfyUI image generation job and return a job_id immediately without waiting. Use job_status to check progress or wait_for_job to block until done. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleGenerateImageAsync)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_and_generate_async",
		Description: "Enhance a prompt via LLM and queue a ComfyUI image generation job in one step. Returns a job_id immediately without waiting. Uses the 'default' enhancement profile unless a different one is specified. Uses the default workflow if workflow is empty or 'default'. Use job_status to check progress or wait_for_job to block until done.",
	}, handlers.handleEnhanceAndGenerateAsync)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "wait_for_job",
		Description: "Block until a job completes, fails, or times out. Returns the full result including image URLs or base64 data.",
	}, handlers.handleWaitForJob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "job_status",
		Description: "Check the status and result of a job by its ID. Includes queue position and ETA when available.",
	}, handlers.handleJobStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List recent jobs with optional status filter.",
	}, handlers.handleListJobs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a queued job that has not yet started running.",
	}, handlers.handleCancelJob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_enhancements",
		Description: "List available prompt enhancement profiles with descriptions. Use the enhancement name in enhance_prompt or enhance_and_generate.",
	}, handlers.handleListEnhancements)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_workflows",
		Description: "List available ComfyUI workflows with descriptions and typical generation times. Use the workflow name in generate_image or enhance_and_generate.",
	}, handlers.handleListWorkflows)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "queue_status",
		Description: "Get overview of the job queue: counts of queued, running, completed, failed jobs, plus per-job ETA estimates.",
	}, handlers.handleQueueStatus)

	return server
}

func createFullServer(cfg Config, handlers *ToolHandlers) *mcp.Server {
	server := newServer(cfg, cfg.Server.Name,
		"MCP server for ComfyUI image generation and prompt enhancement. Use generate_image or enhance_and_generate for one-step image generation (blocks until done). Use generate_image_async or enhance_and_generate_async to queue jobs and get a job_id immediately, then check with job_status or wait_for_job.")

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_prompt",
		Description: "Enhance a raw image generation prompt using an LLM. Returns an enhanced positive prompt and a negative prompt. Uses the 'default' enhancement profile unless a different one is specified.",
	}, handlers.handleEnhancePrompt)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "generate_image",
		Description: "Generate an image using ComfyUI. Blocks until generation completes and returns the image result directly. This is the simplest way to generate an image. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleGenerateImage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "generate_image_async",
		Description: "Queue a ComfyUI image generation job and return a job_id immediately without waiting. Use job_status to check progress or wait_for_job to block until done. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleGenerateImageAsync)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_and_generate",
		Description: "Enhance a prompt via LLM and then generate an image using ComfyUI in one step. Blocks until both enhancement and generation complete and returns the image result directly. This is the simplest way to enhance and generate. Uses the 'default' enhancement profile unless a different one is specified. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleEnhanceAndGenerate)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_and_generate_async",
		Description: "Enhance a prompt via LLM and queue a ComfyUI image generation job in one step. Returns a job_id immediately without waiting. Uses the 'default' enhancement profile unless a different one is specified. Uses the default workflow if workflow is empty or 'default'. Use job_status to check progress or wait_for_job to block until done.",
	}, handlers.handleEnhanceAndGenerateAsync)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "wait_for_job",
		Description: "Block until a job completes, fails, or times out. Returns the full result including image URLs or base64 data.",
	}, handlers.handleWaitForJob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "job_status",
		Description: "Check the status and result of a job by its ID. Includes queue position and ETA when available.",
	}, handlers.handleJobStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List recent jobs with optional status filter.",
	}, handlers.handleListJobs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a queued job that has not yet started running.",
	}, handlers.handleCancelJob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_enhancements",
		Description: "List available prompt enhancement profiles with descriptions. Use the enhancement name in enhance_prompt or enhance_and_generate.",
	}, handlers.handleListEnhancements)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_workflows",
		Description: "List available ComfyUI workflows with descriptions and typical generation times. Use the workflow name in generate_image or enhance_and_generate.",
	}, handlers.handleListWorkflows)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "queue_status",
		Description: "Get overview of the job queue: counts of queued, running, completed, failed jobs, plus per-job ETA estimates.",
	}, handlers.handleQueueStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload_image",
		Description: "Upload base64-encoded image data to the configured file host and return the URL.",
	}, handlers.handleUploadImage)

	return server
}
