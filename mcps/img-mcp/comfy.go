package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"os"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type ComfyWorkflow map[string]ComfyNode

var comfySchemeRegex = regexp.MustCompile(`^https?://`)

type ComfyNode struct {
	Inputs map[string]interface{} `json:"inputs"`
	Class  string                 `json:"class_type"`
	Meta   *comfyNodeMeta         `json:"_meta,omitempty"`
}

type comfyNodeMeta struct {
	Title string `json:"title"`
}

type ComfyPromptRequest struct {
	Prompt   ComfyWorkflow `json:"prompt"`
	ClientID string        `json:"client_id"`
}

type ComfyPromptResponse struct {
	PromptID string `json:"prompt_id"`
}

type ComfyHistoryResponse map[string]ComfyHistoryEntry

type ComfyHistoryEntry struct {
	Outputs map[string]ComfyOutput `json:"outputs"`
	Status  *ComfyHistoryStatus    `json:"status,omitempty"`
}

// ComfyHistoryStatus decodes the "status" chunk of a ComfyUI history entry.
// Messages is an array of ["event", {...data}] JSON tuples. Tuples are kept
// as raw JSON because encoding/json cannot decode arrays into structs
// positionally — the event name and data object are decoded per tuple.
type ComfyHistoryStatus struct {
	Messages [][]json.RawMessage `json:"messages"`
}

type ComfyStatusData struct {
	Timestamp int64  `json:"timestamp"`
	PromptID  string `json:"prompt_id"`
}

type ComfyOutput struct {
	Images []ComfyImage `json:"images"`
}

type ComfyImage struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

type ComfyResult struct {
	Images      []ComfyImageData
	ComfyImages []ComfyImage
	// ExecStartedAt / ExecSuccessAt are ComfyUI's own epoch-ms timestamps
	// (execution_start / execution_success) parsed from the history status
	// messages (ComfyUI 0.33+). Diagnostics only: they let the
	// "generation detected" log line distinguish a slow generation from slow
	// completion detection. Nil on ComfyUI versions without status messages.
	ExecStartedAt *int64
	ExecSuccessAt *int64
	// DownloadMS / DownloadBytes aggregate the /view image-download phase
	// (summed across images) so the "generation detected" line can show what
	// downloading cost without needing the per-image lines.
	DownloadMS    int64
	DownloadBytes int64
}

type ComfyImageData struct {
	Data     []byte
	Filename string
}

func loadComfyWorkflow(path string) (ComfyWorkflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading workflow: %w", err)
	}
	var workflow ComfyWorkflow
	if err := json.Unmarshal(data, &workflow); err != nil {
		return nil, fmt.Errorf("parsing workflow: %w", err)
	}
	return workflow, nil
}

func randSeed() int64 {
	return rand.Int63()
}

// davePromptNoteNodeID is the node key img-mcp injects into every submitted
// workflow. String key (not numeric) so it can never collide with the numeric
// IDs editors export.
const davePromptNoteNodeID = "dave_original_prompt"

const davePromptNoteTitle = "dave original prompt"

// Cap on the reasoning text embedded in the prompt note node. The note is
// baked into every generated image's metadata permanently (uploads are
// permanent gallery entries), so a pathological rambling summary must not
// bloat every file. Normal enhancement reasoning is a few hundred runes;
// 16k is generous headroom. Counted in runes, not bytes, so truncation
// always lands on a rune boundary and the embedded JSON stays valid UTF-8.
const maxPromptNoteReasoningRunes = 16 * 1024

const promptNoteTruncationMarker = "…[truncated]"

// truncateForPromptNote caps reasoning text for the note payload. Inputs at
// or below the cap pass through untouched (byte-identical).
func truncateForPromptNote(reasoning string) string {
	if len(reasoning) <= maxPromptNoteReasoningRunes {
		// Fast path: rune count is always <= byte count, so a short-enough
		// byte length needs no rune walk.
		return reasoning
	}
	runes := []rune(reasoning)
	if len(runes) <= maxPromptNoteReasoningRunes {
		return reasoning
	}
	return string(runes[:maxPromptNoteReasoningRunes]) + promptNoteTruncationMarker
}

