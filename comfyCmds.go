package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
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

func comfy(network Network, c *girc.Client, e girc.Event, cfg ComfyConfig, args ...string) {
	logger := logxi.New(network.Name + ".comfy." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

	if len(args) == 0 {
		c.Cmd.Reply(e, "Usage: <prompt>")
		return
	}

	startedRunning(network.Name + e.Params[0])
	defer stoppedRunning(network.Name + e.Params[0])

	workflow, err := loadComfyWorkflow(cfg.WorkflowPath)
	if err != nil {
		c.Cmd.Reply(e, "Failed to load workflow: "+err.Error())
		logger.Error("Failed to load workflow", "error", err.Error())
		return
	}

	workflow[cfg.PromptNode].Inputs["text"] = args[0]

	for _, nodeID := range cfg.SeedNodes {
		if node, ok := workflow[nodeID]; ok {
			if _, hasSeed := node.Inputs["seed"]; hasSeed {
				node.Inputs["seed"] = randSeed()
			}
		}
	}

	baseURL := config.Services[cfg.Service].BaseURL

	wsURL := "ws://" + comfySchemeRegex.ReplaceAllString(baseURL, "") + "/ws?clientId=" + cfg.ClientID
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		c.Cmd.Reply(e, "Failed to connect to ComfyUI websocket: "+err.Error())
		logger.Error("WebSocket connection failed", "error", err.Error())
		return
	}
	defer wsConn.Close()

	promptReq := ComfyPromptRequest{
		Prompt:   workflow,
		ClientID: cfg.ClientID,
	}
	jsonData, err := json.Marshal(promptReq)
	if err != nil {
		c.Cmd.Reply(e, "Failed to marshal prompt: "+err.Error())
		logger.Error("Failed to marshal prompt", "error", err.Error())
		return
	}

	resp, err := http.Post(baseURL+"/prompt", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		c.Cmd.Reply(e, "Failed to submit prompt: "+err.Error())
		logger.Error("Failed to submit prompt", "error", err.Error())
		return
	}
	defer resp.Body.Close()

	var promptResp ComfyPromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&promptResp); err != nil {
		c.Cmd.Reply(e, "Failed to read prompt response: "+err.Error())
		logger.Error("Failed to decode prompt response", "error", err.Error())
		return
	}
	logger.Debug("Prompt submitted", "prompt_id", promptResp.PromptID)

	wsConn.SetReadDeadline(time.Now().Add(time.Duration(cfg.Timeout) * time.Second))
	for {
		if _, _, err := wsConn.ReadMessage(); err != nil {
			c.Cmd.Reply(e, "WebSocket read error: "+err.Error())
			logger.Error("WebSocket read error", "error", err.Error())
			return
		}

		history, err := getComfyHistory(baseURL, promptResp.PromptID)
		if err != nil {
			logger.Debug("History fetch failed", "error", err.Error())
			continue
		}

		if entry, ok := history[promptResp.PromptID]; ok {
			if output, ok := entry.Outputs[cfg.OutputNode]; ok {
				for _, img := range output.Images {
					imgData, err := downloadComfyImage(baseURL, img)
					if err != nil {
						c.Cmd.Reply(e, "Failed to download image: "+err.Error())
						logger.Error("Failed to download image", "error", err.Error())
						continue
					}
					url, err := uploadDotBeer(imgData, img.Filename)
					if err != nil {
						c.Cmd.Reply(e, "Failed to upload image: "+err.Error())
						logger.Error("Failed to upload image", "error", err.Error())
						continue
					}
					c.Cmd.Reply(e, url)
				}
				c.Cmd.Reply(e, "All done ;)")
				return
			}
		}
	}
}

func loadComfyWorkflow(path string) (ComfyWorkflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var workflow ComfyWorkflow
	if err := json.Unmarshal(data, &workflow); err != nil {
		return nil, err
	}

	return workflow, nil
}

func getComfyHistory(baseURL, promptID string) (ComfyHistoryResponse, error) {
	resp, err := http.Get(baseURL + "/history/" + promptID)
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

func randSeed() int64 {
	return rand.Int63()
}
