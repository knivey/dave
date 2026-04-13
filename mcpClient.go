package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gogpt "github.com/sashabaranov/go-openai"
)

type MCPServer struct {
	Config    MCPConfig
	Client    *mcp.Client
	Session   *mcp.ClientSession
	Tools     []*mcp.Tool
	Resources []*mcp.Resource
	Prompts   []*mcp.Prompt
}

var (
	mcpServers      map[string]*MCPServer
	mcpServersMu    sync.Mutex
	mcpToolToServer map[string]string // tool name → server name
)

func init() {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)
}

func initMCPClients() {
	mcpServersMu.Lock()
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)
	mcpServersMu.Unlock()

	if len(config.MCPs) == 0 {
		return
	}

	ctx := context.Background()
	for name, mcpCfg := range config.MCPs {
		logger.Info("connecting MCP server", "name", name, "transport", mcpCfg.Transport)

		client := mcp.NewClient(&mcp.Implementation{Name: "dave-irc", Version: "1.0.0"}, nil)

		var transport mcp.Transport
		if mcpCfg.Transport == "stdio" {
			cmd := exec.Command(mcpCfg.Command, mcpCfg.Args...)
			transport = &mcp.CommandTransport{Command: cmd}
		} else {
			transport = &mcp.StreamableClientTransport{Endpoint: mcpCfg.URL}
		}

		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			logger.Error("failed to connect MCP server", "name", name, "error", err)
			continue
		}

		srv := &MCPServer{
			Config:  mcpCfg,
			Client:  client,
			Session: session,
		}

		if session.InitializeResult().Capabilities.Tools != nil {
			for tool, err := range session.Tools(ctx, nil) {
				if err != nil {
					logger.Error("failed to list MCP tools", "name", name, "error", err)
					continue
				}
				srv.Tools = append(srv.Tools, tool)
			}
		}

		if session.InitializeResult().Capabilities.Resources != nil {
			for res, err := range session.Resources(ctx, nil) {
				if err != nil {
					logger.Error("failed to list MCP resources", "name", name, "error", err)
					continue
				}
				srv.Resources = append(srv.Resources, res)
			}
		}

		if session.InitializeResult().Capabilities.Prompts != nil {
			for prompt, err := range session.Prompts(ctx, nil) {
				if err != nil {
					logger.Error("failed to list MCP prompts", "name", name, "error", err)
					continue
				}
				srv.Prompts = append(srv.Prompts, prompt)
			}
		}

		logger.Info("MCP server connected", "name", name,
			"tools", len(srv.Tools),
			"resources", len(srv.Resources),
			"prompts", len(srv.Prompts))

		mcpServersMu.Lock()
		mcpServers[name] = srv
		for _, tool := range srv.Tools {
			mcpToolToServer[tool.Name] = name
		}
		mcpServersMu.Unlock()
	}
}

func closeMCPClients() {
	mcpServersMu.Lock()
	defer mcpServersMu.Unlock()
	for name, srv := range mcpServers {
		logger.Info("closing MCP server", "name", name)
		if srv.Session != nil {
			srv.Session.Close()
		}
	}
}

func closeAndClearMCPClients() {
	mcpServersMu.Lock()
	defer mcpServersMu.Unlock()
	for name, srv := range mcpServers {
		logger.Info("closing MCP server", "name", name)
		if srv.Session != nil {
			srv.Session.Close()
		}
		delete(mcpServers, name)
	}
	mcpToolToServer = make(map[string]string)
}

func reloadMCPClients(newMCPs map[string]MCPConfig) {
	closeAndClearMCPClients()
	config.MCPs = newMCPs
	initMCPClients()
}

