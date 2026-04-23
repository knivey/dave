package main

import (
	"context"
	"encoding/json"
	"fmt"
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

func mcpCmd(network Network, c *girc.Client, e girc.Event, cfg MCPCommandConfig, ctx context.Context, output chan<- string, args ...string) {
	log := logxi.New(network.Name + ".mcp." + cfg.Name)
	log.SetLevel(logxi.LevelAll)

	if cfg.Arg != "" && len(args) == 0 {
		select {
		case output <- "Usage: <" + cfg.Arg + ">":
		case <-ctx.Done():
		}
		return
	}

	toolArgs := make(map[string]any)
	for k, v := range cfg.Args {
		toolArgs[k] = v
	}
	if cfg.Arg != "" {
		toolArgs[cfg.Arg] = args[0]
	}

	log.Debug("calling MCP tool", "tool", cfg.Tool, "mcp", cfg.MCP, "timeout", cfg.Timeout.String())

	result, err := callMCPToolWithTimeoutContext(ctx, cfg.Tool, toolArgs, cfg.Timeout)
	if err != nil {
		select {
		case output <- errorMsg(fmt.Sprintf("MCP tool call failed: %s", err)):
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
		select {
		case output <- "All done ;)":
		case <-ctx.Done():
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
