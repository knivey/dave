package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// errJobAlreadyTerminal is returned by the fenced updates below
// (dbCompleteJob/dbFailJob/dbCancelJob/dbUpdateJobRunning) when their
// guarded UPDATE matched no row: another write already won the race (or
// the row is gone). In-memory job state stays authoritative — callers log
// the skip and move on; terminal rows keep the FIRST terminal transition
// so a restart mid-race can't resurface the job with the wrong terminal
// status.
var errJobAlreadyTerminal = errors.New("job already reached a terminal status")

//go:embed migrations/*.sql
var embedMigrations embed.FS

type dbJob struct {
	ID             int64  `db:"id"`
	JobID          string `db:"job_id"`
	Type           string `db:"type"`
	Status         string `db:"status"`
	Workflow       string `db:"workflow"`
	Prompt         string `db:"prompt"`
	NegativePrompt string `db:"negative_prompt"`
	Enhancement    string `db:"enhancement"`
	Seed           *int64 `db:"seed"`
	// Vestige: the jobs.output_format column is no longer written (removed
	// from JobInput — results are always url; new rows take the 'url'
	// DEFAULT) but old rows still carry values, so the scan stays.
	OutputFormat string `db:"output_format"`
	LLMGenerated bool   `db:"llm_generated"`
	// Network/Channel/Nick are IRC provenance for the imgsite upload meta.
	// Insert/recovery-only columns: no terminal UPDATE statement touches
	// them, so they cannot interfere with the terminal-update fencing.
	Network string `db:"network"`
	Channel string `db:"channel"`
	Nick    string `db:"nick"`
	// Safety is the resolved safety verdict ("safe"/"unsafe"/"unknown");
	// empty = never classified (skip_networks, or a crash before the
	// verdict resolved) — deliberately distinct from "unknown", which is
	// vetted-but-unresolved and equally final. Written once via
	// dbUpdateJobSafety when the verdict resolves; recovery-read so
	// restarts never re-vet. Like the provenance columns it stays out of
	// every terminal-fenced statement.
	Safety string `db:"safety"`
	// InputImages is the JSON of resolved edit-input upload coordinates
	// (InputImageRef) stamped by dbUpdateJobInputImages after processJob
	// uploads them; empty = never resolved (non-edit jobs, or a crash
	// before the uploads). Insert/recovery-only column: no terminal
	// UPDATE statement touches it.
	InputImages string `db:"input_images"`
	// Vestige: the jobs.strength column is no longer written (the edit
	// strength knob was removed — Qwen Image 2.1 edit is denoise-1.0-only,
	// the workflow file's own KSampler value is authoritative) but old rows
	// still carry values, so the scan stays.
	Strength      *float64 `db:"strength"`
	Error         *string  `db:"error"`
	ComfyPromptID *string  `db:"comfy_prompt_id"`
	CreatedAt     string   `db:"created_at"`
	StartedAt     *string  `db:"started_at"`
	CompletedAt   *string  `db:"completed_at"`
}

type dbJobImage struct {
	ID        int64  `db:"id"`
	JobID     string `db:"job_id"`
	URL       string `db:"url"`
	Filename  string `db:"filename"`
	Subfolder string `db:"subfolder"`
	ImgType   string `db:"img_type"`
	MIMEType  string `db:"mime_type"`
	Ordinal   int    `db:"ordinal"`
}

func initDB(dbPath string) (*sqlx.DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating database directory %s: %w", dir, err)
		}
	}

	sqldb, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", dbPath, err)
	}

	sqldb.SetMaxOpenConns(1)

	if err := goose.SetDialect("sqlite3"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("setting goose dialect: %w", err)
	}

	goose.SetBaseFS(embedMigrations)

	if err := goose.Up(sqldb, "migrations"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	db := sqlx.NewDb(sqldb, "sqlite")

	loggerDB.Info("Database initialized", "path", dbPath)
	return db, nil
}

