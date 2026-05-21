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
	fetcher *Fetcher
	mdCache *MarkdownCache
}

func NewToolHandlers(cfg Config) (*ToolHandlers, error) {
	mdCache := NewMarkdownCache(cfg.Cache.MaxMarkdownAge)
	fetcher, err := NewFetcher(cfg, mdCache)
	if err != nil {
		return nil, fmt.Errorf("creating fetcher: %w", err)
	}
	return &ToolHandlers{
		cfg:     cfg,
		fetcher: fetcher,
		mdCache: mdCache,
	}, nil
}

func (h *ToolHandlers) getConfig() Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *ToolHandlers) setConfig(cfg Config) error {
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
	return nil
}

type FetchInput struct {
	URL        string `json:"url" jsonschema:"The URL to fetch and convert to markdown"`
	MaxLength  int    `json:"max_length,omitempty" jsonschema:"Maximum number of characters to return. Default 20000."`
	StartIndex int    `json:"start_index,omitempty" jsonschema:"On return output documenting where to start the next page. Start from 0 on the first call."`
	Raw        bool   `json:"raw,omitempty" jsonschema:"If true, skip markdown conversion and return raw content"`
}

type FetchOutput struct {
	Content           string `json:"content"`
	ContentType       string `json:"content_type,omitempty"`
	Title             string `json:"title,omitempty"`
	StartIndex        int    `json:"start_index"`
	NextIndex         int    `json:"next_index,omitempty"`
	Truncated         bool   `json:"truncated"`
	CacheStatus       string `json:"cache_status,omitempty"`
	FromMarkdownCache bool   `json:"from_markdown_cache,omitempty"`
}

const maxAllowedLength = 100000

func (h *ToolHandlers) handleFetch(ctx context.Context, req *mcp.CallToolRequest, input FetchInput) (*mcp.CallToolResult, FetchOutput, error) {
	maxLength := input.MaxLength
	if maxLength <= 0 {
		maxLength = 20000
	}
	if maxLength > maxAllowedLength {
		maxLength = maxAllowedLength
	}

	result, err := h.fetcher.Fetch(ctx, input.URL, input.StartIndex, maxLength)
	if err != nil {
		return nil, FetchOutput{}, fmt.Errorf("fetch failed: %w", err)
	}

	content := result.Markdown
	if result.Truncated && result.NextIndex > 0 {
		content += fmt.Sprintf("\n\n<error>Content truncated. Call again with start_index=%d to get more content.</error>", result.NextIndex)
	}

	contentType := "markdown"
	if result.IsRawContent {
		contentType = "raw"
	}

	output := FetchOutput{
		Content:           content,
		ContentType:       contentType,
		Title:             result.Title,
		StartIndex:        result.StartIndex,
		NextIndex:         result.NextIndex,
		Truncated:         result.Truncated,
		CacheStatus:       result.CacheStatus,
		FromMarkdownCache: result.FromMarkdownCache,
	}

	logger.Info("fetch completed",
		"url", input.URL,
		"cache_status", result.CacheStatus,
		"from_md_cache", result.FromMarkdownCache,
		"truncated", result.Truncated,
		"start_index", result.StartIndex,
		"next_index", result.NextIndex,
	)

	return nil, output, nil
}