// promptNotePayload is the JSON stored in the note node. ComfyUI embeds the
// full submitted workflow graph (including disconnected nodes) in the image's
// "prompt" metadata chunk, so this survives inside every generated file.
// EnhancementReasoning is omitempty: plain-generate jobs (and Chat
// Completions enhancement, which exposes no reasoning summaries) produce the
// same compact payload as before the field existed.
type promptNotePayload struct {
	Prompt               string `json:"prompt"`
	LLMGenerated         bool   `json:"llm_generated"`
	JobID                string `json:"job_id"`
	EnhancementReasoning string `json:"enhancement_reasoning,omitempty"`
}

func buildPromptNote(job *Job, enhancementReasoning string) (string, error) {
	data, err := json.Marshal(promptNotePayload{
		Prompt:               job.Input.Prompt,
		LLMGenerated:         job.Input.LLMGenerated,
		JobID:                job.ID,
		EnhancementReasoning: truncateForPromptNote(enhancementReasoning),
	})
	if err != nil {
		return "", fmt.Errorf("marshaling prompt note: %w", err)
	}
	return string(data), nil
}

func prepareComfyWorkflow(cfg Config, workflowName, prompt, negativePrompt string, seedOverride *int64, promptNote string) (ComfyWorkflow, error) {
	wc, ok := cfg.Workflows[workflowName]
	if !ok {
		return nil, fmt.Errorf("workflow %q not found", workflowName)
	}

	workflow, err := loadComfyWorkflow(wc.WorkflowPath)
	if err != nil {
		return nil, fmt.Errorf("loading workflow: %w", err)
	}

	// Guards return errors instead of panicking on workflow-file/config
	// mismatches: indexing a missing node yields a zero ComfyNode whose
	// Inputs map is nil, and assigning into it kills the whole process.
	promptNode, ok := workflow[wc.PromptNode]
	if !ok {
		return nil, fmt.Errorf("prompt node %q not found in workflow %q", wc.PromptNode, wc.WorkflowPath)
	}
	if promptNode.Inputs == nil {
		return nil, fmt.Errorf("prompt node %q in workflow %q has no inputs map", wc.PromptNode, wc.WorkflowPath)
	}
	promptNode.Inputs["text"] = prompt

	if wc.NegativePromptNode != "" && negativePrompt != "" {
		negNode, ok := workflow[wc.NegativePromptNode]
		if !ok {
			return nil, fmt.Errorf("negative prompt node %q not found in workflow %q", wc.NegativePromptNode, wc.WorkflowPath)
		}
		if negNode.Inputs == nil {
			return nil, fmt.Errorf("negative prompt node %q in workflow %q has no inputs map", wc.NegativePromptNode, wc.WorkflowPath)
		}
		negNode.Inputs["text"] = negativePrompt
	}

	for _, nodeID := range wc.SeedNodes {
		if node, ok := workflow[nodeID]; ok {
			if _, hasSeed := node.Inputs["seed"]; hasSeed {
				if seedOverride != nil {
					node.Inputs["seed"] = *seedOverride
				} else {
					node.Inputs["seed"] = randSeed()
				}
			}
		}
	}

	// DESIGN NOTE: The prompt note node is intentionally disconnected from the
	// rest of the graph. ComfyUI only validates the INPUTS of nodes reachable
	// from output nodes, so this node costs nothing at generation time and
	// needs no inputs beyond its text — the production workflows already carry
	// a text-only orphan CLIPTextEncode (an unused negative prompt) that the
	// server accepts. The full submitted graph — orphan nodes included — is
	// embedded in the file metadata by the save node, so the original user
	// prompt (pre-enhancement, otherwise overwritten in the prompt node above)
	// and provenance are recoverable from the image file itself.
	//
	// The class MUST be a backend-registered core type: validate_prompt checks
	// class_type registration for EVERY node in the prompt, even unreachable
	// ones. "Note" is frontend-only (the editor strips it from API exports and
	// the backend rejects it with missing_node_type — learned the hard way),
	// so we use CLIPTextEncode.
	//
	// The write overwrites any pre-existing key with this ID — dave's node
	// wins. Editors export numeric node IDs only, so a collision means a
	// hand-authored workflow deliberately used our namespaced ID.
	workflow[davePromptNoteNodeID] = ComfyNode{
		Inputs: map[string]interface{}{"text": promptNote},
		Class:  "CLIPTextEncode",
		Meta:   &comfyNodeMeta{Title: davePromptNoteTitle},
	}

	return workflow, nil
}

