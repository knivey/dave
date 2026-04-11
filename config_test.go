package main

import (
	"os"
	"testing"
	"text/template"
)

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

func TestComfyConfigApplyDefaults(t *testing.T) {
	tests := []struct {
		name   string
		cfg    ComfyConfig
		svc    Service
		expect ComfyConfig
	}{
		{
			name: "timeout zero uses service default",
			cfg:  ComfyConfig{},
			svc: Service{
				ComfyTimeout: 120,
			},
			expect: ComfyConfig{
				Timeout: 120,
			},
		},
		{
			name: "timeout zero uses 300 when service also zero",
			cfg:  ComfyConfig{},
			svc: Service{
				ComfyTimeout: 0,
			},
			expect: ComfyConfig{
				Timeout: 300,
			},
		},
		{
			name: "preserves existing timeout",
			cfg: ComfyConfig{
				Timeout: 600,
			},
			svc: Service{
				ComfyTimeout: 120,
			},
			expect: ComfyConfig{
				Timeout: 600,
			},
		},
		{
			name: "timeout of 0 is replaced",
			cfg: ComfyConfig{
				Timeout: 0,
			},
			svc: Service{
				ComfyTimeout: 60,
			},
			expect: ComfyConfig{
				Timeout: 60,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.ApplyDefaults(tt.svc)
			if cfg.Timeout != tt.expect.Timeout {
				t.Errorf("Timeout = %d, want %d", cfg.Timeout, tt.expect.Timeout)
			}
		})
	}
}

func TestSDConfigApplyDefaults(t *testing.T) {
	cfg := SDConfig{}
	svc := Service{}
	cfg.ApplyDefaults(svc)
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
			got := n.getNextServer()
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
			got := n.getNextServer()
			if got.Host != "single" {
				t.Errorf("getNextServer() = %q, want %q", got.Host, "single")
			}
		}
	})
}

func TestChatCommandNameSetting(t *testing.T) {
	// Create a temporary TOML config file
	tomlContent := `
[services.test]
maxtokens = 100
maxhistory = 10

[commands.chats.chat1]
service = "test"

[commands.chats.chat2]
service = "test"
regex = "custom"

[commands.chats.chat3]
service = "test"
system = "Hello {{.Nick}}"

[commands.chats.chat4]
service = "test"
system = "Static message"
`
	tempFile, err := os.CreateTemp("", "test_config_*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tempFile.Name())

	if _, err := tempFile.WriteString(tomlContent); err != nil {
		t.Fatal(err)
	}
	tempFile.Close()

	// Load config using the actual loadConfigOrDie
	config := loadConfigOrDie(tempFile.Name())

	// Verify Name and Regex are set correctly
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
		name    string
		toml    string
		wantW   int
		wantH   int
		wantFmt string
		wantQ   int
	}{
		{
			name: "defaults applied",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"`,
			wantW:   1024,
			wantH:   1024,
			wantFmt: "jpg",
			wantQ:   75,
		},
		{
			name: "valid jpg format",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"
imageformat = "jpg"`,
			wantFmt: "jpg",
		},
		{
			name: "valid webp format",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"
imageformat = "webp"`,
			wantFmt: "webp",
		},
		{
			name: "valid jpeg format",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"
imageformat = "jpeg"`,
			wantFmt: "jpeg",
		},
		{
			name: "valid quality 50",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"
imagequality = 50`,
			wantQ: 50,
		},
		{
			name: "valid maximagesize 1024x1024",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"
maximagesize = "1024x1024"`,
			wantW: 1024,
			wantH: 1024,
		},
		{
			name: "valid maximagesize 1024x768",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"
maximagesize = "1024x768"`,
			wantW: 1024,
			wantH: 768,
		},
		{
			name: "valid maximagesize 1920x1080",
			toml: `[services.test]
maxtokens = 100
[commands.chats.chat1]
service = "test"
maximagesize = "1920x1080"`,
			wantW: 1920,
			wantH: 1080,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempFile, err := os.CreateTemp("", "test_config_*.toml")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(tempFile.Name())

			if _, err := tempFile.WriteString(tt.toml); err != nil {
				t.Fatal(err)
			}
			tempFile.Close()

			config := loadConfigOrDie(tempFile.Name())
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
