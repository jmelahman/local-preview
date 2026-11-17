package ingest_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/ingest"
)

func TestNativeS3SupersededDeployStillPublishesOwnUpload(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		name := "create"
		if overwrite {
			name = "overwrite"
		}
		t.Run(name, func(t *testing.T) {
			c := newControl(t, 8)
			defer c.cancel()
			sha := c.sourceRepo(t, "printf built > built.txt")
			_, data := archive(t, "uploaded frontend")
			reservation := stage(t, c, sha, data, func(req *ingest.Request) { req.Deploy = true; req.Overwrite = overwrite })
			old, err := c.database.ClaimJob("old-worker", "old-token", time.Now(), time.Minute, 3)
			if err != nil {
				t.Fatal(err)
			}
			if old.ID != reservation.ID {
				t.Fatal("wrong upload claimed")
			}
			newer, err := c.coord.ReserveBuild(c.ctx, "demo", sha, true, "newer actor")
			if err != nil {
				t.Fatal(err)
			}
			lease, err := c.database.ClaimJob("new-worker", "new-token", time.Now(), time.Minute, 3)
			if err != nil {
				t.Fatal(err)
			}
			if lease.ID != newer.ID {
				t.Fatal("wrong newer job claimed")
			}
			if err := c.coord.Complete(newer.ID, lease.LeaseToken, ingest.Completion{Execution: build.ExecutionResult{Deploy: &build.ExecutedDeploy{SHA: sha}}}); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte("frontend inputs"))
			hash := hex.EncodeToString(digest[:])
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("uploaded frontend"), 0600); err != nil {
				t.Fatal(err)
			}
			if overwrite {
				initial := t.TempDir()
				if err := os.WriteFile(filepath.Join(initial, "index.html"), []byte("old frontend"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := c.tier.Save(c.ctx, "demo", "fe", hash, initial); err != nil {
					t.Fatal(err)
				}
			}
			attempt, err := c.tier.Attempt(old.ID, old.LeaseToken)
			if err != nil {
				t.Fatal(err)
			}
			if err := attempt.Save(c.ctx, "demo", "fe", hash, directory); err != nil {
				t.Fatal(err)
			}
			completion := ingest.Completion{Execution: build.ExecutionResult{Upload: &build.UploadResult{SHA: sha, Side: build.SideFrontend, Hash: hash}, Deploy: &build.ExecutedDeploy{SHA: sha, FrontendHash: hash}}, Outputs: attempt.ListOutputs()}
			c.startPublisher()
			waitFor(t, c.ctx, func() bool { job, err := c.database.GetJob(newer.ID); return err == nil && job.Status == db.JobDone })
			if err := c.coord.Complete(old.ID, old.LeaseToken, completion); err != nil {
				t.Fatal(err)
			}
			waitFor(t, c.ctx, func() bool { job, err := c.database.GetJob(old.ID); return err == nil && job.Status == db.JobDone })
			row, err := c.database.GetDeployBySHA(c.repo.ID, sha)
			if err != nil {
				t.Fatal(err)
			}
			if row.IngestionJobID != newer.ID || row.FeHash != "" {
				t.Fatalf("older upload changed the newer deploy: %+v", row)
			}
			job, err := c.database.GetJob(old.ID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := c.coord.Result(c.ctx, job)
			if err != nil || result.Upload == nil || !result.Upload.Published {
				t.Fatalf("own upload was discarded: %+v error %v", result, err)
			}
			if err := c.files.Hydrate(c.ctx, "demo", "fe", hash); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(c.files.FrontendDir("demo", hash), "index.html"))
			if err != nil || string(content) != "uploaded frontend" {
				t.Fatalf("own upload missing: %q error %v", content, err)
			}
			waitFor(t, c.ctx, func() bool {
				_, err := c.tier.StatUpload(c.ctx, old.ID)
				return err != nil
			})
		})
	}
}
