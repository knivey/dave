package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type IncidentConfig struct {
	Enabled *bool  `toml:"enabled"`
	Dir     string `toml:"dir"`
}

func (c IncidentConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

type IncidentLogger struct {
	dir string
}

var incidentLogger *IncidentLogger

func NewIncidentLogger(cfg IncidentConfig) (*IncidentLogger, error) {
	if !cfg.IsEnabled() {
		return nil, nil
	}
	dir := cfg.Dir
	if dir == "" {
		dir = "incidents"
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(".", dir)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating incidents dir: %w", err)
	}
	return &IncidentLogger{dir: dir}, nil
}

func initIncidentLogger(cfg Config) {
	il, err := NewIncidentLogger(cfg.IncidentLog)
	if err != nil {
		logger.Error("incident logger init failed", "error", err)
		return
	}
	incidentLogger = il
	if il != nil {
		logger.Info("incident logging enabled", "dir", il.dir)
	}
}

type incidentInfo struct {
	Timestamp    string `json:"timestamp"`
	Error        string `json:"error"`
	APIPath      string `json:"api_path"`
	Streaming    bool   `json:"streaming"`
	Iteration    int    `json:"iteration"`
	Network      string `json:"network"`
	Channel      string `json:"channel"`
	Nick         string `json:"nick"`
	CtxKey       string `json:"ctx_key"`
	SessionID    int64  `json:"session_id"`
	CommandName  string `json:"command_name"`
	ServiceName  string `json:"service_name"`
	Model        string `json:"model"`
	ConvID       string `json:"conv_id,omitempty"`
	ResponseID   string `json:"response_id,omitempty"`
	APILogCopied bool   `json:"api_log_copied"`
}

type sanitizedAIConfig struct {
	Name                string         `json:"name"`
	Service             string         `json:"service"`
	Model               string         `json:"model"`
	Streaming           bool           `json:"streaming"`
	MaxTokens           int            `json:"maxtokens,omitempty"`
	MaxCompletionTokens int            `json:"maxcompletiontokens,omitempty"`
	Temperature         float32        `json:"temperature,omitempty"`
	MaxHistory          int            `json:"maxhistory,omitempty"`
	RenderMarkdown      bool           `json:"rendermarkdown"`
	MCPs                []string       `json:"mcps,omitempty"`
	TopP                float32        `json:"topp,omitempty"`
	PresencePenalty     float32        `json:"presencepenalty,omitempty"`
	FrequencyPenalty    float32        `json:"frequencypenalty,omitempty"`
	ReasoningEffort     string         `json:"reasoningeffort,omitempty"`
	ResponsesAPI        bool           `json:"responses_api"`
	PreviousResponseID  bool           `json:"previous_response_id"`
	Timeout             string         `json:"timeout,omitempty"`
	StreamTimeout       string         `json:"streamtimeout,omitempty"`
	ExtraBody           map[string]any `json:"extra_body,omitempty"`
	System              string         `json:"system,omitempty"`
}

func sanitizeAIConfig(cfg AIConfig) sanitizedAIConfig {
	s := sanitizedAIConfig{
		Name:                cfg.Name,
		Service:             cfg.Service,
		Model:               cfg.Model,
		Streaming:           cfg.Streaming,
		MaxTokens:           cfg.MaxTokens,
		MaxCompletionTokens: cfg.MaxCompletionTokens,
		Temperature:         cfg.Temperature,
		MaxHistory:          cfg.MaxHistory,
		RenderMarkdown:      cfg.RenderMarkdown,
		MCPs:                cfg.MCPs,
		TopP:                cfg.TopP,
		PresencePenalty:     cfg.PresencePenalty,
		FrequencyPenalty:    cfg.FrequencyPenalty,
		ReasoningEffort:     cfg.ReasoningEffort,
		ResponsesAPI:        cfg.ResponsesAPI,
		PreviousResponseID:  cfg.PreviousResponseID,
		ExtraBody:           cfg.ExtraBody,
		System:              cfg.System,
	}
	if cfg.Timeout != 0 {
		s.Timeout = cfg.Timeout.String()
	}
	if cfg.StreamTimeout != 0 {
		s.StreamTimeout = cfg.StreamTimeout.String()
	}
	return s
}

func (il *IncidentLogger) logIncident(cr *chatRunner, apiErr error, messages []ChatMessage, iteration int, apiPath string) {
	if il == nil {
		return
	}

	ts := time.Now()
	chatCtx := GetContext(cr.ctxKey)

	incidentDirName := fmt.Sprintf("%s_%s", ts.Format("20060102-150405"), sanitizeKey(cr.ctxKey))
	incidentDir := filepath.Join(il.dir, incidentDirName)
	if err := os.MkdirAll(incidentDir, 0755); err != nil {
		cr.logger.Error("failed to create incident directory", "dir", incidentDir, "error", err)
		return
	}

	info := incidentInfo{
		Timestamp:   ts.UTC().Format(time.RFC3339Nano),
		Error:       apiErr.Error(),
		APIPath:     apiPath,
		Streaming:   cr.cfg.Streaming,
		Iteration:   iteration,
		Network:     cr.network.Name,
		Channel:     cr.channel,
		Nick:        cr.nick,
		CtxKey:      cr.ctxKey,
		SessionID:   chatCtx.SessionID,
		CommandName: cr.cfg.Name,
		ServiceName: cr.cfg.Service,
		Model:       cr.cfg.Model,
		ConvID:      chatCtx.ConvID,
		ResponseID:  chatCtx.ResponseID,
	}

	info.APILogCopied = copyAPILog(chatCtx.SessionID, incidentDir)

	if err := writeJSONFile(filepath.Join(incidentDir, "incident.json"), info); err != nil {
		cr.logger.Error("failed to write incident.json", "error", err)
	}

	if err := writeJSONFile(filepath.Join(incidentDir, "messages.json"), messages); err != nil {
		cr.logger.Error("failed to write messages.json", "error", err)
	}

	if lastReq := cr.transport.getLastRequestBody(); len(lastReq) > 0 {
		if err := writeRawJSONFile(filepath.Join(incidentDir, "request.json"), lastReq); err != nil {
			cr.logger.Error("failed to write request.json", "error", err)
		}
	}

	if err := writeJSONFile(filepath.Join(incidentDir, "config.json"), sanitizeAIConfig(cr.cfg)); err != nil {
		cr.logger.Error("failed to write config.json", "error", err)
	}

	cr.logger.Info("incident logged", "dir", incidentDirName)
}

func copyAPILog(sessionID int64, destDir string) bool {
	if apiLogger == nil {
		return false
	}
	srcPath := apiLogger.GetSessionFilePath(sessionID)
	if srcPath == "" {
		return false
	}
	apiLogger.SyncSession(sessionID)

	src, err := os.Open(srcPath)
	if err != nil {
		return false
	}
	defer src.Close()

	dst, err := os.Create(filepath.Join(destDir, "api_log.jsonl"))
	if err != nil {
		return false
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err == nil
}

func writeJSONFile(path string, data any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}

func writeRawJSONFile(path string, data json.RawMessage) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	return enc.Encode(data)
}
