package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"
)

var loggerImgRef = newLogger("imageref")

// ImageRef maps a conversation image's ref ID to its bytes on disk. Refs are
// minted wherever an image enters the model's context (user pastes: usr_,
// view_image: web_) and are the seam a future img-mcp edit workflow resolves
// through: the model passes the ref it sees in the image's label text;
// dave expands usr_/web_ refs to bytes; img-mcp will resolve its own gen_
// refs. The store must outlive live history because compaction strips image
// parts to [image] placeholders — the ref label text survives, the data URI
// does not.
type ImageRef struct {
	ID        uint64 `gorm:"primaryKey"`
	Ref       string `gorm:"uniqueIndex;size:64"`
	SessionID int64  `gorm:"index"`
	Origin    string `gorm:"size:16"` // "user" (paste) | "view" (view_image)
	SourceURL string `gorm:"type:text"`
	MIME      string `gorm:"size:64"`
	FilePath  string `gorm:"type:text"`
	Width     int
	Height    int
	CreatedAt time.Time
}

// imageRefRandLen: 6 random bytes -> 12 hex chars. 48 bits of randomness with
// a unique index; registerImageRef still retries once on collision so the
// contract is mechanical rather than probabilistic.
const imageRefRandLen = 6

func mintImageRef(prefix string) (string, error) {
	b := make([]byte, imageRefRandLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("minting image ref: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}

func imageRefPrefix(origin string) string {
	switch origin {
	case "user":
		return "usr"
	case "view":
		return "web"
	default:
		return "img"
	}
}

func imageExtForMIME(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp":
		return "bmp"
	default:
		return "bin"
	}
}

// registerImageRef stores data at <images_dir>/<ref>.<ext> and inserts the
// row. BEST-EFFORT CONTRACT: callers label the ref in the conversation
// REGARDLESS of the returned error — seeing the image is the point, and an
// unresolvable ref degrades to a readable error in the future edit tool,
// never a broken turn. When minting succeeded, the ref is returned non-empty
// even alongside an error so callers can keep the label shape stable.
func registerImageRef(sessionID int64, origin, sourceURL, mime string, width, height int, data []byte) (string, error) {
	prefix := imageRefPrefix(origin)
	ref, err := mintImageRef(prefix)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return ref, errors.New("image ref store: no image data")
	}

	var dir string
	readConfig(func() { dir = config.ImagesDir })
	if dir == "" {
		dir = "data/images"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		loggerImgRef.Warn("image ref store: mkdir failed (ref stays labeled, unresolvable)", "ref", ref, "error", err.Error())
		return ref, fmt.Errorf("image ref store: %w", err)
	}

	writeFile := func(r string) (string, error) {
		p := filepath.Join(dir, r+"."+imageExtForMIME(mime))
		return p, os.WriteFile(p, data, 0o644)
	}

	row := ImageRef{SessionID: sessionID, Origin: origin, SourceURL: sourceURL, MIME: mime, Width: width, Height: height, CreatedAt: time.Now()}

	if theDB == nil {
		path, werr := writeFile(ref)
		if werr != nil {
			loggerImgRef.Warn("image ref store: write failed (ref stays labeled, unresolvable)", "ref", ref, "error", werr.Error())
			return ref, fmt.Errorf("image ref store: %w", werr)
		}
		row.FilePath = path
		loggerImgRef.Warn("image ref store: no database (ref stays labeled, unresolvable)", "ref", ref)
		return ref, errors.New("image ref store: database not initialized")
	}

	path, werr := writeFile(ref)
	if werr != nil {
		loggerImgRef.Warn("image ref store: write failed (ref stays labeled, unresolvable)", "ref", ref, "error", werr.Error())
		return ref, fmt.Errorf("image ref store: %w", werr)
	}
	row.Ref, row.FilePath = ref, path
	if err := theDB.Create(&row).Error; err != nil {
		// One collision retry (see imageRefRandLen note), then give up —
		// the orphaned file is removed so the sweep never sees it.
		os.Remove(path)
		if ref2, e2 := mintImageRef(prefix); e2 == nil {
			if path2, w2 := writeFile(ref2); w2 == nil {
				row.Ref, row.FilePath = ref2, path2
				e3 := theDB.Create(&row).Error
				if e3 == nil {
					loggerImgRef.Debug("image ref registered (retry)", "ref", ref2, "origin", origin)
					return ref2, nil
				}
				os.Remove(path2)
				ref = ref2
				err = e3
			}
		}
		loggerImgRef.Warn("image ref store: insert failed (ref stays labeled, unresolvable)", "ref", ref, "error", err.Error())
		return ref, fmt.Errorf("image ref store: %w", err)
	}
	loggerImgRef.Debug("image ref registered", "ref", ref, "origin", origin, "session", sessionID)
	return ref, nil
}

// resolveImageRef looks a ref up. Deliberately unused by production today —
// it is the seam the img-mcp edit workflow's tool-call interception resolves
// usr_/web_ refs through; these tests keep it honest so it cannot rot.
func resolveImageRef(ref string) (*ImageRef, error) {
	if theDB == nil {
		return nil, errors.New("image ref store: database not initialized")
	}
	var row ImageRef
	if err := theDB.Where("ref = ?", ref).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("unknown image ref %q", ref)
		}
		return nil, err
	}
	return &row, nil
}

// cleanupImageRefsByAge removes ref rows (and their files) older than
// maxAgeDays — refs for long-gone sessions are dead weight. Rides the same
// boot/periodic pass and age knob as session cleanup (CleanupContexts).
func cleanupImageRefsByAge(maxAgeDays int) (int64, error) {
	if theDB == nil {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
	var rows []ImageRef
	if err := theDB.Where("created_at < ?", cutoff).Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	for _, row := range rows {
		if row.FilePath != "" {
			if err := os.Remove(row.FilePath); err != nil && !os.IsNotExist(err) {
				loggerImgRef.Warn("image ref cleanup: unlink failed", "ref", row.Ref, "error", err.Error())
			}
		}
	}
	res := theDB.Where("created_at < ?", cutoff).Delete(&ImageRef{})
	return res.RowsAffected, res.Error
}

// sweepOrphanImageFiles deletes files in dir with no matching row and a
// modtime older than grace — covers crashes between file write and row
// insert.
func sweepOrphanImageFiles(dir string, grace time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	known := map[string]bool{}
	if theDB != nil {
		var rows []ImageRef
		if err := theDB.Find(&rows).Error; err != nil {
			return 0, err
		}
		for _, r := range rows {
			if r.FilePath != "" {
				known[filepath.Base(r.FilePath)] = true
			}
		}
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || known[e.Name()] {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < grace {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr == nil {
			removed++
		}
	}
	return removed, nil
}