// comfyClientID derives the per-job ComfyUI client id used at prompt
// submission AND on the monitor's /ws connection. ComfyUI keys /ws sockets
// by clientId and evicts same-id reconnects, so with concurrent workers a
// shared id would deafen every earlier monitor's push socket. It also routes
// per-prompt events (executing/executed/progress) only to the socket
// registered under the client id the prompt was submitted with, so submit
// and monitor MUST derive the same value or the monitor hears nothing. The
// job id — not a process-local counter — is the suffix because it is
// persisted in the DB: restart recovery re-derives the same id the previous
// process submitted under, keeping push delivery alive for recovered jobs.
// (Caveat: this only holds while the configured clientid is unchanged —
// it hot-reloads, and a restart with a different value means the recovered
// monitor listens under an id nobody submitted with. The monitor's /history
// poll fallback covers that case; push is never assumed.)
func comfyClientID(wc WorkflowConfig, jobID string) string {
	return wc.ClientID + "-" + jobID
}

func submitComfyPrompt(ctx context.Context, cfg Config, workflowName string, workflow ComfyWorkflow, jobID string) (string, error) {
	wc := cfg.Workflows[workflowName]

	promptReq := ComfyPromptRequest{
		Prompt:   workflow,
		ClientID: comfyClientID(wc, jobID),
	}
	jsonData, err := json.Marshal(promptReq)
	if err != nil {
		return "", fmt.Errorf("marshaling prompt: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Comfy.BaseURL+"/prompt", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("creating prompt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("submitting prompt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		loggerComfy.Error("comfyui prompt rejected", "status", resp.StatusCode, "body", string(body))
		return "", fmt.Errorf("comfyui returned status %d: %s", resp.StatusCode, string(body))
	}

	var promptResp ComfyPromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&promptResp); err != nil {
		return "", fmt.Errorf("decoding prompt response: %w", err)
	}

	return promptResp.PromptID, nil
}

func interruptComfyPrompt(ctx context.Context, cfg Config, promptID string) error {
	body := map[string]string{"prompt_id": promptID}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling interrupt request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Comfy.BaseURL+"/api/interrupt", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("creating interrupt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending interrupt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		loggerComfy.Warn("comfyui interrupt returned non-OK", "prompt_id", promptID, "status", resp.StatusCode)
	} else {
		loggerComfy.Info("comfyui interrupt sent", "prompt_id", promptID)
	}
	return nil
}

// deleteComfyQueuedPrompt removes a PENDING prompt from ComfyUI's internal
// queue (POST /queue {"delete": [prompt_id]}). ComfyUI's prompt_id-targeted
// /api/interrupt only fires when the prompt is the one currently executing
// (verified against v0.33.3: it skips non-running ids), so under
// queue.max_workers > 1 a cancelled job whose prompt is still pending behind
// another worker's generation would otherwise be executed anyway after the
// cancel — orphan output files nobody downloads and wasted GPU time. The
// delete is a no-op for running or finished prompts, so it is always safe to
// send alongside the interrupt: together the two calls cover both states.
func deleteComfyQueuedPrompt(ctx context.Context, cfg Config, promptID string) error {
	body := map[string][]string{"delete": {promptID}}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshaling queue delete request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Comfy.BaseURL+"/queue", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("creating queue delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending queue delete: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		loggerComfy.Warn("comfyui queue delete returned non-OK", "prompt_id", promptID, "status", resp.StatusCode)
	} else {
		loggerComfy.Info("comfyui queued prompt deleted", "prompt_id", promptID)
	}
	return nil
}

// comfyPollInterval is how often monitorComfyGeneration falls back to polling
// the /history endpoint while waiting for websocket events. It bounds
// completion-detection latency when ComfyUI's push delivery is delayed or
// dropped entirely.
const comfyPollInterval = 1 * time.Second

func monitorComfyGeneration(ctx context.Context, cfg Config, workflowName, promptID, jobID string) (ComfyResult, error) {
	wc := cfg.Workflows[workflowName]
	baseURL := cfg.Comfy.BaseURL

	loggerComfy.Info("monitoring generation", "prompt_id", promptID, "workflow", workflowName, "client_id", comfyClientID(wc, jobID))

	start := time.Now()
	wsURL := "ws://" + comfySchemeRegex.ReplaceAllString(baseURL, "") + "/ws?clientId=" + comfyClientID(wc, jobID)
	wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		loggerComfy.Error("websocket connect failed", "prompt_id", promptID, "error", err)
		return ComfyResult{}, fmt.Errorf("websocket connect: %w", err)
	}
	defer wsConn.Close()
	loggerComfy.Debug("websocket connected", "prompt_id", promptID, "dial_ms", time.Since(start).Milliseconds())

	timeout := time.Duration(wc.Timeout) * time.Second

	// DESIGN NOTE: completion detection is deliberately belt-and-suspenders.
	// The websocket gives sub-second reaction when ComfyUI's push works, but a
	// push-only monitor proved fragile in production: ComfyUI (0.33.3) delayed
	// completion events by 9-17s under post-execution load — worst right after
	// a restart, likely asset scans/GIL-starved event loop — and ComfyUI keys
	// sockets by clientId, so a same-clientId reconnect silently evicts the
	// previous socket and stops ALL delivery to it, broadcasts included. The
	// per-job client id (comfyClientID) keeps concurrent monitors from evicting
	// each other, but the eviction hazard is why this monitor must never assume
	// push works. The poll ticker bounds detection latency at
	// comfyPollInterval no matter what the websocket does; the message path
	// keeps the common case instant. The read-error and timeout branches still
	// do one final history check because the generation may have completed
	// even though push delivery never did.
	//
	// wsMessages counts every websocket message received, so the
	// "generation detected" log line can tell a deaf socket (only the
	// single connect-status message, or none at all) from delayed delivery
	// (messages flowing, detection late). When a ws message and the poll
	// tick race, the select picks randomly — the source label is a coin
	// flip in that case, but the history check that follows is identical.
	var wsMessages atomic.Int64
	msgCh := make(chan struct{}, 1)
	readErrCh := make(chan error, 1)
	go func() {
		_ = wsConn.SetReadDeadline(time.Now().Add(timeout))
		for {
			if _, _, err := wsConn.ReadMessage(); err != nil {
				readErrCh <- err
				return
			}
			wsMessages.Add(1)
			select {
			case msgCh <- struct{}{}:
			default:
			}
		}
	}()

	// Poll aggregation: websocket messages and the 1s ticker both trigger
	// history checks (~2/s during an active generation), and on a VPN-path
	// ComfyUI even healthy checks can exceed any fixed slowness threshold —
	// so per-check logging is spam (43 DBG lines for one 20s production
	// generation before this was learned). Instead the monitor counts checks
	// and keeps the slowest one; detected() reports both on the summary line.
	// Caveats: check_ms_max includes the detecting check's image download
	// (download_ms is reported separately); the finalCheck fallback paths do
	// not count their own 5s-bounded check.
	var polls, checkMSMax int64

	// detected logs the observability summary — detection source (push vs
	// poll vs final-check fallback), total monitor elapsed time, websocket
	// message count, and ComfyUI's own execution duration when the history
	// entry carries status timestamps. One line that separates every failure
	// mode this monitor can hit.
	detected := func(source string, result ComfyResult) ComfyResult {
		args := []interface{}{
			"prompt_id", promptID,
			"source", source,
			"elapsed_ms", time.Since(start).Milliseconds(),
			"ws_messages", wsMessages.Load(),
			"polls", polls,
			"check_ms_max", checkMSMax,
		}
		if result.ExecStartedAt != nil && result.ExecSuccessAt != nil {
			args = append(args, "exec_ms", *result.ExecSuccessAt-*result.ExecStartedAt)
		}
		if result.DownloadBytes > 0 {
			args = append(args,
				"download_ms", result.DownloadMS,
				"dl_mb_s", math.Round(mbPerSecond(result.DownloadBytes, result.DownloadMS)*100)/100,
			)
		}
		loggerComfy.Info("generation detected", args...)
		return result
	}

	// finalCheck runs one bounded history check on the way out (read error or
	// overall timeout): the prompt may be complete even though push delivery
	// failed, and succeeding here beats failing a finished job.
	finalCheck := func() (ComfyResult, bool) {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return checkComfyOutput(checkCtx, cfg, wc, baseURL, promptID)
	}

	countCheck := func(checkStart time.Time) {
		polls++
		if ms := time.Since(checkStart).Milliseconds(); ms > checkMSMax {
			checkMSMax = ms
		}
	}

	ticker := time.NewTicker(comfyPollInterval)
	defer ticker.Stop()
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	var wakeSource string
	for {
		select {
		case <-ctx.Done():
			return ComfyResult{}, ctx.Err()
		case <-msgCh:
			// websocket message arrived — check history now
			wakeSource = "ws"
		case <-ticker.C:
			// no message since the last tick — poll history anyway
			wakeSource = "poll"
		case readErr := <-readErrCh:
			if result, found := finalCheck(); found {
				return detected("read_error_final_check", result), nil
			}
			return ComfyResult{}, fmt.Errorf("websocket read error: %w", readErr)
		case <-timeoutTimer.C:
			if result, found := finalCheck(); found {
				return detected("timeout_final_check", result), nil
			}
			return ComfyResult{}, fmt.Errorf("generation timed out after %s (prompt_id %s)", timeout, promptID)
		}

		checkStart := time.Now()
		result, found := checkComfyOutput(ctx, cfg, wc, baseURL, promptID)
		countCheck(checkStart)
		if found {
			return detected(wakeSource, result), nil
		}
	}
}

