package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var mcpServicesTOML = `
[test]
maxtokens = 100
`

func TestMCPConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		mcpsTOML  string
		chatsTOML string
		wantErr   string
	}{
		{
			name: "valid stdio MCP",
			mcpsTOML: `[test]
transport = "stdio"
command = "echo"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "",
		},
		{
			name: "valid http MCP",
			mcpsTOML: `[test]
transport = "http"
url = "http://localhost:3000/mcp"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "",
		},
		{
			name: "valid sse MCP",
			mcpsTOML: `[test]
transport = "sse"
url = "http://localhost:8081/sse"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "",
		},
		{
			name: "missing transport",
			mcpsTOML: `[test]
command = "echo"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "transport is required",
		},
		{
			name: "invalid transport",
			mcpsTOML: `[test]
transport = "websocket"
url = "http://localhost:3000"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "transport must be",
		},
		{
			name: "stdio missing command",
			mcpsTOML: `[test]
transport = "stdio"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "command is required for stdio",
		},
		{
			name: "http missing url",
			mcpsTOML: `[test]
transport = "http"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "url is required for http",
		},
		{
			name: "sse missing url",
			mcpsTOML: `[test]
transport = "sse"`,
			chatsTOML: `[chat]
service = "test"`,
			wantErr: "url is required for sse",
		},
		{
			name:     "command references unknown MCP",
			mcpsTOML: ``,
			chatsTOML: `[chat]
service = "test"
mcps = ["nonexistent"]`,
			wantErr: "references undefined MCP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extraFiles := map[string]string{
				"services.toml": mcpServicesTOML,
				"chats.toml":    tt.chatsTOML,
			}
			if tt.mcpsTOML != "" {
				extraFiles["mcps.toml"] = tt.mcpsTOML
			}
			dir := createTestConfigDir(t, "", extraFiles)
			defer os.RemoveAll(dir)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", "run", ".", dir)
			cmd.Env = append(os.Environ(), "LOGXI_FORMAT=maxcol=9999", "DAVE_NO_TUI=1")
			output, _ := cmd.CombinedOutput()
			outStr := string(output)

			if tt.wantErr == "" {
				assert.NotContains(t, outStr, "transport is required", "unexpected config error: %s", outStr)
				assert.NotContains(t, outStr, "transport must be", "unexpected config error: %s", outStr)
				assert.NotContains(t, outStr, "command is required", "unexpected config error: %s", outStr)
				assert.NotContains(t, outStr, "url is required", "unexpected config error: %s", outStr)
				assert.NotContains(t, outStr, "references undefined MCP", "unexpected config error: %s", outStr)
			} else {
				assert.Contains(t, outStr, tt.wantErr, "expected error containing %q, got: %s", tt.wantErr, outStr)
			}
		})
	}
}

func TestMCPToolSchemaConversion(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Config: MCPConfig{Timeout: 10 * time.Second},
		Tools: []*mcp.Tool{
			{
				Name:        "read_file",
				Description: "Read a file from disk",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "File path to read"},
					},
				},
			},
		},
	}

	mcpServers["test"] = srv
	mcpToolToServer["read_file"] = "test"

	tools := getMCPTools([]string{"test"}, nil)

	require.Len(t, tools, 1, "expected 1 tool")

	assert.Equal(t, "function", tools[0].Type, "type mismatch")
	assert.Equal(t, "read_file", tools[0].Function.Name, "name mismatch")
	assert.Equal(t, "Read a file from disk", tools[0].Function.Description, "description mismatch")

	params, ok := tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok, "expected parameters to be map[string]any")

	assert.Equal(t, "object", params["type"], "params type mismatch")
}

func TestMCPToolResultToText(t *testing.T) {
	tests := []struct {
		name   string
		result *mcp.CallToolResult
		want   string
	}{
		{
			name: "single text content",
			result: &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "hello world"},
				},
			},
			want: "hello world",
		},
		{
			name: "multiple text contents",
			result: &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "line1"},
					&mcp.TextContent{Text: "line2"},
				},
			},
			want: "line1\nline2",
		},
		{
			name: "empty result",
			result: &mcp.CallToolResult{
				Content: []mcp.Content{},
			},
			want: "(no output)",
		},
		{
			name: "image content",
			result: &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.ImageContent{MIMEType: "image/png"},
				},
			},
			want: "[image: image/png]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mcpToolResultToText(tt.result))
		})
	}
}

func TestGetMCPToolInfo(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	mcpServers["fs"] = &MCPServer{
		Tools: []*mcp.Tool{
			{Name: "read_file"},
			{Name: "write_file"},
		},
	}
	mcpToolToServer["read_file"] = "fs"
	mcpToolToServer["write_file"] = "fs"

	mcpServers["github"] = &MCPServer{
		Tools: []*mcp.Tool{
			{Name: "search_repos"},
		},
	}
	mcpToolToServer["search_repos"] = "github"

	info := getMCPToolInfo([]string{"fs", "github"})

	assert.Contains(t, info, "fs(read_file,write_file)", "expected fs tools in info")
	assert.Contains(t, info, "github(search_repos)", "expected github tools in info")
	assert.True(t, strings.HasPrefix(info, "MCP tools: "), "expected 'MCP tools: ' prefix, got: %s", info)
}

func TestMCPInMemoryIntegration(t *testing.T) {
	ctx := context.Background()

	type Input struct {
		Name string `json:"name" jsonschema:"the name to greet"`
	}
	type Output struct {
		Greeting string `json:"greeting" jsonschema:"the greeting"`
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "greet", Description: "say hi"}, func(ctx context.Context, req *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, Output, error) {
		return nil, Output{Greeting: "Hello, " + input.Name + "!"}, nil
	})

	t1, t2 := mcp.NewInMemoryTransports()

	_, err := server.Connect(ctx, t1, nil)
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	require.NoError(t, err)
	defer session.Close()

	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Config:  MCPConfig{Timeout: 10 * time.Second},
		Client:  client,
		Session: session,
	}

	for tool, err := range session.Tools(ctx, nil) {
		require.NoError(t, err)
		srv.Tools = append(srv.Tools, tool)
	}

	mcpServers["test"] = srv
	mcpToolToServer["greet"] = "test"

	tools := getMCPTools([]string{"test"}, nil)
	require.Len(t, tools, 1, "expected 1 tool")

	result, err := callMCPTool("greet", map[string]any{"name": "World"})
	require.NoError(t, err, "callMCPTool failed")

	text := mcpToolResultToText(result)
	assert.Contains(t, text, "Hello, World!", "expected greeting in result")
}

func TestMCPToolToServerMapping(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	mcpServers["serverA"] = &MCPServer{
		Config: MCPConfig{Timeout: 5 * time.Second},
		Tools:  []*mcp.Tool{{Name: "tool_a"}},
	}
	mcpToolToServer["tool_a"] = "serverA"

	mcpServers["serverB"] = &MCPServer{
		Config: MCPConfig{Timeout: 5 * time.Second},
		Tools:  []*mcp.Tool{{Name: "tool_b"}},
	}
	mcpToolToServer["tool_b"] = "serverB"

	_, err := callMCPTool("unknown_tool", nil)
	assert.Error(t, err, "expected error for unknown tool")
	assert.Contains(t, err.Error(), "unknown MCP tool", "expected 'unknown MCP tool' error")
}

func TestMCPToolInfoEmpty(t *testing.T) {
	assert.Equal(t, "", getMCPToolInfo([]string{"nonexistent"}), "expected empty string for nonexistent server")
}

func TestMCPConfigTimeoutDefault(t *testing.T) {
	dir := t.TempDir()
	mcpsTOML := `
[test]
transport = "stdio"
command = "echo"
`
	chatsTOML := `[chat]
service = "test"
`
	servicesTOML := `
[test]
maxtokens = 100
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(""), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcps.toml"), []byte(mcpsTOML), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chats.toml"), []byte(chatsTOML), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "services.toml"), []byte(servicesTOML), 0644))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", dir)
	cmd.Env = append(os.Environ(), "LOGXI_FORMAT=maxcol=9999", "DAVE_NO_TUI=1")
	output, _ := cmd.CombinedOutput()
	outStr := string(output)

	assert.Contains(t, outStr, "connecting MCP server", "MCP connection attempt not found in output")
}

