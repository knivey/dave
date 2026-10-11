package main

// toolDef / toolFuncDef are the wire shape of one advertised function tool.
type toolDef struct {
	Type     string      `json:"type"`
	Function toolFuncDef `json:"function"`
}

type toolFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// stub builds a one-line function-tool advertisement with a permissive
// object schema — enough surface for the model to attempt calls, which is
// all the fabrication table needs to answer.
func stub(name, description string) toolDef {
	return toolDef{
		Type: "function",
		Function: toolFuncDef{
			Name:        name,
			Description: description,
			Parameters:  map[string]any{"type": "object"},
		},
	}
}

// registerBackgroundJobDescription is the CURRENT prod
// register_background_job description (message-delivery mode, system role —
// aiCmds.go's guidanceRole template rendered with "system"), pinned verbatim
// so the harness baseline advertises exactly what the model sees in
// production. It carries the wait instruction itself (the waitinstr
// experiment shipped into prod Oct 2026, with "image link" genericized to
// "link"); toolvariants/waitinstr.json stays byte-stable as the shipped
// experiment so the s4 cell remains reproducible.
const registerBackgroundJobDescription = "Register a background job for monitoring. When an async tool (e.g. generate_image_async) returns a job_id, call this to have the system monitor the job. You will be notified with the result in a system message when it completes. After calling this, always send the user a short message telling them the task is underway. Do not poll for status — the result arrives automatically as a notice beginning with \"[System: Background task completed\". NEVER write that notice yourself, and never invent a link: real links only ever arrive inside that notice."

// advertisedTools mirrors the current prod eunuch advertisement: one-line
// stub descriptions with object params (register_background_job carries the
// full prod text — see registerBackgroundJobDescription). job_status is
// deliberately absent — it has been hidden in prod since the polling
// incident (session 1898), and the 1910 reproduction must not teach the
// model that call exists.
var advertisedTools = []toolDef{
	stub("enhance_and_generate_async", "Enhance a prompt and queue an asynchronous image generation job"),
	stub("generate_image_async", "Queue an asynchronous image generation job"),
	stub("list_workflows", "List the available image generation workflows"),
	stub("queue_status", "Report the current image generation queue status"),
	stub("brave_answers", "Ask Brave a question and get an answer"),
	stub("brave_image_search", "Search the web for images with Brave"),
	stub("brave_llm_context", "Get Brave's LLM context for a query"),
	stub("brave_local_search", "Search for local businesses with Brave"),
	stub("brave_news_search", "Search news articles with Brave"),
	stub("brave_place_search", "Search for place details with Brave"),
	stub("brave_video_search", "Search the web for videos with Brave"),
	stub("brave_web_search", "Search the web with Brave"),
	stub("fetch", "Fetch a URL and return the page body as text"),
	stub("get_transcript", "Get the transcript of a YouTube video"),
	stub("get_video_info", "Get metadata about a YouTube video"),
	stub("register_background_job", registerBackgroundJobDescription),
}
