package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

type APILogConfig struct {
	Dir          string `toml:"dir"`
	LogRawStream bool   `toml:"log_raw_stream"`
}

type apiLogEntry struct {
	Type string          `json:"type"`
	Ts   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type APISession struct {
	file   *os.File
	mu     sync.Mutex
	logKey string
}

type APILogger struct {
	cfg      APILogConfig
	dir      string
	mu       sync.Mutex
	sessions map[int64]*APISession
}

var apiLogger *APILogger

// ephemeralAPILogSeq seeds per-run api-log session ids for ephemeral
// (generator) turns. Seeded from the clock once at boot and stepped
// atomically: ids are unique within a boot AND across restarts, and
// negative so they can never collide with DB session ids (which start
// at 1). DESIGN NOTE: the alternative — one shared session-0 bucket
// per network/channel/user — was rejected by the owner: every
// generator run would append into one ever-growing file; per-run ids
// give each run its own file, exactly like real sessions.
var ephemeralAPILogSeq atomic.Int64

func init() {
	ephemeralAPILogSeq.Store(time.Now().UnixNano())
}

// nextEphemeralAPILogID allocates the api-log session id for one
// ephemeral run.
func nextEphemeralAPILogID() int64 {
	return -ephemeralAPILogSeq.Add(1)
}

var nonAlphaNum = regexp.MustCompile(`[^a-zA-Z0-9]`)

func sanitizeKey(key string) string {
	return nonAlphaNum.ReplaceAllString(key, "_")
}

func NewAPILogger(cfg APILogConfig, configDir string) (*APILogger, error) {
	dir := cfg.Dir
	if dir == "" {
		dir = "api_logs"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(".", dir)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating api log dir: %w", err)
	}
	return &APILogger{
		cfg:      cfg,
		dir:      dir,
		sessions: make(map[int64]*APISession),
	}, nil
}

func apiLogFilename(network, channel string, userID, sessionID int64) string {
	return fmt.Sprintf("%s_%d.jsonl", sanitizeKey(fmt.Sprintf("%s_%s_user%d", network, channel, userID)), sessionID)
}

func (l *APILogger) getSession(sessionID int64, network, channel string, userID int64) (*APISession, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.sessions[sessionID]; ok {
		return s, nil
	}
	if network == "" {
		return nil, fmt.Errorf("no api log session for id %d and no network/channel/userID provided", sessionID)
	}
	path := filepath.Join(l.dir, apiLogFilename(network, channel, userID, sessionID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating api log file: %w", err)
	}
	s := &APISession{file: f, logKey: fmt.Sprintf("%s_%s_user%d", network, channel, userID)}
	l.sessions[sessionID] = s
	return s, nil
}

func (l *APILogger) RestoreSession(sessionID int64, network, channel string, userID int64) {
	if l == nil || sessionID == 0 {
		return
	}
	if _, err := l.getSession(sessionID, network, channel, userID); err != nil {
		logger.Error("failed to restore api log session", "session_id", sessionID, "error", err)
	}
}

func (s *APISession) writeEntry(entryType string, body json.RawMessage) error {
	entry := apiLogEntry{
		Type: entryType,
		Ts:   time.Now().UTC().Format(time.RFC3339Nano),
		Body: body,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = s.file.Write(data)
	return err
}

func (l *APILogger) LogRequest(sessionID int64, body []byte) {
	if l == nil || sessionID == 0 {
		return
	}
	s, err := l.getSession(sessionID, "", "", 0)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEntry("request", json.RawMessage(body))
}

func (l *APILogger) LogResponse(sessionID int64, body []byte) {
	if l == nil || sessionID == 0 {
		return
	}
	s, err := l.getSession(sessionID, "", "", 0)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEntry("response", json.RawMessage(body))
}

func (l *APILogger) LogStreamChunk(sessionID int64, chunk []byte) {
	if l == nil || !l.cfg.LogRawStream || sessionID == 0 {
		return
	}
	s, err := l.getSession(sessionID, "", "", 0)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEntry("stream_chunk", json.RawMessage(chunk))
}

func (l *APILogger) LogStreamResponse(sessionID int64, reconstructed json.RawMessage) {
	if l == nil || sessionID == 0 {
		return
	}
	s, err := l.getSession(sessionID, "", "", 0)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEntry("response", reconstructed)
}

func (l *APILogger) CloseAll() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, s := range l.sessions {
		s.file.Close()
		delete(l.sessions, key)
	}
}

func (l *APILogger) GetSessionFilePath(sessionID int64) string {
	if l == nil || sessionID == 0 {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sessions[sessionID]
	if !ok {
		return ""
	}
	return s.file.Name()
}

func (l *APILogger) SyncSession(sessionID int64) {
	if l == nil || sessionID == 0 {
		return
	}
	s, err := l.getSession(sessionID, "", "", 0)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.file.Sync()
}

func initAPILogger(cfg Config, dir string) {
	var err error
	apiLogger, err = NewAPILogger(cfg.APILog, dir)
	if err != nil {
		logger.Error("api log init failed", "error", err)
	}
	if apiLogger != nil {
		logger.Info("api logging enabled", "dir", apiLogger.dir)
	}
}
