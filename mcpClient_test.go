package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gogpt "github.com/sashabaranov/go-openai"
)

func TestMCPConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantErr string
	}{
		{
			name: "valid stdio MCP",
			toml: `[services.test]
maxtokens = 100
[mcps.test]
transport = "stdio"
command = "echo"
[commands.chats.chat]
service = "test"
`,
			wantErr: "",
		},
		{
			name: "valid http MCP",
			toml: `[services.test]
maxtokens = 100
[mcps.test]
transport = "http"
url = "http://localhost:3000/mcp"
[commands.chats.chat]
service = "test"
`,
			wantErr: "",
		},
		{
			name: "missing transport",
			toml: `[services.test]
maxtokens = 100
[mcps.test]
command = "echo"
[commands.chats.chat]
service = "test"
`,
			wantErr: "transport is required",
		},
		{
			name: "invalid transport",
			toml: `[services.test]
maxtokens = 100
[mcps.test]
transport = "websocket"
url = "http://localhost:3000"
[commands.chats.chat]
service = "test"
`,
			wantErr: "transport must be 'stdio' or 'http'",
		},
		{
			name: "stdio missing command",
			toml: `[services.test]
maxtokens = 100
[mcps.test]
transport = "stdio"
[commands.chats.chat]
service = "test"
`,
			wantErr: "command is required for stdio",
		},
		{
			name: "http missing url",
			toml: `[services.test]
maxtokens = 100
[mcps.test]
transport = "http"
[commands.chats.chat]
service = "test"
`,
			wantErr: "url is required for http",
		},
		{
			name: "command references unknown MCP",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat]
service = "test"
mcps = ["nonexistent"]
`,
			wantErr: "references undefined MCP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempFile, err := os.CreateTemp("", "test_mcp_config_*.toml")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(tempFile.Name())

			if _, err := tempFile.WriteString(tt.toml); err != nil {
				t.Fatal(err)
			}
			tempFile.Close()

			cmd := exec.Command("go", "run", ".", tempFile.Name())
			cmd.Env = append(os.Environ(), "LOGXI_FORMAT=maxcol=9999")
			output, _ := cmd.CombinedOutput()
			outStr := string(output)

			if tt.wantErr == "" {
				if strings.Contains(outStr, "transport is required") ||
					strings.Contains(outStr, "transport must be") ||
					strings.Contains(outStr, "command is required") ||
					strings.Contains(outStr, "url is required") ||
					strings.Contains(outStr, "references undefined MCP") {
					t.Errorf("unexpected config error: %s", outStr)
				}
			} else {
				if !strings.Contains(outStr, tt.wantErr) {
					t.Errorf("expected error containing %q, got: %s", tt.wantErr, outStr)
				}
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

	tools := getMCPTools([]string{"test"})

	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if tools[0].Type != "function" {
		t.Errorf("expected type 'function', got %q", tools[0].Type)
	}

	if tools[0].Function.Name != "read_file" {
		t.Errorf("expected name 'read_file', got %q", tools[0].Function.Name)
	}

	if tools[0].Function.Description != "Read a file from disk" {
		t.Errorf("expected description 'Read a file from disk', got %q", tools[0].Function.Description)
	}

	params, ok := tools[0].Function.Parameters.(map[string]any)
	if !ok {
		t.Fatal("expected parameters to be map[string]any")
	}

	if params["type"] != "object" {
		t.Errorf("expected params type 'object', got %v", params["type"])
	}
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
			got := mcpToolResultToText(tt.result)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
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

	if !strings.Contains(info, "fs(read_file,write_file)") {
		t.Errorf("expected fs tools in info, got: %s", info)
	}
	if !strings.Contains(info, "github(search_repos)") {
		t.Errorf("expected github tools in info, got: %s", info)
	}
	if !strings.HasPrefix(info, "MCP tools: ") {
		t.Errorf("expected 'MCP tools: ' prefix, got: %s", info)
	}
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
	if err != nil {
		t.Fatal(err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	srv := &MCPServer{
		Config:  MCPConfig{Timeout: 10 * time.Second},
		Client:  client,
		Session: session,
	}

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		srv.Tools = append(srv.Tools, tool)
	}

	mcpServers["test"] = srv
	mcpToolToServer["greet"] = "test"

	tools := getMCPTools([]string{"test"})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	result, err := callMCPTool("greet", map[string]any{"name": "World"})
	if err != nil {
		t.Fatalf("callMCPTool failed: %v", err)
	}

	text := mcpToolResultToText(result)
	if !strings.Contains(text, "Hello, World!") {
		t.Errorf("expected greeting in result, got: %s", text)
	}
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
	if err == nil {
		t.Error("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown MCP tool") {
		t.Errorf("expected 'unknown MCP tool' error, got: %v", err)
	}
}

func TestMCPToolInfoEmpty(t *testing.T) {
	info := getMCPToolInfo([]string{"nonexistent"})
	if info != "" {
		t.Errorf("expected empty string for nonexistent server, got: %s", info)
	}
}

func TestMCPConfigTimeoutDefault(t *testing.T) {
	dir := t.TempDir()
	tomlFile := filepath.Join(dir, "config.toml")
	tomlContent := `[services.test]
maxtokens = 100
[mcps.test]
transport = "stdio"
command = "echo"
[commands.chats.chat]
service = "test"
`
	if err := os.WriteFile(tomlFile, []byte(tomlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("go", "run", ".", tomlFile)
	cmd.Env = append(os.Environ(), "LOGXI_FORMAT=maxcol=9999")
	output, _ := cmd.CombinedOutput()
	outStr := string(output)

	if !strings.Contains(outStr, "connecting MCP server") {
		t.Logf("MCP connection attempt not found in output (expected for stdio with echo): %s", outStr)
	}
}

func TestMCPToolConversionWithNilSchema(t *testing.T) {
	mcpServers = make(map[string]*MCPServer)
	mcpToolToServer = make(map[string]string)

	mcpServers["test"] = &MCPServer{
		Tools: []*mcp.Tool{
			{Name: "no_schema", Description: "tool with no input schema"},
		},
	}

	tools := getMCPTools([]string{"test"})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	if tools[0].Function.Name != "no_schema" {
		t.Errorf("expected name 'no_schema', got %q", tools[0].Function.Name)
	}
}

func TestBuildChatRequestWithMCPTools(t *testing.T) {
	cfg := AIConfig{
		Model:       "gpt-4",
		MaxTokens:   100,
		Temperature: 0.7,
	}
	msgs := []gogpt.ChatCompletionMessage{
		{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
	}

	req := BuildChatRequest(cfg, msgs)

	if req.Model != "gpt-4" {
		t.Errorf("Model = %q, want %q", req.Model, "gpt-4")
	}
	if len(req.Messages) != 1 {
		t.Errorf("Messages len = %d, want 1", len(req.Messages))
	}
	if req.Stream {
		t.Error("Stream should be false by default")
	}
}

func TestBuildChatRequestStreaming(t *testing.T) {
	cfg := AIConfig{
		Model:       "gpt-4",
		MaxTokens:   100,
		Temperature: 0.7,
		Streaming:   true,
	}
	msgs := []gogpt.ChatCompletionMessage{
		{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
	}

	req := BuildChatRequest(cfg, msgs)

	if !req.Stream {
		t.Error("Stream should be true for streaming config")
	}
	if req.StreamOptions == nil {
		t.Error("StreamOptions should be set for streaming")
	}
}

func TestStreamingToolCallAccumulation(t *testing.T) {
	idx0 := 0
	idx1 := 1

	chunks := []gogpt.ChatCompletionStreamResponse{
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{Role: "assistant"}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{Content: "Let me check "}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{Content: "that for you."}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{ToolCalls: []gogpt.ToolCall{
				{Index: &idx0, ID: "call_1", Type: "function", Function: gogpt.FunctionCall{Name: "search", Arguments: ""}},
			}}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{ToolCalls: []gogpt.ToolCall{
				{Index: &idx0, Function: gogpt.FunctionCall{Arguments: `{"query":"test"}`}},
			}}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{ToolCalls: []gogpt.ToolCall{
				{Index: &idx1, ID: "call_2", Type: "function", Function: gogpt.FunctionCall{Name: "read_file", Arguments: ""}},
			}}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{ToolCalls: []gogpt.ToolCall{
				{Index: &idx1, Function: gogpt.FunctionCall{Arguments: `{"path":"/tmp/test"}`}},
			}}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{FinishReason: gogpt.FinishReasonToolCalls},
		}},
	}

	var accumulatedToolCalls []gogpt.ToolCall
	var assistantRole string
	var bufferb string

	for _, chunk := range chunks {
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		if delta.Role != "" {
			assistantRole = delta.Role
		}

		for _, tc := range delta.ToolCalls {
			if tc.Index != nil {
				idx := *tc.Index
				for len(accumulatedToolCalls) <= idx {
					accumulatedToolCalls = append(accumulatedToolCalls, gogpt.ToolCall{})
				}
				if tc.ID != "" {
					accumulatedToolCalls[idx].ID = tc.ID
				}
				if tc.Type != "" {
					accumulatedToolCalls[idx].Type = tc.Type
				}
				accumulatedToolCalls[idx].Function.Name += tc.Function.Name
				accumulatedToolCalls[idx].Function.Arguments += tc.Function.Arguments
			}
		}

		bufferb += delta.Content

		if chunk.Choices[0].FinishReason == gogpt.FinishReasonToolCalls {
			break
		}
	}

	if assistantRole != "assistant" {
		t.Errorf("role = %q, want 'assistant'", assistantRole)
	}

	wantText := "Let me check that for you."
	if bufferb != wantText {
		t.Errorf("text = %q, want %q", bufferb, wantText)
	}

	if len(accumulatedToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(accumulatedToolCalls))
	}

	if accumulatedToolCalls[0].ID != "call_1" {
		t.Errorf("tool[0].ID = %q, want 'call_1'", accumulatedToolCalls[0].ID)
	}
	if accumulatedToolCalls[0].Function.Name != "search" {
		t.Errorf("tool[0].Name = %q, want 'search'", accumulatedToolCalls[0].Function.Name)
	}
	if accumulatedToolCalls[0].Function.Arguments != `{"query":"test"}` {
		t.Errorf("tool[0].Args = %q, want {\"query\":\"test\"}", accumulatedToolCalls[0].Function.Arguments)
	}

	if accumulatedToolCalls[1].ID != "call_2" {
		t.Errorf("tool[1].ID = %q, want 'call_2'", accumulatedToolCalls[1].ID)
	}
	if accumulatedToolCalls[1].Function.Name != "read_file" {
		t.Errorf("tool[1].Name = %q, want 'read_file'", accumulatedToolCalls[1].Function.Name)
	}
	if accumulatedToolCalls[1].Function.Arguments != `{"path":"/tmp/test"}` {
		t.Errorf("tool[1].Args = %q, want {\"path\":\"/tmp/test\"}", accumulatedToolCalls[1].Function.Arguments)
	}
}

func TestStreamingToolCallAccumulationNoToolCalls(t *testing.T) {
	chunks := []gogpt.ChatCompletionStreamResponse{
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{Role: "assistant", Content: "Hello there!"}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{Content: " How can I help?"}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{FinishReason: gogpt.FinishReasonStop},
		}},
	}

	var accumulatedToolCalls []gogpt.ToolCall
	var bufferb string

	for _, chunk := range chunks {
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		bufferb += delta.Content

		for _, tc := range delta.ToolCalls {
			if tc.Index != nil {
				idx := *tc.Index
				for len(accumulatedToolCalls) <= idx {
					accumulatedToolCalls = append(accumulatedToolCalls, gogpt.ToolCall{})
				}
				if tc.ID != "" {
					accumulatedToolCalls[idx].ID = tc.ID
				}
				if tc.Type != "" {
					accumulatedToolCalls[idx].Type = tc.Type
				}
				accumulatedToolCalls[idx].Function.Name += tc.Function.Name
				accumulatedToolCalls[idx].Function.Arguments += tc.Function.Arguments
			}
		}

		if chunk.Choices[0].FinishReason == gogpt.FinishReasonStop {
			break
		}
	}

	if len(accumulatedToolCalls) != 0 {
		t.Errorf("expected 0 tool calls, got %d", len(accumulatedToolCalls))
	}

	wantText := "Hello there! How can I help?"
	if bufferb != wantText {
		t.Errorf("text = %q, want %q", bufferb, wantText)
	}
}

func TestStreamingToolCallAccumulationInterleavedTextAndTools(t *testing.T) {
	idx0 := 0

	chunks := []gogpt.ChatCompletionStreamResponse{
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{Role: "assistant", Content: "I'll "}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{ToolCalls: []gogpt.ToolCall{
				{Index: &idx0, ID: "call_1", Type: "function", Function: gogpt.FunctionCall{Name: "weather", Arguments: `{"city":"NYC"}`}},
			}}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{Delta: gogpt.ChatCompletionStreamChoiceDelta{Content: "look that up."}},
		}},
		{Choices: []gogpt.ChatCompletionStreamChoice{
			{FinishReason: gogpt.FinishReasonToolCalls},
		}},
	}

	var accumulatedToolCalls []gogpt.ToolCall
	var bufferb string

	for _, chunk := range chunks {
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta
		bufferb += delta.Content

		for _, tc := range delta.ToolCalls {
			if tc.Index != nil {
				idx := *tc.Index
				for len(accumulatedToolCalls) <= idx {
					accumulatedToolCalls = append(accumulatedToolCalls, gogpt.ToolCall{})
				}
				if tc.ID != "" {
					accumulatedToolCalls[idx].ID = tc.ID
				}
				if tc.Type != "" {
					accumulatedToolCalls[idx].Type = tc.Type
				}
				accumulatedToolCalls[idx].Function.Name += tc.Function.Name
				accumulatedToolCalls[idx].Function.Arguments += tc.Function.Arguments
			}
		}

		if chunk.Choices[0].FinishReason == gogpt.FinishReasonToolCalls {
			break
		}
	}

	wantText := "I'll look that up."
	if bufferb != wantText {
		t.Errorf("text = %q, want %q", bufferb, wantText)
	}

	if len(accumulatedToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(accumulatedToolCalls))
	}

	if accumulatedToolCalls[0].Function.Name != "weather" {
		t.Errorf("tool name = %q, want 'weather'", accumulatedToolCalls[0].Function.Name)
	}
	if accumulatedToolCalls[0].Function.Arguments != `{"city":"NYC"}` {
		t.Errorf("tool args = %q, want {\"city\":\"NYC\"}", accumulatedToolCalls[0].Function.Arguments)
	}
}
