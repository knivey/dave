package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
	logxi "github.com/mgutz/logxi/v1"
)

// loggerEditInputs is assigned in initLogger (logging.go) alongside the other
// package loggers so every channel shares the same writer.
var loggerEditInputs logxi.Logger

// InputImageRef pairs the caller's original reference string (usr_/web_/gen_
// or the data URI — provenance for the note + upload meta) with the ComfyUI
// upload coordinates the bytes landed at.
type InputImageRef struct {
	Ref   string     `json:"ref"`
	Comfy ComfyImage `json:"comfy"`
}

// extForDataMIME maps a data-URI mime to a filename extension ComfyUI's
// picker recognizes. The mime segment arrives with its parameters intact
// ("data:image/png;base64"), so strip everything from the ';' before
// matching or every real data URI (they all carry ;base64) would fall to
// the .jpg default.
func extForDataMIME(mime string) string {
	mime = strings.TrimPrefix(mime, "data:")
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = mime[:i]
	}
	switch mime {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	default:
		return ".jpg"
	}
}

// resolveEditInputs turns edit tool input_images entries into uploaded
// ComfyUI coordinates: data URIs (dave-resolved usr_/web_ refs) decode and
// upload; gen_ refs fetch from ComfyUI/imgsite and upload; anything else is
// an error teaching the model the ref loop. Entries are resolved in order.
func resolveEditInputs(ctx context.Context, db *sqlx.DB, cfg Config, jobID string, entries []string) ([]InputImageRef, error) {
	if len(entries) == 0 {
		// Defense-in-depth for the crash window processJob guards: zero
		// entries would otherwise "resolve" to zero refs with no error,
		// and prepareComfyEditWorkflow accepts zero images (its only
		// count guard is over-supply) — the edit would silently generate
		// from the workflow file's default image instead of the user's.
		return nil, fmt.Errorf("no input images provided")
	}
	var out []InputImageRef
	for i, entry := range entries {
		var (
			data []byte
			ext  = ".jpg"
			ref  = entry
		)
		if strings.HasPrefix(entry, "data:") {
			comma := strings.Index(entry, ",")
			if comma < 0 {
				return nil, fmt.Errorf("input_images[%d]: malformed data URI", i)
			}
			b64 := entry[comma+1:]
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("input_images[%d]: decoding data URI: %v", i, err)
			}
			data = raw
			ext = extForDataMIME(entry[:comma])
		} else if _, _, ok := parseGenRef(entry); ok {
			raw, err := fetchGenRefBytes(ctx, db, cfg.Comfy.BaseURL, entry)
			if err != nil {
				return nil, fmt.Errorf("input_images[%d]: %v", i, err)
			}
			data = raw
			ext = ".png"
		} else {
			return nil, fmt.Errorf("input_images[%d]: %q is not an image ref — view the image first (view_image) and pass its ref (usr_/web_) or a gen_ ref from a generation result", i, entry)
		}
		comfy, err := uploadComfyImage(ctx, cfg.Comfy.BaseURL, data, fmt.Sprintf("edit_%s_%d%s", jobID, i, ext))
		if err != nil {
			return nil, fmt.Errorf("input_images[%d]: %v", i, err)
		}
		out = append(out, InputImageRef{Ref: ref, Comfy: comfy})
	}
	return out, nil
}

// fetchGenRefBytes resolves a gen_<jobid>_<n> ref to bytes: the job_images
// row's ComfyUI /view coordinates first, the imgsite URL as fallback (WARN)
// when ComfyUI's output dir no longer has the file. First-party bytes only —
// img-mcp never downloads arbitrary URLs (that is dave's pipeline's job).
func fetchGenRefBytes(ctx context.Context, db *sqlx.DB, comfyURL string, ref string) ([]byte, error) {
	jobID, ordinal, ok := parseGenRef(ref)
	if !ok {
		return nil, fmt.Errorf("not a gen_ ref: %q", ref)
	}
	img, fallbackURL, err := dbGetJobImage(db, jobID, ordinal)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", ref, err)
	}
	data, _, err := downloadComfyImage(ctx, comfyURL, img)
	if err == nil && len(data) > 0 {
		return data, nil
	}
	loggerEditInputs.Warn("gen ref /view download failed, falling back to imgsite url", "ref", ref, "error", fmt.Sprintf("%v", err))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fallbackURL, nil)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", ref, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resolving %q (view and imgsite both failed): %w", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolving %q: imgsite fallback returned %d", ref, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}
