package main

import (
	"os"
	"path/filepath"
	"testing"
	"text/template"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
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
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "retry_on_empty preserves explicit value",
			cfg:  AIConfig{RetryOnEmpty: intPtr(3)},
			svc:  Service{},
			expect: func(cfg AIConfig) AIConfig {
				cfg.RetryOnEmpty = intPtr(3)
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "retry_on_empty zero disables retry",
			cfg:  AIConfig{RetryOnEmpty: intPtr(0)},
			svc:  Service{},
			expect: func(cfg AIConfig) AIConfig {
				cfg.RetryOnEmpty = intPtr(0)
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				return cfg
			},
		},
		{
			name: "disabled_builtin_tools nil inherits from service",
			cfg:  AIConfig{},
			svc:  Service{DisabledBuiltinTools: []string{"ban_user"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.DisabledBuiltinTools = []string{"ban_user"}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "disabled_builtin_tools set overrides service",
			cfg:  AIConfig{DisabledBuiltinTools: []string{"check_ban_history"}},
			svc:  Service{DisabledBuiltinTools: []string{"ban_user"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.DisabledBuiltinTools = []string{"check_ban_history"}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "disabled_builtin_tools empty slice overrides service to none disabled",
			cfg:  AIConfig{DisabledBuiltinTools: []string{}},
			svc:  Service{DisabledBuiltinTools: []string{"ban_user"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.DisabledBuiltinTools = []string{}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "disabled_builtin_tools both nil means nil no disable",
			cfg:  AIConfig{},
			svc:  Service{},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
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
			assert.Equal(t, want.RetryOnEmpty, cfg.RetryOnEmpty, "RetryOnEmpty")
			assert.Equal(t, want.DisabledBuiltinTools, cfg.DisabledBuiltinTools, "DisabledBuiltinTools")
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
			err = validateTemplate(tmpl)
			assert.Equal(t, tt.wantErr, err != nil, "validateTemplate() error")
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

func TestLoadConfigDirNetworkIdentityDefaults(t *testing.T) {
	t.Run("user and real_name default to nick", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "mybot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		assert.Equal(t, "mybot", net.User, "User should default to Nick")
		assert.Equal(t, "mybot", net.RealName, "RealName should default to Nick")
	})

	t.Run("user and real_name can be set independently", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "mybot"
user = "botuser"
real_name = "The Bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		assert.Equal(t, "mybot", net.Nick)
		assert.Equal(t, "botuser", net.User)
		assert.Equal(t, "The Bot", net.RealName)
	})
}

func TestLoadConfigDirNetworkReconnectDelay(t *testing.T) {
	t.Run("reconnect_delay defaults to 60s", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		assert.Equal(t, 60*time.Second, *net.ReconnectDelay)
	})

	t.Run("reconnect_delay can be configured", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
reconnect_delay = "30s"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		assert.Equal(t, 30*time.Second, *net.ReconnectDelay)
	})

	t.Run("reconnect_delay can be set to 0s", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
reconnect_delay = "0s"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		require.NotNil(t, net.ReconnectDelay)
		assert.Equal(t, time.Duration(0), *net.ReconnectDelay)
	})
}

func TestLoadConfigDirNetworkSASL(t *testing.T) {
	t.Run("no sasl config by default", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		assert.Nil(t, net.SASL)
	})

	t.Run("sasl config loads correctly", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"

[networks.testnet.sasl]
user = "mybot"
pass = "secret123"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)
		net := config.Networks["testnet"]
		require.NotNil(t, net.SASL)
		assert.Equal(t, "mybot", net.SASL.User)
		assert.Equal(t, "secret123", net.SASL.Pass)
	})
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

func TestLoadConfigDirHiddenToolsDefault(t *testing.T) {
	t.Run("hidden_tools defaults to all builtin tools", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		cfg := loadConfigDirOrDie(dir)
		assert.Equal(t, []string{"register_background_job", "check_ban_history"}, cfg.HiddenTools)
	})

	t.Run("hidden_tools explicit value overrides default", func(t *testing.T) {
		mainTOML := `
hidden_tools = ["ban_user"]
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		cfg := loadConfigDirOrDie(dir)
		assert.Equal(t, []string{"ban_user"}, cfg.HiddenTools)
	})

	t.Run("disabled_builtin_tools defaults to nil", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		cfg := loadConfigDirOrDie(dir)
		assert.Nil(t, cfg.DisabledBuiltinTools, "disabled_builtin_tools should default to nil")
	})

	t.Run("disabled_builtin_tools explicit value", func(t *testing.T) {
		mainTOML := `
disabled_builtin_tools = ["ban_user", "check_ban_history"]
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		cfg := loadConfigDirOrDie(dir)
		assert.Equal(t, []string{"ban_user", "check_ban_history"}, cfg.DisabledBuiltinTools)
	})
}

