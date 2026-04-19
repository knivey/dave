package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
)

func uploadImage(cfg Config, data []byte, filename string) (string, error) {
	body := &bytes.Buffer{}
	wr := multipart.NewWriter(body)

	formFile, err := wr.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}

	if _, err := formFile.Write(data); err != nil {
		return "", fmt.Errorf("writing form data: %w", err)
	}

	wr.WriteField("url_len", fmt.Sprintf("%d", cfg.Upload.URLLen))
	wr.WriteField("expiry", fmt.Sprintf("%d", cfg.Upload.Expiry))

	if err := wr.Close(); err != nil {
		return "", fmt.Errorf("closing multipart writer: %w", err)
	}

	resp, err := http.Post(cfg.Upload.URL, wr.FormDataContentType(), bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", fmt.Errorf("uploading: %w", err)
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading upload response: %w", err)
	}

	return strings.TrimSpace(string(b)), nil
}

func guessMIMEType(filename string, fallback string) string {
	if fallback == "" {
		fallback = "application/octet-stream"
	}
	ext := filepath.Ext(filename)
	if ext == "" {
		return fallback
	}
	mt := mime.TypeByExtension(ext)
	if mt == "" {
		return fallback
	}
	return mt
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
