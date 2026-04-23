package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os/exec"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gogpt "github.com/sashabaranov/go-openai"
)

const (
	reconnectBaseDelay  = 1 * time.Second
	reconnectMaxDelay   = 60 * time.Second
	reconnectGrowFactor = 2.0
)

type MCPServer struct {
	Config    MCPConfig
	Client    *mcp.Client
	Session   *mcp.ClientSession
	Tools     []*mcp.Tool
	Resources []*mcp.Resource
	Prompts   []*mcp.Prompt

	reconnectMu    sync.Mutex
	reconnectCount int
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

func connectMCPServer(name string, mcpCfg MCPConfig) (*MCPServer, error) {
	ctx := context.Background()

	var clientOpts *mcp.ClientOptions
	if mcpCfg.KeepAlive > 0 {
		clientOpts = &mcp.ClientOptions{KeepAlive: mcpCfg.KeepAlive}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "dave-irc", Version: "1.0.0"}, clientOpts)

	var transport mcp.Transport
	if mcpCfg.Transport == "stdio" {
		cmd := exec.Command(mcpCfg.Command, mcpCfg.Args...)
		transport = &mcp.CommandTransport{Command: cmd}
	} else {
		transport = &mcp.StreamableClientTransport{Endpoint: mcpCfg.URL}
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect failed: %w", err)
	}

	srv := &MCPServer{
		Config:  mcpCfg,
		Client:  client,
		Session: session,
	}

	if session.InitializeResult().Capabilities.Tools != nil {
		for tool, err := range session.Tools(ctx, nil) {
			if err != nil {
				if logger != nil {
					logger.Error("failed to list MCP tools", "name", name, "error", err)
				}
				continue
			}
			srv.Tools = append(srv.Tools, tool)
		}
	}

	if session.InitializeResult().Capabilities.Resources != nil {
		for res, err := range session.Resources(ctx, nil) {
			if err != nil {
				if logger != nil {
					logger.Error("failed to list MCP resources", "name", name, "error", err)
				}
				continue
			}
			srv.Resources = append(srv.Resources, res)
		}
	}

	if session.InitializeResult().Capabilities.Prompts != nil {
		for prompt, err := range session.Prompts(ctx, nil) {
			if err != nil {
				if logger != nil {
					logger.Error("failed to list MCP prompts", "name", name, "error", err)
				}
				continue
			}
			srv.Prompts = append(srv.Prompts, prompt)
		}
	}

	return srv, nil
}