func TestMCPToolConversionWithNilSchema(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	mcpServers["test"] = &MCPServer{
		Tools: []*mcp.Tool{
			{Name: "no_schema", Description: "tool with no input schema"},
		},
	}

	tools := getMCPTools([]string{"test"}, nil)
	require.Len(t, tools, 1, "expected 1 tool")

	assert.Equal(t, "no_schema", tools[0].Function.Name, "name mismatch")

	params, ok := tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok, "expected parameters to be map[string]any")
	assert.Equal(t, "object", params["type"], "params type mismatch")
	assert.Contains(t, params, "properties", "expected params to have 'properties' key for nil schema")
}

func TestMCPToolConversionObjectWithoutProperties(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	mcpServers["test"] = &MCPServer{
		Tools: []*mcp.Tool{
			{
				Name:        "list_enhancements",
				Description: "List available prompt enhancement profiles",
				InputSchema: map[string]any{
					"type": "object",
				},
			},
		},
	}

	tools := getMCPTools([]string{"test"}, nil)
	require.Len(t, tools, 1, "expected 1 tool")

	params, ok := tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok, "expected parameters to be map[string]any")
	assert.Equal(t, "object", params["type"], "params type mismatch")
	assert.Contains(t, params, "properties", "expected params to have 'properties' key added for object schema without one")
	props, ok := params["properties"].(map[string]any)
	require.True(t, ok, "expected properties to be map[string]any")
	assert.Empty(t, props, "expected empty properties map")
}

