package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	return New(
		filepath.Join(root, "artifacts"),
		filepath.Join(root, "state"),
		filepath.Join(root, "tmp"),
	)
}

func stageDir(t *testing.T, s *Store, content string) string {
	t.Helper()
	dir, _, err := s.NewScratchDir("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readArtifact(t *testing.T, s *Store, repo, hash string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.FrontendDir(repo, hash), "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPublishIdempotentAndOverwrite(t *testing.T) {
	s := newTestStore(t)
	if s.HasFrontend("demo", "h1") {
		t.Fatal("artifact should not exist yet")
	}

	if err := s.PublishFrontend("demo", "h1", stageDir(t, s, "v1"), false); err != nil {
		t.Fatal(err)
	}
	if !s.HasFrontend("demo", "h1") || readArtifact(t, s, "demo", "h1") != "v1" {
		t.Fatal("publish did not land")
	}

	// Idempotent: a second publish without overwrite keeps the original.
	if err := s.PublishFrontend("demo", "h1", stageDir(t, s, "v2"), false); err != nil {
		t.Fatal(err)
	}
	if readArtifact(t, s, "demo", "h1") != "v1" {
		t.Fatal("non-overwrite publish replaced content")
	}

	// Overwrite (--rebuild) replaces it.
	if err := s.PublishFrontend("demo", "h1", stageDir(t, s, "v3"), true); err != nil {
		t.Fatal(err)
	}
	if readArtifact(t, s, "demo", "h1") != "v3" {
		t.Fatal("overwrite publish kept old content")
	}
}

func TestPublishConcurrentSameHash(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		dir := stageDir(t, s, "same")
		wg.Go(func() {
			errs <- s.PublishFrontend("demo", "h", dir, false)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if readArtifact(t, s, "demo", "h") != "same" {
		t.Fatal("artifact missing after concurrent publish")
	}
}

func TestStateDirForkAndInit(t *testing.T) {
	s := newTestStore(t)
	if err := s.InitFreshStateDir("demo", "be1"); err != nil {
		t.Fatal(err)
	}
	if !s.HasStateDir("demo", "be1") {
		t.Fatal("fresh state dir missing")
	}
	// Simulate app state, including a nested dir.
	if err := os.MkdirAll(filepath.Join(s.StateDirPath("demo", "be1"), "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.StateDirPath("demo", "be1"), "nested", "db.sqlite"), []byte("data-v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.ForkStateDir("demo", "be1", "be2"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(s.StateDirPath("demo", "be2"), "nested", "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data-v1" {
		t.Fatalf("forked content = %q", got)
	}
	st, err := os.Stat(filepath.Join(s.StateDirPath("demo", "be2"), "nested", "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("forked file mode = %v, want 0600", st.Mode().Perm())
	}

	// Fork is a copy: mutating the fork must not touch the source.
	if err := os.WriteFile(filepath.Join(s.StateDirPath("demo", "be2"), "nested", "db.sqlite"), []byte("data-v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(s.StateDirPath("demo", "be1"), "nested", "db.sqlite"))
	if string(src) != "data-v1" {
		t.Fatal("mutating the fork changed the source")
	}

	// Idempotent; missing source is an error.
	if err := s.ForkStateDir("demo", "be1", "be2"); err != nil {
		t.Fatal(err)
	}
	if err := s.ForkStateDir("demo", "missing", "be3"); err == nil {
		t.Fatal("fork from missing source should fail")
	}
}

func TestSweepTmp(t *testing.T) {
	s := newTestStore(t)
	old, _, err := s.NewScratchDir("old")
	if err != nil {
		t.Fatal(err)
	}
	fresh, _, err := s.NewScratchDir("fresh")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := s.SweepTmp(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old scratch dir survived sweep")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh scratch dir was swept")
	}
}
