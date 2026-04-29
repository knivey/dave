package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var pastebinHTTPClient = &http.Client{Timeout: 15 * time.Second}

type pastebinCreateRequest struct {
	Content string `json:"content"`
}

type pastebinCreateResponse struct {
	Slug string `json:"slug"`
	URL  string `json:"url"`
}

type pastebinErrorResponse struct {
	Error string `json:"error"`
}

func uploadToPastebin(text string) (string, error) {
	if config.Pastebin.URL == "" {
		return "", fmt.Errorf("pastebin: no url configured")
	}
	if config.Pastebin.APIKey == "" {
		return "", fmt.Errorf("pastebin: no api_key configured")
	}

	body, err := json.Marshal(pastebinCreateRequest{Content: text})
	if err != nil {
		return "", fmt.Errorf("pastebin: encoding request: %w", err)
	}

	url := strings.TrimRight(config.Pastebin.URL, "/") + "/api/pastes"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("pastebin: creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", config.Pastebin.APIKey)

	resp, err := pastebinHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pastebin: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("pastebin: reading response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		var errResp pastebinErrorResponse
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			return "", fmt.Errorf("pastebin: %s", errResp.Error)
		}
		return "", fmt.Errorf("pastebin: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var result pastebinCreateResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("pastebin: parsing response: %w", err)
	}

	return result.URL, nil
}
