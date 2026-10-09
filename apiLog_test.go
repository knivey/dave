package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestNextEphemeralAPILogIDUniqueNegative(t *testing.T) {
	seen := make(map[int64]bool)
	for i := 0; i < 100; i++ {
		id := nextEphemeralAPILogID()
		assert.Negative(t, id, "ids must never collide with DB session ids (>= 1)")
		assert.False(t, seen[id], "id %d repeated within a boot", id)
		seen[id] = true
	}
}

func TestNextEphemeralAPILogIDCrossBoot(t *testing.T) {
	// Simulate a restart: re-seed the counter as a later boot would and
	// confirm the new ids never repeat the old boot's (clock seed differs).
	old := map[int64]bool{
		nextEphemeralAPILogID(): true,
		nextEphemeralAPILogID(): true,
	}
	ephemeralAPILogSeq.Store(time.Now().UnixNano() + int64(time.Hour))
	for i := 0; i < 10; i++ {
		id := nextEphemeralAPILogID()
		assert.False(t, old[id], "id %d repeated across boots", id)
	}
}

func TestSyncAPISessionIDPrefersEphemeralOverride(t *testing.T) {
	transport := newDaveTransport(nil, nil)
	cr := &chatRunner{transport: transport, sessionID: 77}
	cr.syncAPISessionID()
	assert.Equal(t, int64(77), transport.sessionID, "zero override means log under sessionID")

	cr.apiLogSessionID = -42
	cr.syncAPISessionID()
	assert.Equal(t, int64(-42), transport.sessionID, "ephemeral override wins")
}

func TestEphemeralAPILogPerRunFiles(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAPILogger(APILogConfig{Dir: dir}, dir)
	require.NoError(t, err)

	id1, id2 := nextEphemeralAPILogID(), nextEphemeralAPILogID()
	l.RestoreSession(id1, "net", "#a", 7)
	l.RestoreSession(id2, "net", "#a", 7)
	l.LogRequest(id1, []byte(`{"a":1}`))
	l.LogRequest(id2, []byte(`{"b":2}`))

	p1 := l.GetSessionFilePath(id1)
	p2 := l.GetSessionFilePath(id2)
	require.NotEmpty(t, p1)
	require.NotEmpty(t, p2)
	assert.NotEqual(t, p1, p2, "each run gets its own file")

	b1, err := os.ReadFile(p1)
	require.NoError(t, err)
	assert.Contains(t, string(b1), `{"a":1}`)
	assert.NotContains(t, string(b1), `{"b":2}`, "no cross-run bleed")

	b2, err := os.ReadFile(p2)
	require.NoError(t, err)
	assert.Contains(t, string(b2), `{"b":2}`)
	assert.NotContains(t, string(b2), `{"a":1}`)

	// The legacy guard is untouched: session 0 still refuses.
	assert.Empty(t, l.GetSessionFilePath(0))
}