func resumeComfyGeneration(ctx context.Context, cfg Config, workflowName, promptID, jobID string) (ComfyResult, error) {
	wc := cfg.Workflows[workflowName]
	baseURL := cfg.Comfy.BaseURL

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	result, found := checkComfyOutput(checkCtx, cfg, wc, baseURL, promptID)
	cancel()
	if found {
		return result, nil
	}

	return monitorComfyGeneration(ctx, cfg, workflowName, promptID, jobID)
}

func checkComfyOutput(ctx context.Context, cfg Config, wc WorkflowConfig, baseURL, promptID string) (ComfyResult, bool) {
	history, err := getComfyHistory(ctx, baseURL, promptID)
	if err != nil {
		loggerComfy.Debug("history check failed", "prompt_id", promptID, "error", err)
		return ComfyResult{}, false
	}

	entry, ok := history[promptID]
	if !ok {
		return ComfyResult{}, false
	}

	output, ok := entry.Outputs[wc.OutputNode]
	if !ok {
		return ComfyResult{}, false
	}

	var result ComfyResult
	for _, img := range output.Images {
		data, st, err := downloadComfyImage(ctx, baseURL, img)
		if err != nil {
			loggerComfy.Warn("failed to download comfyui image", "filename", img.Filename, "error", err)
			continue
		}
		loggerComfy.Info("image downloaded",
			"filename", st.Filename,
			"size_bytes", st.SizeBytes,
			"size", humanBytes(st.SizeBytes),
			"fresh_connect", st.FreshConnect,
			"connect_ms", st.ConnectMS,
			"ttfb_ms", st.TTFBMS,
			"transfer_ms", st.TransferMS,
			"total_ms", st.TotalMS,
			"mb_per_s", math.Round(mbPerSecond(st.SizeBytes, st.TotalMS)*100)/100,
		)
		result.Images = append(result.Images, ComfyImageData{
			Data:     data,
			Filename: img.Filename,
		})
		result.ComfyImages = append(result.ComfyImages, img)
		result.DownloadMS += st.TotalMS
		result.DownloadBytes += st.SizeBytes
	}

	if len(result.Images) == 0 {
		return ComfyResult{}, false
	}

	if entry.Status != nil {
		for _, msg := range entry.Status.Messages {
			if len(msg) != 2 {
				continue
			}
			var event string
			if err := json.Unmarshal(msg[0], &event); err != nil {
				continue
			}
			if event != "execution_start" && event != "execution_success" {
				continue
			}
			var data ComfyStatusData
			if err := json.Unmarshal(msg[1], &data); err != nil {
				continue
			}
			ts := data.Timestamp
			if event == "execution_start" {
				result.ExecStartedAt = &ts
			} else {
				result.ExecSuccessAt = &ts
			}
		}
	}

	return result, true
}

