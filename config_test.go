package main

import (
	"os"
	"path/filepath"
	"testing"
	"text/template"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestConfigDir(t *testing.T, mainTOML string, extraFiles map[string]string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dave_test_config_*")
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(mainTOML), 0644))

	for name, content := range extraFiles {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
	}

	return dir
}

func TestAIConfigApplyDefaults(t *testing.T) {
	tests := []struct {
		name   string
		cfg    AIConfig
		svc    Service
		expect func(AIConfig) AIConfig
	}{
		{
			name: "all zeros applies all defaults",
			cfg:  AIConfig{},
			svc: Service{
				MaxTokens:           100,
				MaxCompletionTokens: 200,
				Temperature:         0.7,
				MaxHistory:          10,
			},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxTokens = 100
				cfg.MaxCompletionTokens = 200
				cfg.Temperature = 0.7
				cfg.MaxHistory = 10
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "preserves existing values",
			cfg: AIConfig{
				MaxTokens:   500,
				Temperature: 1.5,
				MaxImages:   3,
			},
			svc: Service{
				MaxTokens:   100,
				Temperature: 0.7,
				MaxHistory:  10,
			},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxTokens = 500
				cfg.Temperature = 1.5
				cfg.MaxHistory = 10
				cfg.MaxImages = 3
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "maxTokens zero uses MaxCompletionTokens from service",
			cfg: AIConfig{
				MaxTokens: 0,
			},
			svc: Service{
				MaxTokens:           0,
				MaxCompletionTokens: 300,
			},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxTokens = 0
				cfg.MaxCompletionTokens = 300
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "maxTokens set but maxCompletionTokens zero uses service default",
			cfg: AIConfig{
				MaxTokens: 500,
			},
			svc: Service{
				MaxTokens:           100,
				MaxCompletionTokens: 300,
			},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxTokens = 500
				cfg.MaxCompletionTokens = 300
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "maxImages defaults to 5 when zero",
			cfg:  AIConfig{},
			svc:  Service{},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "maxImages preserves non-zero value",
			cfg: AIConfig{
				MaxImages: 10,
			},
			svc: Service{},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxImages = 10
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "maxContextImages defaults to 5 when zero",
			cfg: AIConfig{
				MaxImages: 3,
			},
			svc: Service{},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxImages = 3
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "api_user inherits from service when empty",
			cfg:  AIConfig{},
			svc:  Service{APIUser: "dave/{{.Network}}/{{.Nick}}"},
			expect: func(cfg AIConfig) AIConfig {
				cfg.APIUser = "dave/{{.Network}}/{{.Nick}}"
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "api_user preserves command-level value",
			cfg:  AIConfig{APIUser: "irc:{{.Nick}}"},
			svc:  Service{APIUser: "dave/{{.Network}}/{{.Nick}}"},
			expect: func(cfg AIConfig) AIConfig {
				cfg.APIUser = "irc:{{.Nick}}"
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.ApplyDefaults(tt.svc)
			want := tt.expect(AIConfig{})
			assert.Equal(t, want.MaxTokens, cfg.MaxTokens, "MaxTokens")
			assert.Equal(t, want.MaxCompletionTokens, cfg.MaxCompletionTokens, "MaxCompletionTokens")
			assert.Equal(t, want.Temperature, cfg.Temperature, "Temperature")
			assert.Equal(t, want.MaxHistory, cfg.MaxHistory, "MaxHistory")
			assert.Equal(t, want.MaxImages, cfg.MaxImages, "MaxImages")
			assert.Equal(t, want.MaxContextImages, cfg.MaxContextImages, "MaxContextImages")
			assert.Equal(t, want.ImageFormat, cfg.ImageFormat, "ImageFormat")
			assert.Equal(t, want.ImageQuality, cfg.ImageQuality, "ImageQuality")
			assert.Equal(t, want.MaxImageSize, cfg.MaxImageSize, "MaxImageSize")
			assert.Equal(t, want.APIUser, cfg.APIUser, "APIUser")
		})
	}
}

func TestServerGetPort(t *testing.T) {
	tests := []struct {
		name string
		s    Server
		want int
	}{
		{
			name: "port set returns port",
			s:    Server{Port: 8080},
			want: 8080,
		},
		{
			name: "SSL defaults to 6697",
			s:    Server{Ssl: true},
			want: 6697,
		},
		{
			name: "non-SSL defaults to 6667",
			s:    Server{},
			want: 6667,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.s.GetPort(), "GetPort()")
		})
	}
}

