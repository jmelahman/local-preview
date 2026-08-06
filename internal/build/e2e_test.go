package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

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

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// newFixtureRepo copies testdata/fixture-repo into a temp dir and commits it.
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	copyTree(t, "testdata/fixture-repo", dir)
	runTestGit(t, dir, "init", "-q", "-b", "main")
	runTestGit(t, dir, "add", "-A")
	runTestGit(t, dir, "commit", "-qm", "initial")
	return dir
}

type env struct {
	q      *Queue
	db     *db.Store
	files  *store.Store
	super  *supervise.Manager
	git    *gitrepo.Manager
	repoID int64
}

func newEnv(t *testing.T, srcRepo string) *env {
	t.Helper()
	ctx := context.Background()
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
	gitMgr := gitrepo.NewManager(filepath.Join(root, "repos"))
	repo, err := gitMgr.Add(ctx, "demo", srcRepo)
	if err != nil {
		t.Fatal(err)
	}
	dbRepo, err := database.CreateRepo("demo", srcRepo, repo.Path)
	if err != nil {
		t.Fatal(err)
	}
	super := supervise.New(database, files, filepath.Join(root, "logs"))
	t.Cleanup(super.StopAll)
	q := NewQueue(database, gitMgr, files, super, filepath.Join(root, "logs"), nil)
	qctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	q.Start(qctx, 2)
	return &env{q: q, db: database, files: files, super: super, git: gitMgr, repoID: dbRepo.ID}
}

// deployAndWait requests a deploy and polls until it reaches a terminal
// status.
func (e *env) deployAndWait(t *testing.T, ref string) db.DeployRow {
	t.Helper()
	row, err := e.q.RequestDeploy(context.Background(), "demo", ref, false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := e.db.GetDeployByID(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch got.Status {
		case db.DeployReady:
			return got
		case db.DeployFailed:
			log := readLogTail(t, got.FeBuildLogPath) + readLogTail(t, got.BeBuildLogPath)
			t.Fatalf("deploy failed: %s\n%s", got.Error, log)
		}
		if time.Now().After(deadline) {
			t.Fatalf("deploy did not finish; status=%s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readLogTail(t *testing.T, path string) string {
	t.Helper()
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

func httpGet(t *testing.T, port int, path string) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func TestVerticalSlice(t *testing.T) {
	src := newFixtureRepo(t)
	e := newEnv(t, src)
	ctx := context.Background()

	// Commit A, deployed by branch name.
	a := e.deployAndWait(t, "main")
	if a.FeHash == "" || a.BeHash == "" || a.Ref != "main" {
		t.Fatalf("deploy A: %+v", a)
	}
	if !e.files.HasFrontend("demo", a.FeHash) || !e.files.HasBackend("demo", a.BeHash) {
		t.Fatal("artifacts not published")
	}
	if b, err := os.ReadFile(filepath.Join(e.files.FrontendDir("demo", a.FeHash), "index.html")); err != nil || !strings.Contains(string(b), "fixture frontend v1") {
		t.Fatalf("frontend artifact content: %v %q", err, b)
	}

	portA, err := e.super.EnsureRunning(ctx, e.repoID, "demo", a.BeHash)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpGet(t, portA, "/api/counter"); got != "1" {
		t.Fatalf("counter = %s, want 1", got)
	}

	// Commit B: frontend-only change → same backend hash, same process.
	if err := os.WriteFile(filepath.Join(src, "web", "src", "index.html"), []byte("<html>v2</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, src, "commit", "-qam", "fe change")
	shaB := runTestGit(t, src, "rev-parse", "HEAD")

	b := e.deployAndWait(t, shaB)
	if b.BeHash != a.BeHash {
		t.Fatalf("frontend-only commit changed be_hash: %s → %s", a.BeHash, b.BeHash)
	}
	if b.FeHash == a.FeHash {
		t.Fatal("frontend change did not change fe_hash")
	}
	portB, err := e.super.EnsureRunning(ctx, e.repoID, "demo", b.BeHash)
	if err != nil {
		t.Fatal(err)
	}
	if portB != portA {
		t.Fatalf("frontend-only deploy restarted the backend (port %d → %d)", portA, portB)
	}

	// Commit C: backend-only change → new hash, new process, forked state.
	mainGo := filepath.Join(src, "backend", "main.go")
	code, err := os.ReadFile(mainGo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainGo, []byte(strings.Replace(string(code), `"ok"`, `"ok-v2"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, src, "commit", "-qam", "be change")
	shaC := runTestGit(t, src, "rev-parse", "HEAD")

	c := e.deployAndWait(t, shaC)
	if c.FeHash != b.FeHash {
		t.Fatalf("backend-only commit changed fe_hash")
	}
	if c.BeHash == b.BeHash {
		t.Fatal("backend change did not change be_hash")
	}
	art, err := e.db.GetBackendArtifact(e.repoID, c.BeHash)
	if err != nil || art.ForkedFrom != a.BeHash {
		t.Fatalf("lineage: artifact = %+v, %v (want forked_from %s)", art, err, a.BeHash)
	}

	portC, err := e.super.EnsureRunning(ctx, e.repoID, "demo", c.BeHash)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpGet(t, portC, "/api/counter"); got != "2" {
		t.Fatalf("forked counter = %s, want 2 (state continuity from commit A)", got)
	}
	if got := httpGet(t, portC, "/api/health"); got != "ok-v2" {
		t.Fatalf("health = %q, want new backend code", got)
	}

	// Redeploy of the same sha is idempotent (no rebuild).
	again, err := e.q.RequestDeploy(ctx, "demo", shaC, false)
	if err != nil || again.ID != c.ID || again.Status != db.DeployReady {
		t.Fatalf("redeploy: %+v, %v", again, err)
	}
}

func TestFailedBuildSurfacesInLog(t *testing.T) {
	src := newFixtureRepo(t)
	// Break the backend build.
	toml, err := os.ReadFile(filepath.Join(src, "preview.toml"))
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(toml),
		`[["go", "build", "-o", "bin/fixture-server", "."]]`,
		`[["sh", "-c", "echo compile exploded >&2; exit 1"]]`, 1)
	if err := os.WriteFile(filepath.Join(src, "preview.toml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, src, "commit", "-qam", "break build")

	e := newEnv(t, src)
	row, err := e.q.RequestDeploy(context.Background(), "demo", "main", false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	var got db.DeployRow
	for {
		got, err = e.db.GetDeployByID(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == db.DeployFailed {
			break
		}
		if got.Status == db.DeployReady || time.Now().After(deadline) {
			t.Fatalf("expected failure, status=%s", got.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(got.Error, "backend build") {
		t.Fatalf("error = %q", got.Error)
	}
	logContent, err := os.ReadFile(got.BeBuildLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logContent), "compile exploded") {
		t.Fatalf("stderr missing from log: %s", logContent)
	}

	// A failed deploy can be re-requested (reset + attempt_count bump).
	again, err := e.q.RequestDeploy(context.Background(), "demo", got.SHA, false)
	if err != nil {
		t.Fatal(err)
	}
	if again.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1", again.AttemptCount)
	}
}

func TestRequestDeployUnknownRepo(t *testing.T) {
	src := newFixtureRepo(t)
	e := newEnv(t, src)
	_, err := e.q.RequestDeploy(context.Background(), "nope", "main", false)
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
