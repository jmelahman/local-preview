package ingest_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
)

func TestNativeS3LateUploadSubmissionReacquiresCapacity(t *testing.T) {
	for _, full := range []bool{false, true} {
		name := "capacity available"
		if full {
			name = "full quota then retry"
		}
		t.Run(name, func(t *testing.T) {
			c := newControl(t, 1)
			defer c.cancel()
			sha := c.sourceRepo(t, "printf built > built.txt")
			_, data := archive(t, "slow frontend upload")
			staged := stage(t, c, sha, data)
			started := time.Now().Add(-11 * time.Minute)
			hardDeadline := started.Add(25 * time.Minute).Unix()
			database, err := sql.Open("sqlite", c.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := database.Exec(`UPDATE ingestion_jobs SET status = 'uploading', created_at = ?, expires_at = ? WHERE id = ?`, started.Unix(), hardDeadline, staged.ID)
			closeErr := database.Close()
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			waiting, err := c.coord.Get(staged.ID)
			if err != nil || waiting.Status != db.JobUploading {
				t.Fatalf("valid long upload expired before submit: %+v error=%v", waiting, err)
			}
			if full {
				blocker, err := c.coord.ReserveBuild(c.ctx, "demo", sha, false, "other request")
				if err != nil {
					t.Fatal(err)
				}
				if code := requestJSON(t, c.ctx, c.public.URL, "", "POST", "/api/repos/demo/ingestions/"+staged.ID+"/submit", struct{}{}, nil); code != 429 {
					t.Fatalf("full late-submit HTTP%d", code)
				}
				waiting, err = c.coord.Get(staged.ID)
				if err != nil || waiting.Status != db.JobUploading {
					t.Fatalf("retryable late upload changed state: %+v error=%v", waiting, err)
				}
				if err := c.database.CancelJob(blocker.ID, "drained", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			var submitted reservation
			if code := requestJSON(t, c.ctx, c.public.URL, "", "POST", "/api/repos/demo/ingestions/"+staged.ID+"/submit", struct{}{}, &submitted); code != 202 || submitted.Status != db.JobQueued {
				t.Fatalf("late-submit HTTP%d result=%+v", code, submitted)
			}
			queued, err := c.database.GetJob(staged.ID)
			if err != nil || queued.ExpiresAt != hardDeadline {
				t.Fatalf("submission extended hard deadline: %+v error=%v", queued, err)
			}
		})
	}
}
