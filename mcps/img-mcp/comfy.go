package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
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

// promptNotePayload is the JSON stored in the note node. ComfyUI embeds the
// full submitted workflow graph (including disconnected nodes) in the image's
// "prompt" metadata chunk, so this survives inside every generated file.
type promptNotePayload struct {
	Prompt       string `json:"prompt"`
	LLMGenerated bool   `json:"llm_generated"`
	JobID        string `json:"job_id"`
}

func buildPromptNote(job *Job) (string, error) {
	data, err := json.Marshal(promptNotePayload{
		Prompt:       job.Input.Prompt,
		LLMGenerated: job.Input.LLMGenerated,
		JobID:        job.ID,
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

func submitComfyPrompt(ctx context.Context, cfg Config, workflowName string, workflow ComfyWorkflow) (string, error) {
	wc := cfg.Workflows[workflowName]

	promptReq := ComfyPromptRequest{
		Prompt:   workflow,
		ClientID: wc.ClientID,
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

// comfyPollInterval is how often monitorComfyGeneration falls back to polling
// the /history endpoint while waiting for websocket events. It bounds
// completion-detection latency when ComfyUI's push delivery is delayed or
// dropped entirely.
const comfyPollInterval = 1 * time.Second

func monitorComfyGeneration(ctx context.Context, cfg Config, workflowName, promptID string) (ComfyResult, error) {
	wc := cfg.Workflows[workflowName]
	baseURL := cfg.Comfy.BaseURL

	loggerComfy.Info("monitoring generation", "prompt_id", promptID, "workflow", workflowName)

	start := time.Now()
	wsURL := "ws://" + comfySchemeRegex.ReplaceAllString(baseURL, "") + "/ws?clientId=" + wc.ClientID
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
	// poll ticker bounds detection latency at comfyPollInterval no matter what
	// the websocket does; the message path keeps the common case instant. The
	// read-error and timeout branches still do one final history check because
	// the generation may have completed even though push delivery never did.
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
		}
		if result.ExecStartedAt != nil && result.ExecSuccessAt != nil {
			args = append(args, "exec_ms", *result.ExecSuccessAt-*result.ExecStartedAt)
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

		if result, found := checkComfyOutput(ctx, cfg, wc, baseURL, promptID); found {
			return detected(wakeSource, result), nil
		}
	}
}

func resumeComfyGeneration(ctx context.Context, cfg Config, workflowName, promptID string) (ComfyResult, error) {
	wc := cfg.Workflows[workflowName]
	baseURL := cfg.Comfy.BaseURL

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	result, found := checkComfyOutput(checkCtx, cfg, wc, baseURL, promptID)
	cancel()
	if found {
		return result, nil
	}

	return monitorComfyGeneration(ctx, cfg, workflowName, promptID)
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
		data, err := downloadComfyImage(baseURL, img)
		if err != nil {
			loggerComfy.Warn("failed to download comfyui image", "filename", img.Filename, "error", err)
			continue
		}
		result.Images = append(result.Images, ComfyImageData{
			Data:     data,
			Filename: img.Filename,
		})
		result.ComfyImages = append(result.ComfyImages, img)
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
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/history/"+promptID, nil)
	if err != nil {
		return nil, err
	}
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
	return history, nil
}

func downloadComfyImage(baseURL string, img ComfyImage) ([]byte, error) {
	url := fmt.Sprintf("%s/view?filename=%s&subfolder=%s&type=%s",
		baseURL, img.Filename, img.Subfolder, img.Type)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