func TestGetMCPToolsHidden(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Tools: []*mcp.Tool{
			{Name: "generate_image", Description: "Generate an image"},
			{Name: "wait_for_job", Description: "Wait for a job"},
			{Name: "cancel_job", Description: "Cancel a job"},
			{Name: "get_job_status", Description: "Get job status"},
		},
	}
	mcpServers["img-mcp"] = srv
	mcpToolToServer["generate_image"] = "img-mcp"
	mcpToolToServer["wait_for_job"] = "img-mcp"
	mcpToolToServer["cancel_job"] = "img-mcp"
	mcpToolToServer["get_job_status"] = "img-mcp"
	defer func() {
		delete(mcpServers, "img-mcp")
		delete(mcpToolToServer, "generate_image")
		delete(mcpToolToServer, "wait_for_job")
		delete(mcpToolToServer, "cancel_job")
		delete(mcpToolToServer, "get_job_status")
	}()

	tests := []struct {
		name      string
		servers   []string
		hidden    []string
		wantNames []string
	}{
		{
			name:      "no hidden",
			servers:   []string{"img-mcp"},
			hidden:    nil,
			wantNames: []string{"generate_image", "wait_for_job", "cancel_job", "get_job_status"},
		},
		{
			name:      "hide by bare name",
			servers:   []string{"img-mcp"},
			hidden:    []string{"wait_for_job", "cancel_job"},
			wantNames: []string{"generate_image", "get_job_status"},
		},
		{
			name:      "hide by server prefixed name",
			servers:   []string{"img-mcp"},
			hidden:    []string{"img-mcp.wait_for_job"},
			wantNames: []string{"generate_image", "cancel_job", "get_job_status"},
		},
		{
			name:      "server prefixed wrong server does not hide",
			servers:   []string{"img-mcp"},
			hidden:    []string{"other.wait_for_job"},
			wantNames: []string{"generate_image", "wait_for_job", "cancel_job", "get_job_status"},
		},
		{
			name:      "hide all",
			servers:   []string{"img-mcp"},
			hidden:    []string{"generate_image", "wait_for_job", "cancel_job", "get_job_status"},
			wantNames: nil,
		},
		{
			name:      "unknown server returns nothing",
			servers:   []string{"nonexistent"},
			hidden:    nil,
			wantNames: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tools := getMCPTools(tt.servers, tt.hidden)
			var names []string
			for _, tool := range tools {
				names = append(names, tool.Function.Name)
			}
			assert.Equal(t, tt.wantNames, names)
		})
	}
}

