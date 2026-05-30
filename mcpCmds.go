package main

import (
	"context"
	"encoding/json"
	"strings"
	"text/template"

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

func executeToolTemplate(tmpl *template.Template, data map[string]any, buf *strings.Builder) error {
	return tmpl.Execute(buf, data)
}

func mcpCmd(network Network, c *girc.Client, e girc.Event, cfg MCPCommandConfig, ctx context.Context, output chan<- string, args ...string) {
	log := newLogger(network.Name + ".mcp." + cfg.Name)
	n := getNotices()

	if cfg.Arg != "" && len(args) == 0 {
		sendOrDone(ctx, output, expandNotice(n.Tools.Usage, map[string]string{"arg": cfg.Arg}))
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

	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	nick := e.Source.Name
	resolvedUser, _ := resolveIRCUser(network, c, nick, e.Source)
	var userID int64
	if resolvedUser != nil {
		userID = resolvedUser.ID
	}

	injectScopeArgs(toolArgs, cfg.Tool, map[string]any{
		"network": network.Name,
		"channel": channel,
		"user_id": userID,
		"nick":    nick,
	})

	if !cfg.Sync {
		mcpCmdAsync(network, cfg, ctx, output, toolArgs, prompt, channel, nick, userID, log)
		return
	}

	log.Debug("calling MCP tool", "tool", cfg.Tool, "mcp", cfg.MCP, "timeout", cfg.Timeout.String())

	result, err := callMCPToolWithTimeoutContext(ctx, cfg.Tool, toolArgs, cfg.Timeout)
	if err != nil {
		sendOrDone(ctx, output, errorNotice(n.Tools.Failed, map[string]string{"error": err.Error()}))
		log.Error("MCP tool call failed", "error", err.Error())
		return
	}

	text := mcpToolResultToText(result)
	if result.IsError {
		sendOrDone(ctx, output, errorMsg(text))
		return
	}

	if cfg.outputTmpl != nil {
		var data map[string]any
		if err := json.Unmarshal([]byte(text), &data); err != nil {
			log.Warn("failed to parse tool result as JSON for template, sending raw", "error", err)
			sendImageOrTextResult(text, ctx, output)
			return
		}
		data["_nick"] = nick
		data["_channel"] = channel
		data["_network"] = network.Name
		var buf strings.Builder
		if err := executeToolTemplate(cfg.outputTmpl, data, &buf); err != nil {
			log.Warn("template execution failed, sending raw result", "error", err)
			sendImageOrTextResult(text, ctx, output)
			return
		}
		rendered := strings.TrimSpace(buf.String())
		for _, line := range strings.Split(rendered, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				if !sendOrDone(ctx, output, line) {
					return
				}
			}
		}
		return
	}

	sendImageOrTextResult(text, ctx, output)
}

func mcpCmdAsync(network Network, cfg MCPCommandConfig, ctx context.Context, output chan<- string, toolArgs map[string]any, prompt string, channel string, nick string, userID int64, log logxi.Logger) {
	n := getNotices()
	asyncTool := cfg.GetAsyncTool()

	injectScopeArgs(toolArgs, asyncTool, map[string]any{
		"network": network.Name,
		"channel": channel,
		"user_id": userID,
		"nick":    nick,
	})

	log.Debug("calling async MCP tool", "tool", asyncTool, "timeout", cfg.Timeout.String())

	result, err := callMCPToolWithTimeoutContext(ctx, asyncTool, toolArgs, cfg.Timeout)
	if err != nil {
		sendOrDone(ctx, output, errorNotice(n.Tools.Failed, map[string]string{"error": err.Error()}))
		log.Error("async MCP tool call failed", "error", err.Error())
		return
	}

	text := mcpToolResultToText(result)
	if result.IsError {
		sendOrDone(ctx, output, errorMsg(text))
		return
	}

	var submitResult mcpAsyncSubmitResult
	if err := json.Unmarshal([]byte(text), &submitResult); err != nil || submitResult.JobID == "" {
		log.Error("failed to parse async job_id from result", "text", text, "error", err)
		sendOrDone(ctx, output, errorMsg(n.Tools.Unexpected))
		return
	}

	log.Info("async job submitted", "job_id", submitResult.JobID, "tool", asyncTool)

	if !sendOrDone(ctx, output, expandNotice(n.Queue.AsyncSubmitted, map[string]string{"nick": nick})) {
		return
	}

	registerToolAsyncJob(submitResult.JobID, asyncTool, cfg.MCP, network.Name, channel, nick, prompt, userID)
}

func sendImageOrTextResult(text string, ctx context.Context, output chan<- string) {
	var imgResult mcpImageResult
	if err := json.Unmarshal([]byte(text), &imgResult); err == nil && len(imgResult.Images) > 0 {
		if imgResult.Error != "" {
			sendOrDone(ctx, output, errorMsg(imgResult.Error))
			return
		}
		for _, img := range imgResult.Images {
			if img.URL != "" {
				if !sendOrDone(ctx, output, img.URL) {
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
			if !sendOrDone(ctx, output, line) {
				return
			}
		}
	}
}
