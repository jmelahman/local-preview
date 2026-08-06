package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestOpenFile(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
}

// TestOpenMigratesOldSchema opens a database whose backend_artifacts table
// predates the init_done_at column and expects Open's migrations to add it.
func TestOpenMigratesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE backend_artifacts (
		repo_id INTEGER NOT NULL,
		be_hash TEXT NOT NULL,
		forked_from TEXT NOT NULL DEFAULT '',
		state_dir TEXT NOT NULL,
		run_config TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		PRIMARY KEY (repo_id, be_hash)
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(
		`INSERT INTO backend_artifacts (repo_id, be_hash, state_dir, run_config) VALUES (1, 'be1', '/state', '{}')`,
	); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.GetBackendArtifact(1, "be1")
	if err != nil || a.InitDoneAt != "" {
		t.Fatalf("migrated artifact = %+v, %v", a, err)
	}
	if err := s.MarkBackendInitDone(1, "be1"); err != nil {
		t.Fatal(err)
	}
}

func TestRepoCRUD(t *testing.T) {
	s := newTestStore(t)

	r, err := s.CreateRepo("demo", "/src/demo", "/data/repos/demo.git")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID == 0 || r.Name != "demo" || r.CreatedAt == "" {
		t.Fatalf("unexpected repo: %+v", r)
	}

	if _, err := s.CreateRepo("demo", "/elsewhere", "/x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name err = %v, want ErrConflict", err)
	}

	got, err := s.GetRepoByName("demo")
	if err != nil || got.ID != r.ID {
		t.Fatalf("GetRepoByName = %+v, %v", got, err)
	}
	if _, err := s.GetRepoByName("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing repo err = %v, want ErrNotFound", err)
	}

	repos, err := s.ListRepos()
	if err != nil || len(repos) != 1 {
		t.Fatalf("ListRepos = %+v, %v", repos, err)
	}

	if r.Watch || r.WatchBranches != "" {
		t.Fatalf("new repo should not be watched: %+v", r)
	}
	w, err := s.SetRepoWatch(r.ID, true, "main,release/*")
	if err != nil || !w.Watch || w.WatchBranches != "main,release/*" {
		t.Fatalf("SetRepoWatch = %+v, %v", w, err)
	}
	if got, _ := s.GetRepoByName("demo"); !got.Watch || got.WatchBranches != "main,release/*" {
		t.Fatalf("watch not persisted: %+v", got)
	}
	if w, err = s.SetRepoWatch(r.ID, false, ""); err != nil || w.Watch || w.WatchBranches != "" {
		t.Fatalf("unwatch = %+v, %v", w, err)
	}
	if _, err := s.SetRepoWatch(999, true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetRepoWatch missing repo err = %v, want ErrNotFound", err)
	}
}

const shaA = "aaaaaaa1111111111111111111111111111111111"

