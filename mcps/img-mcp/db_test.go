package main

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertTerminalTestJob inserts a job row in the given pre-terminal status.
func insertTerminalTestJob(t *testing.T, db *sqlx.DB, jobID, status string) {
	t.Helper()
	job := &Job{
		ID:       jobID,
		Type:     JobTypeGenerate,
		Status:   JobStatus(status),
		Workflow: "test",
		Input:    JobInput{Prompt: "a cat"},
	}
	require.NoError(t, dbInsertJob(db, job), "dbInsertJob")
}

func completeCall(db *sqlx.DB, jobID string) error {
	return dbCompleteJob(db, jobID, &JobResult{Images: []ImageData{{MIMEType: "image/png", URL: "https://x/img.png"}}}, nil)
}

func jobImageCount(t *testing.T, db *sqlx.DB, jobID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.Get(&n, "SELECT COUNT(*) FROM job_images WHERE job_id = ?", jobID), "counting job_images")
	return n
}

// TestTerminalDBUpdatesAreFenced covers the DB-side fence for terminal job
// transitions. dbCancelJob/dbFailJob/dbCompleteJob used to do unconditional
// UPDATEs outside q.mu, so a cancel racing a failure could land in the DB in
// either order — a restart mid-race would resurface the job with the wrong
// terminal status (and, for cancels, an error string like "generation
// failed: context canceled" on a user-cancelled job). The fence makes each
// terminal write conditional on the row still being queued/running; the
// first terminal write wins and the loser must report that it was skipped
// instead of overwriting.
func TestTerminalDBUpdatesAreFenced(t *testing.T) {
	tests := []struct {
		name         string
		startStatus  string
		winner       func(db *sqlx.DB, jobID string) error
		loser        func(db *sqlx.DB, jobID string) error
		winnerStatus string
		expectImages int
	}{
		{
			name:        "cancel beats fail from running",
			startStatus: "running",
			winner:      func(db *sqlx.DB, jobID string) error { return dbCancelJob(db, jobID) },
			loser: func(db *sqlx.DB, jobID string) error {
				return dbFailJob(db, jobID, "generation failed: context canceled")
			},
			winnerStatus: "cancelled",
			expectImages: 0,
		},
		{
			name:         "fail beats cancel from running",
			startStatus:  "running",
			winner:       func(db *sqlx.DB, jobID string) error { return dbFailJob(db, jobID, "boom") },
			loser:        func(db *sqlx.DB, jobID string) error { return dbCancelJob(db, jobID) },
			winnerStatus: "failed",
			expectImages: 0,
		},
		{
			name:         "cancel beats fail from queued",
			startStatus:  "queued",
			winner:       func(db *sqlx.DB, jobID string) error { return dbCancelJob(db, jobID) },
			loser:        func(db *sqlx.DB, jobID string) error { return dbFailJob(db, jobID, "queue full during recovery") },
			winnerStatus: "cancelled",
			expectImages: 0,
		},
		{
			name:         "complete beats cancel",
			startStatus:  "running",
			winner:       completeCall,
			loser:        func(db *sqlx.DB, jobID string) error { return dbCancelJob(db, jobID) },
			winnerStatus: "completed",
			expectImages: 1,
		},
		{
			name:         "complete beats fail",
			startStatus:  "running",
			winner:       completeCall,
			loser:        func(db *sqlx.DB, jobID string) error { return dbFailJob(db, jobID, "upload failed") },
			winnerStatus: "completed",
			expectImages: 1,
		},
		{
			name:         "cancel beats complete: loser writes no images",
			startStatus:  "running",
			winner:       func(db *sqlx.DB, jobID string) error { return dbCancelJob(db, jobID) },
			loser:        completeCall,
			winnerStatus: "cancelled",
			expectImages: 0,
		},
		{
			name:         "fail beats complete: loser writes no images",
			startStatus:  "running",
			winner:       func(db *sqlx.DB, jobID string) error { return dbFailJob(db, jobID, "boom") },
			loser:        completeCall,
			winnerStatus: "failed",
			expectImages: 0,
		},
		{
			name:         "complete beats second complete",
			startStatus:  "running",
			winner:       completeCall,
			loser:        completeCall,
			winnerStatus: "completed",
			expectImages: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := setupTestDB(t)
			jobID := "fencejob"
			insertTerminalTestJob(t, db, jobID, tt.startStatus)

			require.NoError(t, tt.winner(db, jobID), "first terminal write must succeed")
			assert.Equal(t, tt.winnerStatus, dbJobStatus(t, db, jobID), "DB status after winner")

			loserErr := tt.loser(db, jobID)
			require.ErrorIs(t, loserErr, errJobAlreadyTerminal,
				"loser of the terminal race must report errJobAlreadyTerminal")
			assert.Equal(t, tt.winnerStatus, dbJobStatus(t, db, jobID),
				"first terminal write must win; loser must not overwrite it")
			assert.Equal(t, tt.expectImages, jobImageCount(t, db, jobID),
				"job_images must reflect exactly the winner's writes (a losing dbCompleteJob must not insert)")
		})
	}
}

