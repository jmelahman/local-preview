package supervise

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/manifest"
	"github.com/jmelahman/local-preview/internal/store"
)

// TestMain doubles as the supervised child: when re-executed with
// --helper-server (or --helper-crash) the test binary acts as a tiny
// backend, so tests need no compiled fixture.
func TestMain(m *testing.M) {
	if i := slices.Index(os.Args, "--helper-server"); i >= 0 && i+2 < len(os.Args) {
		runHelperServer(os.Args[i+1], os.Args[i+2])
		return
	}
	if slices.Contains(os.Args, "--helper-crash") {
		fmt.Fprintln(os.Stderr, "helper crashing on purpose")
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func runHelperServer(portStr, stateDir string) {
	countFile := filepath.Join(stateDir, "count")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/count", func(w http.ResponseWriter, r *http.Request) {
		n := 0
		if b, err := os.ReadFile(countFile); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		n++
		os.WriteFile(countFile, []byte(strconv.Itoa(n)), 0o644)
		fmt.Fprintf(w, "%d", n)
	})
	http.ListenAndServe("127.0.0.1:"+portStr, mux) //nolint:errcheck
}

type fixture struct {
	m      *Manager
	db     *db.Store
	files  *store.Store
	repoID int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	root := t.TempDir()
	files := store.New(
		filepath.Join(root, "artifacts"),
		filepath.Join(root, "state"),
		filepath.Join(root, "tmp"),
	)
	m := New(database, files, filepath.Join(root, "logs"))
	m.healthInterval = 25 * time.Millisecond
	t.Cleanup(m.StopAll)
	repo, err := database.CreateRepo("demo", "/src", "/bare")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{m: m, db: database, files: files, repoID: repo.ID}
}