func TestToolCallAccumulation(t *testing.T) {
	type chunkDelta struct {
		role      string
		content   string
		toolCalls []ToolCall
	}
	type chunk struct {
		delta        chunkDelta
		finishReason string
	}

	tests := []struct {
		name              string
		chunks            []chunk
		wantText          string
		wantToolCallCount int
		wantToolCalls     []ToolCall
	}{
		{
			name: "multiple tool calls accumulated",
			chunks: []chunk{
				{delta: chunkDelta{role: "assistant"}},
				{delta: chunkDelta{content: "Let me check "}},
				{delta: chunkDelta{content: "that for you."}},
				{delta: chunkDelta{toolCalls: []ToolCall{
					{Index: 0, ID: "call_1", Type: "function", Function: FunctionCall{Name: "search", Arguments: ""}},
				}}},
				{delta: chunkDelta{toolCalls: []ToolCall{
					{Index: 0, Function: FunctionCall{Arguments: `{"query":"test"}`}},
				}}},
				{delta: chunkDelta{toolCalls: []ToolCall{
					{Index: 1, ID: "call_2", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: ""}},
				}}},
				{delta: chunkDelta{toolCalls: []ToolCall{
					{Index: 1, Function: FunctionCall{Arguments: `{"path":"/tmp/test"}`}},
				}}},
				{finishReason: "tool_calls"},
			},
			wantText:          "Let me check that for you.",
			wantToolCallCount: 2,
			wantToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: FunctionCall{Name: "search", Arguments: `{"query":"test"}`}},
				{ID: "call_2", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"/tmp/test"}`}},
			},
		},
		{
			name: "no tool calls",
			chunks: []chunk{
				{delta: chunkDelta{role: "assistant", content: "Hello there!"}},
				{delta: chunkDelta{content: " How can I help?"}},
				{finishReason: "stop"},
			},
			wantText:          "Hello there! How can I help?",
			wantToolCallCount: 0,
		},
		{
			name: "interleaved text and tools",
			chunks: []chunk{
				{delta: chunkDelta{role: "assistant", content: "I'll "}},
				{delta: chunkDelta{toolCalls: []ToolCall{
					{Index: 0, ID: "call_1", Type: "function", Function: FunctionCall{Name: "weather", Arguments: `{"city":"NYC"}`}},
				}}},
				{delta: chunkDelta{content: "look that up."}},
				{finishReason: "tool_calls"},
			},
			wantText:          "I'll look that up.",
			wantToolCallCount: 1,
			wantToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: FunctionCall{Name: "weather", Arguments: `{"city":"NYC"}`}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var accumulated []ToolCall
			var bufferb string

			for _, c := range tt.chunks {
				bufferb += c.delta.content

				for _, tc := range c.delta.toolCalls {
					idx := tc.Index
					for len(accumulated) <= idx {
						accumulated = append(accumulated, ToolCall{})
					}
					if tc.ID != "" {
						accumulated[idx].ID = tc.ID
					}
					if tc.Type != "" {
						accumulated[idx].Type = tc.Type
					}
					accumulated[idx].Function.Name += tc.Function.Name
					accumulated[idx].Function.Arguments += tc.Function.Arguments
				}

				if c.finishReason == "tool_calls" || c.finishReason == "stop" {
					break
				}
			}

			assert.Equal(t, tt.wantText, bufferb, "text mismatch")

			require.Len(t, accumulated, tt.wantToolCallCount, "tool call count mismatch")

			for i, want := range tt.wantToolCalls {
				if i >= len(accumulated) {
					break
				}
				assert.Equal(t, want.ID, accumulated[i].ID, "tool[%d].ID mismatch", i)
				assert.Equal(t, want.Function.Name, accumulated[i].Function.Name, "tool[%d].Name mismatch", i)
				assert.Equal(t, want.Function.Arguments, accumulated[i].Function.Arguments, "tool[%d].Args mismatch", i)
			}
		})
	}
}

func TestReconnectBackoff(t *testing.T) {
	tests := []struct {
		count int
		min   time.Duration
		max   time.Duration
	}{
		{0, 0, 0},
		{1, 1 * time.Second, 2 * time.Second},
		{2, 2 * time.Second, 4 * time.Second},
		{3, 4 * time.Second, 8 * time.Second},
		{6, 32 * time.Second, 60 * time.Second},
		{10, 60 * time.Second, 60*time.Second + 30*time.Second},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("count_%d", tt.count), func(t *testing.T) {
			got := reconnectBackoff(tt.count)
			assert.GreaterOrEqual(t, got, tt.min, "reconnectBackoff(%d) too low", tt.count)
			assert.LessOrEqual(t, got, tt.max, "reconnectBackoff(%d) too high", tt.count)
		})
	}
}

func TestCallMCPToolAutoReconnect(t *testing.T) {
	ctx := context.Background()

	type Input struct {
		Name string `json:"name" jsonschema:"the name to greet"`
	}
	type Output struct {
		Greeting string `json:"greeting" jsonschema:"the greeting"`
	}

	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	mcpCfg := MCPConfig{Transport: "http", Timeout: 10 * time.Second}

	srv := &MCPServer{
		Config:  mcpCfg,
		Client:  nil,
		Session: nil,
		Tools:   []*mcp.Tool{{Name: "greet"}},
	}
	mcpServers["test"] = srv
	mcpToolToServer["greet"] = "test"

	createSession := func() (*mcp.Client, *mcp.ClientSession) {
		server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
		mcp.AddTool(server, &mcp.Tool{Name: "greet", Description: "say hi"}, func(ctx context.Context, req *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, Output, error) {
			return nil, Output{Greeting: "Hello, " + input.Name + "!"}, nil
		})

		t1, t2 := mcp.NewInMemoryTransports()
		_, err := server.Connect(ctx, t1, nil)
		require.NoError(t, err)

		client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, t2, nil)
		require.NoError(t, err)
		return client, session
	}

	client, session := createSession()
	srv.Client = client
	srv.Session = session

	result, err := callMCPTool("greet", map[string]any{"name": "before_disconnect"})
	require.NoError(t, err, "callMCPTool before disconnect failed")
	text := mcpToolResultToText(result)
	assert.Contains(t, text, "Hello, before_disconnect!", "expected greeting before disconnect")

	session.Close()

	createAndSwapSession := func() {
		newClient, newSession := createSession()

		mcpServersMu.Lock()
		srv.Client = newClient
		srv.Session = newSession
		srv.Tools = []*mcp.Tool{{Name: "greet"}}
		for _, tool := range srv.Tools {
			mcpToolToServer[tool.Name] = "test"
		}
		mcpServersMu.Unlock()
	}

	origConnectMCPServer := connectMCPServerImpl
	connectMCPServerImpl = func(name string, cfg MCPConfig) (*MCPServer, error) {
		newClient, newSession := createSession()
		return &MCPServer{
			Config:  cfg,
			Client:  newClient,
			Session: newSession,
			Tools:   []*mcp.Tool{{Name: "greet"}},
		}, nil
	}
	defer func() { connectMCPServerImpl = origConnectMCPServer }()

	_ = createAndSwapSession

	mcpServersMu.Lock()
	srv.Session = nil
	mcpServersMu.Unlock()

	_, err = callMCPTool("greet", map[string]any{"name": "after_disconnect"})
	require.NoError(t, err, "callMCPTool after disconnect should have reconnected")

	mcpServersMu.Lock()
	newSrv := mcpServers["test"]
	mcpServersMu.Unlock()
	assert.NotNil(t, newSrv.Session, "expected session to be re-established after reconnect")
	assert.Equal(t, 0, newSrv.reconnectCount, "expected reconnectCount to be reset to 0")
}

func TestConcurrentReconnect(t *testing.T) {
	ctx := context.Background()

	type Input struct {
		Name string `json:"name" jsonschema:"the name to greet"`
	}
	type Output struct {
		Greeting string `json:"greeting" jsonschema:"the greeting"`
	}

	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	mcpCfg := MCPConfig{Transport: "http", Timeout: 10 * time.Second}

	srv := &MCPServer{
		Config:  mcpCfg,
		Client:  nil,
		Session: nil,
		Tools:   []*mcp.Tool{{Name: "greet"}},
	}
	mcpServers["test"] = srv
	mcpToolToServer["greet"] = "test"

	origConnectMCPServer := connectMCPServerImpl
	defer func() { connectMCPServerImpl = origConnectMCPServer }()

	connectMCPServerImpl = func(name string, cfg MCPConfig) (*MCPServer, error) {
		server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
		mcp.AddTool(server, &mcp.Tool{Name: "greet", Description: "say hi"}, func(ctx context.Context, req *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, Output, error) {
			return nil, Output{Greeting: "Hello, " + input.Name + "!"}, nil
		})

		t1, t2 := mcp.NewInMemoryTransports()
		_, err := server.Connect(ctx, t1, nil)
		if err != nil {
			return nil, err
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, t2, nil)
		if err != nil {
			return nil, err
		}

		return &MCPServer{
			Config:  cfg,
			Client:  client,
			Session: session,
			Tools:   []*mcp.Tool{{Name: "greet"}},
		}, nil
	}

	var wg sync.WaitGroup
	errors := make([]error, 3)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := callMCPTool("greet", map[string]any{"name": "concurrent"})
			errors[idx] = err
		}(i)
	}
	wg.Wait()

	successCount := 0
	for i, err := range errors {
		if err == nil {
			successCount++
		} else {
			t.Logf("goroutine %d error: %v", i, err)
		}
	}

	assert.Greater(t, successCount, 0, "expected at least one successful tool call after concurrent reconnect")
}

func saveAndResetMCPServers(t *testing.T) {
	t.Helper()
	mcpServersMu.Lock()
	oldServers := mcpServers
	oldToolMap := mcpToolToServer
	mcpServersMu.Unlock()
	t.Cleanup(func() {
		mcpServersMu.Lock()
		mcpServers = oldServers
		mcpToolToServer = oldToolMap
		mcpServersMu.Unlock()
	})
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)
}

func TestStripInjectFieldsFromSchema(t *testing.T) {
	tests := []struct {
		name   string
		input  map[string]any
		expect map[string]any
	}{
		{
			name: "strips inject fields from properties and required",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":                  map[string]any{"type": "string"},
					"_dave_inject_network": map[string]any{"type": "string"},
					"_dave_inject_channel": map[string]any{"type": "string"},
					"_dave_inject_user_id": map[string]any{"type": "integer"},
					"_dave_inject_nick":    map[string]any{"type": "string"},
				},
				"required": []any{"key", "_dave_inject_network", "_dave_inject_channel", "_dave_inject_user_id", "_dave_inject_nick"},
			},
			expect: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{"type": "string"},
				},
				"required": []any{"key"},
			},
		},
		{
			name: "no inject fields leaves schema unchanged",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer"},
				},
				"required": []any{"query"},
			},
			expect: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
					"limit": map[string]any{"type": "integer"},
				},
				"required": []any{"query"},
			},
		},
		{
			name:   "empty schema",
			input:  map[string]any{},
			expect: map[string]any{},
		},
		{
			name: "missing required array",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"_dave_inject_network": map[string]any{"type": "string"},
				},
			},
			expect: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stripInjectFieldsFromSchema(tt.input)
			assert.Equal(t, tt.expect, tt.input)
		})
	}
}

func TestGetMCPToolsStripInjectFields(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Config: MCPConfig{Timeout: 10 * time.Second},
		Tools: []*mcp.Tool{
			{
				Name:        "db_query",
				Description: "Query the database",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sql":                  map[string]any{"type": "string"},
						"_dave_inject_network": map[string]any{"type": "string"},
						"_dave_inject_channel": map[string]any{"type": "string"},
						"_dave_inject_user_id": map[string]any{"type": "integer"},
						"_dave_inject_nick":    map[string]any{"type": "string"},
					},
					"required": []any{"sql", "_dave_inject_network", "_dave_inject_channel"},
				},
			},
		},
	}
	mcpServers["db-mcp"] = srv
	mcpToolToServer["db_query"] = "db-mcp"

	tools := getMCPTools([]string{"db-mcp"}, nil)
	require.Len(t, tools, 1)

	params := tools[0].Function.Parameters.(map[string]any)
	props := params["properties"].(map[string]any)

	assert.Contains(t, props, "sql")
	assert.NotContains(t, props, "_dave_inject_network")
	assert.NotContains(t, props, "_dave_inject_channel")
	assert.NotContains(t, props, "_dave_inject_user_id")
	assert.NotContains(t, props, "_dave_inject_nick")

	required := params["required"].([]any)
	assert.Equal(t, []any{"sql"}, required)
}

func TestGetToolInjectFields(t *testing.T) {
	saveAndResetMCPServers(t)

	mcpServers["db-mcp"] = &MCPServer{
		Config: MCPConfig{Timeout: 10 * time.Second},
		Tools: []*mcp.Tool{
			{
				Name: "db_query",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sql":                  map[string]any{"type": "string"},
						"_dave_inject_network": map[string]any{"type": "string"},
						"_dave_inject_channel": map[string]any{"type": "string"},
						"_dave_inject_user_id": map[string]any{"type": "integer"},
						"_dave_inject_nick":    map[string]any{"type": "string"},
					},
				},
			},
			{
				Name: "db_no_inject",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sql": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	mcpToolToServer["db_query"] = "db-mcp"
	mcpToolToServer["db_no_inject"] = "db-mcp"

	t.Run("returns inject fields for tool with inject fields", func(t *testing.T) {
		fields := getToolInjectFields("db_query")
		require.NotNil(t, fields)
		assert.Equal(t, "network", fields["_dave_inject_network"])
		assert.Equal(t, "channel", fields["_dave_inject_channel"])
		assert.Equal(t, "user_id", fields["_dave_inject_user_id"])
		assert.Equal(t, "nick", fields["_dave_inject_nick"])
	})

	t.Run("returns nil for tool without inject fields", func(t *testing.T) {
		fields := getToolInjectFields("db_no_inject")
		assert.Nil(t, fields)
	})

	t.Run("returns nil for unknown tool", func(t *testing.T) {
		fields := getToolInjectFields("nonexistent_tool")
		assert.Nil(t, fields)
	})
}

func TestInjectScopeArgs(t *testing.T) {
	saveAndResetMCPServers(t)

	mcpServers["db-mcp"] = &MCPServer{
		Config: MCPConfig{Timeout: 10 * time.Second},
		Tools: []*mcp.Tool{
			{
				Name: "db_query",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sql":                  map[string]any{"type": "string"},
						"_dave_inject_network": map[string]any{"type": "string"},
						"_dave_inject_channel": map[string]any{"type": "string"},
						"_dave_inject_user_id": map[string]any{"type": "integer"},
						"_dave_inject_nick":    map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	mcpToolToServer["db_query"] = "db-mcp"

	cr := &chatRunner{
		network: Network{Name: "Libera"},
		channel: "#test",
		nick:    "testuser",
		userID:  42,
		logger:  logxi.New("test"),
	}

	toolArgs := map[string]any{
		"sql": "SELECT * FROM users",
	}

	injectScopeArgs(toolArgs, "db_query", cr)

	assert.Equal(t, "SELECT * FROM users", toolArgs["sql"])
	assert.Equal(t, "Libera", toolArgs["_dave_inject_network"])
	assert.Equal(t, "#test", toolArgs["_dave_inject_channel"])
	assert.Equal(t, int64(42), toolArgs["_dave_inject_user_id"])
	assert.Equal(t, "testuser", toolArgs["_dave_inject_nick"])
}

func TestInjectScopeArgs_NoInjectFields(t *testing.T) {
	saveAndResetMCPServers(t)

	mcpServers["db-mcp"] = &MCPServer{
		Config: MCPConfig{Timeout: 10 * time.Second},
		Tools: []*mcp.Tool{
			{
				Name: "db_query",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sql": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
	mcpToolToServer["db_query"] = "db-mcp"

	cr := &chatRunner{
		network: Network{Name: "Libera"},
		channel: "#test",
		nick:    "testuser",
		userID:  42,
		logger:  logxi.New("test"),
	}

	toolArgs := map[string]any{
		"sql": "SELECT * FROM users",
	}

	injectScopeArgs(toolArgs, "db_query", cr)

	assert.Equal(t, "SELECT * FROM users", toolArgs["sql"])
	assert.Len(t, toolArgs, 1)
}

func TestSignalMCPServer_UnknownServer(t *testing.T) {
	saveAndResetMCPServers(t)

	_, err := signalMCPServer("nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown MCP server")
}

func TestSignalMCPServer_Stdio_SendsSIGHUP(t *testing.T) {
	saveAndResetMCPServers(t)

	cmd := exec.Command("sleep", "999")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cmd.Process.Kill() })

	srv := &MCPServer{
		Config: MCPConfig{Transport: "stdio"},
		cmd:    cmd,
	}
	mcpServersMu.Lock()
	mcpServers["img-mcp"] = srv
	mcpServersMu.Unlock()

	result, err := signalMCPServer("img-mcp")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Warnings)
}

func TestSignalMCPServer_Stdio_NoProcess(t *testing.T) {
	saveAndResetMCPServers(t)

	srv := &MCPServer{
		Config: MCPConfig{Transport: "stdio"},
		cmd:    nil,
	}
	mcpServersMu.Lock()
	mcpServers["img-mcp"] = srv
	mcpServersMu.Unlock()

	_, err := signalMCPServer("img-mcp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no process")
}

func TestSignalMCPServerHTTP_Success(t *testing.T) {
	saveAndResetMCPServers(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/admin/reload", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok"}`)
	}))
	defer server.Close()

	srv := &MCPServer{
		Config: MCPConfig{Transport: "http", URL: server.URL + "/mcp"},
	}
	mcpServersMu.Lock()
	mcpServers["img-mcp"] = srv
	mcpServersMu.Unlock()

	result, err := signalMCPServer("img-mcp")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Warnings)
}

