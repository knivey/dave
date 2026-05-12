package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPILogFilename(t *testing.T) {
	tests := []struct {
		name      string
		network   string
		channel   string
		userID    int64
		sessionID int64
		want      string
	}{
		{
			name:      "basic",
			network:   "birdnest",
			channel:   "#channel",
			userID:    3,
			sessionID: 42,
			want:      "birdnest__channel_user3_42.jsonl",
		},
		{
			name:      "special chars sanitized",
			network:   "my-net",
			channel:   "#chan!nel",
			userID:    10,
			sessionID: 7,
			want:      "my_net__chan_nel_user10_7.jsonl",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apiLogFilename(tt.network, tt.channel, tt.userID, tt.sessionID)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAPILoggerRestoreAndGetSession(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAPILogger(APILogConfig{Dir: dir}, "")
	require.NoError(t, err)
	defer l.CloseAll()

	sessionID := int64(100)

	l.RestoreSession(sessionID, "testnet", "#chan", 5)

	path := l.GetSessionFilePath(sessionID)
	require.NotEmpty(t, path)
	assert.Equal(t, filepath.Join(dir, "testnet__chan_user5_100.jsonl"), path)

	_, err = os.Stat(path)
	assert.NoError(t, err, "log file should exist on disk")
}

func TestAPILoggerRestoreSessionNil(t *testing.T) {
	var l *APILogger
	assert.NotPanics(t, func() {
		l.RestoreSession(1, "net", "#chan", 1)
	})
}

func TestAPILoggerRestoreSessionZeroID(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAPILogger(APILogConfig{Dir: dir}, "")
	require.NoError(t, err)
	defer l.CloseAll()

	l.RestoreSession(0, "net", "#chan", 1)
	assert.Empty(t, l.GetSessionFilePath(0))
}

func TestAPILoggerLogRequestAfterRestore(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAPILogger(APILogConfig{Dir: dir}, "")
	require.NoError(t, err)
	defer l.CloseAll()

	sessionID := int64(200)
	l.RestoreSession(sessionID, "net", "#chan", 42)

	assert.NotPanics(t, func() {
		l.LogRequest(sessionID, []byte(`{"test":true}`))
	})

	path := l.GetSessionFilePath(sessionID)
	require.NotEmpty(t, path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"request"`)
	assert.Contains(t, string(data), `"test":true`)
}

func TestAPILoggerLogWithoutRestore(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAPILogger(APILogConfig{Dir: dir}, "")
	require.NoError(t, err)
	defer l.CloseAll()

	assert.NotPanics(t, func() {
		l.LogRequest(999, []byte(`{}`))
	})
}

func TestAPILoggerSameSessionIDDifferentUser(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAPILogger(APILogConfig{Dir: dir}, "")
	require.NoError(t, err)
	defer l.CloseAll()

	sessionID := int64(300)
	l.RestoreSession(sessionID, "net", "#chan", 1)

	l.RestoreSession(sessionID, "net", "#chan", 2)

	path := l.GetSessionFilePath(sessionID)
	assert.Contains(t, path, "user1", "first restore wins since sessionID already cached")
}

func TestAPILoggerDefaultDir(t *testing.T) {
	l, err := NewAPILogger(APILogConfig{}, ".")
	require.NoError(t, err)
	defer l.CloseAll()
	assert.Equal(t, "api_logs", filepath.Base(l.dir))
}
