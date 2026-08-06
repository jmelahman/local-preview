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
	if i := slices.Index(os.Args, "--helper-init"); i >= 0 && i+2 < len(os.Args) {
		os.Exit(runHelperInit(os.Args[i+1], os.Args[i+2]))
	}
	os.Exit(m.Run())
}

// runHelperInit appends one marker per invocation to <stateDir>/init-runs so
// tests can count executions, then behaves per mode: "ok" succeeds, "fail"
// always exits nonzero, "fail-once" fails only the first invocation, and
// "sleep" hangs to trip the init timeout.
func runHelperInit(stateDir, mode string) int {
	runsFile := filepath.Join(stateDir, "init-runs")
	prev, _ := os.ReadFile(runsFile)
	os.WriteFile(runsFile, append(prev, 'x'), 0o644)
	switch mode {
	case "fail":
		return 3
	case "fail-once":
		if len(prev) == 0 {
			return 3
		}
	case "sleep":
		time.Sleep(30 * time.Second)
	}
	return 0
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
	f.provisionIdle(t, beHash, argv, 0)
}

// provisionIdle is provision with an explicit idle_timeout.
func (f *fixture) provisionIdle(t *testing.T, beHash string, argv []string, idle time.Duration) {
	t.Helper()
	f.provisionCfg(t, beHash, manifest.Backend{
		Run:          argv,
		HealthPath:   "/api/health",
		StartTimeout: manifest.Duration(10 * time.Second),
		IdleTimeout:  manifest.Duration(idle),
	})
}