func getComfyHistory(ctx context.Context, baseURL, promptID string) (ComfyHistoryResponse, error) {
	// Trace the connection lifecycle so the poll loop has connection-cost
	// visibility: see the comment where the fresh-connection line is logged below.
	var connectStart, connectDone time.Time
	var reusedConn bool
	trace := &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			if connectStart.IsZero() {
				connectStart = time.Now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			connectDone = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			reusedConn = info.Reused
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", baseURL+"/history/"+promptID, nil)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("history returned status %d", resp.StatusCode)
	}

	var history ComfyHistoryResponse
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		return nil, err
	}

	// Connection-cost visibility without the noise. Log ONLY when this poll
	// dialed a fresh TCP connection: occasional fresh dials are normal (idle
	// pool expiry between generations), while every-poll-fresh would mean
	// keep-alive pooling is broken. Per-poll slow round trips are NOT logged
	// — on a VPN-path ComfyUI the degraded baseline exceeds any fixed
	// threshold (130-160ms observed in production), which turned per-poll
	// logging into ~2 lines/second for the whole monitor window. Slow checks
	// are aggregated instead: monitorComfyGeneration reports polls and
	// check_ms_max on its "generation detected" line.
	if !reusedConn {
		elapsed := time.Since(start)
		connectMS := int64(0)
		if !connectStart.IsZero() && connectDone.After(connectStart) {
			connectMS = connectDone.Sub(connectStart).Milliseconds()
		}
		loggerComfy.Debug("history poll fresh connection",
			"prompt_id", promptID,
			"history_ms", elapsed.Milliseconds(),
			"connect_ms", connectMS,
		)
	}
	return history, nil
}