func callMCPTool(toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	mcpServersMu.Lock()
	serverName, ok := mcpToolToServer[toolName]
	if !ok {
		mcpServersMu.Unlock()
		return nil, fmt.Errorf("unknown MCP tool: %s", toolName)
	}
	srv := mcpServers[serverName]
	mcpServersMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), srv.Config.Timeout)
	defer cancel()

	if logger != nil {
		logger.Info("calling MCP tool", "server", serverName, "tool", toolName, "args", args)
	}

	result, err := srv.Session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
	if err != nil {
		if logger != nil {
			logger.Error("MCP tool call failed", "server", serverName, "tool", toolName, "error", err)
		}
		return nil, err
	}

	if result.IsError {
		if logger != nil {
			logger.Warn("MCP tool returned error", "server", serverName, "tool", toolName)
		}
	}

	return result, nil
}

func getMCPTools(serverNames []string) []gogpt.Tool {
	var tools []gogpt.Tool

	for _, serverName := range serverNames {
		mcpServersMu.Lock()
		srv, ok := mcpServers[serverName]
		mcpServersMu.Unlock()
		if !ok {
			continue
		}

		for _, tool := range srv.Tools {
			params := make(map[string]any)
			if tool.InputSchema != nil {
				raw, err := json.Marshal(tool.InputSchema)
				if err != nil {
					logger.Error("failed to marshal MCP tool schema", "tool", tool.Name, "error", err)
					continue
				}
				if err := json.Unmarshal(raw, &params); err != nil {
					logger.Error("failed to unmarshal MCP tool schema", "tool", tool.Name, "error", err)
					continue
				}
			}

			tools = append(tools, gogpt.Tool{
				Type: "function",
				Function: &gogpt.FunctionDefinition{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  params,
				},
			})
		}
	}

	return tools
}

func mcpToolResultToText(result *mcp.CallToolResult) string {
	var parts []string
	for _, c := range result.Content {
		switch content := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, content.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image: %s]", content.MIMEType))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio: %s]", content.MIMEType))
		case *mcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource: %s]", content.URI))
		case *mcp.EmbeddedResource:
			if text, ok := embeddedResourceToText(content); ok {
				parts = append(parts, text)
			}
		}
	}
	if len(parts) == 0 {
		return "(no output)"
	}
	return joinStrings(parts, "\n")
}

func embeddedResourceToText(r *mcp.EmbeddedResource) (string, bool) {
	if r.Resource == nil {
		return "", false
	}
	if r.Resource.Text != "" {
		return r.Resource.Text, true
	}
	if r.Resource.URI != "" {
		return fmt.Sprintf("[blob resource: %s]", r.Resource.URI), true
	}
	return "", false
}

func joinStrings(parts []string, sep string) string {
	result := parts[0]
	for _, s := range parts[1:] {
		result += sep + s
	}
	return result
}

func readMCPResource(serverName, uri string) (*mcp.ReadResourceResult, error) {
	mcpServersMu.Lock()
	srv, ok := mcpServers[serverName]
	mcpServersMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown MCP server: %s", serverName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), srv.Config.Timeout)
	defer cancel()

	return srv.Session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
}

func getMCPPrompt(serverName, promptName string, args map[string]string) (*mcp.GetPromptResult, error) {
	mcpServersMu.Lock()
	srv, ok := mcpServers[serverName]
	mcpServersMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown MCP server: %s", serverName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), srv.Config.Timeout)
	defer cancel()

	return srv.Session.GetPrompt(ctx, &mcp.GetPromptParams{
		Name:      promptName,
		Arguments: args,
	})
}

func getMCPToolInfo(serverNames []string) string {
	var parts []string
	for _, serverName := range serverNames {
		mcpServersMu.Lock()
		srv, ok := mcpServers[serverName]
		mcpServersMu.Unlock()
		if !ok || len(srv.Tools) == 0 {
			continue
		}
		var toolNames []string
		for _, t := range srv.Tools {
			toolNames = append(toolNames, t.Name)
		}
		parts = append(parts, serverName+"("+joinStrings(toolNames, ",")+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return "MCP tools: " + joinStrings(parts, " ")
}
