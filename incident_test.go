package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIncidentConfigIsEnabled(t *testing.T) {
	tests := []struct {
		name   string
		cfg    IncidentConfig
		expect bool
	}{
		{
			name:   "nil Enabled defaults to true",
			cfg:    IncidentConfig{},
			expect: true,
		},
		{
			name:   "explicit true",
			cfg:    IncidentConfig{Enabled: boolPtr(true)},
			expect: true,
		},
		{
			name:   "explicit false",
			cfg:    IncidentConfig{Enabled: boolPtr(false)},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEnabled(); got != tt.expect {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestNewIncidentLoggerDefaults(t *testing.T) {
	t.Run("no config section creates logger", func(t *testing.T) {
		cfg := IncidentConfig{}
		il, err := NewIncidentLogger(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if il == nil {
			t.Fatal("expected non-nil IncidentLogger when Enabled is nil (default true)")
		}
	})

	t.Run("explicit enabled true creates logger", func(t *testing.T) {
		cfg := IncidentConfig{Enabled: boolPtr(true)}
		il, err := NewIncidentLogger(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if il == nil {
			t.Fatal("expected non-nil IncidentLogger")
		}
	})

	t.Run("explicit enabled false returns nil", func(t *testing.T) {
		cfg := IncidentConfig{Enabled: boolPtr(false)}
		il, err := NewIncidentLogger(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if il != nil {
			t.Fatal("expected nil IncidentLogger when explicitly disabled")
		}
	})

	t.Run("custom dir is used", func(t *testing.T) {
		tmpDir := t.TempDir()
		customDir := filepath.Join(tmpDir, "my-incidents")
		cfg := IncidentConfig{Dir: customDir}
		il, err := NewIncidentLogger(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if il == nil {
			t.Fatal("expected non-nil IncidentLogger")
		}
		if il.dir != customDir {
			t.Errorf("dir = %q, want %q", il.dir, customDir)
		}
		if _, err := os.Stat(customDir); os.IsNotExist(err) {
			t.Error("incidents directory was not created")
		}
	})
}

func TestLoadConfigDirIncidentDefault(t *testing.T) {
	t.Run("missing incident_log section defaults to enabled", func(t *testing.T) {
		dir := createTestConfigDir(t, "", nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)

		if !config.IncidentLog.IsEnabled() {
			t.Error("IncidentLog should be enabled by default when section is missing")
		}
	})

	t.Run("explicit enabled false in config", func(t *testing.T) {
		mainTOML := `
[incident_log]
enabled = false
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)

		if config.IncidentLog.IsEnabled() {
			t.Error("IncidentLog should be disabled when explicitly set to false")
		}
	})

	t.Run("explicit enabled true in config", func(t *testing.T) {
		mainTOML := `
[incident_log]
enabled = true
dir = "test-incidents"
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)

		if !config.IncidentLog.IsEnabled() {
			t.Error("IncidentLog should be enabled when explicitly set to true")
		}
		if config.IncidentLog.Dir != "test-incidents" {
			t.Errorf("Dir = %q, want %q", config.IncidentLog.Dir, "test-incidents")
		}
	})
}
