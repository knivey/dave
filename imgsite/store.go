package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Store is the content-addressed file store. Originals live at
// <originals>/<sha[:2]>/<sha> and thumbnails (from the thumbnails milestone)
// at <thumbs>/<sha[:2]>/<sha>-<size>.jpg. Hash-keyed layout makes dedupe a
// stat away and spreads files across 256 fan-out directories.
type Store struct {
	originalsPath string
	thumbsPath    string
}

func NewStore(originalsPath, thumbsPath string) *Store {
	return &Store{
		originalsPath: originalsPath,
		thumbsPath:    thumbsPath,
	}
}

// OriginalPath returns the on-disk path of the stored original for a hash.
func (s *Store) OriginalPath(hash string) string {
	return filepath.Join(s.originalsPath, hash[:2], hash)
}

// ThumbPath returns the on-disk path of the thumbnail for a hash at a given
// pixel width. Widths are resolved from config at generation time; readers
// that must tolerate width hot-reload use FindThumbPath instead.
func (s *Store) ThumbPath(hash string, width int) string {
	return filepath.Join(s.thumbsPath, hash[:2], fmt.Sprintf("%s-%d.jpg", hash, width))
}

// FindThumbPath resolves the derivative file for a hash: the exact
// current-config width first, then a glob of <hash>-*.jpg in the
// fan-out directory.
//
// DESIGN NOTE (width hot-reload): thumbnails.small_width/display_width
// are reloadable, but derivative files are keyed by the width resolved
// at GENERATION time and ready rows are terminal — nothing ever
// regenerates an existing entry at a new width, and the startup
// re-scan only re-enqueues pending rows. Without this fallback, every
// pre-existing ready row would 404 permanently after a width reload.
// Serving the stale-width file is safe: content for a hash never
// changes once written, so the immutable year-long Cache-Control on
// the thumb route stays valid. A hash can legitimately have several
// width files side by side (its small and display derivatives), so
// among the glob matches the width closest to the preferred one wins,
// ties to the smaller width — a deterministic pick, keeping a given
// URL stable across requests.
func (s *Store) FindThumbPath(hash string, preferredWidth int) (string, bool) {
	if len(hash) < 2 {
		return "", false
	}
	if exact := s.ThumbPath(hash, preferredWidth); fileExists(exact) {
		return exact, true
	}
	matches, err := filepath.Glob(filepath.Join(s.thumbsPath, hash[:2], hash+"-*.jpg"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	best, bestDist, bestWidth := "", -1, 0
	for _, m := range matches {
		w, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), hash+"-"), ".jpg"))
		if err != nil {
			continue // foreign filename in the fan-out dir; ignore
		}
		dist := w - preferredWidth
		if dist < 0 {
			dist = -dist
		}
		// Ties prefer the smaller width: glob order is lexical (so
		// "-1280.jpg" sorts before "-480.jpg") and must not decide
		// which derivative a URL serves.
		if bestDist == -1 || dist < bestDist || (dist == bestDist && w < bestWidth) {
			best, bestDist, bestWidth = m, dist, w
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// WriteOriginal stores the bytes for hash, skipping the write when the file
// already exists (dedupe: N rows may share one stored file). The write goes
// through a temp file + rename so a crash mid-write can never leave a
// truncated file under the content address. Files are 0644 (no execute
// bits), directories 0755. Returns whether the file was newly written.
func (s *Store) WriteOriginal(hash string, data []byte) (bool, error) {
	if len(hash) < 2 {
		return false, fmt.Errorf("invalid hash %q: need at least 2 chars for fan-out", hash)
	}
	dir := filepath.Join(s.originalsPath, hash[:2])
	target := filepath.Join(dir, hash)

	if _, err := os.Stat(target); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking %s: %w", target, err)
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return false, fmt.Errorf("creating store directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return false, fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, fmt.Errorf("writing %s: %w", tmpName, err)
	}
	// Crash durability: sync before the rename so the content is on disk
	// before the name exists. The DB insert that follows can then never
	// reference a truncated file — the worst crash window (between rename
	// and INSERT) leaves only a harmless orphaned file.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return false, fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return false, fmt.Errorf("renaming into %s: %w", target, err)
	}

	loggerStore.Info("stored original", "sha256", hash, "bytes", len(data))
	return true, nil
}

// WriteThumb stores a derivative at ThumbPath(hash, width) with the same
// temp+sync+rename durability as WriteOriginal. Unlike originals,
// overwriting is fine — regeneration of a derivative is idempotent from
// the reader's perspective.
func (s *Store) WriteThumb(hash string, width int, data []byte) error {
	if len(hash) < 2 {
		return fmt.Errorf("invalid hash %q: need at least 2 chars for fan-out", hash)
	}
	target := s.ThumbPath(hash, width)
	dir := filepath.Dir(target)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating thumbs directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("renaming into %s: %w", target, err)
	}
	return nil
}