func TestNetworkGetNextServer(t *testing.T) {
	t.Run("cycles through servers", func(t *testing.T) {
		n := Network{
			Servers:    []Server{{Host: "a"}, {Host: "b"}, {Host: "c"}},
			nextServer: 0,
		}

		want := []string{"a", "b", "c", "a", "b"}
		for _, w := range want {
			got, err := n.getNextServer()
			require.NoError(t, err, "unexpected error")
			assert.Equal(t, w, got.Host, "getNextServer()")
		}
	})

	t.Run("handles single server", func(t *testing.T) {
		n := Network{
			Servers:    []Server{{Host: "single"}},
			nextServer: 0,
		}

		for i := 0; i < 5; i++ {
			got, err := n.getNextServer()
			require.NoError(t, err, "unexpected error")
			assert.Equal(t, "single", got.Host, "getNextServer()")
		}
	})

	t.Run("returns error for empty servers", func(t *testing.T) {
		n := Network{Name: "testnet"}
		_, err := n.getNextServer()
		require.Error(t, err, "expected error for empty servers")
		assert.Contains(t, err.Error(), "no servers", "error message")
	})
}

func TestChatCommandNameSetting(t *testing.T) {
	mainTOML := ``
	servicesTOML := `
[test]
maxtokens = 100
maxhistory = 10
`
	chatsTOML := `
[chat1]
service = "test"

[chat2]
service = "test"
regex = "custom"

[chat3]
service = "test"
system = "Hello {{.Nick}}"

[chat4]
service = "test"
system = "Static message"
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"services.toml": servicesTOML,
		"chats.toml":    chatsTOML,
	})
	defer os.RemoveAll(dir)

	config := loadConfigDirOrDie(dir)

	tests := []struct {
		name      string
		wantName  string
		wantRegex string
		hasTmpl   bool
	}{
		{"chat1", "chat1", "chat1", false},
		{"chat2", "chat2", "custom", false},
		{"chat3", "chat3", "chat3", true},
		{"chat4", "chat4", "chat4", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, ok := config.Commands.Chats[tt.name]
			require.True(t, ok, "command %s not found", tt.name)
			assert.Equal(t, tt.wantName, cfg.Name, "Name")
			assert.Equal(t, tt.wantRegex, cfg.Regex, "Regex")
			assert.Equal(t, tt.hasTmpl, cfg.SystemTmpl != nil, "SystemTmpl presence")
		})
	}
}

func TestSystemPromptTemplateValidation(t *testing.T) {
	tests := []struct {
		name        string
		templateStr string
		wantErr     bool
	}{
		{
			name:        "valid template with all variables",
			templateStr: "Hello {{.Nick}} from {{.BotNick}} in {{.Channel}} on {{.Network}}",
			wantErr:     false,
		},
		{
			name:        "valid template with some variables",
			templateStr: "Welcome {{.Nick}}!",
			wantErr:     false,
		},
		{
			name:        "valid template no variables",
			templateStr: "Static system prompt",
			wantErr:     false,
		},
		{
			name:        "invalid template undefined variable",
			templateStr: "Hello {{.Undefined}}",
			wantErr:     true,
		},
		{
			name:        "invalid template extra variables",
			templateStr: "Hello {{.Nick}} {{.Extra}}",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := template.New("test").Parse(tt.templateStr)
			require.NoError(t, err, "failed to parse template")
			err = validateSystemPromptTemplate(tmpl)
			assert.Equal(t, tt.wantErr, err != nil, "validateSystemPromptTemplate() error")
		})
	}
}

func TestImageConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		mainTOML  string
		chatsTOML string
		wantW     int
		wantH     int
		wantFmt   string
		wantQ     int
	}{
		{
			name:     "defaults applied",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"`,
			wantW:   1024,
			wantH:   1024,
			wantFmt: "jpg",
			wantQ:   75,
		},
		{
			name:     "valid jpg format",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"
imageformat = "jpg"`,
			wantFmt: "jpg",
		},
		{
			name:     "valid webp format",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"
imageformat = "webp"`,
			wantFmt: "webp",
		},
		{
			name:     "valid jpeg format",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"
imageformat = "jpeg"`,
			wantFmt: "jpeg",
		},
		{
			name:     "valid quality 50",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"
imagequality = 50`,
			wantQ: 50,
		},
		{
			name:     "valid maximagesize 1024x1024",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"
maximagesize = "1024x1024"`,
			wantW: 1024,
			wantH: 1024,
		},
		{
			name:     "valid maximagesize 1024x768",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"
maximagesize = "1024x768"`,
			wantW: 1024,
			wantH: 768,
		},
		{
			name:     "valid maximagesize 1920x1080",
			mainTOML: ``,
			chatsTOML: `[chat1]
service = "test"
maximagesize = "1920x1080"`,
			wantW: 1920,
			wantH: 1080,
		},
	}

	var servicesTOML = `
[test]
maxtokens = 100
`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := createTestConfigDir(t, tt.mainTOML, map[string]string{
				"services.toml": servicesTOML,
				"chats.toml":    tt.chatsTOML,
			})
			defer os.RemoveAll(dir)

			config := loadConfigDirOrDie(dir)
			cfg := config.Commands.Chats["chat1"]

			if tt.wantW > 0 {
				assert.Equal(t, tt.wantW, cfg.MaxImageWidth, "MaxImageWidth")
			}
			if tt.wantH > 0 {
				assert.Equal(t, tt.wantH, cfg.MaxImageHeight, "MaxImageHeight")
			}
			if tt.wantFmt != "" {
				assert.Equal(t, tt.wantFmt, cfg.ImageFormat, "ImageFormat")
			}
			if tt.wantQ > 0 {
				assert.Equal(t, tt.wantQ, cfg.ImageQuality, "ImageQuality")
			}
		})
	}
}

