package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ToolHandlers struct {
	mu      sync.RWMutex
	cfg     Config
	rotator *ProxyRotator
}

func NewToolHandlers(cfg Config) (*ToolHandlers, error) {
	stateDir := cfg.Ytdlp.TempDir
	rotator, err := NewProxyRotator(cfg.Ytdlp.ResolvedProxyFile, stateDir)
	if err != nil {
		return nil, fmt.Errorf("loading proxy file: %w", err)
	}

	return &ToolHandlers{cfg: cfg, rotator: rotator}, nil
}

func (h *ToolHandlers) getConfig() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *ToolHandlers) setConfig(cfg Config) error {
	if cfg.Ytdlp.ResolvedProxyFile != h.getConfig().Ytdlp.ResolvedProxyFile {
		if err := h.rotator.Reload(cfg.Ytdlp.ResolvedProxyFile); err != nil {
			return err
		}
	}
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
	return nil
}

type GetTranscriptInput struct {
	URL      string `json:"url" jsonschema:"YouTube video URL (e.g. https://www.youtube.com/watch?v=...)"`
	Language string `json:"language,omitempty" jsonschema:"language code for subtitles (e.g. en, es, fr). Defaults to configured language."`
}

type GetTranscriptOutput struct {
	Transcript string `json:"transcript"`
	VideoID    string `json:"video_id"`
	Language   string `json:"language"`
	Truncated  bool   `json:"truncated"`
}

func (h *ToolHandlers) handleGetTranscript(ctx context.Context, req *mcp.CallToolRequest, input GetTranscriptInput) (*mcp.CallToolResult, GetTranscriptOutput, error) {
	cfg := h.getConfig()

	timeoutCtx, cancel := context.WithTimeout(ctx, cfg.Ytdlp.Timeout)
	defer cancel()

	transcript, err := fetchTranscript(timeoutCtx, cfg.Ytdlp, input.URL, input.Language, h.rotator)
	if err != nil {
		return nil, GetTranscriptOutput{}, fmt.Errorf("failed to fetch transcript: %w", err)
	}

	videoID, _ := extractVideoID(input.URL)

	lang := input.Language
	if lang == "" {
		lang = cfg.Ytdlp.Languages[0]
	}

	truncated := len(transcript) >= cfg.Ytdlp.MaxLength

	return nil, GetTranscriptOutput{
		Transcript: transcript,
		VideoID:    videoID,
		Language:   lang,
		Truncated:  truncated,
	}, nil
}

type GetVideoInfoInput struct {
	URL string `json:"url" jsonschema:"YouTube video URL (e.g. https://www.youtube.com/watch?v=...)"`
}

type GetVideoInfoOutput struct {
	VideoInfo *VideoInfo `json:"video_info"`
}

func (h *ToolHandlers) handleGetVideoInfo(ctx context.Context, req *mcp.CallToolRequest, input GetVideoInfoInput) (*mcp.CallToolResult, GetVideoInfoOutput, error) {
	cfg := h.getConfig()

	timeoutCtx, cancel := context.WithTimeout(ctx, cfg.Ytdlp.Timeout)
	defer cancel()

	info, err := fetchVideoInfo(timeoutCtx, cfg.Ytdlp, input.URL, h.rotator)
	if err != nil {
		return nil, GetVideoInfoOutput{}, fmt.Errorf("failed to fetch video info: %w", err)
	}

	return nil, GetVideoInfoOutput{VideoInfo: info}, nil
}