func TestSignalMCPServerHTTP_WithWarnings(t *testing.T) {
	saveAndResetMCPServers(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","warnings":["queue.max_workers changed from 1 to 4: requires restart"]}`)
	}))
	defer server.Close()

	srv := &MCPServer{
		Config: MCPConfig{Transport: "http", URL: server.URL + "/mcp"},
	}
	mcpServersMu.Lock()
	mcpServers["img-mcp"] = srv
	mcpServersMu.Unlock()

	result, err := signalMCPServer("img-mcp")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "max_workers")
}

func TestSignalMCPServerHTTP_ErrorResponse(t *testing.T) {
	saveAndResetMCPServers(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"error","message":"config error: comfy.baseurl is required"}`)
	}))
	defer server.Close()

	srv := &MCPServer{
		Config: MCPConfig{Transport: "http", URL: server.URL + "/mcp"},
	}
	mcpServersMu.Lock()
	mcpServers["img-mcp"] = srv
	mcpServersMu.Unlock()

	_, err := signalMCPServer("img-mcp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config error")
}

func TestSignalMCPServerHTTP_ConnectionFailed(t *testing.T) {
	saveAndResetMCPServers(t)

	srv := &MCPServer{
		Config: MCPConfig{Transport: "http", URL: "http://127.0.0.1:1/mcp"},
	}
	mcpServersMu.Lock()
	mcpServers["img-mcp"] = srv
	mcpServersMu.Unlock()

	_, err := signalMCPServer("img-mcp")
	require.Error(t, err)
}

func TestSignalMCPServerHTTP_Non200Status(t *testing.T) {
	saveAndResetMCPServers(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "internal error")
	}))
	defer server.Close()

	srv := &MCPServer{
		Config: MCPConfig{Transport: "http", URL: server.URL + "/mcp"},
	}
	mcpServersMu.Lock()
	mcpServers["img-mcp"] = srv
	mcpServersMu.Unlock()

	_, err := signalMCPServer("img-mcp")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestMCPServerCmd_Stdio(t *testing.T) {
	saveAndResetMCPServers(t)

	origImpl := connectMCPServerImpl
	defer func() { connectMCPServerImpl = origImpl }()

	var capturedCmd *exec.Cmd
	connectMCPServerImpl = func(name string, cfg MCPConfig) (*MCPServer, error) {
		srv := &MCPServer{
			Config: cfg,
		}
		if cfg.Transport == "stdio" {
			cmd := exec.Command("echo", "test")
			srv.cmd = cmd
			capturedCmd = cmd
		}
		return srv, nil
	}

	mcpCfg := MCPConfig{Transport: "stdio", Command: "echo", Args: []string{"test"}}
	srv, err := connectMCPServerImpl("test", mcpCfg)
	require.NoError(t, err)
	assert.NotNil(t, srv.cmd, "cmd should be set for stdio transport")
	assert.NotNil(t, capturedCmd)
}

func TestMCPServerCmd_HTTP(t *testing.T) {
	saveAndResetMCPServers(t)

	origImpl := connectMCPServerImpl
	defer func() { connectMCPServerImpl = origImpl }()

	connectMCPServerImpl = func(name string, cfg MCPConfig) (*MCPServer, error) {
		srv := &MCPServer{
			Config: cfg,
		}
		return srv, nil
	}

	mcpCfg := MCPConfig{Transport: "http", URL: "http://localhost:8080/mcp"}
	srv, err := connectMCPServerImpl("test", mcpCfg)
	require.NoError(t, err)
	assert.Nil(t, srv.cmd, "cmd should be nil for http transport")
}

func TestHeaderTransport(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ht := &headerTransport{
		base: http.DefaultTransport,
		headers: map[string]string{
			"X-API-Key":     "dave_testkey123",
			"X-Custom-Auth": "bearer-token",
		},
	}

	client := &http.Client{Transport: ht}
	req, err := http.NewRequest("GET", server.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "dave_testkey123", gotHeaders.Get("X-API-Key"))
	assert.Equal(t, "bearer-token", gotHeaders.Get("X-Custom-Auth"))
}

func TestHeaderTransport_EmptyHeaders(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ht := &headerTransport{
		base:    http.DefaultTransport,
		headers: map[string]string{},
	}

	client := &http.Client{Transport: ht}
	req, err := http.NewRequest("GET", server.URL, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Empty(t, gotHeaders.Get("X-API-Key"))
}

func TestMCPConfigHeadersParsed(t *testing.T) {
	dir := t.TempDir()
	mcpsTOML := `
[test]
transport = "http"
url = "http://localhost:8081/message"
headers = { "X-API-Key" = "dave_abc123", "Authorization" = "Bearer token456" }
`
	chatsTOML := `[chat]
service = "test"
`
	servicesTOML := `
[test]
maxtokens = 100
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(""), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mcps.toml"), []byte(mcpsTOML), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "chats.toml"), []byte(chatsTOML), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "services.toml"), []byte(servicesTOML), 0644))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", dir)
	cmd.Env = append(os.Environ(), "LOGXI_FORMAT=maxcol=9999", "DAVE_NO_TUI=1")
	output, _ := cmd.CombinedOutput()
	outStr := string(output)

	assert.NotContains(t, outStr, "transport is required", "unexpected config error: %s", outStr)
	assert.NotContains(t, outStr, "url is required", "unexpected config error: %s", outStr)
}