func closeDB(db *sqlx.DB) {
	if db != nil {
		db.Close()
	}
}

func dbInsertJob(db *sqlx.DB, job *Job) error {
	// output_format and strength are deliberately absent from the column
	// list: output_format was removed from JobInput first (owner ruling —
	// never used, results are always url), and strength followed it (Qwen
	// Image 2.1 edit is denoise-1.0-only; the workflow file's own KSampler
	// denoise is authoritative). New rows take both columns' DEFAULTs
	// ('url' / NULL); the columns themselves stay for old rows, and dbJob
	// still scans them as vestiges.
	//
	// input_images starts empty (the column's DEFAULT is '' anyway, but the
	// insert carries it explicitly so a job submitted with pre-resolved refs
	// — a future path — lands complete).
	inputImages := ""
	if len(job.Input.InputImages) > 0 {
		b, err := json.Marshal(job.Input.InputImages)
		if err != nil {
			return fmt.Errorf("marshaling input_images: %w", err)
		}
		inputImages = string(b)
	}
	_, err := db.Exec(
		`INSERT INTO jobs (job_id, type, status, workflow, prompt, negative_prompt, enhancement, seed, llm_generated, network, channel, nick, input_images)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, string(job.Type), string(job.Status), job.Workflow,
		job.Input.Prompt, job.Input.NegativePrompt, job.Input.Enhancement,
		job.Input.Seed, job.Input.LLMGenerated,
		job.Input.Network, job.Input.Channel, job.Input.Nick,
		inputImages,
	)
	return err
}

// dbUpdateJobRunning is fenced like the terminal updates: only a queued row
// may become running. A cancelled row must stay cancelled — an unconditional
// update here would resurrect the row into the terminal guard set, letting a
// later dbCompleteJob legitimately overwrite the user's cancel.
func dbUpdateJobRunning(db *sqlx.DB, jobID string) error {
	res, err := db.Exec(
		`UPDATE jobs SET status = ?, started_at = CURRENT_TIMESTAMP
		 WHERE job_id = ? AND status = 'queued'`,
		string(StatusRunning), jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errJobAlreadyTerminal
	}
	return nil
}

func dbUpdateJobComfyPromptID(db *sqlx.DB, jobID, comfyPromptID string) error {
	_, err := db.Exec(
		`UPDATE jobs SET comfy_prompt_id = ? WHERE job_id = ?`,
		comfyPromptID, jobID,
	)
	return err
}

// dbUpdateJobSafety persists a resolved safety verdict. Deliberately
// unfenced like dbUpdateJobComfyPromptID: the safety column never touches
// status, so it cannot interact with the terminal-update guard set (once
// the row is terminal the verdict is dead data), and the verdict resolves
// at exactly one point per job — first-write-wins has no second writer to
// arbitrate.
func dbUpdateJobSafety(db *sqlx.DB, jobID, safety string) error {
	_, err := db.Exec(
		`UPDATE jobs SET safety = ? WHERE job_id = ?`,
		safety, jobID,
	)
	return err
}

// dbUpdateJobInputImages persists resolved edit-input upload coordinates
// after processJob uploads them — the recovery re-submit seam. Deliberately
// unfenced like jobs.safety: it touches only these columns and can never
// contend with a terminal status write.
func dbUpdateJobInputImages(db *sqlx.DB, jobID string, imgs []InputImageRef) error {
	b, err := json.Marshal(imgs)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE jobs SET input_images = ? WHERE job_id = ?`, string(b), jobID)
	return err
}

// dbUpdateJobStatus is deliberately UNfenced: it is only used by
// recoverJobs' running→queued requeue, which runs synchronously inside
// NewJobQueue before MCP serving starts, so no Cancel/terminal write can
// race it. If recovery ever moves off the startup path, fence it like its
// siblings first.
func dbUpdateJobStatus(db *sqlx.DB, jobID string, status JobStatus) error {
	_, err := db.Exec(
		`UPDATE jobs SET status = ? WHERE job_id = ?`,
		string(status), jobID,
	)
	return err
}

