package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
)

type mcpImageResult struct {
	Images []mcpImageData `json:"images"`
	Status string         `json:"status"`
	Error  string         `json:"error,omitempty"`
}

type mcpImageData struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MIMEType string `json:"mime_type"`
}

type mcpAsyncSubmitResult struct {
	JobID string `json:"job_id"`
}

func mcpCmd(network Network, c *girc.Client, e girc.Event, cfg MCPCommandConfig, ctx context.Context, output chan<- string, args ...string) {
	log := newLogger(network.Name + ".mcp." + cfg.Name)
	n := getNotices()

	if cfg.Arg != "" && len(args) == 0 {
		select {
		case output <- expandNotice(n.Tools.Usage, map[string]string{"arg": cfg.Arg}):
		case <-ctx.Done():
		}
		return
	}

	toolArgs := make(map[string]any)
	for k, v := range cfg.Args {
		toolArgs[k] = v
	}
	var prompt string
	if cfg.Arg != "" && len(args) > 0 {
		toolArgs[cfg.Arg] = args[0]
		prompt = args[0]
	}

	if !cfg.Sync {
		mcpCmdAsync(network, c, e, cfg, ctx, output, toolArgs, prompt, log)
		return
	}

	log.Debug("calling MCP tool", "tool", cfg.Tool, "mcp", cfg.MCP, "timeout", cfg.Timeout.String())

	result, err := callMCPToolWithTimeoutContext(ctx, cfg.Tool, toolArgs, cfg.Timeout)
	if err != nil {
		select {
		case output <- errorNotice(n.Tools.Failed, map[string]string{"error": err.Error()}):
		case <-ctx.Done():
		}
		log.Error("MCP tool call failed", "error", err.Error())
		return
	}

	text := mcpToolResultToText(result)
	if result.IsError {
		select {
		case output <- errorMsg(text):
		case <-ctx.Done():
		}
		return
	}

	sendImageOrTextResult(text, ctx, output)
}

func mcpCmdAsync(network Network, c *girc.Client, e girc.Event, cfg MCPCommandConfig, ctx context.Context, output chan<- string, toolArgs map[string]any, prompt string, log logxi.Logger) {
	n := getNotices()
	asyncTool := cfg.GetAsyncTool()
	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	nick := e.Source.Name

	log.Debug("calling async MCP tool", "tool", asyncTool, "timeout", cfg.Timeout.String())

	result, err := callMCPToolWithTimeoutContext(ctx, asyncTool, toolArgs, cfg.Timeout)
	if err != nil {
		select {
		case output <- errorNotice(n.Tools.Failed, map[string]string{"error": err.Error()}):
		case <-ctx.Done():
		}
		log.Error("async MCP tool call failed", "error", err.Error())
		return
	}

	text := mcpToolResultToText(result)
	if result.IsError {
		select {
		case output <- errorMsg(text):
		case <-ctx.Done():
		}
		return
	}

	var submitResult mcpAsyncSubmitResult
	if err := json.Unmarshal([]byte(text), &submitResult); err != nil || submitResult.JobID == "" {
		log.Error("failed to parse async job_id from result", "text", text, "error", err)
		select {
		case output <- errorMsg(n.Tools.Unexpected):
		case <-ctx.Done():
		}
		return
	}

	log.Info("async job submitted", "job_id", submitResult.JobID, "tool", asyncTool)

	select {
	case output <- expandNotice(n.Queue.AsyncSubmitted, map[string]string{"nick": nick}):
	case <-ctx.Done():
		return
	}

	resolvedUser, _ := resolveIRCUser(network, c, nick, e.Source)
	var userID int64
	if resolvedUser != nil {
		userID = resolvedUser.ID
	}

	registerToolAsyncJob(submitResult.JobID, asyncTool, cfg.MCP, network.Name, channel, nick, prompt, userID)
}

func sendImageOrTextResult(text string, ctx context.Context, output chan<- string) {
	var imgResult mcpImageResult
	if err := json.Unmarshal([]byte(text), &imgResult); err == nil && len(imgResult.Images) > 0 {
		if imgResult.Error != "" {
			select {
			case output <- errorMsg(imgResult.Error):
			case <-ctx.Done():
			}
			return
		}
		for _, img := range imgResult.Images {
			if img.URL != "" {
				select {
				case output <- img.URL:
				case <-ctx.Done():
					return
				}
			}
		}
		return
	}

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			select {
			case output <- line:
			case <-ctx.Done():
				return
			}
		}
	}
}