func TestSignalMCPServerHTTP_SendsHeaders(t *testing.T) {
	var gotHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	srv := &MCPServer{
		Config: MCPConfig{
			Transport: "http",
			URL:       server.URL + "/sync",
			Headers: map[string]string{
				"X-API-Key":     "test-secret-123",
				"X-Custom-Auth": "custom-value",
			},
		},
	}

	result, err := signalMCPServerHTTP("test-server", srv)
	require.NoError(t, err)
	assert.Empty(t, result.Warnings)
	assert.Equal(t, "test-secret-123", gotHeaders.Get("X-API-Key"))
	assert.Equal(t, "custom-value", gotHeaders.Get("X-Custom-Auth"))
}

func TestSignalMCPServerHTTP_NoHeaders(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	srv := &MCPServer{
		Config: MCPConfig{
			Transport: "http",
			URL:       server.URL + "/sync",
		},
	}

	result, err := signalMCPServerHTTP("test-server", srv)
	require.NoError(t, err)
	assert.True(t, called)
	assert.Empty(t, result.Warnings)
}

func TestSignalMCPServerHTTP_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "correct-key" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("unauthorized"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	t.Run("wrong key returns error", func(t *testing.T) {
		srv := &MCPServer{
			Config: MCPConfig{
				Transport: "http",
				URL:       server.URL + "/sync",
				Headers:   map[string]string{"X-API-Key": "wrong-key"},
			},
		}
		_, err := signalMCPServerHTTP("test-server", srv)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
	})

	t.Run("correct key succeeds", func(t *testing.T) {
		srv := &MCPServer{
			Config: MCPConfig{
				Transport: "http",
				URL:       server.URL + "/sync",
				Headers:   map[string]string{"X-API-Key": "correct-key"},
			},
		}
		result, err := signalMCPServerHTTP("test-server", srv)
		require.NoError(t, err)
		assert.Empty(t, result.Warnings)
	})
}
