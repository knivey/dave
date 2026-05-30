package main

import (
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteToolTemplate(t *testing.T) {
	tests := []struct {
		name         string
		template     string
		toolResult   string
		wantContains []string
	}{
		{
			name:         "SimpleTemplate",
			template:     "Queued: {{.queued}}, Running: {{.running}}",
			toolResult:   `{"queued": 0, "running": 1}`,
			wantContains: []string{"Queued: 0, Running: 1"},
		},
		{
			name:         "TemplateWithTable",
			template:     "Jobs:{{table .jobs \"id,status\"}}",
			toolResult:   `{"jobs": [{"id": "abc", "status": "running"}]}`,
			wantContains: []string{"abc", "running"},
		},
		{
			name:         "TemplateWithContext",
			template:     "Nick: {{._nick}}, Channel: {{._channel}}",
			toolResult:   `{"ok": true}`,
			wantContains: []string{"Nick: testnick", "Channel: #test"},
		},
		{
			name:         "TemplateTrimSpace",
			template:     "\n\nResult: {{.count}}\n\n",
			toolResult:   `{"count": 5}`,
			wantContains: []string{"Result: 5"},
		},
		{
			name:         "TemplateEmptyArray",
			template:     "Queue: {{.queued}}{{if .jobs}}{{table .jobs \"id\"}}{{end}}",
			toolResult:   `{"queued": 0, "jobs": []}`,
			wantContains: []string{"Queue: 0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := template.New("test").Funcs(toolTemplateFuncMap).Parse(tt.template)
			require.NoError(t, err)

			var data map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.toolResult), &data))

			data["_nick"] = "testnick"
			data["_channel"] = "#test"
			data["_network"] = "testnet"

			var buf strings.Builder
			fallback, err := executeToolTemplate(tmpl, data, &buf)
			assert.NoError(t, err)
			assert.False(t, fallback)

			result := strings.TrimSpace(buf.String())
			for _, want := range tt.wantContains {
				assert.Contains(t, result, want, "expected to contain %q", want)
			}
		})
	}
}

func TestExecuteToolTemplate_FallbackOnError(t *testing.T) {
	tmpl, err := template.New("bad").Funcs(toolTemplateFuncMap).Parse("{{.foo.Bar}}")
	require.NoError(t, err)

	data := map[string]any{"foo": "not a struct"}
	var buf strings.Builder
	fallback, err := executeToolTemplate(tmpl, data, &buf)

	assert.Error(t, err)
	assert.True(t, fallback)
}