func TestDeployLifecycle(t *testing.T) {
	s := newTestStore(t)
	r, err := s.CreateRepo("demo", "/src", "/bare")
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.CreateDeploy(r.ID, shaA, DeployMeta{
		Ref: "main", Branch: "main", AuthorName: "Ada", AuthorEmail: "ada@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DeployQueued || d.ShortSHA != shaA[:7] || d.Ref != "main" {
		t.Fatalf("unexpected deploy: %+v", d)
	}
	if d.Branch != "main" || d.AuthorName != "Ada" || d.AuthorEmail != "ada@example.com" {
		t.Fatalf("commit metadata not stored: %+v", d)
	}

	if err := s.SetDeployBuilding(d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeployHashes(d.ID, "fe1", "be1", "/logs/fe1.log", "/logs/be1.log"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeployReady(d.ID); err != nil {
		t.Fatal(err)
	}

	row, err := s.GetDeployByID(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != DeployReady || row.FeHash != "fe1" || row.RepoName != "demo" {
		t.Fatalf("unexpected row: %+v", row)
	}

	if err := s.SetDeployFailed(d.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetDeploy(d.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetDeployBySHA(r.ID, shaA)
	if got.Status != DeployQueued || got.Error != "" || got.AttemptCount != 1 {
		t.Fatalf("after reset: %+v", got)
	}

	if err := s.SetDeployReady(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of missing deploy = %v, want ErrNotFound", err)
	}
}

func TestShortSHAGrowsOnCollision(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("demo", "/src", "/bare")

	// Two shas sharing a 7-char prefix force the second to use 8 chars.
	shaX := "abcdef0" + strings.Repeat("1", 33)
	shaY := "abcdef0" + strings.Repeat("2", 33)
	d1, err := s.CreateDeploy(r.ID, shaX, DeployMeta{})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDeploy(r.ID, shaY, DeployMeta{})
	if err != nil {
		t.Fatal(err)
	}
	if d1.ShortSHA != "abcdef0" {
		t.Fatalf("d1.ShortSHA = %q", d1.ShortSHA)
	}
	if d2.ShortSHA != "abcdef02" {
		t.Fatalf("d2.ShortSHA = %q, want 8-char prefix", d2.ShortSHA)
	}

	// Same sha again is a conflict (redeploy resets the row instead).
	if _, err := s.CreateDeploy(r.ID, shaX, DeployMeta{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate sha err = %v, want ErrConflict", err)
	}
}

func TestListDeploysFilter(t *testing.T) {
	s := newTestStore(t)
	r1, _ := s.CreateRepo("app", "/src/app", "/bare/app")
	r2, _ := s.CreateRepo("lib", "/src/lib", "/bare/lib")

	shaB := "bbbbbbb2222222222222222222222222222222222"
	shaC := "ccccccc3333333333333333333333333333333333"
	seed := []struct {
		repoID int64
		sha    string
		meta   DeployMeta
	}{
		{r1.ID, shaA, DeployMeta{Branch: "main", AuthorName: "Ada Lovelace", AuthorEmail: "ada@example.com"}},
		{r1.ID, shaB, DeployMeta{Branch: "feature", AuthorName: "Grace Hopper", AuthorEmail: "grace@example.com"}},
		{r2.ID, shaC, DeployMeta{Branch: "main", AuthorName: "Ada Lovelace", AuthorEmail: "ada@example.com"}},
	}
	for _, sd := range seed {
		if _, err := s.CreateDeploy(sd.repoID, sd.sha, sd.meta); err != nil {
			t.Fatal(err)
		}
	}

	for name, tc := range map[string]struct {
		f    DeployFilter
		want int
	}{
		"all":              {DeployFilter{}, 3},
		"repo":             {DeployFilter{Repo: "app"}, 2},
		"branch":           {DeployFilter{Branch: "main"}, 2},
		"repo+branch":      {DeployFilter{Repo: "app", Branch: "main"}, 1},
		"author name":      {DeployFilter{Author: "grace hopper"}, 1},
		"author email":     {DeployFilter{Author: "ADA@example"}, 2},
		"author substring": {DeployFilter{Author: "lovelace"}, 2},
		"author wildcards": {DeployFilter{Author: "%"}, 0},
		"no match":         {DeployFilter{Branch: "gone"}, 0},
	} {
		got, err := s.ListDeploys(tc.f)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: %d deploys, want %d", name, len(got), tc.want)
		}
	}
}

func TestMigrateAddsColumnsToExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A repos table from before the watch columns existed.
	if _, err := old.Exec(`CREATE TABLE repos (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		source TEXT NOT NULL,
		bare_path TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	// A deploys table from before the commit-metadata columns existed.
	if _, err := old.Exec(`CREATE TABLE deploys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo_id INTEGER NOT NULL,
		sha TEXT NOT NULL,
		short_sha TEXT NOT NULL,
		ref TEXT NOT NULL DEFAULT '',
		fe_hash TEXT NOT NULL DEFAULT '',
		be_hash TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'queued',
		error TEXT NOT NULL DEFAULT '',
		attempt_count INTEGER NOT NULL DEFAULT 0,
		fe_build_log_path TEXT NOT NULL DEFAULT '',
		be_build_log_path TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '',
		UNIQUE (repo_id, sha),
		UNIQUE (repo_id, short_sha)
	)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.CreateRepo("demo", "/src", "/bare")
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDeploy(r.ID, shaA, DeployMeta{Branch: "main", AuthorName: "Ada"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Branch != "main" || d.AuthorName != "Ada" {
		t.Fatalf("migrated columns not usable: %+v", d)
	}
	if r, err = s.SetRepoWatch(r.ID, true, "main"); err != nil || !r.Watch {
		t.Fatalf("migrated repos columns not usable: %+v, %v", r, err)
	}
}

func TestDeploysBySHAPrefix(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("demo", "/src", "/bare")
	if _, err := s.CreateDeploy(r.ID, shaA, DeployMeta{}); err != nil {
		t.Fatal(err)
	}

	got, err := s.DeploysBySHAPrefix(r.ID, "aaaaaaa")
	if err != nil || len(got) != 1 {
		t.Fatalf("prefix match = %+v, %v", got, err)
	}
	got, err = s.DeploysBySHAPrefix(r.ID, "bbbb999")
	if err != nil || len(got) != 0 {
		t.Fatalf("no-match = %+v, %v", got, err)
	}
	if _, err := s.DeploysBySHAPrefix(r.ID, "AB%'--"); err == nil {
		t.Fatal("non-hex prefix should be rejected")
	}
}

func TestBackendArtifactsAndProcesses(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("demo", "/src", "/bare")

	a := BackendArtifact{RepoID: r.ID, BeHash: "be1", StateDir: "/state/demo/be1", RunConfig: `{"run":["x"]}`}
	if err := s.CreateBackendArtifact(a); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	a2 := a
	a2.ForkedFrom = "should-not-replace"
	if err := s.CreateBackendArtifact(a2); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBackendArtifact(r.ID, "be1")
	if err != nil || got.ForkedFrom != "" || got.RunConfig != a.RunConfig {
		t.Fatalf("GetBackendArtifact = %+v, %v", got, err)
	}
	if _, err := s.GetBackendArtifact(r.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing artifact err = %v", err)
	}

	if got.InitDoneAt != "" {
		t.Fatalf("InitDoneAt before mark = %q, want empty", got.InitDoneAt)
	}
	if err := s.MarkBackendInitDone(r.ID, "be1"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetBackendArtifact(r.ID, "be1")
	if err != nil || got.InitDoneAt == "" {
		t.Fatalf("after MarkBackendInitDone: %+v, %v", got, err)
	}

	rec := ProcessRecord{RepoID: r.ID, BeHash: "be1", PID: 123, PGID: 123, Port: 40001}
	if err := s.UpsertProcessRecord(rec); err != nil {
		t.Fatal(err)
	}
	rec.Port = 40002
	if err := s.UpsertProcessRecord(rec); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListProcessRecords()
	if err != nil || len(list) != 1 || list[0].Port != 40002 {
		t.Fatalf("ListProcessRecords = %+v, %v", list, err)
	}
	if err := s.DeleteProcessRecord(r.ID, "be1"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListProcessRecords()
	if len(list) != 0 {
		t.Fatalf("records after delete = %+v", list)
	}

	if err := s.AddProcessEvent(r.ID, "be1", "start_attempt", "port 40001"); err != nil {
		t.Fatal(err)
	}
}
