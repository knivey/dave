package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type APILogConfig struct {
	Enabled      bool   `toml:"enabled"`
	Dir          string `toml:"dir"`
	LogRawStream bool   `toml:"log_raw_stream"`
}

type apiLogEntry struct {
	Type string          `json:"type"`
	Ts   string          `json:"ts"`
	Body json.RawMessage `json:"body"`
}

type APISession struct {
	file *os.File
	mu   sync.Mutex
}

type APILogger struct {
	cfg      APILogConfig
	dir      string
	mu       sync.Mutex
	sessions map[string]*APISession
}

var apiLogger *APILogger

func NewAPILogger(cfg APILogConfig, configDir string) (*APILogger, error) {
	if !cfg.Enabled {
		return nil, nil
	}
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
		sessions: make(map[string]*APISession),
	}, nil
}

var nonAlphaNum = regexp.MustCompile(`[^a-zA-Z0-9]`)

func sanitizeKey(key string) string {
	return nonAlphaNum.ReplaceAllString(key, "_")
}

func (l *APILogger) getSession(ctxKey string) (*APISession, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.sessions[ctxKey]; ok {
		return s, nil
	}
	ts := time.Now().Format("20060102-150405")
	name := fmt.Sprintf("%s_%s.jsonl", ts, sanitizeKey(ctxKey))
	path := filepath.Join(l.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("creating api log file: %w", err)
	}
	s := &APISession{file: f}
	l.sessions[ctxKey] = s
	return s, nil
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

func (l *APILogger) LogRequest(ctxKey string, body []byte) {
	if l == nil {
		return
	}
	s, err := l.getSession(ctxKey)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEntry("request", json.RawMessage(body))
}

func (l *APILogger) LogResponse(ctxKey string, body []byte) {
	if l == nil {
		return
	}
	s, err := l.getSession(ctxKey)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEntry("response", json.RawMessage(body))
}

func (l *APILogger) LogStreamChunk(ctxKey string, chunk []byte) {
	if l == nil || !l.cfg.LogRawStream {
		return
	}
	s, err := l.getSession(ctxKey)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeEntry("stream_chunk", json.RawMessage(chunk))
}

func (l *APILogger) LogStreamResponse(ctxKey string, reconstructed json.RawMessage) {
	if l == nil {
		return
	}
	s, err := l.getSession(ctxKey)
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