func TestNetworkIsEnabled(t *testing.T) {
	tests := []struct {
		name    string
		enabled *bool
		expect  bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", boolPtr(true), true},
		{"explicit false", boolPtr(false), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := Network{Enabled: tt.enabled}
			assert.Equal(t, tt.expect, n.IsEnabled(), "IsEnabled()")
		})
	}
}

func TestLoadConfigDirNetworkEnabledDefault(t *testing.T) {
	t.Run("network without enabled field defaults to true", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net, ok := config.Networks["testnet"]
		require.True(t, ok, "network testnet not found")
		assert.True(t, net.IsEnabled(), "network should be enabled by default when enabled field is omitted")
	})

	t.Run("network with enabled false", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
enabled = false
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		assert.False(t, net.IsEnabled(), "network should be disabled when explicitly set to false")
	})
}

func boolPtr(b bool) *bool {
	return &b
}

func intPtr(i int) *int {
	return &i
}

func TestGetPastebinPreviewLines(t *testing.T) {
	tests := []struct {
		name     string
		chCfg    ChannelConfig
		pastebin PastebinConfig
		want     int
	}{
		{
			name:     "defaults to 3 when nothing set",
			chCfg:    ChannelConfig{},
			pastebin: PastebinConfig{},
			want:     3,
		},
		{
			name:     "channel overrides pastebin config",
			chCfg:    ChannelConfig{PastebinPreviewLines: intPtr(1)},
			pastebin: PastebinConfig{PastebinPreviewLines: intPtr(5)},
			want:     1,
		},
		{
			name:     "falls back to pastebin config when channel not set",
			chCfg:    ChannelConfig{},
			pastebin: PastebinConfig{PastebinPreviewLines: intPtr(3)},
			want:     3,
		},
		{
			name:     "falls back to pastebin config clamped by default maxLines",
			chCfg:    ChannelConfig{},
			pastebin: PastebinConfig{PastebinPreviewLines: intPtr(7)},
			want:     4,
		},
		{
			name:     "pastebin config with explicit maxLines not clamped",
			chCfg:    ChannelConfig{MaxLines: 10},
			pastebin: PastebinConfig{PastebinPreviewLines: intPtr(7)},
			want:     7,
		},
		{
			name:     "zero disables preview",
			chCfg:    ChannelConfig{PastebinPreviewLines: intPtr(0)},
			pastebin: PastebinConfig{},
			want:     0,
		},
		{
			name:     "negative value clamped to 0",
			chCfg:    ChannelConfig{PastebinPreviewLines: intPtr(-5)},
			pastebin: PastebinConfig{},
			want:     0,
		},
		{
			name:     "value equal to maxLines clamped to maxLines-1",
			chCfg:    ChannelConfig{MaxLines: 5, PastebinPreviewLines: intPtr(5)},
			pastebin: PastebinConfig{},
			want:     4,
		},
		{
			name:     "value greater than maxLines clamped to maxLines-1",
			chCfg:    ChannelConfig{MaxLines: 3, PastebinPreviewLines: intPtr(10)},
			pastebin: PastebinConfig{},
			want:     2,
		},
		{
			name:     "maxLines defaults to 5 when clamping",
			chCfg:    ChannelConfig{PastebinPreviewLines: intPtr(5)},
			pastebin: PastebinConfig{},
			want:     4,
		},
		{
			name:     "pastebin config value also clamped by maxLines",
			chCfg:    ChannelConfig{MaxLines: 2},
			pastebin: PastebinConfig{PastebinPreviewLines: intPtr(10)},
			want:     1,
		},
		{
			name:     "channel zero disables even when pastebin config set",
			chCfg:    ChannelConfig{PastebinPreviewLines: intPtr(0)},
			pastebin: PastebinConfig{PastebinPreviewLines: intPtr(5)},
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.chCfg.GetPastebinPreviewLines(tt.pastebin)
			assert.Equal(t, tt.want, got, "GetPastebinPreviewLines()")
		})
	}
}