func initMCPClients() {
	mcpServersMu.Lock()
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)
	mcpServersMu.Unlock()

	if len(config.MCPs) == 0 {
		return
	}

	for name, mcpCfg := range config.MCPs {
		logger.Info("connecting MCP server", "name", name, "transport", mcpCfg.Transport)

		srv, err := connectMCPServer(name, mcpCfg)
		if err != nil {
			logger.Error("failed to connect MCP server", "name", name, "error", err)
			continue
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

func reconnectBackoff(count int) time.Duration {
	if count <= 0 {
		return 0
	}
	delay := time.Duration(float64(reconnectBaseDelay) * math.Pow(reconnectGrowFactor, float64(count-1)))
	if delay > reconnectMaxDelay {
		delay = reconnectMaxDelay
	}
	jitter := rand.N(delay / 2)
	return delay + jitter
}

var connectMCPServerImpl = connectMCPServer

func reconnectMCPServer(name string, ctx context.Context) error {
	mcpServersMu.Lock()
	srv, ok := mcpServers[name]
	mcpServersMu.Unlock()
	if !ok {
		return fmt.Errorf("unknown MCP server: %s", name)
	}

	srv.reconnectMu.Lock()
	defer srv.reconnectMu.Unlock()

	delay := reconnectBackoff(srv.reconnectCount)
	if delay > 0 {
		if logger != nil {
			logger.Info("reconnect backoff", "server", name, "delay", delay, "attempt", srv.reconnectCount+1)
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	mcpCfg := srv.Config

	if srv.Session != nil {
		srv.Session.Close()
	}

	newSrv, err := connectMCPServerImpl(name, mcpCfg)
	if err != nil {
		srv.reconnectCount++
		if logger != nil {
			logger.Error("MCP reconnect failed", "server", name, "error", err, "attempt", srv.reconnectCount)
		}
		return fmt.Errorf("reconnect failed for %s after %d attempts: %w", name, srv.reconnectCount, err)
	}

	mcpServersMu.Lock()
	for _, tool := range srv.Tools {
		if existing, ok := mcpToolToServer[tool.Name]; ok && existing == name {
			delete(mcpToolToServer, tool.Name)
		}
	}
	mcpServers[name] = newSrv
	for _, tool := range newSrv.Tools {
		mcpToolToServer[tool.Name] = name
	}
	mcpServersMu.Unlock()

	if logger != nil {
		logger.Info("MCP server reconnected", "name", name,
			"tools", len(newSrv.Tools),
			"resources", len(newSrv.Resources),
			"prompts", len(newSrv.Prompts))
	}

	return nil
}

func getMCPServerForTool(toolName string) string {
	mcpServersMu.Lock()
	defer mcpServersMu.Unlock()
	return mcpToolToServer[toolName]
}

func callMCPTool(toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	return callMCPToolWithTimeout(toolName, args, 0)
}

func callMCPToolWithContext(ctx context.Context, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	return callMCPToolWithTimeoutContext(ctx, toolName, args, 0)
}

func callMCPToolWithTimeoutContext(ctx context.Context, toolName string, args map[string]any, timeout time.Duration) (*mcp.CallToolResult, error) {
	mcpServersMu.Lock()
	serverName, ok := mcpToolToServer[toolName]
	if !ok {
		mcpServersMu.Unlock()
		return nil, fmt.Errorf("unknown MCP tool: %s", toolName)
	}
	srv := mcpServers[serverName]
	mcpServersMu.Unlock()

	if logger != nil {
		logger.Info("calling MCP tool", "server", serverName, "tool", toolName, "args", args)
	}

	result, err := srv.callToolWithContext(ctx, toolName, args, timeout)
	if err != nil {
		if logger != nil {
			logger.Warn("MCP tool call failed, attempting reconnect", "server", serverName, "tool", toolName, "error", err)
		}
		if reconnectErr := reconnectMCPServer(serverName, ctx); reconnectErr != nil {
			return nil, fmt.Errorf("tool call failed (%v) and reconnect failed: %w", err, reconnectErr)
		}
		mcpServersMu.Lock()
		srv = mcpServers[serverName]
		mcpServersMu.Unlock()
		result, err = srv.callToolWithContext(ctx, toolName, args, timeout)
		if err != nil {
			if logger != nil {
				logger.Error("MCP tool call failed after reconnect", "server", serverName, "tool", toolName, "error", err)
			}
			return nil, err
		}
	}

	if result.IsError {
		if logger != nil {
			logger.Warn("MCP tool returned error", "server", serverName, "tool", toolName)
		}
	}

	srv.reconnectMu.Lock()
	srv.reconnectCount = 0
	srv.reconnectMu.Unlock()

	return result, nil
}

func callMCPToolWithTimeout(toolName string, args map[string]any, timeout time.Duration) (*mcp.CallToolResult, error) {
	mcpServersMu.Lock()
	serverName, ok := mcpToolToServer[toolName]
	if !ok {
		mcpServersMu.Unlock()
		return nil, fmt.Errorf("unknown MCP tool: %s", toolName)
	}
	srv := mcpServers[serverName]
	mcpServersMu.Unlock()

	if logger != nil {
		logger.Info("calling MCP tool", "server", serverName, "tool", toolName, "args", args)
	}

	result, err := srv.callTool(toolName, args, timeout)
	if err != nil {
		if logger != nil {
			logger.Warn("MCP tool call failed, attempting reconnect", "server", serverName, "tool", toolName, "error", err)
		}
		if reconnectErr := reconnectMCPServer(serverName, context.Background()); reconnectErr != nil {
			return nil, fmt.Errorf("tool call failed (%v) and reconnect failed: %w", err, reconnectErr)
		}
		mcpServersMu.Lock()
		srv = mcpServers[serverName]
		mcpServersMu.Unlock()
		result, err = srv.callTool(toolName, args, timeout)
		if err != nil {
			if logger != nil {
				logger.Error("MCP tool call failed after reconnect", "server", serverName, "tool", toolName, "error", err)
			}
			return nil, err
		}
	}

	if result.IsError {
		if logger != nil {
			logger.Warn("MCP tool returned error", "server", serverName, "tool", toolName)
		}
	}

	srv.reconnectMu.Lock()
	srv.reconnectCount = 0
	srv.reconnectMu.Unlock()

	return result, nil
}

func (srv *MCPServer) callTool(toolName string, args map[string]any, timeout time.Duration) (*mcp.CallToolResult, error) {
	if srv.Session == nil {
		return nil, fmt.Errorf("MCP session is nil for server")
	}
	if timeout == 0 {
		timeout = srv.Config.Timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
}

func (srv *MCPServer) callToolWithContext(parentCtx context.Context, toolName string, args map[string]any, timeout time.Duration) (*mcp.CallToolResult, error) {
	if srv.Session == nil {
		return nil, fmt.Errorf("MCP session is nil for server")
	}
	if timeout == 0 {
		timeout = srv.Config.Timeout
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	return srv.Session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
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

			if typ, ok := params["type"]; ok && typ == "object" {
				if _, hasProps := params["properties"]; !hasProps {
					params["properties"] = map[string]any{}
				}
			} else if len(params) == 0 {
				params["type"] = "object"
				params["properties"] = map[string]any{}
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

	result, err := srv.readResource(uri)
	if err != nil {
		if logger != nil {
			logger.Warn("MCP resource read failed, attempting reconnect", "server", serverName, "error", err)
		}
		if reconnectErr := reconnectMCPServer(serverName, context.Background()); reconnectErr != nil {
			return nil, fmt.Errorf("resource read failed (%v) and reconnect failed: %w", err, reconnectErr)
		}
		mcpServersMu.Lock()
		srv = mcpServers[serverName]
		mcpServersMu.Unlock()
		return srv.readResource(uri)
	}

	srv.reconnectMu.Lock()
	srv.reconnectCount = 0
	srv.reconnectMu.Unlock()

	return result, nil
}

func (srv *MCPServer) readResource(uri string) (*mcp.ReadResourceResult, error) {
	if srv.Session == nil {
		return nil, fmt.Errorf("MCP session is nil for server")
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

	result, err := srv.getPrompt(promptName, args)
	if err != nil {
		if logger != nil {
			logger.Warn("MCP prompt get failed, attempting reconnect", "server", serverName, "error", err)
		}
		if reconnectErr := reconnectMCPServer(serverName, context.Background()); reconnectErr != nil {
			return nil, fmt.Errorf("prompt get failed (%v) and reconnect failed: %w", err, reconnectErr)
		}
		mcpServersMu.Lock()
		srv = mcpServers[serverName]
		mcpServersMu.Unlock()
		return srv.getPrompt(promptName, args)
	}

	srv.reconnectMu.Lock()
	srv.reconnectCount = 0
	srv.reconnectMu.Unlock()

	return result, nil
}

func (srv *MCPServer) getPrompt(promptName string, args map[string]string) (*mcp.GetPromptResult, error) {
	if srv.Session == nil {
		return nil, fmt.Errorf("MCP session is nil for server")
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
