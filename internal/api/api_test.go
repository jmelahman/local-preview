package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/build"
	"github.com/jmelahman/local-preview/internal/config"
	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

const fixtureManifest = `
[frontend]
path  = "web"
build = [["sh", "-c", "mkdir -p dist && cp index.html dist/"]]
dist  = "dist"

[backend]
path        = "srv"
build       = [["true"]]
run         = ["./never-started"]
health_path = "/api/health"
`

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

// newSourceRepo builds a minimal deployable repo (trivial build commands).
func newSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"preview.toml":   fixtureManifest,
		"web/index.html": "<html>hi</html>",
		"srv/main.txt":   "backend-ish",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runTestGit(t, dir, "init", "-q", "-b", "main")
	runTestGit(t, dir, "add", "-A")
	runTestGit(t, dir, "commit", "-qm", "initial")
	return dir
}

func newTestMux(t *testing.T) (*http.ServeMux, string) {
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
	gitMgr := gitrepo.NewManager(filepath.Join(root, "repos"))
	super := supervise.New(database, files, filepath.Join(root, "logs"))
	t.Cleanup(super.StopAll)
	queue := build.NewQueue(database, gitMgr, files, super, filepath.Join(root, "logs"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue.Start(ctx, 1)

	return NewMux(Deps{
		Store:               database,
		Build:               BuildInfo{Version: "test"},
		Config:              config.Config{DataDir: root, PreviewDomain: "preview.localhost"},
		Git:                 gitMgr,
		Queue:               queue,
		Super:               super,
		GitHubWebhookSecret: testWebhookSecret,
		Addr:                ":8080",
	}), root
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHealth(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := doJSON(t, mux, "GET", "/api/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" || resp["version"] != "test" {
		t.Fatalf("unexpected body: %v", resp)
	}
}

func TestRepoEndpoints(t *testing.T) {
	mux, _ := newTestMux(t)
	src := newSourceRepo(t)

	rec := doJSON(t, mux, "POST", "/api/repos", `{"name":"demo","source":`+jsonQuote(src)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create repo: %d %s", rec.Code, rec.Body)
	}

	for body, want := range map[string]int{
		`{"name":"demo","source":` + jsonQuote(src) + `}`: http.StatusConflict,
		`{"name":"Bad.Name","source":"/x"}`:               http.StatusBadRequest,
		`{"name":"orphan","source":"/does/not/exist"}`:    http.StatusBadRequest,
		`{`: http.StatusBadRequest,
	} {
		if rec := doJSON(t, mux, "POST", "/api/repos", body); rec.Code != want {
			t.Errorf("POST %s: %d, want %d", body, rec.Code, want)
		}
	}

	rec = doJSON(t, mux, "GET", "/api/repos", "")
	var repos []db.Repo
	if err := json.Unmarshal(rec.Body.Bytes(), &repos); err != nil || len(repos) != 1 {
		t.Fatalf("list repos: %s (%v)", rec.Body, err)
	}
	if rec := doJSON(t, mux, "GET", "/api/repos/demo", ""); rec.Code != http.StatusOK {
		t.Fatalf("get repo: %d", rec.Code)
	}
	if rec := doJSON(t, mux, "GET", "/api/repos/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing repo: %d", rec.Code)
	}
}

func TestDeployEndpoints(t *testing.T) {
	mux, _ := newTestMux(t)
	src := newSourceRepo(t)
	if rec := doJSON(t, mux, "POST", "/api/repos", `{"name":"demo","source":`+jsonQuote(src)+`}`); rec.Code != 201 {
		t.Fatalf("create repo: %d %s", rec.Code, rec.Body)
	}

	rec := doJSON(t, mux, "POST", "/api/deploys", `{"repo":"demo","ref":"main"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create deploy: %d %s", rec.Code, rec.Body)
	}
	var d struct {
		ID          int64  `json:"id"`
		Status      string `json:"status"`
		PreviewURL  string `json:"preview_url"`
		ShortSHA    string `json:"short_sha"`
		Branch      string `json:"branch"`
		AuthorName  string `json:"author_name"`
		AuthorEmail string `json:"author_email"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Branch != "main" || d.AuthorName != "test" || d.AuthorEmail != "test@example.com" {
		t.Fatalf("commit metadata missing from response: %s", rec.Body)
	}

	deadline := time.Now().Add(30 * time.Second)
	for d.Status != "ready" {
		if d.Status == "failed" || time.Now().After(deadline) {
			logs := doJSON(t, mux, "GET", "/api/deploys/1/logs", "")
			t.Fatalf("deploy status = %s; logs:\n%s", d.Status, logs.Body)
		}
		time.Sleep(50 * time.Millisecond)
		rec = doJSON(t, mux, "GET", "/api/deploys/1", "")
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
	}
	wantURL := "http://" + d.ShortSHA + ".demo.preview.localhost:8080/"
	if d.PreviewURL != wantURL {
		t.Fatalf("preview_url = %q, want %q", d.PreviewURL, wantURL)
	}

	for query, want := range map[string]int{
		"?repo=demo":                         1,
		"?branch=main":                       1,
		"?author=test%40exam":                1,
		"?author=TEST":                       1,
		"?repo=demo&branch=main&author=test": 1,
		"?branch=other":                      0,
		"?author=someone-else":               0,
	} {
		rec = doJSON(t, mux, "GET", "/api/deploys"+query, "")
		var list []json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != want {
			t.Fatalf("list deploys %s: got %d (%v), want %d: %s", query, len(list), err, want, rec.Body)
		}
	}

	rec = doJSON(t, mux, "GET", "/api/deploys/1/logs", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "frontend build") {
		t.Fatalf("logs: %d %s", rec.Code, rec.Body)
	}

	if rec := doJSON(t, mux, "POST", "/api/deploys", `{"repo":"nope","ref":"main"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("deploy unknown repo: %d", rec.Code)
	}
	if rec := doJSON(t, mux, "POST", "/api/deploys", `{"repo":"demo","ref":"no-such-branch"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("deploy bad ref: %d", rec.Code)
	}
	if rec := doJSON(t, mux, "GET", "/api/deploys/99", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get missing deploy: %d", rec.Code)
	}
}

func TestDeleteRepo(t *testing.T) {
	mux, root := newTestMux(t)
	src := newSourceRepo(t)
	if rec := doJSON(t, mux, "POST", "/api/repos", `{"name":"demo","source":`+jsonQuote(src)+`}`); rec.Code != http.StatusCreated {
		t.Fatalf("create repo: %d %s", rec.Code, rec.Body)
	}
	// Build one deploy to ready so there are artifacts, state, and logs to
	// remove.
	if rec := doJSON(t, mux, "POST", "/api/deploys", `{"repo":"demo","ref":"main"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("create deploy: %d %s", rec.Code, rec.Body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		rec := doJSON(t, mux, "GET", "/api/deploys/1", "")
		var d struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		if d.Status == "ready" {
			break
		}
		if d.Status == "failed" || time.Now().After(deadline) {
			t.Fatalf("deploy status = %s", d.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if rec := doJSON(t, mux, "DELETE", "/api/repos/demo", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete repo: %d %s", rec.Code, rec.Body)
	}
	if rec := doJSON(t, mux, "GET", "/api/repos/demo", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get deleted repo: %d", rec.Code)
	}
	rec := doJSON(t, mux, "GET", "/api/deploys", "")
	var list []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 0 {
		t.Fatalf("deploys after delete: %s (%v)", rec.Body, err)
	}
	for _, dir := range []string{
		filepath.Join(root, "artifacts", "demo"),
		filepath.Join(root, "state", "demo"),
		filepath.Join(root, "logs", "demo"),
		filepath.Join(root, "repos", "demo.git"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s still exists after delete (err=%v)", dir, err)
		}
	}
	if rec := doJSON(t, mux, "DELETE", "/api/repos/demo", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing repo: %d", rec.Code)
	}
	// The name is immediately reusable.
	if rec := doJSON(t, mux, "POST", "/api/repos", `{"name":"demo","source":`+jsonQuote(src)+`}`); rec.Code != http.StatusCreated {
		t.Fatalf("re-create repo: %d %s", rec.Code, rec.Body)
	}
}

func TestRepoWatchEndpoints(t *testing.T) {
	mux, _ := newTestMux(t)
	src := newSourceRepo(t)

	rec := doJSON(t, mux, "POST", "/api/repos",
		`{"name":"demo","source":`+jsonQuote(src)+`,"watch":true,"watch_branches":"main"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create watched repo: %d %s", rec.Code, rec.Body)
	}
	var repo struct {
		Watch         bool   `json:"watch"`
		WatchBranches string `json:"watch_branches"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatal(err)
	}
	if !repo.Watch || repo.WatchBranches != "main" {
		t.Fatalf("created repo watch fields: %s", rec.Body)
	}

	// Disable watching without touching the stored branch filter.
	rec = doJSON(t, mux, "PATCH", "/api/repos/demo", `{"watch":false}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s (%v)", rec.Code, rec.Body, err)
	}
	if repo.Watch || repo.WatchBranches != "main" {
		t.Fatalf("watch off should keep branches: %s", rec.Body)
	}

	// Re-enable with a new filter; whitespace canonicalizes.
	rec = doJSON(t, mux, "PATCH", "/api/repos/demo", `{"watch":true,"watch_branches":" main , release/* "}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &repo); err != nil {
		t.Fatal(err)
	}
	if !repo.Watch || repo.WatchBranches != "main,release/*" {
		t.Fatalf("re-enable: %s", rec.Body)
	}

	for body, want := range map[string]int{
		`{"watch_branches":"bad["}`: http.StatusBadRequest,
		`{}`:                        http.StatusBadRequest,
		`{bad json`:                 http.StatusBadRequest,
	} {
		if rec := doJSON(t, mux, "PATCH", "/api/repos/demo", body); rec.Code != want {
			t.Errorf("PATCH %s: %d, want %d", body, rec.Code, want)
		}
	}
	if rec := doJSON(t, mux, "PATCH", "/api/repos/nope", `{"watch":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("patch missing repo: %d", rec.Code)
	}
	if rec := doJSON(t, mux, "POST", "/api/repos",
		`{"name":"bad","source":"/x","watch_branches":"oops["}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create with bad pattern: %d", rec.Code)
	}
}

func TestUnknownAPIPathIs404(t *testing.T) {
	mux, _ := newTestMux(t)
	rec := doJSON(t, mux, "GET", "/api/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// jsonQuote JSON-quotes a string (paths may contain characters needing
// escaping).
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