func TestParallelToolCallsCascading(t *testing.T) {
	tests := []struct {
		name   string
		cfg    AIConfig
		svc    Service
		expect bool
	}{
		{
			name:   "not set in either defaults to true",
			cfg:    AIConfig{},
			svc:    Service{},
			expect: true,
		},
		{
			name: "set only in service cascades to command",
			cfg:  AIConfig{},
			svc: Service{
				ParallelToolCalls: boolPtr(false),
			},
			expect: false,
		},
		{
			name: "set only in command uses command value",
			cfg: AIConfig{
				ParallelToolCalls: boolPtr(false),
			},
			svc:    Service{},
			expect: false,
		},
		{
			name: "set in both command overrides service",
			cfg: AIConfig{
				ParallelToolCalls: boolPtr(true),
			},
			svc: Service{
				ParallelToolCalls: boolPtr(false),
			},
			expect: true,
		},
		{
			name: "explicit true in service cascades to command",
			cfg:  AIConfig{},
			svc: Service{
				ParallelToolCalls: boolPtr(true),
			},
			expect: true,
		},
		{
			name: "explicit false in command overrides service true",
			cfg: AIConfig{
				ParallelToolCalls: boolPtr(false),
			},
			svc: Service{
				ParallelToolCalls: boolPtr(true),
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.ApplyDefaults(tt.svc)
			got := true
			if cfg.ParallelToolCalls != nil {
				got = *cfg.ParallelToolCalls
			}
			assert.Equal(t, tt.expect, got, "ParallelToolCalls")
		})
	}
}

func TestLoadCommandsDirAPIUser(t *testing.T) {
	t.Run("valid api_user template parses correctly", func(t *testing.T) {
		mainTOML := `
trigger = "."
`
		chatsTOML := `
[-test]
service = "svc"
model = "m"
api_user = "dave/{{.Network}}/{{.Nick}}"
`
		servicesTOML := `
[svc]
baseurl = "http://localhost"
`
		dir := createTestConfigDir(t, mainTOML, map[string]string{
			"chats.toml":    chatsTOML,
			"services.toml": servicesTOML,
		})
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		cfg, ok := config.Commands.Chats["-test"]
		require.True(t, ok)
		assert.Equal(t, "dave/{{.Network}}/{{.Nick}}", cfg.APIUser)
		assert.NotNil(t, cfg.apiUserTmpl)
	})

	t.Run("invalid api_user template fails to load", func(t *testing.T) {
		mainTOML := `
trigger = "."
`
		chatsTOML := `
[-test]
service = "svc"
model = "m"
api_user = "{{.BadField"
`
		servicesTOML := `
[svc]
baseurl = "http://localhost"
`
		dir := createTestConfigDir(t, mainTOML, map[string]string{
			"chats.toml":    chatsTOML,
			"services.toml": servicesTOML,
		})
		defer os.RemoveAll(dir)

		_, err := loadConfigDir(dir)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "api_user template parse error")
	})

	t.Run("empty api_user does not create template", func(t *testing.T) {
		mainTOML := `
trigger = "."
`
		chatsTOML := `
[-test]
service = "svc"
model = "m"
`
		servicesTOML := `
[svc]
baseurl = "http://localhost"
`
		dir := createTestConfigDir(t, mainTOML, map[string]string{
			"chats.toml":    chatsTOML,
			"services.toml": servicesTOML,
		})
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		cfg, ok := config.Commands.Chats["-test"]
		require.True(t, ok)
		assert.Equal(t, "", cfg.APIUser)
		assert.Nil(t, cfg.apiUserTmpl)
	})

	t.Run("api_user inherits from service", func(t *testing.T) {
		mainTOML := `
trigger = "."
`
		chatsTOML := `
[-test]
service = "svc"
model = "m"
`
		servicesTOML := `
[svc]
baseurl = "http://localhost"
api_user = "svc/{{.Nick}}"
`
		dir := createTestConfigDir(t, mainTOML, map[string]string{
			"chats.toml":    chatsTOML,
			"services.toml": servicesTOML,
		})
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		cfg, ok := config.Commands.Chats["-test"]
		require.True(t, ok)
		assert.Equal(t, "svc/{{.Nick}}", cfg.APIUser)
		assert.NotNil(t, cfg.apiUserTmpl)
	})
}
