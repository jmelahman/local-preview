package build

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

// TestStartResumesWithoutBlocking: a backlog larger than the work buffer must
// not wedge Start — the caller goes on to serve HTTP, and a server that never
// listens can't be told to cancel the backlog either.
func TestStartResumesWithoutBlocking(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.CreateRepo("demo", "/src", "/bare", db.RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	files := store.New(
		filepath.Join(root, "artifacts"),
		filepath.Join(root, "state"),
		filepath.Join(root, "tmp"),
	)
	super := supervise.New(database, files, filepath.Join(root, "logs"))
	t.Cleanup(super.StopAll)
	q := NewQueue(database, gitrepo.NewManager(filepath.Join(root, "repos")), files, super, filepath.Join(root, "logs"), nil)

	for i := range cap(q.work) + 1 {
		if _, err := database.CreateDeploy(repo.ID, fmt.Sprintf("%040x", i), db.DeployMeta{}); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	// No workers: nothing drains the channel, so a blocking resume never
	// returns.
	go func() {
		q.Start(ctx, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Start blocked on a backlog larger than the work buffer")
	}
}
