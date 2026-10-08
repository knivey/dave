package main

import (
	"encoding/json"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAsyncToolRoundTripWireShape pins that both serializers emit the
// synthetic pair in the exact shape providers validate: an assistant
// message carrying the tool call, then a tool message referencing its id.
func TestAsyncToolRoundTripWireShape(t *testing.T) {
	msgs, err := asyncToolRoundTrip(PendingJob{JobID: "j1", ToolName: "t"}, "result payload")
	require.NoError(t, err)

	// Chat Completions wire shape
	params := messagesToChatCompletionParams(msgs)
	require.Len(t, params, 2)
	first, err := json.Marshal(params[0])
	require.NoError(t, err)
	assert.Contains(t, string(first), `"role":"assistant"`)
	assert.Contains(t, string(first), `"job_status"`)
	second, err := json.Marshal(params[1])
	require.NoError(t, err)
	assert.Contains(t, string(second), `"role":"tool"`)
	assert.Contains(t, string(second), `"tool_call_id"`)

	// Responses API wire shape
	items := messagesToResponseInputItems(msgs)
	require.Len(t, items, 2)
	callJSON, err := json.Marshal(items[0])
	require.NoError(t, err)
	assert.Contains(t, string(callJSON), `"type":"function_call"`)
	assert.Contains(t, string(callJSON), `"job_status"`)
	outJSON, err := json.Marshal(items[1])
	require.NoError(t, err)
	assert.Contains(t, string(outJSON), `"type":"function_call_output"`)
}

// TestAsyncResultRoleTemplateBranch pins the template-engine if/else the
// async instructions use: system prompts can tell the model the truth
// about which role delivers background results (guidance Knob 1) via
// {{if eq .AsyncResultRole "user"}} … {{else}} … {{end}}.
func TestAsyncResultRoleTemplateBranch(t *testing.T) {
	tmpl := template.Must(template.New("sys").Parse(
		`Result arrives in {{if eq .AsyncResultRole "user"}}a user message{{else if eq .AsyncResultRole "developer"}}a developer message{{else}}a system message{{end}}.`))

	userCfg := AIConfig{InjectionRole: RoleUser}
	userCfg.SystemTmpl = tmpl
	assert.Equal(t, "Result arrives in a user message.",
		renderFreshSystemPrompt(userCfg, Network{}, nil, "#c", "nick", "fallback"))

	devCfg := AIConfig{InjectionRole: RoleDeveloper}
	devCfg.SystemTmpl = tmpl
	assert.Equal(t, "Result arrives in a developer message.",
		renderFreshSystemPrompt(devCfg, Network{}, nil, "#c", "nick", "fallback"))

	sysCfg := AIConfig{}
	sysCfg.SystemTmpl = tmpl
	assert.Equal(t, "Result arrives in a system message.",
		renderFreshSystemPrompt(sysCfg, Network{}, nil, "#c", "nick", "fallback"))

	toolCfg := AIConfig{AsyncResultDelivery: asyncDeliveryTool}
	toolTmpl := template.Must(template.New("sys").Parse(
		`Result arrives in {{if eq .AsyncResultRole "tool"}}a tool response{{else}}a message{{end}}.`))
	toolCfg.SystemTmpl = toolTmpl
	assert.Equal(t, "Result arrives in a tool response.",
		renderFreshSystemPrompt(toolCfg, Network{}, nil, "#c", "nick", "fallback"))
}

// TestAsyncResultDelivery pins the delivery-mode resolver and its
// interaction with the .AsyncResultRole template variable: "tool" mode
// reports "tool", everything else reports the guidance Knob 1 role.
func TestAsyncResultDelivery(t *testing.T) {
	tests := []struct {
		name            string
		cfg             AIConfig
		wantDelivery    string
		wantAsyncResult string
	}{
		{"empty defaults to message/system", AIConfig{}, asyncDeliveryMessage, RoleSystem},
		{"message passes through", AIConfig{AsyncResultDelivery: asyncDeliveryMessage}, asyncDeliveryMessage, RoleSystem},
		{"tool passes through", AIConfig{AsyncResultDelivery: asyncDeliveryTool}, asyncDeliveryTool, asyncDeliveryTool},
		{"unknown falls back to message", AIConfig{AsyncResultDelivery: "carrier-pigeon"}, asyncDeliveryMessage, RoleSystem},
		{"tool mode wins over injection_role", AIConfig{AsyncResultDelivery: asyncDeliveryTool, InjectionRole: RoleUser}, asyncDeliveryTool, asyncDeliveryTool},
		{"message mode uses injection_role", AIConfig{InjectionRole: RoleUser}, asyncDeliveryMessage, RoleUser},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantDelivery, asyncResultDelivery(tt.cfg))
			assert.Equal(t, tt.wantAsyncResult, asyncResultRole(tt.cfg))
		})
	}
}

