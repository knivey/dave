package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDbGetJobImage(t *testing.T) {
	db := setupTestDB(t)
	// Seed a completed-style job + two job_images rows the way dbCompleteJob does.
	_, err := db.Exec(`INSERT INTO jobs (job_id, type, status, workflow, prompt) VALUES ('j1','generate','completed','w','p')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('j1','https://img/1','f1.png','s','output','image/png',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('j1','https://img/2','f2.png','s','output','image/png',1)`)
	require.NoError(t, err)

	img, url, err := dbGetJobImage(db, "j1", 1)
	require.NoError(t, err)
	assert.Equal(t, "f2.png", img.Filename)
	assert.Equal(t, "https://img/2", url)

	_, _, err = dbGetJobImage(db, "j1", 5)
	require.Error(t, err, "ordinal out of range errors")
	_, _, err = dbGetJobImage(db, "nope", 0)
	require.Error(t, err, "unknown job errors")
}

func TestFetchGenRefBytesViewPrimaryImgsiteFallback(t *testing.T) {
	db := setupTestDB(t)
	_, err := db.Exec(`INSERT INTO jobs (job_id, type, status, workflow, prompt) VALUES ('j1','generate','completed','w','p')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO job_images (job_id, url, filename, subfolder, img_type, mime_type, ordinal) VALUES ('j1','IMGURL','f1.png','','output','image/png',0)`)
	require.NoError(t, err)

	// /view serves the bytes -> used, no imgsite hit.
	var imgsiteHits atomic.Int64
	comfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/view" {
			fmt.Fprint(w, "VIEWBYTES")
			return
		}
		http.Error(w, "not found", 404)
	}))
	defer comfy.Close()
	imgsite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imgsiteHits.Add(1)
		fmt.Fprint(w, "IMGSITEBYTES")
	}))
	defer imgsite.Close()
	// point the row's url at the imgsite stub:
	_, err = db.Exec(`UPDATE job_images SET url = ? WHERE job_id='j1' AND ordinal=0`, imgsite.URL+"/i/1")
	require.NoError(t, err)

	b, err := fetchGenRefBytes(context.Background(), db, comfy.URL, "gen_j1")
	require.NoError(t, err)
	assert.Equal(t, []byte("VIEWBYTES"), b)
	assert.Zero(t, imgsiteHits.Load())

	// /view fails (500) -> imgsite fallback serves the bytes.
	comfy2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer comfy2.Close()
	b, err = fetchGenRefBytes(context.Background(), db, comfy2.URL, "gen_j1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), imgsiteHits.Load())
	assert.Equal(t, []byte("IMGSITEBYTES"), b, "fallback bytes come from the imgsite url")
}
