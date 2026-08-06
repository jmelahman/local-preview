package db

import (
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
}

const shaA = "aaaaaaa1111111111111111111111111111111111"

func TestDeployLifecycle(t *testing.T) {
	s := newTestStore(t)
	r, err := s.CreateRepo("demo", "/src", "/bare")
	if err != nil {
		t.Fatal(err)
	}

	d, err := s.CreateDeploy(r.ID, shaA, "main")
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != DeployQueued || d.ShortSHA != shaA[:7] || d.Ref != "main" {
		t.Fatalf("unexpected deploy: %+v", d)
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
	d1, err := s.CreateDeploy(r.ID, shaX, "")
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.CreateDeploy(r.ID, shaY, "")
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
	if _, err := s.CreateDeploy(r.ID, shaX, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate sha err = %v, want ErrConflict", err)
	}
}

func TestDeploysBySHAPrefix(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.CreateRepo("demo", "/src", "/bare")
	if _, err := s.CreateDeploy(r.ID, shaA, ""); err != nil {
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
