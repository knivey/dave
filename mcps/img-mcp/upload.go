package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// uploadHTTPClient never follows redirects: the upload site answers
// POST /updo with a 303 to the new photo's page, and GET /<id>/orig/<file>
// with a 307 to the backing file. Both Location headers are data we need
// to read, not hops to follow.
var uploadHTTPClient = &http.Client{
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
	Timeout: 2 * time.Minute,
}

// uploadImage POSTs the image to the photo site's /updo endpoint and
// returns the photo's /orig/ URL (e.g. https://img.zkpq.ca/4F4/orig/x.png),
// which serves the original bytes via a 307 to /file/<hash>/<name>.
//
// Wire protocol (verified live against img.zkpq.ca):
//
//	POST <base>/updo              multipart: file, toirc=0 -> 303 + Location: /<id>/<slug>
//	GET  <base>/<id>/orig/<file>                            -> 307 + Location: /file/...
//
// DESIGN NOTE: toirc=0 must be sent explicitly on every upload. The
// field defaults to ON when absent, and the site then announces each
// upload to IRC itself — a duplicate of the notice dave posts, so we
// suppress it. The GET on the /orig/ URL is a validation hop: the URL
// is derived from the 303's photo ID plus the filename we chose, so a
// server-side filename mismatch would surface as a non-redirect here
// instead of as a dead link pasted to IRC.
func uploadImage(cfg Config, data []byte, filename string) (string, error) {
	filename = sanitizeUploadFilename(filename)
	base := strings.TrimRight(cfg.Upload.URL, "/")
	logger.Info("uploading image", "filename", filename, "size", len(data), "url", base)

	body := &bytes.Buffer{}
	wr := multipart.NewWriter(body)

	formFile, err := wr.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}

	if _, err := formFile.Write(data); err != nil {
		return "", fmt.Errorf("writing form data: %w", err)
	}

	wr.WriteField("toirc", "0")

	if err := wr.Close(); err != nil {
		return "", fmt.Errorf("closing multipart writer: %w", err)
	}

	postStart := time.Now()
	resp, err := uploadHTTPClient.Post(base+"/updo", wr.FormDataContentType(), bytes.NewReader(body.Bytes()))
	postDur := time.Since(postStart)
	if err != nil {
		logger.Error("upload request failed", "filename", filename, "error", err)
		return "", fmt.Errorf("uploading: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusSeeOther {
		logger.Error("upload returned unexpected status", "filename", filename, "status", resp.StatusCode)
		return "", fmt.Errorf("unexpected status from upload: %d", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("upload response missing Location header")
	}
	locURL, err := url.Parse(loc)
	if err != nil {
		return "", fmt.Errorf("parsing upload Location %q: %w", loc, err)
	}
	segments := strings.Split(strings.Trim(locURL.Path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", fmt.Errorf("upload Location %q has no photo id", loc)
	}

	origURL := base + "/" + url.PathEscape(segments[0]) + "/orig/" + url.PathEscape(filename)

	verifyStart := time.Now()
	vresp, err := uploadHTTPClient.Get(origURL)
	verifyDur := time.Since(verifyStart)
	if err != nil {
		logger.Error("verifying upload failed", "filename", filename, "url", origURL, "error", err)
		return "", fmt.Errorf("verifying upload: %w", err)
	}
	defer vresp.Body.Close()
	io.Copy(io.Discard, vresp.Body)

	if vresp.StatusCode < 300 || vresp.StatusCode > 399 {
		logger.Error("upload verification returned unexpected status", "filename", filename, "url", origURL, "status", vresp.StatusCode)
		return "", fmt.Errorf("verifying %s: unexpected status %d", origURL, vresp.StatusCode)
	}
	if vresp.Header.Get("Location") == "" {
		return "", fmt.Errorf("verifying %s: redirect missing Location header", origURL)
	}

	logger.Info("upload complete", "filename", filename, "url", origURL,
		"post_ms", postDur.Milliseconds(), "verify_ms", verifyDur.Milliseconds())
	return origURL, nil
}

// sanitizeUploadFilename keeps the /orig/ URL derivable: the URL path is
// <base>/<id>/orig/<filename>, so directory components would break it,
// and an empty name (possible from the upload_image tool's caller)
// needs a default the site will echo back in its redirect target.
// Anything that could corrupt the multipart part header (control
// characters), smuggle a path past filepath.Base (Windows separators —
// Base does not split them on Linux), traverse paths (".."), or render
// oddly in the URL (non-printable-ASCII) falls back to "image.png";
// callers reach this with ComfyUI-generated names or LLM-chosen ones,
// neither of which is trusted.
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
