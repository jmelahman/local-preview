package watch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmelahman/local-preview/internal/build"
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

func commitFile(t *testing.T, dir, name, content, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", "-A")
	runTestGit(t, dir, "commit", "-qm", msg)
	return runTestGit(t, dir, "rev-parse", "HEAD")
}

// newWatchFixture builds a source repo with a main branch, registers it
// (watched, with the given branch filter), and returns everything a poll
// needs. The queue's workers are never started, so requested deploys stay
// queued — these tests assert on rows, not builds.
func newWatchFixture(t *testing.T, branches string) (*Watcher, db.Repo, string) {
	t.Helper()
	src := t.TempDir()
	runTestGit(t, src, "init", "-q", "-b", "main")
	commitFile(t, src, "a.txt", "one", "initial")

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
	super := supervise.New(database, files, filepath.Join(root, "logs"))
	queue := build.NewQueue(database, gitMgr, files, super, filepath.Join(root, "logs"), nil)

	gr, err := gitMgr.Add(context.Background(), "demo", src, nil)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepo("demo", src, gr.Path, db.RepoReady)
	if err != nil {
		t.Fatal(err)
	}
	if repo, err = database.SetRepoWatch(repo.ID, true, branches); err != nil {
		t.Fatal(err)
	}
	return New(database, gitMgr, queue, DefaultInterval), repo, src
}

// deploysByBranch maps branch → sha for every deploy row.
func deploysByBranch(t *testing.T, w *Watcher) map[string]string {
	t.Helper()
	rows, err := w.db.ListDeploys(db.DeployFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range rows {
		got[d.Branch] = d.SHA
	}
	return got
}

func TestPollDeploysNewBranchTips(t *testing.T) {
	w, _, src := newWatchFixture(t, "")
	ctx := context.Background()

	w.PollAll(ctx)
	first := deploysByBranch(t, w)
	if len(first) != 1 || first["main"] == "" {
		t.Fatalf("after first poll: %v", first)
	}

	// Unchanged tips must not re-deploy.
	w.PollAll(ctx)
	if rows, _ := w.db.ListDeploys(db.DeployFilter{}); len(rows) != 1 {
		t.Fatalf("re-poll created rows: %d", len(rows))
	}

	// A new commit on a new branch deploys once the poller sees it.
	runTestGit(t, src, "checkout", "-qb", "feature")
	sha := commitFile(t, src, "b.txt", "two", "feature work")
	w.PollAll(ctx)
	got := deploysByBranch(t, w)
	if got["feature"] != sha {
		t.Fatalf("feature tip not deployed: %v", got)
	}

	// Advancing an existing branch deploys the new tip alongside the old.
	sha2 := commitFile(t, src, "c.txt", "three", "more feature work")
	w.PollAll(ctx)
	rows, _ := w.db.ListDeploys(db.DeployFilter{})
	if len(rows) != 3 {
		t.Fatalf("deploy count = %d, want 3", len(rows))
	}
	found := false
	for _, d := range rows {
		found = found || d.SHA == sha2
	}
	if !found {
		t.Fatalf("advanced tip %s not deployed: %+v", sha2, rows)
	}
}

func TestPollEvictsDeletedBranchDeploys(t *testing.T) {
	w, repo, src := newWatchFixture(t, "")
	ctx := context.Background()

	// main plus an unmerged feature branch each get a (queued) deploy.
	runTestGit(t, src, "checkout", "-qb", "feature")
	featSHA := commitFile(t, src, "b.txt", "two", "feature work")
	runTestGit(t, src, "checkout", "-q", "main")
	mainSHA := runTestGit(t, src, "rev-parse", "HEAD")
	w.PollAll(ctx)
	if rows, _ := w.db.ListDeploys(db.DeployFilter{}); len(rows) != 2 {
		t.Fatalf("deploy count = %d, want 2", len(rows))
	}

	// Deleting the unmerged branch evicts its deploy on the next poll; the
	// main deploy is untouched because its commit is still a live tip.
	runTestGit(t, src, "branch", "-D", "feature")
	w.PollAll(ctx)

	feat, err := w.db.GetDeployBySHA(repo.ID, featSHA)
	if err != nil {
		t.Fatal(err)
	}
	if feat.Status != db.DeployEvicted {
		t.Fatalf("feature deploy status = %q, want %q", feat.Status, db.DeployEvicted)
	}
	main, err := w.db.GetDeployBySHA(repo.ID, mainSHA)
	if err != nil {
		t.Fatal(err)
	}
	if main.Status == db.DeployEvicted {
		t.Fatal("main deploy was evicted, want it left alone")
	}

	// Re-poll is idempotent: an already-evicted deploy is not re-processed and
	// no live deploy is disturbed.
	w.PollAll(ctx)
	if feat, _ = w.db.GetDeployBySHA(repo.ID, featSHA); feat.Status != db.DeployEvicted {
		t.Fatalf("after re-poll feature status = %q, want %q", feat.Status, db.DeployEvicted)
	}
}

func TestPollHonorsBranchFilter(t *testing.T) {
	w, _, src := newWatchFixture(t, "main,release/*")
	runTestGit(t, src, "branch", "release/1.0")
	runTestGit(t, src, "checkout", "-qb", "scratch")
	commitFile(t, src, "s.txt", "x", "scratch work")

	w.PollAll(context.Background())
	rows, _ := w.db.ListDeploys(db.DeployFilter{})
	// main and release/1.0 share a tip, so one deploy covers both; the
	// scratch tip is filtered out.
	if len(rows) != 1 {
		t.Fatalf("deploy count = %d, want 1: %+v", len(rows), rows)
	}
	if got := deploysByBranch(t, w); got["scratch"] != "" {
		t.Fatalf("filtered branch deployed: %v", got)
	}
}

func TestPollSkipsUnwatchedRepos(t *testing.T) {
	w, repo, _ := newWatchFixture(t, "")
	if _, err := w.db.SetRepoWatch(repo.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	w.PollAll(context.Background())
	if rows, _ := w.db.ListDeploys(db.DeployFilter{}); len(rows) != 0 {
		t.Fatalf("unwatched repo deployed: %d rows", len(rows))
	}
}

func TestMatchBranch(t *testing.T) {
	cases := []struct {
		patterns string
		branch   string
		want     bool
	}{
		{"", "anything", true},
		{"main", "main", true},
		{"main", "master", false},
		{"main,develop", "develop", true},
		{"release/*", "release/1.0", true},
		{"release/*", "release/1.0/hotfix", false}, // globs don't cross '/'
		{"release/*", "main", false},
		{" main , develop ", "develop", true},
	}
	for _, c := range cases {
		if got := MatchBranch(SplitPatterns(c.patterns), c.branch); got != c.want {
			t.Errorf("MatchBranch(%q, %q) = %v, want %v", c.patterns, c.branch, got, c.want)
		}
	}
}

func TestValidatePatterns(t *testing.T) {
	if got, err := ValidatePatterns(" main , release/* ,"); err != nil || got != "main,release/*" {
		t.Errorf("ValidatePatterns = %q, %v", got, err)
	}
	if got, err := ValidatePatterns(""); err != nil || got != "" {
		t.Errorf("ValidatePatterns(empty) = %q, %v", got, err)
	}
	if _, err := ValidatePatterns("release/["); err == nil {
		t.Error("bad pattern accepted")
	}
}