func TestHiddenMCPToolsCascade(t *testing.T) {
	tests := []struct {
		name   string
		cfg    AIConfig
		svc    Service
		expect func(AIConfig) AIConfig
	}{
		{
			name: "hidden_mcp_tools nil inherits from service",
			cfg:  AIConfig{},
			svc:  Service{HiddenMCPTools: []string{"wait_for_job"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.HiddenMCPTools = []string{"wait_for_job"}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "hidden_mcp_tools set overrides service",
			cfg:  AIConfig{HiddenMCPTools: []string{"cancel_job"}},
			svc:  Service{HiddenMCPTools: []string{"wait_for_job"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.HiddenMCPTools = []string{"cancel_job"}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "hidden_mcp_tools empty slice overrides service to none",
			cfg:  AIConfig{HiddenMCPTools: []string{}},
			svc:  Service{HiddenMCPTools: []string{"wait_for_job"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.HiddenMCPTools = []string{}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "hidden_mcp_tool_sets nil inherits from service",
			cfg:  AIConfig{},
			svc:  Service{HiddenMCPToolSets: []string{"img-async-management"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.HiddenMCPToolSets = []string{"img-async-management"}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "hidden_mcp_tool_sets set overrides service",
			cfg:  AIConfig{HiddenMCPToolSets: []string{"custom-set"}},
			svc:  Service{HiddenMCPToolSets: []string{"img-async-management"}},
			expect: func(cfg AIConfig) AIConfig {
				cfg.HiddenMCPToolSets = []string{"custom-set"}
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
		{
			name: "both nil means nil",
			cfg:  AIConfig{},
			svc:  Service{},
			expect: func(cfg AIConfig) AIConfig {
				cfg.MaxImages = 5
				cfg.MaxContextImages = 5
				cfg.ImageFormat = "jpg"
				cfg.ImageQuality = 75
				cfg.MaxImageSize = "1024x1024"
				cfg.RetryOnEmpty = intPtr(1)
				return cfg
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.ApplyDefaults(tt.svc)
			want := tt.expect(AIConfig{})
			assert.Equal(t, want.HiddenMCPTools, cfg.HiddenMCPTools, "HiddenMCPTools")
			assert.Equal(t, want.HiddenMCPToolSets, cfg.HiddenMCPToolSets, "HiddenMCPToolSets")
		})
	}
}

func TestResolveHiddenMCPToolsFrom(t *testing.T) {
	sets := map[string][]string{
		"img-async-management": {"wait_for_job", "cancel_job", "get_job_status", "list_jobs"},
		"debug-tools":          {"verbose_log", "dump_state"},
	}

	tests := []struct {
		name     string
		tools    []string
		setNames []string
		want     []string
	}{
		{
			name:     "nil inputs",
			tools:    nil,
			setNames: nil,
			want:     nil,
		},
		{
			name:     "empty inputs",
			tools:    []string{},
			setNames: []string{},
			want:     nil,
		},
		{
			name:     "tools only",
			tools:    []string{"wait_for_job", "cancel_job"},
			setNames: nil,
			want:     []string{"wait_for_job", "cancel_job"},
		},
		{
			name:     "set only",
			tools:    nil,
			setNames: []string{"img-async-management"},
			want:     []string{"wait_for_job", "cancel_job", "get_job_status", "list_jobs"},
		},
		{
			name:     "set and tools merged deduped",
			tools:    []string{"wait_for_job", "extra_tool"},
			setNames: []string{"img-async-management"},
			want:     []string{"wait_for_job", "cancel_job", "get_job_status", "list_jobs", "extra_tool"},
		},
		{
			name:     "multiple sets",
			tools:    nil,
			setNames: []string{"img-async-management", "debug-tools"},
			want:     []string{"wait_for_job", "cancel_job", "get_job_status", "list_jobs", "verbose_log", "dump_state"},
		},
		{
			name:     "unknown set ignored",
			tools:    []string{"my_tool"},
			setNames: []string{"nonexistent"},
			want:     []string{"my_tool"},
		},
		{
			name:     "dedup across set and tools",
			tools:    []string{"wait_for_job"},
			setNames: []string{"img-async-management"},
			want:     []string{"wait_for_job", "cancel_job", "get_job_status", "list_jobs"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveHiddenMCPToolsFrom(sets, tt.tools, tt.setNames)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsMCPToolHidden(t *testing.T) {
	hidden := []string{"wait_for_job", "cancel_job", "img-mcp-async.get_job_status", "other-server.specific_tool"}

	tests := []struct {
		name       string
		toolName   string
		serverName string
		hidden     []string
		want       bool
	}{
		{name: "bare match", toolName: "wait_for_job", serverName: "img-mcp-async", hidden: hidden, want: true},
		{name: "bare no match", toolName: "generate_image", serverName: "img-mcp", hidden: hidden, want: false},
		{name: "server prefixed match", toolName: "get_job_status", serverName: "img-mcp-async", hidden: hidden, want: true},
		{name: "server prefixed wrong server", toolName: "get_job_status", serverName: "img-mcp", hidden: hidden, want: false},
		{name: "server prefixed match other", toolName: "specific_tool", serverName: "other-server", hidden: hidden, want: true},
		{name: "empty hidden list", toolName: "wait_for_job", serverName: "img-mcp-async", hidden: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isMCPToolHidden(tt.toolName, tt.serverName, tt.hidden))
		})
	}
}

func TestLoadMCPsFileWithToolSets(t *testing.T) {
	mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	mcpsTOML := `
[img-mcp]
transport = "http"
url = "http://localhost:8080/sync"

[img-mcp-async]
transport = "http"
url = "http://localhost:8080/async"

[hidden_mcp_tool_sets.img-async-management]
tools = ["wait_for_job", "cancel_job", "get_job_status", "list_jobs"]
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"mcps.toml": mcpsTOML,
	})
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	assert.Contains(t, cfg.MCPs, "img-mcp")
	assert.Contains(t, cfg.MCPs, "img-mcp-async")
	assert.Equal(t, map[string][]string{
		"img-async-management": {"wait_for_job", "cancel_job", "get_job_status", "list_jobs"},
	}, cfg.MCPToolSets)
}

func TestLoadMCPsFileWithoutToolSets(t *testing.T) {
	mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	mcpsTOML := `
[img-mcp]
transport = "http"
url = "http://localhost:8080/sync"
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"mcps.toml": mcpsTOML,
	})
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	assert.Empty(t, cfg.MCPToolSets)
}

func TestLoadMCPsFileMissing(t *testing.T) {
	mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	dir := createTestConfigDir(t, mainTOML, nil)
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	assert.Empty(t, cfg.MCPToolSets)
	assert.Empty(t, cfg.MCPs)
}

func TestRootToServiceCascade(t *testing.T) {
	mainTOML := `
disabled_builtin_tools = ["ban_user"]
hidden_mcp_tools = ["wait_for_job"]
hidden_mcp_tool_sets = ["img-async-management"]
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	servicesTOML := `
[myservice]
baseurl = "http://localhost:8000/v1"
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"services.toml": servicesTOML,
	})
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	svc := cfg.Services["myservice"]
	assert.Equal(t, []string{"ban_user"}, svc.DisabledBuiltinTools, "service should inherit root disabled_builtin_tools")
	assert.Equal(t, []string{"wait_for_job"}, svc.HiddenMCPTools, "service should inherit root hidden_mcp_tools")
	assert.Equal(t, []string{"img-async-management"}, svc.HiddenMCPToolSets, "service should inherit root hidden_mcp_tool_sets")
}

func TestRootToServiceCascadeServiceOverrides(t *testing.T) {
	mainTOML := `
disabled_builtin_tools = ["ban_user"]
hidden_mcp_tools = ["wait_for_job"]
hidden_mcp_tool_sets = ["img-async-management"]
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	servicesTOML := `
[myservice]
baseurl = "http://localhost:8000/v1"
disabled_builtin_tools = ["check_ban_history"]
hidden_mcp_tools = ["cancel_job"]
hidden_mcp_tool_sets = ["custom-set"]
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"services.toml": servicesTOML,
	})
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	svc := cfg.Services["myservice"]
	assert.Equal(t, []string{"check_ban_history"}, svc.DisabledBuiltinTools, "service should override root disabled_builtin_tools")
	assert.Equal(t, []string{"cancel_job"}, svc.HiddenMCPTools, "service should override root hidden_mcp_tools")
	assert.Equal(t, []string{"custom-set"}, svc.HiddenMCPToolSets, "service should override root hidden_mcp_tool_sets")
}

func TestRootToServiceCascadeServiceEmptyOverrides(t *testing.T) {
	mainTOML := `
disabled_builtin_tools = ["ban_user"]
hidden_mcp_tools = ["wait_for_job"]
hidden_mcp_tool_sets = ["img-async-management"]
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	servicesTOML := `
[myservice]
baseurl = "http://localhost:8000/v1"
disabled_builtin_tools = []
hidden_mcp_tools = []
hidden_mcp_tool_sets = []
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"services.toml": servicesTOML,
	})
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	svc := cfg.Services["myservice"]
	assert.Equal(t, []string{}, svc.DisabledBuiltinTools, "service empty slice should override root to none")
	assert.Equal(t, []string{}, svc.HiddenMCPTools, "service empty slice should override root to none")
	assert.Equal(t, []string{}, svc.HiddenMCPToolSets, "service empty slice should override root to none")
}

func TestFullCascadeRootToCommand(t *testing.T) {
	mainTOML := `
hidden_mcp_tools = ["wait_for_job", "cancel_job"]
hidden_mcp_tool_sets = ["img-async-management"]
disabled_builtin_tools = ["ban_user"]
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	servicesTOML := `
[myservice]
baseurl = "http://localhost:8000/v1"
`
	chatsTOML := `
[mychat]
service = "myservice"
system = "test"
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"services.toml": servicesTOML,
		"chats.toml":    chatsTOML,
	})
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	chat := cfg.Commands.Chats["mychat"]
	assert.Equal(t, []string{"ban_user"}, chat.DisabledBuiltinTools, "command should inherit root→service→command disabled_builtin_tools")
	assert.Equal(t, []string{"wait_for_job", "cancel_job"}, chat.HiddenMCPTools, "command should inherit root→service→command hidden_mcp_tools")
	assert.Equal(t, []string{"img-async-management"}, chat.HiddenMCPToolSets, "command should inherit root→service→command hidden_mcp_tool_sets")
}

func TestRegisterCommandsLocked_InvalidRegex(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	t.Run("invalid completions regex returns error", func(t *testing.T) {
		cmds := Commands{
			Completions: map[string]AIConfig{
				"bad": {Regex: "[invalid"},
			},
		}
		err := registerCommandsLocked(cmds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid regex for completions command")
		assert.Contains(t, err.Error(), "[invalid")
	})

	t.Run("invalid chats regex returns error", func(t *testing.T) {
		cmds := Commands{
			Chats: map[string]AIConfig{
				"bad": {Regex: "(?P<name"},
			},
		}
		err := registerCommandsLocked(cmds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid regex for chats command")
	})

	t.Run("invalid tools regex returns error", func(t *testing.T) {
		cmds := Commands{
			Tools: map[string]MCPCommandConfig{
				"bad": {Regex: "[a-z", MCP: "test", Tool: "test"},
			},
		}
		err := registerCommandsLocked(cmds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid regex for tools command")
	})

	t.Run("valid regex does not return error", func(t *testing.T) {
		cmds := Commands{
			Completions: map[string]AIConfig{
				"ok": {Regex: `test\d+`},
			},
		}
		err := registerCommandsLocked(cmds)
		require.NoError(t, err)
	})

	t.Run("error does not mutate configCmds", func(t *testing.T) {
		orig := configCmds
		cmds := Commands{
			Chats: map[string]AIConfig{
				"bad": {Regex: "[invalid"},
			},
		}
		_ = registerCommandsLocked(cmds)
		assert.Equal(t, orig, configCmds, "configCmds should not be mutated on error")
	})
}

func TestIsNetworkCommandDisabled(t *testing.T) {
	tests := []struct {
		name     string
		disabled []string
		cmd      string
		expect   bool
	}{
		{"empty list", nil, "qwen", false},
		{"command in list", []string{"qwen", "zimage"}, "qwen", true},
		{"command not in list", []string{"qwen", "zimage"}, "grok", false},
		{"empty command name", []string{"qwen"}, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := Network{DisabledCommands: tt.disabled}
			assert.Equal(t, tt.expect, isNetworkCommandDisabled(n, tt.cmd))
		})
	}
}

func TestLoadConfigDirNetworkDisabledCommands(t *testing.T) {
	t.Run("network with disabled_commands using builtin names", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
disabled_commands = ["stop", "help"]
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		cfg := loadConfigDirOrDie(dir)
		net := cfg.Networks["testnet"]
		assert.Equal(t, []string{"stop", "help"}, net.DisabledCommands)
	})

	t.Run("network without disabled_commands defaults to nil", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		cfg := loadConfigDirOrDie(dir)
		net := cfg.Networks["testnet"]
		assert.Nil(t, net.DisabledCommands)
	})
}

func TestRegisterCommandsLocked_PopulatesConfigCmdNames(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	cmds := Commands{
		Completions: map[string]AIConfig{
			"comp1": {Regex: `comp1`},
		},
		Chats: map[string]AIConfig{
			"chat1": {Regex: `chat1`},
		},
		Tools: map[string]MCPCommandConfig{
			"tool1": {Regex: `tool1`, MCP: "mcp", Tool: "test"},
		},
	}
	err := registerCommandsLocked(cmds)
	require.NoError(t, err)

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()

	names := make(map[string]bool)
	for _, name := range configCmdNames {
		names[name] = true
	}
	assert.True(t, names["comp1"], "configCmdNames should contain comp1")
	assert.True(t, names["chat1"], "configCmdNames should contain chat1")
	assert.True(t, names["tool1"], "configCmdNames should contain tool1")
	assert.Len(t, configCmdNames, 3)
}

func TestHandleTrigger_DisabledCommandReturnsEarly(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	cmds := Commands{
		Chats: map[string]AIConfig{
			"mychat": {Regex: `mychat`, Service: "svc", Model: "m"},
		},
	}
	err := registerCommandsLocked(cmds)
	require.NoError(t, err)

	network := Network{
		Name:             "testnet",
		Trigger:          ".",
		DisabledCommands: []string{"mychat"},
	}

	client := girc.New(girc.Config{
		Server: "localhost",
		Port:   6667,
		Nick:   "testbot",
	})

	event := girc.Event{
		Source: &girc.Source{Name: "testuser", Ident: "u", Host: "h"},
		Params: []string{"#test"},
	}

	stripped := "mychat hello world"

	assert.NotPanics(t, func() {
		handleTrigger(network, client, event, "#test", ".mychat hello world", stripped)
	}, "disabled command should return early before reaching DB/client code")
}

func TestHandleTrigger_DisabledBuiltinCommand(t *testing.T) {
	if logger == nil {
		logger = logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
	}

	network := Network{
		Name:             "testnet",
		Trigger:          ".",
		DisabledCommands: []string{"stop"},
	}

	client := girc.New(girc.Config{
		Server: "localhost",
		Port:   6667,
		Nick:   "testbot",
	})

	event := girc.Event{
		Source: &girc.Source{Name: "testuser", Ident: "u", Host: "h"},
		Params: []string{"#test"},
	}

	stripped := "stop"

	assert.NotPanics(t, func() {
		handleTrigger(network, client, event, "#test", ".stop", stripped)
	}, "disabled builtin should return early before reaching DB/client code")
}

func TestLoadConfigDirDisabledCommandsValidation(t *testing.T) {
	t.Run("valid disabled_commands loads successfully", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
disabled_commands = ["mychat"]
[[networks.testnet.servers]]
host = "irc.example.com"
`
		chatsTOML := `
[mychat]
regex = "mychat"
service = "svc"
model = "m"
`
		servicesTOML := `
[svc]
api_base = "http://localhost"
api_key = "test"
`
		dir := createTestConfigDir(t, mainTOML, map[string]string{
			"chats.toml":    chatsTOML,
			"services.toml": servicesTOML,
		})
		defer os.RemoveAll(dir)

		cfg, err := loadConfigDir(dir)
		require.NoError(t, err)
		net := cfg.Networks["testnet"]
		assert.Equal(t, []string{"mychat"}, net.DisabledCommands)
	})

	t.Run("unknown command in disabled_commands returns error", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
disabled_commands = ["nonexistent"]
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		_, err := loadConfigDir(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "disabled_commands")
		assert.Contains(t, err.Error(), "nonexistent")
		assert.Contains(t, err.Error(), "not a known command")
	})

	t.Run("builtin name in disabled_commands is valid", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
disabled_commands = ["stop", "help"]
[[networks.testnet.servers]]
host = "irc.example.com"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		_, err := loadConfigDir(dir)
		require.NoError(t, err)
	})
}

func TestLoadReloadableDirDisabledCommands(t *testing.T) {
	t.Run("reload validates disabled_commands against new commands", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
disabled_commands = ["mychat", "removed"]
[[networks.testnet.servers]]
host = "irc.example.com"
`
		servicesTOML := `
[svc]
api_base = "http://localhost"
api_key = "test"
`
		chatsWithBothTOML := `
[mychat]
regex = "mychat"
service = "svc"
model = "m"

[removed]
regex = "removed"
service = "svc"
model = "m"
`
		chatsWithoutRemovedTOML := `
[mychat]
regex = "mychat"
service = "svc"
model = "m"
`
		dir := createTestConfigDir(t, mainTOML, map[string]string{
			"chats.toml":    chatsWithBothTOML,
			"services.toml": servicesTOML,
		})
		defer os.RemoveAll(dir)

		cfg, err := loadConfigDir(dir)
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(filepath.Join(dir, "chats.toml"), []byte(chatsWithoutRemovedTOML), 0644))

		err = loadReloadableDir(dir, &cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "disabled_commands")
		assert.Contains(t, err.Error(), "removed")
		assert.Contains(t, err.Error(), "not a known command")
	})

	t.Run("reload succeeds when disabled_commands all valid", func(t *testing.T) {
		mainTOML := `
[networks.testnet]
nick = "bot"
disabled_commands = ["mychat"]
[[networks.testnet.servers]]
host = "irc.example.com"
`
		servicesTOML := `
[svc]
api_base = "http://localhost"
api_key = "test"
`
		chatsTOML := `
[mychat]
regex = "mychat"
service = "svc"
model = "m"
`
		dir := createTestConfigDir(t, mainTOML, map[string]string{
			"chats.toml":    chatsTOML,
			"services.toml": servicesTOML,
		})
		defer os.RemoveAll(dir)

		cfg, err := loadConfigDir(dir)
		require.NoError(t, err)

		err = loadReloadableDir(dir, &cfg)
		require.NoError(t, err)
	})
}

