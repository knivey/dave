package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// uploadHTTPClient is a plain client with a sane timeout: the imgsite
// upload answers 201 + JSON on the same request, so there are no redirect
// hops to intercept (the old photo site's 303/307 dance is gone).
var uploadHTTPClient = &http.Client{
	Timeout: 2 * time.Minute,
}

// UploadMeta is the provenance payload img-mcp sends alongside the image
// bytes to imgsite's /updo endpoint. Every field is omitempty so empty
// values drop out of the JSON entirely; the site treats absent fields as
// "not provided" and falls back to EXIF extraction (see the merge policy in
// docs/image-site.md).
type UploadMeta struct {
	JobID          string `json:"job_id,omitempty"`
	OriginalPrompt string `json:"original_prompt,omitempty"`
	EnhancedPrompt string `json:"enhanced_prompt,omitempty"`
	NegativePrompt string `json:"negative_prompt,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
	LLMGenerated   bool   `json:"llm_generated,omitempty"`
	WorkflowName   string `json:"workflow_name,omitempty"`
	Network        string `json:"network,omitempty"`
	Channel        string `json:"channel,omitempty"`
	Nick           string `json:"nick,omitempty"`
}

// uploadResponse is the JSON body of imgsite's 201 answer.
type uploadResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Page     string `json:"page"`
	Filename string `json:"filename"`
}

// uploadImage POSTs the image to imgsite's /updo endpoint and returns the
// permanent PAGE link (the gallery details page) for the uploaded image —
// the link dave pastes to IRC.
//
// Wire protocol (docs/image-site.md, "Upload protocol"):
//
//	POST <base>/updo  X-API-Key, multipart: file, meta=<JSON> -> 201 + {id,url,page,filename}
//
// DESIGN NOTE: the returned link is handed to IRC verbatim — no client-side
// derivation, no verification GET. Both ends of this protocol are ours
// (imgsite builds url/page from its configured server.base_url), so a second
// hop would only re-ask the server what it just told us. The old photo-site
// contract (303 + Location parsing, /orig/ URL derivation, redirect-check
// verification, toirc=0 to silence its IRC announcer) was deleted wholesale
// when imgsite replaced it.
//
// The page field is preferred (the details page is where the prompt,
// params, and provenance live — pasting it on IRC gives readers context a
// bare image URL can't). When the response carries an empty page — older
// imgsite deployments — the direct url is the fallback, with a WARN so the
// skew is visible in the logs. A response with neither field is an error.
//
// A 401 means the key didn't match: the error names upload.api_key so the
// admin knows exactly which config knob is wrong. An empty api_key is
// allowed at startup (imgsite is the only consumer and the bot may run with
// generation temporarily broken) but is logged here, per call, so the
// failure mode is discoverable from the logs.
func uploadImage(cfg Config, data []byte, filename string, meta UploadMeta) (string, error) {
	filename = sanitizeUploadFilename(filename)
	base := strings.TrimRight(cfg.Upload.URL, "/")
	logger.Info("uploading image", "filename", filename, "size", len(data), "url", base)

	if cfg.Upload.APIKey == "" {
		logger.Warn("upload.api_key is not configured; imgsite will reject this upload with 401",
			"filename", filename)
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshaling upload meta: %w", err)
	}

	body := &bytes.Buffer{}
	wr := multipart.NewWriter(body)

	formFile, err := wr.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}

	if _, err := formFile.Write(data); err != nil {
		return "", fmt.Errorf("writing form data: %w", err)
	}

	wr.WriteField("meta", string(metaJSON))

	if err := wr.Close(); err != nil {
		return "", fmt.Errorf("closing multipart writer: %w", err)
	}

	// httptrace splits post_ms into its real costs: DNS+dial, TLS
	// handshake (the suspected Atom-CPU tax), request-body upload, and
	// the server's processing wait. Zero-value stages mean the event
	// never fired (e.g. no dial/tls on a reused connection).
	var connectDone, tlsDone, wroteReq, firstByte time.Time
	var connReused bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			connReused = info.Reused
		},
		ConnectDone: func(network, addr string, err error) {
			connectDone = time.Now()
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			tlsDone = time.Now()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			wroteReq = time.Now()
		},
		GotFirstResponseByte: func() {
			firstByte = time.Now()
		},
	}
	postStart := time.Now()
	req, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(context.Background(), trace),
		http.MethodPost, base+"/updo", bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", fmt.Errorf("creating upload request: %w", err)
	}
	req.Header.Set("Content-Type", wr.FormDataContentType())
	if cfg.Upload.APIKey != "" {
		req.Header.Set("X-API-Key", cfg.Upload.APIKey)
	}
	resp, err := uploadHTTPClient.Do(req)
	postDur := time.Since(postStart)
	if err != nil {
		logger.Error("upload request failed", "filename", filename, "error", err)
		return "", fmt.Errorf("uploading: %w", err)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized {
		logger.Error("upload rejected: unauthorized", "filename", filename, "status", resp.StatusCode)
		return "", fmt.Errorf("upload rejected with 401 unauthorized: check upload.api_key in the img-mcp config (it must match imgsite's auth.api_key)")
	}
	if resp.StatusCode != http.StatusCreated {
		logger.Error("upload returned unexpected status", "filename", filename, "status", resp.StatusCode)
		return "", fmt.Errorf("unexpected status from upload: %d: %s", resp.StatusCode, snippet(string(respBody), 200))
	}

	// Status came from the response headers, so the status checks above stay
	// authoritative; but a 201 whose body died mid-read must not fall
	// through to json.Unmarshal — a truncated body only produces a
	// confusing parse error. Surface the transport failure instead.
	if readErr != nil {
		logger.Error("reading upload response body failed", "filename", filename, "error", readErr)
		return "", fmt.Errorf("reading upload response: %w", readErr)
	}
	var parsed uploadResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("parsing upload response: %w", err)
	}

	// Page link first; empty page (older imgsite) falls back to the
	// direct url so a version skew degrades to the old paste, never to a
	// failed job.
	link := parsed.Page
	if link == "" {
		logger.Warn("upload response has an empty page field; falling back to the direct url (older imgsite deployment?)",
			"filename", filename, "image_id", parsed.ID, "url", parsed.URL)
		link = parsed.URL
	}
	if link == "" {
		return "", fmt.Errorf("upload response missing page and url")
	}

	// Stage boundaries for the log; stages that never fired (reused
	// connection skips dial/tls) stay zero and report 0ms.
	connReady := postStart
	if !connectDone.IsZero() {
		connReady = connectDone
	}
	if !tlsDone.IsZero() {
		connReady = tlsDone
	}

	logger.Info("upload complete", "filename", filename, "link", link, "url", parsed.URL,
		"image_id", parsed.ID,
		"post_ms", postDur.Milliseconds(),
		"conn_reused", connReused,
		"dial_ms", elapsedMs(postStart, connectDone),
		"tls_ms", elapsedMs(connectDone, tlsDone),
		"send_ms", elapsedMs(connReady, wroteReq),
		"srv_ms", elapsedMs(wroteReq, firstByte))
	return link, nil
}

// elapsedMs reports the milliseconds between two httptrace timestamps,
// or 0 when either boundary never fired (zero time).
func elapsedMs(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return 0
	}
	return to.Sub(from).Milliseconds()
}

// snippet clamps s to at most max runes for embedding in an error string
// (the upload site's bodies are short human strings, but the client must
// not trust that). Truncation happens on rune boundaries so a multi-byte
// character straddling the cut is never split into invalid UTF-8 — these
// strings end up in logs and pasted to IRC.
func snippet(s string, max int) string {
	// Fast path for the common short body: within the byte budget and
	// valid UTF-8 means at most max runes, so there is nothing to slice.
	if len(s) <= max && utf8.ValidString(s) {
		return s
	}
	// Walk runes until the budget is spent, slicing only at boundaries.
	// Invalid bytes decode as one-byte runes (RuneError, size 1) and pass
	// through untouched, so garbage bodies keep their bytes — just never
	// cut through the middle of a valid multi-byte sequence.
	cut := 0
	for runes := 0; runes < max; runes++ {
		_, size := utf8.DecodeRuneInString(s[cut:])
		if size == 0 { // decoded to the end: max runes or fewer
			return s
		}
		cut += size
	}
	if cut == len(s) {
		return s // exactly max runes — fits whole
	}
	return s[:cut] + "…"
}

// sanitizeUploadFilename keeps the /orig/ URL valid: imgsite builds the
// public direct link as <base>/<id>/orig/<filename>, so directory
// components would break it, and an empty name (possible from the
// upload_image tool's caller) needs a default the site will echo back in
// its response. Anything that could corrupt the multipart part header
// (control characters), smuggle a path past filepath.Base (Windows
// separators — Base does not split them on Linux), traverse paths (".."),
// or render oddly in the URL (non-printable-ASCII) falls back to
// "image.png"; callers reach this with ComfyUI-generated names or
// LLM-chosen ones, neither of which is trusted. imgsite carries a port of
// this function — keep the rules in sync.
func sanitizeUploadFilename(filename string) string {
	filename = filepath.Base(filename)
	if filename == "" || filename == "." || filename == ".." ||
		strings.ContainsAny(filename, "\x00\r\n\\/") ||
		strings.IndexFunc(filename, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
		return "image.png"
	}
	return filename
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
