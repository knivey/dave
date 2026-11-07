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

// editToolBaseDescription is the shared teaching text for the edit tools:
// input ordering (FIRST image is the edit target), the ref grammar
// (usr_/web_ conversation refs, gen_ generation refs), the per-workflow
// prompt grammar pointer, and the default-edit-workflow rule.
// The flavor tail (blocking vs queued) and the [safety] edit_policy suffix
// are appended by editToolDescription.
const editToolBaseDescription = "Edit images using an image-editing workflow. input_images is ORDERED: the FIRST image is the edit target — the edit applies to it and the output dimensions follow it; any additional images are references. Pass image references: the ref ids shown next to images in the conversation (usr_/web_) or gen_ refs from generation results. Some workflows expect images mentioned in the prompt (e.g. as <image1>, <image2>) — check the workflow's description with list_workflows. Uses the default edit workflow if workflow is empty or 'default'."

// editToolDescription builds the edit tools' advertised description: base
// teaching text + the flavor tail + the configured content-policy suffix
// (the owner's rules; empty edit_policy = no suffix). The policy is the
// instruction-layer guard for the safeguard-free Qwen Image Edit model —
// every model that can call the tools reads it here.
func editToolDescription(cfg Config, async bool) string {
	desc := editToolBaseDescription
	if async {
		desc += " Queues the edit job and returns a job_id immediately without waiting."
	} else {
		desc += " Blocks until the edit completes and returns the image result directly."
	}
	if p := cfg.Safety.EditPolicy; p != "" {
		desc += "\n\nContent rules you MUST follow when editing: " + p
	}
	return desc
}

// registerEditToolSync registers the blocking edit_image tool. Split out of
// the createXServer builders so live re-registration (edit_policy
// hot-reload) shares the exact registration code — the description is
// frozen at AddTool time, so a policy change means RemoveTools + this.
func registerEditToolSync(server *mcp.Server, handlers *ToolHandlers, cfg Config) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "edit_image",
		Description: editToolDescription(cfg, false),
	}, handlers.handleEditImage)
}

// registerEditToolAsync is registerEditToolSync for the non-blocking
// edit_image_async tool.
func registerEditToolAsync(server *mcp.Server, handlers *ToolHandlers, cfg Config) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "edit_image_async",
		Description: editToolDescription(cfg, true),
	}, handlers.handleEditImageAsync)
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

	registerEditToolSync(server, handlers, cfg)

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
		Description: "List recent image generation jobs with optional status filter.",
	}, handlers.handleListJobs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "queue_status",
		Description: "Overview of the image generation queue: counts of queued, running, completed, failed jobs, plus per-job ETA estimates.",
	}, handlers.handleQueueStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "server_status",
		Description: "Check if the image generation server has finished startup recovery. Returns {ready: true} when fully operational.",
	}, handlers.handleServerStatus)

	return server
}

func createAsyncServer(cfg Config, handlers *ToolHandlers) *mcp.Server {
	server := newServer(cfg, cfg.Server.Name+"-async",
		"MCP server for ComfyUI image generation (async). Use generate_image_async or enhance_and_generate_async to queue image generation jobs and get a job_id immediately.")

	mcp.AddTool(server, &mcp.Tool{
		Name:        "generate_image_async",
		Description: "Queue a ComfyUI image generation job and return a job_id immediately without waiting. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleGenerateImageAsync)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_and_generate_async",
		Description: "Enhance a prompt via LLM and queue a ComfyUI image generation job in one step. Returns a job_id immediately without waiting. Uses the 'default' enhancement profile unless a different one is specified. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleEnhanceAndGenerateAsync)

	registerEditToolAsync(server, handlers, cfg)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "wait_for_job",
		Description: "Block until an image generation job completes, fails, or times out. Returns the full result including image URLs.",
	}, handlers.handleWaitForJob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "job_status",
		Description: "Check the status and result of an image generation job by its ID. Includes queue position and ETA when available.",
	}, handlers.handleJobStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List recent image generation jobs with optional status filter.",
	}, handlers.handleListJobs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a queued image generation job that has not yet started running.",
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
		Description: "Overview of the image generation queue: counts of queued, running, completed, failed jobs, plus per-job ETA estimates.",
	}, handlers.handleQueueStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "server_status",
		Description: "Check if the image generation server has finished startup recovery. Returns {ready: true} when fully operational.",
	}, handlers.handleServerStatus)

	return server
}

func createFullServer(cfg Config, handlers *ToolHandlers) *mcp.Server {
	server := newServer(cfg, cfg.Server.Name,
		"MCP server for ComfyUI image generation and prompt enhancement. Use generate_image or enhance_and_generate for one-step image generation (blocks until done). Use generate_image_async or enhance_and_generate_async to queue image generation jobs and get a job_id immediately.")

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
		Description: "Queue a ComfyUI image generation job and return a job_id immediately without waiting. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleGenerateImageAsync)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_and_generate",
		Description: "Enhance a prompt via LLM and then generate an image using ComfyUI in one step. Blocks until both enhancement and generation complete and returns the image result directly. This is the simplest way to enhance and generate. Uses the 'default' enhancement profile unless a different one is specified. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleEnhanceAndGenerate)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "enhance_and_generate_async",
		Description: "Enhance a prompt via LLM and queue a ComfyUI image generation job in one step. Returns a job_id immediately without waiting. Uses the 'default' enhancement profile unless a different one is specified. Uses the default workflow if workflow is empty or 'default'.",
	}, handlers.handleEnhanceAndGenerateAsync)

	registerEditToolSync(server, handlers, cfg)
	registerEditToolAsync(server, handlers, cfg)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "wait_for_job",
		Description: "Block until an image generation job completes, fails, or times out. Returns the full result including image URLs.",
	}, handlers.handleWaitForJob)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "job_status",
		Description: "Check the status and result of an image generation job by its ID. Includes queue position and ETA when available.",
	}, handlers.handleJobStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List recent image generation jobs with optional status filter.",
	}, handlers.handleListJobs)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a queued image generation job that has not yet started running.",
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
		Description: "Overview of the image generation queue: counts of queued, running, completed, failed jobs, plus per-job ETA estimates.",
	}, handlers.handleQueueStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload_image",
		Description: "Upload base64-encoded image data to the configured file host and return the URL.",
	}, handlers.handleUploadImage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "server_status",
		Description: "Check if the image generation server has finished startup recovery. Returns {ready: true} when fully operational.",
	}, handlers.handleServerStatus)

	return server
}
