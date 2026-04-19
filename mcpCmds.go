package main

import (
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

func mcpCmd(network Network, c *girc.Client, e girc.Event, cfg MCPCommandConfig, args ...string) {
	log := logxi.New(network.Name + ".mcp." + cfg.Name)
	log.SetLevel(logxi.LevelAll)

	if cfg.Arg != "" && len(args) == 0 {
		c.Cmd.Reply(e, "Usage: <"+cfg.Arg+">")
		return
	}

	startedRunning(network.Name + e.Params[0])
	defer stoppedRunning(network.Name + e.Params[0])

	toolArgs := make(map[string]any)
	for k, v := range cfg.Args {
		toolArgs[k] = v
	}
	if cfg.Arg != "" {
		toolArgs[cfg.Arg] = args[0]
	}

	log.Debug("calling MCP tool", "tool", cfg.Tool, "mcp", cfg.MCP, "timeout", cfg.Timeout.String())

	result, err := callMCPToolWithTimeout(cfg.Tool, toolArgs, cfg.Timeout)
	if err != nil {
		c.Cmd.Reply(e, errorMsg(fmt.Sprintf("MCP tool call failed: %s", err)))
		log.Error("MCP tool call failed", "error", err.Error())
		return
	}

	text := mcpToolResultToText(result)
	if result.IsError {
		c.Cmd.Reply(e, errorMsg(text))
		return
	}

	var imgResult mcpImageResult
	if err := json.Unmarshal([]byte(text), &imgResult); err == nil && len(imgResult.Images) > 0 {
		if imgResult.Error != "" {
			c.Cmd.Reply(e, errorMsg(imgResult.Error))
			return
		}
		for _, img := range imgResult.Images {
			if img.URL != "" {
				c.Cmd.Reply(e, img.URL)
			}
		}
		c.Cmd.Reply(e, "All done ;)")
		return
	}

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			c.Cmd.Reply(e, line)
		}
	}
}
