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
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	gogpt "github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
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

type EnhancementResponse struct {
	EnhancedPrompt string `json:"enhanced_prompt"`
	Refused        bool   `json:"refused"`
	Reason         string `json:"reason"`
}

func enhancePrompt(rawPrompt string, cfg ComfyConfig, network Network, logger logxi.Logger) (string, error) {
	enhCfg := config.PromptEnhancements[cfg.EnhancePrompt]
	svc := config.Services[enhCfg.Service]

	schema, err := jsonschema.GenerateSchemaForType(EnhancementResponse{})
	if err != nil {
		logger.Warn("Failed to generate schema for structured output", "error", err.Error())
		return "", fmt.Errorf("failed to generate enhancement schema: %w", err)
	}

	aiConfig := gogpt.DefaultConfig(svc.Key)
	aiConfig.BaseURL = svc.BaseURL
	aiClient := gogpt.NewClientWithConfig(aiConfig)

	messages := []gogpt.ChatCompletionMessage{
		{
			Role:    gogpt.ChatMessageRoleSystem,
			Content: enhCfg.SystemPrompt,
		},
		{
			Role:    gogpt.ChatMessageRoleUser,
			Content: rawPrompt,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := aiClient.CreateChatCompletion(ctx, gogpt.ChatCompletionRequest{
		Model:    enhCfg.Model,
		Messages: messages,
		ResponseFormat: &gogpt.ChatCompletionResponseFormat{
			Type: gogpt.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &gogpt.ChatCompletionResponseFormatJSONSchema{
				Name:   "prompt_enhancement",
				Schema: schema,
				Strict: true,
			},
		},
	})
	if err != nil {
		logger.Warn("Prompt enhancement API call failed", "error", err.Error())
		return "", fmt.Errorf("enhancement API call failed: %w", err)
	}

	enhanced := strings.TrimSpace(resp.Choices[0].Message.Content)
	var result EnhancementResponse
	err = schema.Unmarshal(enhanced, &result)
	if err != nil {
		logger.Warn("Failed to unmarshal structured response", "error", err.Error(), "content", enhanced)
		return "", fmt.Errorf("failed to parse enhancement response: %w", err)
	}

	if result.Refused {
		logger.Info("Prompt enhancement refused", "reason", result.Reason)
		return "", fmt.Errorf("enhancement refused: %s", result.Reason)
	}

	if result.EnhancedPrompt == "" {
		logger.Warn("Empty enhanced prompt returned")
		return "", fmt.Errorf("enhancement returned empty prompt")
	}

	logger.Debug("Enhanced prompt", "prompt", result.EnhancedPrompt)
	return strings.TrimSpace(result.EnhancedPrompt), nil
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
		c.Cmd.Reply(e, errorMsg("Failed to load workflow: "+err.Error()))
		logger.Error("Failed to load workflow", "error", err.Error())
		return
	}

	prompt := args[0]
	if cfg.EnhancePrompt != "" {
		c.Cmd.Reply(e, "enhancing prompt...")
		enhanced, err := enhancePrompt(args[0], cfg, network, logger)
		if err != nil {
			c.Cmd.Reply(e, errorMsg("Prompt enhancement failed: "+err.Error()))
			return
		}
		prompt = enhanced
	}

	workflow[cfg.PromptNode].Inputs["text"] = prompt

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
		c.Cmd.Reply(e, errorMsg("Failed to connect to ComfyUI websocket: "+err.Error()))
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
		c.Cmd.Reply(e, errorMsg("Failed to marshal prompt: "+err.Error()))
		logger.Error("Failed to marshal prompt", "error", err.Error())
		return
	}

	resp, err := http.Post(baseURL+"/prompt", "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		c.Cmd.Reply(e, errorMsg("Failed to submit prompt: "+err.Error()))
		logger.Error("Failed to submit prompt", "error", err.Error())
		return
	}
	defer resp.Body.Close()

	var promptResp ComfyPromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&promptResp); err != nil {
		c.Cmd.Reply(e, errorMsg("Failed to read prompt response: "+err.Error()))
		logger.Error("Failed to decode prompt response", "error", err.Error())
		return
	}
	logger.Debug("Prompt submitted", "prompt_id", promptResp.PromptID)

	wsConn.SetReadDeadline(time.Now().Add(time.Duration(cfg.Timeout) * time.Second))
	for {
		if _, _, err := wsConn.ReadMessage(); err != nil {
			// Final check for output in case websocket failed but workflow completed
			logger.Debug("Performing final history check after websocket error")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if processComfyOutput(c, e, cfg, baseURL, promptResp.PromptID, logger, ctx) {
				return
			}
			c.Cmd.Reply(e, errorMsg("WebSocket read error: "+err.Error()))
			logger.Error("WebSocket read error", "error", err.Error())
			return
		}

		if processComfyOutput(c, e, cfg, baseURL, promptResp.PromptID, logger, nil) {
			return
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

func getComfyHistory(ctx context.Context, baseURL, promptID string) (ComfyHistoryResponse, error) {
	var resp *http.Response
	var err error
	if ctx == nil {
		resp, err = http.Get(baseURL + "/history/" + promptID)
	} else {
		req, reqErr := http.NewRequestWithContext(ctx, "GET", baseURL+"/history/"+promptID, nil)
		if reqErr != nil {
			return nil, reqErr
		}
		resp, err = http.DefaultClient.Do(req)
	}
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

func processComfyOutput(c *girc.Client, e girc.Event, cfg ComfyConfig, baseURL string, promptID string, logger logxi.Logger, ctx context.Context) bool {
	history, err := getComfyHistory(ctx, baseURL, promptID)
	if err != nil {
		logger.Debug("History fetch failed", "error", err.Error())
		return false
	}

	if entry, ok := history[promptID]; ok {
		if output, ok := entry.Outputs[cfg.OutputNode]; ok {
			for _, img := range output.Images {
				imgData, err := downloadComfyImage(baseURL, img)
				if err != nil {
					c.Cmd.Reply(e, errorMsg("Failed to download image: "+err.Error()))
					logger.Error("Failed to download image", "error", err.Error())
					continue
				}
				url, err := uploadDotBeer(imgData, img.Filename)
				if err != nil {
					c.Cmd.Reply(e, errorMsg("Failed to upload image: "+err.Error()))
					logger.Error("Failed to upload image", "error", err.Error())
					continue
				}
				c.Cmd.Reply(e, url)
			}
			c.Cmd.Reply(e, "All done ;)")
			return true
		}
	}
	return false
}

func randSeed() int64 {
	return rand.Int63()
}