// ImageDLStats captures per-image download diagnostics for a /view fetch:
// where the wall time went (TCP connect vs server response vs body transfer)
// and whether the connection was pooled. ConnectMS is 0 and FreshConnect is
// false when an idle keep-alive connection was reused — which is exactly what
// distinguishes "slow to connect" from "slow to transfer".
//
// The fields are NOT additive: TTFBMS is measured from request start, so it
// includes ConnectMS (broken out separately — do not add it to TTFBMS);
// TransferMS = TotalMS - TTFBMS.
type ImageDLStats struct {
	Filename     string
	SizeBytes    int64
	FreshConnect bool
	ConnectMS    int64
	TTFBMS       int64
	TransferMS   int64
	TotalMS      int64
}

// comfyDownloadTimeout bounds a single image download. The old bare http.Get
// had no deadline and no context: a stalled /view response could wedge the
// worker forever, and job cancellation could not interrupt it.
const comfyDownloadTimeout = 30 * time.Second

func downloadComfyImage(ctx context.Context, baseURL string, img ComfyImage) ([]byte, ImageDLStats, error) {
	var stats ImageDLStats
	stats.Filename = img.Filename

	// httptrace callbacks fire on transport goroutines, not the caller's: the
	// dial runs via a dedicated goroutine with a cancellation-detached context,
	// so after a cancelled Do returns its error, ConnectDone can still fire
	// (abandoned dial). The reads below are safe ONLY on the success path —
	// delivery via the transport's result channel happens-before Do returns.
	// Do NOT read connectStart/connectDone on error paths. ConnectStart may
	// fire for multiple resolved addresses: keep the first start; ConnectDone
	// is last-wins (the final attempt), so ConnectMS spans the whole dial
	// phase including failed address attempts.
	var connectStart, connectDone, firstByte time.Time
	trace := &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			if connectStart.IsZero() {
				connectStart = time.Now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			connectDone = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			stats.FreshConnect = !info.Reused
		},
		GotFirstResponseByte: func() {
			firstByte = time.Now()
		},
	}

	dlCtx, cancel := context.WithTimeout(ctx, comfyDownloadTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/view?filename=%s&subfolder=%s&type=%s",
		baseURL, img.Filename, img.Subfolder, img.Type)
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(dlCtx, trace), http.MethodGet, url, nil)
	if err != nil {
		return nil, stats, err
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, stats, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, stats, fmt.Errorf("download returned status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, stats, err
	}

	if !connectStart.IsZero() && connectDone.After(connectStart) {
		stats.ConnectMS = connectDone.Sub(connectStart).Milliseconds()
	}
	if !firstByte.IsZero() {
		stats.TTFBMS = firstByte.Sub(start).Milliseconds()
	}
	stats.TotalMS = time.Since(start).Milliseconds()
	stats.TransferMS = stats.TotalMS - stats.TTFBMS
	if stats.TransferMS < 0 {
		stats.TransferMS = 0
	}
	stats.SizeBytes = int64(len(data))
	return data, stats, nil
}

// humanBytes renders a byte count as B/KB/MB/GB (binary units, one decimal
// above 1024) for log readability: 2411824 -> "2.3MB".
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// mbPerSecond converts a transfer of bytes over ms milliseconds into MB/s
// (binary MB, matching humanBytes). ms is clamped to >=1 so a sub-millisecond
// transfer reports a large-but-finite rate instead of dividing by zero.
func mbPerSecond(bytes, ms int64) float64 {
	if ms < 1 {
		ms = 1
	}
	return float64(bytes) / (1 << 20) / (float64(ms) / 1000)
}