// TestAsyncToolRoundTrip pins the synthetic round-trip shape: a complete
// assistant tool-call + tool-result pair with matching ids and the
// audit marker on the result.
func TestAsyncToolRoundTrip(t *testing.T) {
	job := PendingJob{JobID: "job-9", ToolName: "generate_image_async"}
	content := "[System: Background task completed — tool: generate_image_async, job: job-9. Result:\ndone]"

	msgs, err := asyncToolRoundTrip(job, content)
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	call := msgs[0]
	assert.Equal(t, RoleAssistant, call.Role)
	require.Len(t, call.ToolCalls, 1)
	assert.Equal(t, asyncJobStatusTool, call.ToolCalls[0].Function.Name)
	assert.Equal(t, "function", call.ToolCalls[0].Type)
	assert.JSONEq(t, `{"job_id":"job-9"}`, call.ToolCalls[0].Function.Arguments)
	assert.Regexp(t, `^call_[0-9a-f]{24}$`, call.ToolCalls[0].ID)

	result := msgs[1]
	assert.Equal(t, RoleTool, result.Role)
	assert.Equal(t, call.ToolCalls[0].ID, result.ToolCallID, "result must reference the synthetic call id")
	assert.Equal(t, content, result.Content)

	// ids are unique per round-trip
	other, err := asyncToolRoundTrip(job, content)
	require.NoError(t, err)
	assert.NotEqual(t, call.ToolCalls[0].ID, other[0].ToolCalls[0].ID)
}

// TestGuidanceRole pins Knob 1 resolution: valid values pass through,
// everything else (empty, unknown — hand-built configs that skipped
// load-time validation) falls back to system rather than sending garbage
// to the API.
func TestGuidanceRole(t *testing.T) {
	tests := []struct {
		name string
		cfg  AIConfig
		want string
	}{
		{"empty defaults to system", AIConfig{}, RoleSystem},
		{"system passes through", AIConfig{InjectionRole: RoleSystem}, RoleSystem},
		{"developer passes through", AIConfig{InjectionRole: RoleDeveloper}, RoleDeveloper},
		{"user passes through", AIConfig{InjectionRole: RoleUser}, RoleUser},
		{"unknown falls back to system", AIConfig{InjectionRole: "root"}, RoleSystem},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, guidanceRole(tt.cfg))
		})
	}
}

// TestNeedsUserSuffix pins Knob 2 resolution: explicit config wins in both
// directions (false suppresses even the anthropic auto-detect); nil falls
// to the model-based default.
func TestNeedsUserSuffix(t *testing.T) {
	tests := []struct {
		name string
		cfg  AIConfig
		want bool
	}{
		{"nil + anthropic auto-detects", AIConfig{Model: "anthropic/claude-sonnet-4.6"}, true},
		{"nil + plain model is false", AIConfig{Model: "qwen3"}, false},
		{"explicit true wins", AIConfig{Model: "qwen3", NeedsUserSuffix: boolPtr(true)}, true},
		{"explicit false suppresses anthropic", AIConfig{Model: "anthropic/claude-sonnet-4.6", NeedsUserSuffix: boolPtr(false)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, needsUserSuffix(tt.cfg))
		})
	}
}

// TestGuidanceMessages pins the two-row guidance shape: payload under
// Knob 1's role, user suffix only when trailing AND Knob 2 resolves true
// AND the payload is not already a user row.
func TestGuidanceMessages(t *testing.T) {
	const suffix = "SUFFIX"

	tests := []struct {
		name     string
		cfg      AIConfig
		trailing bool
		want     []ChatMessage
	}{
		{
			name:     "plain model, trailing: system payload only",
			cfg:      AIConfig{Model: "qwen3"},
			trailing: true,
			want:     []ChatMessage{{Role: RoleSystem, Content: "C"}},
		},
		{
			name:     "plain model, followed: system payload only",
			cfg:      AIConfig{Model: "anthropic/claude-sonnet-4.6"},
			trailing: false,
			want:     []ChatMessage{{Role: RoleSystem, Content: "C"}},
		},
		{
			name:     "anthropic, trailing: payload + suffix (two rows)",
			cfg:      AIConfig{Model: "anthropic/claude-sonnet-4.6"},
			trailing: true,
			want:     []ChatMessage{{Role: RoleSystem, Content: "C"}, {Role: RoleUser, Content: suffix}},
		},
		{
			name:     "user payload never gets a suffix",
			cfg:      AIConfig{Model: "anthropic/claude-sonnet-4.6", InjectionRole: RoleUser},
			trailing: true,
			want:     []ChatMessage{{Role: RoleUser, Content: "C"}},
		},
		{
			name:     "developer payload keeps its role with suffix",
			cfg:      AIConfig{Model: "anthropic/claude-sonnet-4.6", InjectionRole: RoleDeveloper},
			trailing: true,
			want:     []ChatMessage{{Role: RoleDeveloper, Content: "C"}, {Role: RoleUser, Content: suffix}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guidanceMessages(tt.cfg, "C", suffix, tt.trailing)
			require.Len(t, got, len(tt.want))
			for i := range tt.want {
				assert.Equal(t, tt.want[i].Role, got[i].Role)
				assert.Equal(t, tt.want[i].Content, got[i].Content)
			}
		})
	}
}

func TestMessagesEndWith(t *testing.T) {
	msgs := []ChatMessage{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleSystem, Content: "nudge"},
		{Role: RoleUser, Content: "go"},
	}

	assert.True(t, messagesEndWith(msgs, []ChatMessage{{Role: RoleUser, Content: "go"}}))
	assert.True(t, messagesEndWith(msgs, []ChatMessage{
		{Role: RoleSystem, Content: "nudge"},
		{Role: RoleUser, Content: "go"},
	}))
	assert.False(t, messagesEndWith(msgs, []ChatMessage{{Role: RoleSystem, Content: "go"}}), "role mismatch")
	assert.False(t, messagesEndWith(msgs, []ChatMessage{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleSystem, Content: "nudge"},
		{Role: RoleUser, Content: "go"},
		{Role: RoleUser, Content: "more"},
	}), "seq longer than msgs")
	assert.False(t, messagesEndWith(msgs, nil), "empty seq")
}
