package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/jmoiron/sqlx"
	logxi "github.com/mgutz/logxi/v1"
)

// loggerEditInputs is assigned in initLogger (logging.go) alongside the other
// package loggers so every channel shares the same writer.
var loggerEditInputs logxi.Logger

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