func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// provision publishes an (empty) backend artifact dir, a fresh state dir,
// and the backend_artifacts row with the given run argv.
func (f *fixture) provision(t *testing.T, beHash string, argv []string) {
	t.Helper()
	scratch, _, err := f.files.NewScratchDir("be")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.files.PublishBackend("demo", beHash, scratch, false); err != nil {
		t.Fatal(err)
	}
	if err := f.files.InitFreshStateDir("demo", beHash); err != nil {
		t.Fatal(err)
	}
	cfg := manifest.Backend{
		Run:          argv,
		HealthPath:   "/api/health",
		StartTimeout: manifest.Duration(10 * time.Second),
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.CreateBackendArtifact(db.BackendArtifact{
		RepoID: f.repoID, BeHash: beHash,
		StateDir:  f.files.StateDirPath("demo", beHash),
		RunConfig: string(raw),
	}); err != nil {
		t.Fatal(err)
	}
}

func serverArgv(t *testing.T) []string {
	return []string{testExe(t), "--helper-server", "{port}", "{state_dir}"}
}

func get(t *testing.T, port int, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestEnsureRunningReuseAndStop(t *testing.T) {
	f := newFixture(t)
	f.provision(t, "be-run", serverArgv(t))
	ctx := context.Background()

	port, err := f.m.EnsureRunning(ctx, f.repoID, "demo", "be-run")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, port, "/api/health"); code != 200 {
		t.Fatalf("health = %d", code)
	}
	if got := f.m.Status(f.repoID, "be-run"); got != "running" {
		t.Fatalf("Status = %q", got)
	}

	again, err := f.m.EnsureRunning(ctx, f.repoID, "demo", "be-run")
	if err != nil || again != port {
		t.Fatalf("second EnsureRunning = %d, %v; want same port %d", again, err, port)
	}

	recs, err := f.db.ListProcessRecords()
	if err != nil || len(recs) != 1 || recs[0].Port != port {
		t.Fatalf("process records = %+v, %v", recs, err)
	}

	f.m.Stop(Key{RepoID: f.repoID, BeHash: "be-run"}, "test")
	if got := f.m.Status(f.repoID, "be-run"); got != "stopped" {
		t.Fatalf("Status after stop = %q", got)
	}
	recs, _ = f.db.ListProcessRecords()
	if len(recs) != 0 {
		t.Fatalf("records after stop = %+v", recs)
	}
}

func TestOutOfBandKillRecovers(t *testing.T) {
	f := newFixture(t)
	f.provision(t, "be-kill", serverArgv(t))
	ctx := context.Background()

	port, err := f.m.EnsureRunning(ctx, f.repoID, "demo", "be-kill")
	if err != nil {
		t.Fatal(err)
	}
	recs, _ := f.db.ListProcessRecords()
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	syscall.Kill(-recs[0].PGID, syscall.SIGKILL)

	// The reaper notices and clears state; a new EnsureRunning restarts.
	deadline := time.Now().Add(5 * time.Second)
	for f.m.Status(f.repoID, "be-kill") != "stopped" {
		if time.Now().After(deadline) {
			t.Fatal("kill was not detected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	newPort, err := f.m.EnsureRunning(ctx, f.repoID, "demo", "be-kill")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, newPort, "/api/health"); code != 200 {
		t.Fatalf("health after restart = %d", code)
	}
	_ = port
}

func TestInstantCrashSurfaces(t *testing.T) {
	f := newFixture(t)
	f.provision(t, "be-crash", []string{testExe(t), "--helper-crash"})

	_, err := f.m.EnsureRunning(context.Background(), f.repoID, "demo", "be-crash")
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("err = %v, want exit error", err)
	}
	if got := f.m.Status(f.repoID, "be-crash"); got != "stopped" {
		t.Fatalf("Status = %q", got)
	}
}

// --- lineage fork tests ---

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	runTestGit(t, dir, "add", "-A")
	runTestGit(t, dir, "commit", "-qm", msg)
	return runTestGit(t, dir, "rev-parse", "HEAD")
}

func TestForkOrInitStateDir(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Source repo: c1 then c2.
	src := t.TempDir()
	runTestGit(t, src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	c1 := commit(t, src, "c1")
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	c2 := commit(t, src, "c2")

	mgr := gitrepo.NewManager(filepath.Join(t.TempDir(), "repos"))
	repo, err := mgr.Add(ctx, "demo", src)
	if err != nil {
		t.Fatal(err)
	}

	// c1 was deployed with backend be1; give be1 observable state.
	f.provision(t, "be1", serverArgv(t))
	d1, err := f.db.CreateDeploy(f.repoID, c1, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetDeployHashes(d1.ID, "fe1", "be1", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetDeployReady(d1.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.files.StateDirPath("demo", "be1"), "count"), []byte("42"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start be1 so the fork has to quiesce it.
	if _, err := f.m.EnsureRunning(ctx, f.repoID, "demo", "be1"); err != nil {
		t.Fatal(err)
	}

	// New backend hash at c2 forks be1's state.
	cfg, _ := json.Marshal(manifest.Backend{Run: serverArgv(t), HealthPath: "/api/health"})
	if err := f.m.ForkOrInitStateDir(ctx, repo, f.repoID, "demo", "be2", c2, string(cfg)); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Status(f.repoID, "be1"); got != "stopped" {
		t.Fatalf("ancestor status after fork = %q, want stopped (quiesced)", got)
	}
	forked, err := os.ReadFile(filepath.Join(f.files.StateDirPath("demo", "be2"), "count"))
	if err != nil {
		t.Fatal(err)
	}
	if string(forked) != "42" {
		t.Fatalf("forked state = %q, want 42", forked)
	}
	art, err := f.db.GetBackendArtifact(f.repoID, "be2")
	if err != nil || art.ForkedFrom != "be1" {
		t.Fatalf("artifact row = %+v, %v", art, err)
	}

	// Idempotent.
	if err := f.m.ForkOrInitStateDir(ctx, repo, f.repoID, "demo", "be2", c2, string(cfg)); err != nil {
		t.Fatal(err)
	}

	// A commit with no deployed ancestry gets fresh state.
	if err := f.m.ForkOrInitStateDir(ctx, repo, f.repoID, "demo", "be3", c1, string(cfg)); err != nil {
		t.Fatal(err)
	}
	art3, err := f.db.GetBackendArtifact(f.repoID, "be3")
	if err != nil || art3.ForkedFrom != "" {
		t.Fatalf("fresh artifact row = %+v, %v", art3, err)
	}
	if _, err := os.Stat(filepath.Join(f.files.StateDirPath("demo", "be3"), "count")); !os.IsNotExist(err) {
		t.Fatal("fresh state dir should be empty")
	}
}