func dbCompleteJob(db *sqlx.DB, jobID string, result *JobResult, comfyImages []ComfyImage) error {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if result == nil {
		return fmt.Errorf("result is nil for job %s", jobID)
	}

	// Terminal-update fence: only flip a row that is still queued/running.
	// Concurrent terminal writes (e.g. Cancel racing processJob) run outside
	// q.mu and could otherwise land in either order, leaving the DB
	// disagreeing with the in-memory winner. Rows-affected tells the loser
	// to skip — including the image inserts below, which the deferred
	// rollback then discards.
	res, err := tx.Exec(
		`UPDATE jobs SET status = ?, completed_at = CURRENT_TIMESTAMP
		 WHERE job_id = ? AND status IN ('queued', 'running')`,
		string(StatusCompleted), jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errJobAlreadyTerminal
	}

	for i, img := range result.Images {
		var comfyFilename, comfySubfolder, comfyImgType string
		if i < len(comfyImages) {
			comfyFilename = comfyImages[i].Filename
			comfySubfolder = comfyImages[i].Subfolder
			comfyImgType = comfyImages[i].Type
		}
		_, err = tx.Exec(
			`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			jobID, img.URL, comfyFilename, comfySubfolder, comfyImgType, img.MIMEType, i,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func dbFailJob(db *sqlx.DB, jobID, jobError string) error {
	// Terminal-update fence: see dbCompleteJob. The error string rides the
	// same guarded statement so a cancel that wins the race can never end
	// up with "generation failed: context canceled" stamped on its row.
	res, err := db.Exec(
		`UPDATE jobs SET status = ?, error = ?, completed_at = CURRENT_TIMESTAMP
		 WHERE job_id = ? AND status IN ('queued', 'running')`,
		string(StatusFailed), jobError, jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errJobAlreadyTerminal
	}
	return nil
}

func dbCancelJob(db *sqlx.DB, jobID string) error {
	// Terminal-update fence: see dbCompleteJob.
	res, err := db.Exec(
		`UPDATE jobs SET status = ?, completed_at = CURRENT_TIMESTAMP
		 WHERE job_id = ? AND status IN ('queued', 'running')`,
		string(StatusCancelled), jobID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errJobAlreadyTerminal
	}
	return nil
}

func dbLoadRecoverableJobs(db *sqlx.DB) ([]dbJob, error) {
	var jobs []dbJob
	err := db.Select(&jobs,
		`SELECT * FROM jobs WHERE status IN ('queued', 'running') ORDER BY created_at ASC`)
	return jobs, err
}

func dbLoadRecentCompletedJobs(db *sqlx.DB, ttl time.Duration) ([]dbJob, error) {
	var jobs []dbJob
	secs := int(ttl.Seconds())
	err := db.Select(&jobs,
		`SELECT * FROM jobs WHERE status = 'completed' AND completed_at > datetime('now', ? || ' seconds')`,
		fmt.Sprintf("-%d", secs),
	)
	return jobs, err
}

func dbLoadJobImages(db *sqlx.DB, jobID string) ([]dbJobImage, error) {
	var images []dbJobImage
	err := db.Select(&images,
		`SELECT * FROM job_images WHERE job_id = ? ORDER BY ordinal ASC`, jobID)
	return images, err
}

// dbGetJobImage returns the nth (0-based) job_images row for a job — the
// ComfyUI /view coordinates plus the imgsite fallback URL.
func dbGetJobImage(db *sqlx.DB, jobID string, ordinal int) (ComfyImage, string, error) {
	var row struct {
		Filename  string `db:"filename"`
		Subfolder string `db:"subfolder"`
		ImgType   string `db:"img_type"`
		URL       string `db:"url"`
	}
	err := db.Get(&row, `SELECT filename, subfolder, img_type, url FROM job_images WHERE job_id = ? AND ordinal = ?`, jobID, ordinal)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ComfyImage{}, "", fmt.Errorf("no image %d for job %q", ordinal, jobID)
		}
		return ComfyImage{}, "", err
	}
	return ComfyImage{Filename: row.Filename, Subfolder: row.Subfolder, Type: row.ImgType}, row.URL, nil
}

func dbGetJob(db *sqlx.DB, jobID string) (*dbJob, error) {
	var job dbJob
	err := db.Get(&job, `SELECT * FROM jobs WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func dbCleanupExpiredJobs(db *sqlx.DB, ttl time.Duration) (int64, error) {
	secs := int(ttl.Seconds())
	result, err := db.Exec(
		`DELETE FROM jobs WHERE status IN ('completed', 'failed', 'cancelled') AND completed_at < datetime('now', ? || ' seconds')`,
		fmt.Sprintf("-%d", secs),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func jobFromDBJob(dbj *dbJob) *Job {
	job := &Job{
		ID:            dbj.JobID,
		Type:          JobType(dbj.Type),
		Status:        JobStatus(dbj.Status),
		Workflow:      dbj.Workflow,
		ComfyPromptID: ptrStr(dbj.ComfyPromptID),
		Safety:        dbj.Safety,
		Input: JobInput{
			Prompt:         dbj.Prompt,
			NegativePrompt: dbj.NegativePrompt,
			Enhancement:    dbj.Enhancement,
			Seed:           dbj.Seed,
			LLMGenerated:   dbj.LLMGenerated,
			Network:        dbj.Network,
			Channel:        dbj.Channel,
			Nick:           dbj.Nick,
		},
		Error: ptrStr(dbj.Error),
	}

	// Resolved edit inputs: empty = never resolved (non-edit jobs, or a
	// crash before the uploads) — leave nil so recovery treats the job as
	// having nothing to re-submit.
	if dbj.InputImages != "" {
		var imgs []InputImageRef
		if err := json.Unmarshal([]byte(dbj.InputImages), &imgs); err != nil {
			loggerDB.Warn("unmarshaling persisted input_images", "job", dbj.JobID, "error", err.Error())
		} else {
			job.Input.InputImages = imgs
		}
	}

	if dbj.CreatedAt != "" {
		if t, err := time.Parse("2006-01-02 15:04:05", dbj.CreatedAt); err == nil {
			t = t.UTC()
			job.CreatedAt = t
		}
	}

	if dbj.StartedAt != nil && *dbj.StartedAt != "" {
		if t, err := time.Parse("2006-01-02 15:04:05", *dbj.StartedAt); err == nil {
			t = t.UTC()
			job.StartedAt = &t
		}
	}

	if dbj.CompletedAt != nil && *dbj.CompletedAt != "" {
		if t, err := time.Parse("2006-01-02 15:04:05", *dbj.CompletedAt); err == nil {
			t = t.UTC()
			job.CompletedAt = &t
		}
	}

	return job
}

func buildJobResultFromDB(db *sqlx.DB, jobID string) (*JobResult, []ComfyImage, error) {
	images, err := dbLoadJobImages(db, jobID)
	if err != nil {
		return nil, nil, err
	}

	if len(images) == 0 {
		return &JobResult{}, nil, nil
	}

	result := &JobResult{}
	var comfyImages []ComfyImage

	for _, img := range images {
		result.Images = append(result.Images, ImageData{
			URL:      img.URL,
			MIMEType: img.MIMEType,
			// Same gen_ handles the live path stamped: the DB-rebuilt
			// result (restart recovery / TTL path) stays re-editable.
			Ref: fmt.Sprintf("gen_%s_%d", jobID, img.Ordinal),
		})
		comfyImages = append(comfyImages, ComfyImage{
			Filename:  img.Filename,
			Subfolder: img.Subfolder,
			Type:      img.ImgType,
		})
	}

	return result, comfyImages, nil
}
