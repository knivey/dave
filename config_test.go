package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func createTestConfigDir(t *testing.T, mainTOML string, extraFiles map[string]string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dave_test_config_*")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(mainTOML), 0644); err != nil {
		t.Fatal(err)
	}

	for name, content := range extraFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.ApplyDefaults(tt.svc)
			want := tt.expect(AIConfig{})
			if cfg.MaxTokens != want.MaxTokens {
				t.Errorf("MaxTokens = %d, want %d", cfg.MaxTokens, want.MaxTokens)
			}
			if cfg.MaxCompletionTokens != want.MaxCompletionTokens {
				t.Errorf("MaxCompletionTokens = %d, want %d", cfg.MaxCompletionTokens, want.MaxCompletionTokens)
			}
			if cfg.Temperature != want.Temperature {
				t.Errorf("Temperature = %f, want %f", cfg.Temperature, want.Temperature)
			}
			if cfg.MaxHistory != want.MaxHistory {
				t.Errorf("MaxHistory = %d, want %d", cfg.MaxHistory, want.MaxHistory)
			}
			if cfg.MaxImages != want.MaxImages {
				t.Errorf("MaxImages = %d, want %d", cfg.MaxImages, want.MaxImages)
			}
			if cfg.MaxContextImages != want.MaxContextImages {
				t.Errorf("MaxContextImages = %d, want %d", cfg.MaxContextImages, want.MaxContextImages)
			}
			if cfg.ImageFormat != want.ImageFormat {
				t.Errorf("ImageFormat = %q, want %q", cfg.ImageFormat, want.ImageFormat)
			}
			if cfg.ImageQuality != want.ImageQuality {
				t.Errorf("ImageQuality = %d, want %d", cfg.ImageQuality, want.ImageQuality)
			}
			if cfg.MaxImageSize != want.MaxImageSize {
				t.Errorf("MaxImageSize = %q, want %q", cfg.MaxImageSize, want.MaxImageSize)
			}
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
			if got := tt.s.GetPort(); got != tt.want {
				t.Errorf("GetPort() = %d, want %d", got, tt.want)
			}
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
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Host != w {
				t.Errorf("getNextServer() = %q, want %q", got.Host, w)
			}
		}
	})

	t.Run("handles single server", func(t *testing.T) {
		n := Network{
			Servers:    []Server{{Host: "single"}},
			nextServer: 0,
		}

		for i := 0; i < 5; i++ {
			got, err := n.getNextServer()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Host != "single" {
				t.Errorf("getNextServer() = %q, want %q", got.Host, "single")
			}
		}
	})

	t.Run("returns error for empty servers", func(t *testing.T) {
		n := Network{Name: "testnet"}
		_, err := n.getNextServer()
		if err == nil {
			t.Fatal("expected error for empty servers")
		}
		if !strings.Contains(err.Error(), "no servers") {
			t.Errorf("error = %q, want mention of no servers", err.Error())
		}
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
			if !ok {
				t.Fatalf("command %s not found", tt.name)
			}
			if cfg.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", cfg.Name, tt.wantName)
			}
			if cfg.Regex != tt.wantRegex {
				t.Errorf("Regex = %q, want %q", cfg.Regex, tt.wantRegex)
			}
			if (cfg.SystemTmpl != nil) != tt.hasTmpl {
				t.Errorf("SystemTmpl presence = %v, want %v", cfg.SystemTmpl != nil, tt.hasTmpl)
			}
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
			if err != nil {
				t.Fatalf("failed to parse template: %v", err)
			}
			err = validateSystemPromptTemplate(tmpl)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSystemPromptTemplate() error = %v, wantErr %v", err, tt.wantErr)
			}
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

			if tt.wantW > 0 && cfg.MaxImageWidth != tt.wantW {
				t.Errorf("MaxImageWidth = %d, want %d", cfg.MaxImageWidth, tt.wantW)
			}
			if tt.wantH > 0 && cfg.MaxImageHeight != tt.wantH {
				t.Errorf("MaxImageHeight = %d, want %d", cfg.MaxImageHeight, tt.wantH)
			}
			if tt.wantFmt != "" && cfg.ImageFormat != tt.wantFmt {
				t.Errorf("ImageFormat = %q, want %q", cfg.ImageFormat, tt.wantFmt)
			}
			if tt.wantQ > 0 && cfg.ImageQuality != tt.wantQ {
				t.Errorf("ImageQuality = %d, want %d", cfg.ImageQuality, tt.wantQ)
			}
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
				ParallelToolCalls: func() *bool { b := false; return &b }(),
			},
			expect: false,
		},
		{
			name: "set only in command uses command value",
			cfg: AIConfig{
				ParallelToolCalls: func() *bool { b := false; return &b }(),
			},
			svc:    Service{},
			expect: false,
		},
		{
			name: "set in both command overrides service",
			cfg: AIConfig{
				ParallelToolCalls: func() *bool { b := true; return &b }(),
			},
			svc: Service{
				ParallelToolCalls: func() *bool { b := false; return &b }(),
			},
			expect: true,
		},
		{
			name: "explicit true in service cascades to command",
			cfg:  AIConfig{},
			svc: Service{
				ParallelToolCalls: func() *bool { b := true; return &b }(),
			},
			expect: true,
		},
		{
			name: "explicit false in command overrides service true",
			cfg: AIConfig{
				ParallelToolCalls: func() *bool { b := false; return &b }(),
			},
			svc: Service{
				ParallelToolCalls: func() *bool { b := true; return &b }(),
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
			if got != tt.expect {
				t.Errorf("ParallelToolCalls = %v, want %v", got, tt.expect)
			}
		})
	}
}
