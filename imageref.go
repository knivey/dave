package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// Edit variant columns: empty EditFilePath = no edit variant (legacy
	// rows minted before the knob, or a degraded registration) — consumers
	// fall back to the view variant.
	EditFilePath string `gorm:"type:text"`
	EditMIME     string `gorm:"size:64"`
	EditWidth    int
	EditHeight   int
	CreatedAt    time.Time
}

// imageRefVariant is one fidelity tier of a conversation image: the view
// variant (what the LLM saw in context, capped by maximagesize) and the edit
// variant (what edit tools receive, independently capped by editimagesize —
// typically larger, verbatim original bytes when the original fits the cap).
type imageRefVariant struct {
	MIME   string
	Width  int
	Height int
	Data   []byte
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

// registerImageRef stores each variant at <images_dir>/<ref>.<ext> (view)
// and <images_dir>/<ref>.edit.<ext> (edit) and inserts the row.
// BEST-EFFORT CONTRACT: callers label the ref in the conversation REGARDLESS
// of the returned error — seeing the image is the point, and an unresolvable
// ref degrades to a readable error in the future edit tool, never a broken
// turn. When minting succeeded, the ref is returned non-empty even alongside
// an error so callers can keep the label shape stable. An edit variant whose
// file write fails degrades to a view-only ref (WARN) — never a failed turn.
func registerImageRef(sessionID int64, origin, sourceURL string, view, edit imageRefVariant) (string, error) {
	prefix := imageRefPrefix(origin)
	ref, err := mintImageRef(prefix)
	if err != nil {
		return "", err
	}
	if len(view.Data) == 0 {
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

	writeFile := func(r string, mime string, data []byte) (string, error) {
		p := filepath.Join(dir, r+"."+imageExtForMIME(mime))
		return p, os.WriteFile(p, data, 0o644)
	}

	row := ImageRef{SessionID: sessionID, Origin: origin, SourceURL: sourceURL,
		MIME: view.MIME, Width: view.Width, Height: view.Height, CreatedAt: time.Now()}

	// VIEW file first: a view-write failure then leaves nothing on disk.
	// (Writing the edit file first would orphan it on this path — the sweep
	// would eventually clean it, but there is no reason to leave the window.)
	path, werr := writeFile(ref, view.MIME, view.Data)
	if werr != nil {
		loggerImgRef.Warn("image ref store: write failed (ref stays labeled, unresolvable)", "ref", ref, "error", werr.Error())
		return ref, fmt.Errorf("image ref store: %w", werr)
	}
	row.FilePath = path

	// Edit-variant write is best-effort INSIDE registration too: a failure
	// here must not kill the ref (viewing still works, edits fall back to
	// the view variant).
	var editPath string
	if len(edit.Data) > 0 {
		p, ewerr := writeFile(ref+".edit", edit.MIME, edit.Data)
		if ewerr != nil {
			loggerImgRef.Warn("image ref store: edit variant write failed (edit falls back to view variant)", "ref", ref, "error", ewerr.Error())
		} else {
			editPath = p
			row.EditFilePath, row.EditMIME, row.EditWidth, row.EditHeight = p, edit.MIME, edit.Width, edit.Height
		}
	}

	if theDB == nil {
		loggerImgRef.Warn("image ref store: no database (ref stays labeled, unresolvable)", "ref", ref)
		return ref, errors.New("image ref store: database not initialized")
	}

	row.Ref = ref
	if err := theDB.Create(&row).Error; err != nil {
		// One collision retry (see imageRefRandLen note), then give up —
		// the orphaned files are removed so the sweep never sees them.
		os.Remove(path)
		if editPath != "" {
			os.Remove(editPath)
		}
		if ref2, e2 := mintImageRef(prefix); e2 == nil {
			var path2, editPath2 string
			if p2, w2 := writeFile(ref2, view.MIME, view.Data); w2 == nil {
				path2 = p2
				if len(edit.Data) > 0 {
					if ep2, ew2 := writeFile(ref2+".edit", edit.MIME, edit.Data); ew2 == nil {
						editPath2 = ep2
					} else {
						loggerImgRef.Warn("image ref store: edit variant write failed on retry (edit falls back to view variant)", "ref", ref2, "error", ew2.Error())
					}
				}
				row.Ref, row.FilePath = ref2, path2
				row.EditFilePath, row.EditMIME, row.EditWidth, row.EditHeight = "", "", 0, 0
				if editPath2 != "" {
					row.EditFilePath, row.EditMIME, row.EditWidth, row.EditHeight = editPath2, edit.MIME, edit.Width, edit.Height
				}
				e3 := theDB.Create(&row).Error
				if e3 == nil {
					loggerImgRef.Debug("image ref registered (retry)", "ref", ref2, "origin", origin)
					return ref2, nil
				}
				os.Remove(path2)
				if editPath2 != "" {
					os.Remove(editPath2)
				}
				ref = ref2
				err = e3
			}
		}
		loggerImgRef.Warn("image ref store: insert failed (ref stays labeled, unresolvable)", "ref", ref, "error", err.Error())
		return ref, fmt.Errorf("image ref store: %w", err)
	}
	loggerImgRef.Debug("image ref registered", "ref", ref, "origin", origin, "session", sessionID, "edit_variant", row.EditFilePath != "")
	return ref, nil
}

// resolveImageRef looks a ref up. Production consumers: the input_images
// interception (resolveImageRefArgs) resolves usr_/web_ refs through this
// on every MCP tool-call path (LLM turns and direct tool commands).
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

// resolveImageRefArgs rewrites input_images entries in MCP tool args from
// usr_/web_ refs to data URIs the MCP server can consume (base64 of the
// stored ref file). The FIELD NAME is a dave-MCP convention — the same idea
// as the _dave_inject_ prefix: any tool declaring an input_images string
// array gets its ref entries expanded here. gen_ refs, data URIs, and
// anything else pass through untouched (img-mcp owns gen_ resolution).
// A non-nil error aborts the call: the message names the dead ref.
var resolveImageRefArgsFn = resolveImageRefArgs

func resolveImageRefArgs(toolArgs map[string]any) error {
	raw, ok := toolArgs["input_images"]
	if !ok {
		return nil
	}
	rewrite := func(s string) (string, error) {
		if !strings.HasPrefix(s, "usr_") && !strings.HasPrefix(s, "web_") {
			return s, nil
		}
		row, err := resolveImageRef(s)
		if err != nil {
			return "", fmt.Errorf("image ref %q could not be resolved: %v", s, err)
		}
		// Prefer the edit variant (independently capped by editimagesize —
		// usually larger than the LLM ever saw); fall back to the view
		// variant for legacy rows or an unreadable edit file. ReadFile
		// directly (no Stat first): a file deleted between the check and
		// the read must fall back, not fail the edit.
		var data []byte
		var mime string
		if row.EditFilePath != "" {
			ed, rerr := os.ReadFile(row.EditFilePath)
			if rerr != nil {
				loggerImgRef.Warn("image ref edit file unreadable, falling back to view variant", "ref", s, "error", rerr.Error())
			} else {
				data, mime = ed, row.EditMIME
			}
		}
		if data == nil {
			var verr error
			data, verr = os.ReadFile(row.FilePath)
			if verr != nil {
				return "", fmt.Errorf("image ref %q could not be read: %v", s, verr)
			}
			mime = row.MIME
		}
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	}
	switch entries := raw.(type) {
	case []string:
		for i, s := range entries {
			nv, err := rewrite(s)
			if err != nil {
				return err
			}
			entries[i] = nv
		}
	case []any:
		for i, v := range entries {
			s, ok := v.(string)
			if !ok {
				continue
			}
			nv, err := rewrite(s)
			if err != nil {
				return err
			}
			entries[i] = nv
		}
	}
	return nil
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
		for _, p := range []string{row.FilePath, row.EditFilePath} {
			if p == "" {
				continue
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
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
			if r.EditFilePath != "" {
				known[filepath.Base(r.EditFilePath)] = true
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
