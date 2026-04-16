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
	"time"

	"github.com/gorilla/websocket"
)

type ComfyWorkflow map[string]ComfyNode

var comfySchemeRegex = regexp.MustCompile(`^https?://`)

type ComfyNode struct {
	Inputs map[string]interface{} `json:"inputs"`
	Class  string                 `json:"class_type"`
	Meta   *struct {
		Title string `json:"title"`
	} `json:"_meta,omitempty"`
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
	Images []ComfyImageData
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

func submitComfyGeneration(ctx context.Context, cfg Config, workflowName, prompt, negativePrompt string, seedOverride *int64) (ComfyResult, error) {
	wc, ok := cfg.Workflows[workflowName]
	if !ok {
		return ComfyResult{}, fmt.Errorf("workflow %q not found", workflowName)
	}

	workflow, err := loadComfyWorkflow(wc.WorkflowPath)
	if err != nil {
		return ComfyResult{}, fmt.Errorf("loading workflow: %w", err)
	}

	workflow[wc.PromptNode].Inputs["text"] = prompt

	if wc.NegativePromptNode != "" && negativePrompt != "" {
		workflow[wc.NegativePromptNode].Inputs["text"] = negativePrompt
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

	baseURL := cfg.Comfy.BaseURL

	wsURL := "ws://" + comfySchemeRegex.ReplaceAllString(baseURL, "") + "/ws?clientId=" + wc.ClientID
	wsConn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return ComfyResult{}, fmt.Errorf("websocket connect: %w", err)
	}
	defer wsConn.Close()

	promptReq := ComfyPromptRequest{
		Prompt:   workflow,
		ClientID: wc.ClientID,
	}
	jsonData, err := json.Marshal(promptReq)
	if err != nil {
		return ComfyResult{}, fmt.Errorf("marshaling prompt: %w", err)
	}

	resp, err := http.Post(baseURL+"/prompt", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return ComfyResult{}, fmt.Errorf("submitting prompt: %w", err)
	}
	defer resp.Body.Close()

	var promptResp ComfyPromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&promptResp); err != nil {
		return ComfyResult{}, fmt.Errorf("decoding prompt response: %w", err)
	}

	timeout := time.Duration(wc.Timeout) * time.Second
	wsConn.SetReadDeadline(time.Now().Add(timeout))

	for {
		if _, _, err := wsConn.ReadMessage(); err != nil {
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			result, found := checkComfyOutput(checkCtx, cfg, wc, baseURL, promptResp.PromptID)
			cancel()
			if found {
				return result, nil
			}
			return ComfyResult{}, fmt.Errorf("websocket read error: %w", err)
		}

		if result, found := checkComfyOutput(ctx, cfg, wc, baseURL, promptResp.PromptID); found {
			return result, nil
		}
	}
}

func checkComfyOutput(ctx context.Context, cfg Config, wc WorkflowConfig, baseURL, promptID string) (ComfyResult, bool) {
	history, err := getComfyHistory(ctx, baseURL, promptID)
	if err != nil {
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
			continue
		}
		result.Images = append(result.Images, ComfyImageData{
			Data:     data,
			Filename: img.Filename,
		})
	}

	if len(result.Images) == 0 {
		return ComfyResult{}, false
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
	return io.ReadAll(resp.Body)
}