// provisionCfg is provision with full control over the run config.
func (f *fixture) provisionCfg(t *testing.T, beHash string, cfg manifest.Backend) {
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

// initRuns counts how many times the init helper ran against the state dir.
func (f *fixture) initRuns(t *testing.T, beHash string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.files.StateDirPath("demo", beHash), "init-runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(b)
}

func initArgv(t *testing.T, mode string) []string {
	return []string{testExe(t), "--helper-init", "{state_dir}", mode}
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

	port, err := f.m.EnsureRunning(ctx, BackendKey(f.repoID, "be-run"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, port, "/api/health"); code != 200 {
		t.Fatalf("health = %d", code)
	}
	if got := f.m.Status(BackendKey(f.repoID, "be-run")); got != "running" {
		t.Fatalf("Status = %q", got)
	}

	again, err := f.m.EnsureRunning(ctx, BackendKey(f.repoID, "be-run"), "demo")
	if err != nil || again != port {
		t.Fatalf("second EnsureRunning = %d, %v; want same port %d", again, err, port)
	}

	recs, err := f.db.ListProcessRecords()
	if err != nil || len(recs) != 1 || recs[0].Port != port {
		t.Fatalf("process records = %+v, %v", recs, err)
	}

	f.m.Stop(BackendKey(f.repoID, "be-run"), "test")
	if got := f.m.Status(BackendKey(f.repoID, "be-run")); got != "idle" {
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

	port, err := f.m.EnsureRunning(ctx, BackendKey(f.repoID, "be-kill"), "demo")
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
	for f.m.Status(BackendKey(f.repoID, "be-kill")) != "idle" {
		if time.Now().After(deadline) {
			t.Fatal("kill was not detected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	newPort, err := f.m.EnsureRunning(ctx, BackendKey(f.repoID, "be-kill"), "demo")
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

	_, err := f.m.EnsureRunning(context.Background(), BackendKey(f.repoID, "be-crash"), "demo")
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("err = %v, want exit error", err)
	}
	if got := f.m.Status(BackendKey(f.repoID, "be-crash")); got != "idle" {
		t.Fatalf("Status = %q", got)
	}
}

func TestInitRunsOncePerArtifact(t *testing.T) {
	f := newFixture(t)
	f.provisionCfg(t, "be-init", manifest.Backend{
		Init:         [][]string{initArgv(t, "ok")},
		Run:          serverArgv(t),
		HealthPath:   "/api/health",
		StartTimeout: manifest.Duration(10 * time.Second),
	})
	ctx := context.Background()
	k := BackendKey(f.repoID, "be-init")

	if _, err := f.m.EnsureRunning(ctx, k, "demo"); err != nil {
		t.Fatal(err)
	}
	if runs := f.initRuns(t, "be-init"); runs != 1 {
		t.Fatalf("init runs after first start = %d, want 1", runs)
	}
	art, err := f.db.GetBackendArtifact(f.repoID, "be-init")
	if err != nil || art.InitDoneAt == "" {
		t.Fatalf("artifact after init = %+v, %v", art, err)
	}

	// A cold start of the same artifact must skip init.
	f.m.Stop(k, "test")
	port, err := f.m.EnsureRunning(ctx, k, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := get(t, port, "/api/health"); code != 200 {
		t.Fatalf("health after cold start = %d", code)
	}
	if runs := f.initRuns(t, "be-init"); runs != 1 {
		t.Fatalf("init runs after cold start = %d, want still 1", runs)
	}
}

func TestInitFailureRetriesNextStart(t *testing.T) {
	f := newFixture(t)
	f.provisionCfg(t, "be-init-retry", manifest.Backend{
		Init:         [][]string{initArgv(t, "fail-once")},
		Run:          serverArgv(t),
		HealthPath:   "/api/health",
		StartTimeout: manifest.Duration(10 * time.Second),
	})
	ctx := context.Background()
	k := BackendKey(f.repoID, "be-init-retry")

	_, err := f.m.EnsureRunning(ctx, k, "demo")
	if err == nil || !strings.Contains(err.Error(), "init") {
		t.Fatalf("err = %v, want init failure", err)
	}
	if art, err := f.db.GetBackendArtifact(f.repoID, "be-init-retry"); err != nil || art.InitDoneAt != "" {
		t.Fatalf("failed init must not be recorded done: %+v, %v", art, err)
	}
	if got := f.m.Status(k); got != "stopped" {
		t.Fatalf("Status after init failure = %q", got)
	}

	// The next start attempt re-runs init, which now succeeds.
	if _, err := f.m.EnsureRunning(ctx, k, "demo"); err != nil {
		t.Fatal(err)
	}
	if runs := f.initRuns(t, "be-init-retry"); runs != 2 {
		t.Fatalf("init runs = %d, want 2", runs)
	}
	if art, _ := f.db.GetBackendArtifact(f.repoID, "be-init-retry"); art.InitDoneAt == "" {
		t.Fatal("init success was not recorded")
	}
}

func TestInitTimeout(t *testing.T) {
	f := newFixture(t)
	f.provisionCfg(t, "be-init-slow", manifest.Backend{
		Init:         [][]string{initArgv(t, "sleep")},
		InitTimeout:  manifest.Duration(200 * time.Millisecond),
		Run:          serverArgv(t),
		HealthPath:   "/api/health",
		StartTimeout: manifest.Duration(10 * time.Second),
	})

	_, err := f.m.EnsureRunning(context.Background(), BackendKey(f.repoID, "be-init-slow"), "demo")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want init timeout", err)
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

// waitStatus polls Status until want or the deadline.
func (f *fixture) waitStatus(t *testing.T, k Key, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.m.Status(k) != want {
		if time.Now().After(deadline) {
			t.Fatalf("status(%v) = %q, want %q", k, f.m.Status(k), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIdleReap(t *testing.T) {
	f := newFixture(t)
	f.m.reapInterval = 40 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.m.StartReaper(ctx)

	f.provisionIdle(t, "be-idle", serverArgv(t), 150*time.Millisecond)
	k := BackendKey(f.repoID, "be-idle")
	if _, err := f.m.EnsureRunning(context.Background(), k, "demo"); err != nil {
		t.Fatal(err)
	}
	// Untouched past its idle_timeout → the reaper stops it.
	f.waitStatus(t, k, "idle")
}

func TestLRUWarmCap(t *testing.T) {
	f := newFixture(t)
	f.m.reapInterval = 40 * time.Millisecond
	f.m.SetMaxWarm(1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.m.StartReaper(ctx)

	f.provision(t, "be-old", serverArgv(t))
	f.provision(t, "be-new", serverArgv(t))
	kOld := BackendKey(f.repoID, "be-old")
	kNew := BackendKey(f.repoID, "be-new")
	if _, err := f.m.EnsureRunning(context.Background(), kOld, "demo"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // distinct touch times
	if _, err := f.m.EnsureRunning(context.Background(), kNew, "demo"); err != nil {
		t.Fatal(err)
	}
	// Beyond max-warm 1 the least-recently-used backend stops; the newer
	// one survives.
	f.waitStatus(t, kOld, "idle")
	if got := f.m.Status(kNew); got != "running" {
		t.Fatalf("newer backend = %q, want running", got)
	}
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
	d1, err := f.db.CreateDeploy(f.repoID, c1, db.DeployMeta{Ref: "main"})
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
	if _, err := f.m.EnsureRunning(ctx, BackendKey(f.repoID, "be1"), "demo"); err != nil {
		t.Fatal(err)
	}

	// New backend hash at c2 forks be1's state.
	cfg, _ := json.Marshal(manifest.Backend{Run: serverArgv(t), HealthPath: "/api/health"})
	if err := f.m.ForkOrInitStateDir(ctx, repo, f.repoID, "demo", "be2", c2, string(cfg)); err != nil {
		t.Fatal(err)
	}
	if got := f.m.Status(BackendKey(f.repoID, "be1")); got != "idle" {
		t.Fatalf("ancestor status after fork = %q, want idle (quiesced)", got)
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
