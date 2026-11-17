package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gittest"
	"github.com/jmelahman/local-preview/internal/proxy"
	"github.com/jmelahman/local-preview/internal/supervise"
)

type executionTier struct{ root string }

func (t executionTier) Save(ctx context.Context, repo, side, hash, source string) error {
	path := filepath.Join(t.root, repo, side, hash+".tar")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	output, err := exec.CommandContext(ctx, "tar", "-cf", path, "-C", source, ".").CombinedOutput()
	if err != nil {
		return fmt.Errorf("archive test artifact: %w: %s", err, output)
	}
	return nil
}

func (t executionTier) Open(_ context.Context, repo, side, hash string) (io.ReadCloser, int64, bool, error) {
	file, err := os.Open(filepath.Join(t.root, repo, side, hash+".tar"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	return file, 0, true, nil
}

func TestExecutionPublishesAndServesWithoutControlGit(t *testing.T) {
	ctx := context.Background()
	source := newFixtureRepo(t)
	appendArtifactSection(t, source)
	sha := gittest.Run(t, source, "rev-parse", "HEAD")
	tier := executionTier{root: t.TempDir()}
	runner := &countingRunner{delegate: HostRunner{}}
	executor := Executor{DataDir: t.TempDir(), Runner: runner, MaxExtractBytes: 64 << 20}
	spec := ExecutionSpec{RepoName: "demo", Source: source, Ref: sha, Side: SideFrontend, Deploy: true}
	result, err := executor.Execute(ctx, spec, bytes.NewReader(tarGzBytes(t, map[string]string{"index.html": "INGESTED-FRONTEND"})), tier)
	if err != nil {
		t.Fatal(err)
	}
	if result.Upload == nil || result.Deploy == nil || !result.Upload.Published || result.Upload.Hash != result.Deploy.FrontendHash || result.Deploy.SHA != sha {
		t.Fatalf("execution result = %+v", result)
	}
	if result.Deploy.BackendRunConfig == "" || result.Deploy.BackendHash == "" || result.Deploy.Artifacts["cli"].Status != db.ArtifactBuilding {
		t.Fatalf("deploy contracts = %+v", result.Deploy)
	}
	if runner.count() != 1 {
		t.Fatalf("executed %d build steps before preview readiness, want backend only", runner.count())
	}
	spec.Side = ""
	cached, err := executor.Execute(ctx, spec, nil, tier)
	if err != nil || cached.Deploy == nil || cached.Deploy.BackendHash != result.Deploy.BackendHash || runner.count() != 1 {
		t.Fatalf("cached execution = %+v, %v; builds=%d", cached, err, runner.count())
	}
	entries, err := os.ReadDir(filepath.Join(executor.DataDir, "jobs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("completed execution left scratch files: %v, %v", entries, err)
	}

	control := newEnv(t, source, func(q *Queue) { q.SetAutoStart(false); q.SetArtifactTier(tier) })
	repo, err := control.db.GetRepoByName("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(control.git.Open("demo").Path); err != nil {
		t.Fatal(err)
	}
	row, err := control.q.CompleteExecution(ctx, repo, *result.Deploy, "ci-actor")
	if err != nil || row.Status != db.DeployReady || row.CreatedBy != "ci-actor" {
		t.Fatalf("complete remote deploy = %+v, %v", row, err)
	}
	backend, err := control.db.GetBackendArtifact(repo.ID, row.BeHash)
	if err != nil || backend.RunConfig != result.Deploy.BackendRunConfig || !control.files.HasStateDir("demo", row.BeHash) {
		t.Fatalf("control backend metadata = %+v, %v", backend, err)
	}
	server := httptest.NewServer(proxy.New(control.db, control.files, supervise.LocalBackends{M: control.super}, "preview.localhost", http.NotFoundHandler()))
	defer server.Close()
	request := func(row db.DeployRow, path string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = row.ShortSHA + "-demo.preview.localhost"
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d %s, %v", path, response.StatusCode, body, err)
		}
		return string(body)
	}
	if got := request(row, "/"); got != "INGESTED-FRONTEND" {
		t.Fatalf("served frontend = %q", got)
	}
	if got := request(row, "/api/counter"); got != "1" {
		t.Fatalf("served backend counter = %q", got)
	}
	artifacts, err := executor.Execute(ctx, ExecutionSpec{RepoName: "demo", Source: source, Ref: sha, ArtifactsOnly: true, ExpectedArtifacts: result.Deploy.Artifacts}, nil, tier)
	if err != nil || artifacts.Artifacts == nil {
		t.Fatalf("post-ready artifact execution = %+v, %v", artifacts, err)
	}
	if err := control.q.CompleteArtifactExecution(repo, *artifacts.Artifacts); err != nil {
		t.Fatal(err)
	}
	if err := control.files.Hydrate(ctx, "demo", "dl", row.Artifacts["cli"].Hash); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(control.files.ArtifactDir("demo", row.Artifacts["cli"].Hash), "fixture-cli"))); got != "fixture-cli-v1" {
		t.Fatalf("downloadable artifact = %q", got)
	}
	if _, err := os.Stat(repo.BarePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control recreated its Git mirror: %v", err)
	}

	if err := os.WriteFile(filepath.Join(source, "backend", "version.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, source, "add", "backend/version.txt")
	gittest.Run(t, source, "commit", "-qm", "new backend")
	spec.Ref = gittest.Run(t, source, "rev-parse", "HEAD")
	next, err := executor.Execute(ctx, spec, nil, tier)
	if err != nil || next.Deploy == nil || next.Deploy.BackendHash == row.BeHash {
		t.Fatalf("next execution = %+v, %v", next, err)
	}
	nextRow, err := control.q.CompleteExecution(ctx, repo, *next.Deploy, "ci-actor")
	if err != nil {
		t.Fatal(err)
	}
	nextBackend, err := control.db.GetBackendArtifact(repo.ID, nextRow.BeHash)
	if err != nil || nextBackend.ForkedFrom != row.BeHash {
		t.Fatalf("remote ancestry state fork = %+v, %v", nextBackend, err)
	}
	if got := request(nextRow, "/api/counter"); got != "2" {
		t.Fatalf("forked backend counter = %q", got)
	}
}
