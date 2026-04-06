package main

import "testing"

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