// TestDbUpdateJobRunningFenced covers the queued->running fence. Without
// it, a cancel that raced the worker's start (Cancel's DB write landing
// before dbUpdateJobRunning) was resurrected to 'running' in the DB —
// re-opening the terminal guard set so a later dbCompleteJob would then
// legitimately overwrite the user's cancel.
func TestDbUpdateJobRunningFenced(t *testing.T) {
	t.Run("queued row becomes running", func(t *testing.T) {
		db := setupTestDB(t)
		insertTerminalTestJob(t, db, "runjob", "queued")

		require.NoError(t, dbUpdateJobRunning(db, "runjob"))
		assert.Equal(t, "running", dbJobStatus(t, db, "runjob"))
	})

	t.Run("cancelled row is not resurrected", func(t *testing.T) {
		db := setupTestDB(t)
		insertTerminalTestJob(t, db, "runjob", "queued")
		require.NoError(t, dbCancelJob(db, "runjob"))

		err := dbUpdateJobRunning(db, "runjob")
		require.ErrorIs(t, err, errJobAlreadyTerminal,
			"a terminal row must refuse the queued->running transition")
		assert.Equal(t, "cancelled", dbJobStatus(t, db, "runjob"),
			"running update must not resurrect a cancelled row")
	})
}

// TestTerminalDBUpdatesConcurrentSingleWinner interleaves real concurrent
// terminal calls on one row — every pairing of dbCompleteJob/dbCancelJob/
// dbFailJob. Whatever the arrival order, exactly one caller's UPDATE may
// match: the row must end in the status reported by the single winner
// (with exactly the winner's job_images rows), and every other caller must
// report the skip.
func TestTerminalDBUpdatesConcurrentSingleWinner(t *testing.T) {
	db := setupTestDB(t)

	type call struct {
		status       string
		expectImages int
		fn           func(db *sqlx.DB, jobID string) error
	}
	cancelCall := func(db *sqlx.DB, jobID string) error { return dbCancelJob(db, jobID) }
	failCall := func(db *sqlx.DB, jobID string) error {
		return dbFailJob(db, jobID, "generation failed: context canceled")
	}

	pairings := []struct {
		name  string
		calls [2]call
	}{
		{
			name:  "cancel vs fail",
			calls: [2]call{{"cancelled", 0, cancelCall}, {"failed", 0, failCall}},
		},
		{
			name:  "complete vs cancel",
			calls: [2]call{{"completed", 1, completeCall}, {"cancelled", 0, cancelCall}},
		},
		{
			name:  "complete vs fail",
			calls: [2]call{{"completed", 1, completeCall}, {"failed", 0, failCall}},
		},
	}

	for _, pairing := range pairings {
		t.Run(pairing.name, func(t *testing.T) {
			const rounds = 10
			for i := 0; i < rounds; i++ {
				jobID := "racejob"
				insertTerminalTestJob(t, db, jobID, "running")

				var winners atomic.Int64
				var mu sync.Mutex
				finalStatus := ""
				expectImages := -1
				var loserErrs []error

				var wg sync.WaitGroup
				for _, c := range pairing.calls {
					wg.Add(1)
					go func(c call) {
						defer wg.Done()
						if err := c.fn(db, jobID); err == nil {
							if winners.Add(1) != 1 {
								t.Errorf("job %s: more than one terminal write claimed victory", jobID)
							}
							mu.Lock()
							finalStatus = c.status
							expectImages = c.expectImages
							mu.Unlock()
						} else {
							mu.Lock()
							loserErrs = append(loserErrs, err)
							mu.Unlock()
						}
					}(c)
				}
				wg.Wait()

				require.Equal(t, int64(1), winners.Load(), "round %d: exactly one winner expected", i)
				require.Len(t, loserErrs, 1, "round %d: the non-winner must report a skip", i)
				assert.ErrorIs(t, loserErrs[0], errJobAlreadyTerminal, "round %d", i)
				assert.Equal(t, finalStatus, dbJobStatus(t, db, jobID),
					"round %d: DB must show the status of the single winning write", i)
				assert.Equal(t, expectImages, jobImageCount(t, db, jobID),
					"round %d: job_images must reflect exactly the winner's writes", i)

				// Reset for the next round: the row is terminal now, so
				// delete it (job_images cascades); the top of the loop
				// inserts a fresh running row.
				_, err := db.Exec("DELETE FROM jobs WHERE job_id = ?", jobID)
				require.NoError(t, err, "deleting job row between rounds")
			}
		})
	}
}
