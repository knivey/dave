package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/bartventer/httpcache"
	_ "github.com/bartventer/httpcache/store/memcache"
)

type FetchResult struct {
	Markdown          string
	Title             string
	CacheStatus       string
	FromMarkdownCache bool
	Truncated         bool
	StartIndex        int
	NextIndex         int
	IsRawContent      bool
}

type Fetcher struct {
	client  *http.Client
	mdConv  *converter.Converter
	cfg     Config
	mdCache *MarkdownCache
}

func NewFetcher(cfg Config, mdCache *MarkdownCache) (*Fetcher, error) {
	baseTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	if cfg.Fetch.ProxyURL != "" {
		proxyURL, err := url.Parse(cfg.Fetch.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("parsing proxy URL %q: %w", cfg.Fetch.ProxyURL, err)
		}
		baseTransport.Proxy = http.ProxyURL(proxyURL)
	}

	transport := httpcache.NewTransport(
		cfg.Cache.DSN,
		httpcache.WithUpstream(baseTransport),
	)

	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.Fetch.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= cfg.Fetch.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", cfg.Fetch.MaxRedirects)
			}
			req.Header.Set("User-Agent", cfg.Fetch.UserAgent)
			return nil
		},
	}

	mdConv := converter.NewConverter(
		converter.WithPlugins(
			base.NewBasePlugin(),
			commonmark.NewCommonmarkPlugin(),
		),
	)

	return &Fetcher{
		client:  client,
		mdConv:  mdConv,
		cfg:     cfg,
		mdCache: mdCache,
	}, nil
}

func (f *Fetcher) Fetch(ctx context.Context, rawURL string, startIndex int, maxLength int) (*FetchResult, error) {
	if maxLength <= 0 {
		maxLength = 20000
	}

	cached := f.mdCache.Get(rawURL)
	if cached != nil {
		content, truncated := paginate(cached.Markdown, startIndex, maxLength)
		return &FetchResult{
			Markdown:          content,
			Title:             cached.Title,
			FromMarkdownCache: true,
			CacheStatus:       "HIT",
			Truncated:         truncated,
			StartIndex:        startIndex,
			NextIndex:         nextIndex(startIndex, maxLength, len(cached.Markdown)),
		}, nil
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", f.cfg.Fetch.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching URL: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	cacheStatus := resp.Header.Get("X-Httpcache-Status")

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	if !isHTML(contentType, bodyBytes) {
		content := string(bodyBytes)
		content, truncated := paginate(content, startIndex, maxLength)
		return &FetchResult{
			Markdown:     content,
			CacheStatus:  cacheStatus,
			IsRawContent: true,
			Truncated:    truncated,
			StartIndex:   startIndex,
			NextIndex:    nextIndex(startIndex, maxLength, len(bodyBytes)),
		}, nil
	}

	markdown, title, err := f.convertToMarkdown(rawURL, bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("converting to markdown: %w", err)
	}

	f.mdCache.Set(rawURL, &CachedMarkdown{
		Markdown: markdown,
		Title:    title,
	})

	content, truncated := paginate(markdown, startIndex, maxLength)
	return &FetchResult{
		Markdown:    content,
		Title:       title,
		CacheStatus: cacheStatus,
		Truncated:   truncated,
		StartIndex:  startIndex,
		NextIndex:   nextIndex(startIndex, maxLength, len(markdown)),
	}, nil
}

func (f *Fetcher) convertToMarkdown(pageURL string, body []byte) (string, string, error) {
	parsedURL, _ := url.Parse(pageURL)

	if f.cfg.Readability.Disabled {
		markdown, err := f.mdConv.ConvertString(string(body))
		if err != nil {
			return "", "", fmt.Errorf("markdown conversion: %w", err)
		}
		return markdown, extractTitle(body), nil
	}

	article, err := readability.FromReader(bytes.NewReader(body), parsedURL)
	if err != nil || article.Node == nil {
		markdown, convErr := f.mdConv.ConvertString(string(body))
		if convErr != nil {
			return "", "", fmt.Errorf("readability failed (%v) and markdown conversion failed: %w", err, convErr)
		}
		return markdown, extractTitle(body), nil
	}

	var contentBuf bytes.Buffer
	if renderErr := article.RenderHTML(&contentBuf); renderErr != nil {
		markdown, convErr := f.mdConv.ConvertString(string(body))
		if convErr != nil {
			return "", "", fmt.Errorf("render html failed (%v) and markdown conversion failed: %w", renderErr, convErr)
		}
		return markdown, article.Title(), nil
	}

	markdown, err := f.mdConv.ConvertString(contentBuf.String())
	if err != nil {
		return "", "", fmt.Errorf("markdown conversion: %w", err)
	}

	return markdown, article.Title(), nil
}

func isHTML(contentType string, body []byte) bool {
	if strings.Contains(contentType, "text/html") ||
		strings.Contains(contentType, "application/xhtml") {
		return true
	}
	trimmed := body
	if len(trimmed) > 100 {
		trimmed = trimmed[:100]
	}
	lower := strings.ToLower(string(trimmed))
	return strings.Contains(lower, "<html")
}

func extractTitle(body []byte) string {
	lower := strings.ToLower(string(body))
	start := strings.Index(lower, "<title>")
	if start == -1 {
		return ""
	}
	end := strings.Index(lower[start+7:], "</title>")
	if end == -1 {
		return ""
	}
	return lower[start+7 : start+7+end]
}

func paginate(content string, startIndex, maxLength int) (string, bool) {
	if startIndex >= len(content) {
		return "", false
	}
	remaining := content[startIndex:]
	if len(remaining) <= maxLength {
		return remaining, false
	}
	return remaining[:maxLength], true
}

func nextIndex(startIndex, maxLength, totalLen int) int {
	next := startIndex + maxLength
	if next >= totalLen {
		return 0
	}
	return next
}
