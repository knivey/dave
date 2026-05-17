package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type json3Format struct {
	Events []json3Event `json:"events"`
}

type json3Event struct {
	Segs []json3Seg `json:"segs"`
}

type json3Seg struct {
	UTF8 string `json:"utf8"`
}

var youtubeURLPattern = regexp.MustCompile(`(?:youtube\.com/(?:watch\?.*v=|shorts/|embed/)|youtu\.be/)([a-zA-Z0-9_-]{11})`)

func extractVideoID(url string) (string, error) {
	matches := youtubeURLPattern.FindStringSubmatch(url)
	if len(matches) < 2 {
		return "", fmt.Errorf("could not extract video ID from URL (not a recognized YouTube URL)")
	}
	return matches[1], nil
}

func fetchTranscript(ctx context.Context, cfg YtdlpConfig, url, language string, rotator *ProxyRotator) (string, error) {
	videoID, err := extractVideoID(url)
	if err != nil {
		return "", err
	}

	if language == "" {
		language = cfg.Languages[0]
	}

	outputPath := filepath.Join(cfg.TempDir, fmt.Sprintf("%s.%s.json3", videoID, language))

	proxy := rotator.Next()
	result, err := fetchTranscriptWithProxy(ctx, cfg, url, language, outputPath, proxy)
	for attempts := 0; err != nil && attempts < cfg.Retries; attempts++ {
		proxy = rotator.Next()
		loggerTools.Info("retrying transcript fetch", "videoID", videoID, "attempt", attempts+1, "proxy", proxy)
		result, err = fetchTranscriptWithProxy(ctx, cfg, url, language, outputPath, proxy)
	}
	return result, err
}

func fetchTranscriptWithProxy(ctx context.Context, cfg YtdlpConfig, url, language, outputPath, proxy string) (string, error) {
	args := []string{
		"--write-auto-sub",
		"--sub-lang", language,
		"--skip-download",
		"--sub-format", "json3",
		"-o", outputPath,
	}
	if proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	args = append(args, url)

	loggerTools.Info("fetching transcript", "url", url, "lang", language, "proxy", proxy)

	cmd := exec.CommandContext(ctx, cfg.Path, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("yt-dlp failed: %w\n%s", err, string(output))
	}

	subFile := outputPath + "." + language + ".json3"
	data, err := os.ReadFile(subFile)
	if err != nil {
		return "", fmt.Errorf("reading subtitle file: %w", err)
	}
	defer os.Remove(subFile)

	transcript, err := parseJSON3Transcript(data)
	if err != nil {
		return "", err
	}

	if len(transcript) > cfg.MaxLength {
		transcript = transcript[:cfg.MaxLength] + "\n[...truncated]"
	}

	return transcript, nil
}

func parseJSON3Transcript(data []byte) (string, error) {
	var j3 json3Format
	if err := json.Unmarshal(data, &j3); err != nil {
		return "", fmt.Errorf("parsing json3: %w", err)
	}

	var lines []string
	for _, event := range j3.Events {
		if len(event.Segs) == 0 {
			continue
		}
		var sb strings.Builder
		for _, seg := range event.Segs {
			sb.WriteString(seg.UTF8)
		}
		text := strings.TrimSpace(sb.String())
		if text == "" || text == "\n" {
			continue
		}
		lines = append(lines, text)
	}

	return strings.Join(lines, " "), nil
}

type VideoInfo struct {
	Title       string `json:"title"`
	Channel     string `json:"channel"`
	Duration    string `json:"duration"`
	Description string `json:"description"`
	UploadDate  string `json:"upload_date"`
	ViewCount   int64  `json:"view_count"`
	URL         string `json:"url"`
}

func fetchVideoInfo(ctx context.Context, cfg YtdlpConfig, url string, rotator *ProxyRotator) (*VideoInfo, error) {
	proxy := rotator.Next()
	result, err := fetchVideoInfoWithProxy(ctx, cfg, url, proxy)
	for attempts := 0; err != nil && attempts < cfg.Retries; attempts++ {
		proxy = rotator.Next()
		loggerTools.Info("retrying video info fetch", "url", url, "attempt", attempts+1, "proxy", proxy)
		result, err = fetchVideoInfoWithProxy(ctx, cfg, url, proxy)
	}
	return result, err
}

func fetchVideoInfoWithProxy(ctx context.Context, cfg YtdlpConfig, url, proxy string) (*VideoInfo, error) {
	args := []string{
		"--dump-json",
		"--no-download",
	}
	if proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	args = append(args, url)

	loggerTools.Info("fetching video info", "url", url, "proxy", proxy)

	cmd := exec.CommandContext(ctx, cfg.Path, args...)
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("yt-dlp failed: %w\n%s", err, string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("yt-dlp failed: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("parsing yt-dlp JSON: %w", err)
	}

	info := &VideoInfo{
		Title:       strField(raw, "title"),
		Channel:     strField(raw, "channel"),
		Duration:    strField(raw, "duration_string"),
		Description: strField(raw, "description"),
		UploadDate:  strField(raw, "upload_date"),
		ViewCount:   int64(floatField(raw, "view_count")),
		URL:         strField(raw, "webpage_url"),
	}

	return info, nil
}

func strField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func floatField(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	f, ok := v.(float64)
	if !ok {
		return 0
	}
	return f
}
