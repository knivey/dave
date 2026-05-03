package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			got := tt.cfg.IsEnabled()
			assert.Equal(t, tt.expect, got, "IsEnabled()")
		})
	}
}

func TestNewIncidentLoggerDefaults(t *testing.T) {
	t.Run("no config section creates logger", func(t *testing.T) {
		cfg := IncidentConfig{}
		il, err := NewIncidentLogger(cfg)
		require.NoError(t, err, "unexpected error")
		require.NotNil(t, il, "expected non-nil IncidentLogger when Enabled is nil (default true)")
	})

	t.Run("explicit enabled true creates logger", func(t *testing.T) {
		cfg := IncidentConfig{Enabled: boolPtr(true)}
		il, err := NewIncidentLogger(cfg)
		require.NoError(t, err, "unexpected error")
		require.NotNil(t, il, "expected non-nil IncidentLogger")
	})

	t.Run("explicit enabled false returns nil", func(t *testing.T) {
		cfg := IncidentConfig{Enabled: boolPtr(false)}
		il, err := NewIncidentLogger(cfg)
		require.NoError(t, err, "unexpected error")
		assert.Nil(t, il, "expected nil IncidentLogger when explicitly disabled")
	})

	t.Run("custom dir is used", func(t *testing.T) {
		tmpDir := t.TempDir()
		customDir := filepath.Join(tmpDir, "my-incidents")
		cfg := IncidentConfig{Dir: customDir}
		il, err := NewIncidentLogger(cfg)
		require.NoError(t, err, "unexpected error")
		require.NotNil(t, il, "expected non-nil IncidentLogger")
		assert.Equal(t, customDir, il.dir, "dir")
		_, err = os.Stat(customDir)
		assert.False(t, os.IsNotExist(err), "incidents directory was not created")
	})
}

func TestLoadConfigDirIncidentDefault(t *testing.T) {
	t.Run("missing incident_log section defaults to enabled", func(t *testing.T) {
		dir := createTestConfigDir(t, "", nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)

		assert.True(t, config.IncidentLog.IsEnabled(), "IncidentLog should be enabled by default when section is missing")
	})

	t.Run("explicit enabled false in config", func(t *testing.T) {
		mainTOML := `
[incident_log]
enabled = false
`
		dir := createTestConfigDir(t, mainTOML, nil)
		defer os.RemoveAll(dir)

		config := loadConfigDirOrDie(dir)

		assert.False(t, config.IncidentLog.IsEnabled(), "IncidentLog should be disabled when explicitly set to false")
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

		assert.True(t, config.IncidentLog.IsEnabled(), "IncidentLog should be enabled when explicitly set to true")
		assert.Equal(t, "test-incidents", config.IncidentLog.Dir, "Dir")
	})
}