func TestLoadConfigDirLoggingDefaults(t *testing.T) {
	mainTOML := `
[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	dir := createTestConfigDir(t, mainTOML, nil)
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	logging := cfg.Logging
	assert.False(t, logging.Enabled, "Enabled should default to false")
	assert.Equal(t, "data/logs", logging.Dir, "Dir should default to data/logs")
	assert.Equal(t, "monthly", logging.Rotation, "Rotation should default to monthly")
	assert.Equal(t, 10000, logging.BufferSize, "BufferSize should default to 10000")
	assert.Equal(t, 500, logging.BatchSize, "BatchSize should default to 500")
	assert.Equal(t, 2*time.Second, logging.FlushInterval, "FlushInterval should default to 2s")
}

func TestLoadConfigDirLoggingEnabled(t *testing.T) {
	mainTOML := `
[logging]
enabled = true
dir = "custom/logs"
rotation = "yearly"
buffer_size = 5000
batch_size = 200
flush_interval = "5s"

[networks.testnet]
nick = "bot"
[[networks.testnet.servers]]
host = "irc.example.com"
`
	dir := createTestConfigDir(t, mainTOML, nil)
	defer os.RemoveAll(dir)

	cfg := loadConfigDirOrDie(dir)
	logging := cfg.Logging
	assert.True(t, logging.Enabled, "Enabled should be true")
	assert.Equal(t, "custom/logs", logging.Dir, "Dir should be custom/logs")
	assert.Equal(t, "yearly", logging.Rotation, "Rotation should be yearly")
	assert.Equal(t, 5000, logging.BufferSize, "BufferSize should be 5000")
	assert.Equal(t, 200, logging.BatchSize, "BatchSize should be 200")
	assert.Equal(t, 5*time.Second, logging.FlushInterval, "FlushInterval should be 5s")
}
