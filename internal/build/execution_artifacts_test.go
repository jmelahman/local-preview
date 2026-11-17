package build

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gittest"
)

func TestArtifactFailureRunsOnlyAfterPreviewReadiness(t *testing.T) {
	ctx := context.Background()
	source := newFixtureRepo(t)
	appendArtifactSection(t, source)
	manifestPath := filepath.Join(source, "preview.toml")
	text := readFile(t, manifestPath)
	text = strings.Replace(text, "mkdir -p bin && echo fixture-cli-v1 > bin/fixture-cli && chmod +x bin/fixture-cli", "exit 23", 1)
	if err := os.WriteFile(manifestPath, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, source, "commit", "-qam", "failing downloadable artifact")
	sha := gittest.Run(t, source, "rev-parse", "HEAD")
	tier := executionTier{root: t.TempDir()}
	executor := Executor{DataDir: t.TempDir(), Runner: HostRunner{}, MaxExtractBytes: 64 << 20}
	preview, err := executor.Execute(ctx, ExecutionSpec{RepoName: "demo", Source: source, Ref: sha, Deploy: true}, nil, tier)
	if err != nil || preview.Deploy == nil || preview.Deploy.Artifacts["cli"].Status != db.ArtifactBuilding {
		t.Fatalf("preview was gated by the artifact failure: %+v, %v", preview, err)
	}
	control := newEnv(t, source, func(q *Queue) { q.SetAutoStart(false); q.SetArtifactTier(tier) })
	repo, err := control.db.GetRepoByName("demo")
	if err != nil {
		t.Fatal(err)
	}
	row, err := control.q.CompleteExecution(ctx, repo, *preview.Deploy, "ci")
	if err != nil || row.Status != db.DeployReady {
		t.Fatalf("preview completion = %+v, %v", row, err)
	}
	for _, side := range []string{"fe", "be"} {
		if err := os.RemoveAll(filepath.Join(tier.root, "demo", side)); err != nil {
			t.Fatal(err)
		}
	}
	runner := &countingRunner{delegate: HostRunner{}}
	executor.Runner = runner
	spec := ExecutionSpec{RepoName: "demo", Source: source, Ref: sha, ArtifactsOnly: true, ExpectedArtifacts: preview.Deploy.Artifacts}
	result, err := executor.Execute(ctx, spec, nil, tier)
	if err != nil || result.Artifacts == nil || result.Deploy != nil || result.Upload != nil || runner.count() != 1 {
		t.Fatalf("artifact-only execution = %+v, %v; steps=%d", result, err, runner.count())
	}
	artifact := result.Artifacts.Artifacts["cli"]
	if artifact.Status != db.ArtifactFailed || !strings.Contains(artifact.Error, "23") {
		t.Fatalf("downloadable failure = %+v", artifact)
	}
	if err := control.q.CompleteArtifactExecution(repo, *result.Artifacts); err != nil {
		t.Fatal(err)
	}
	current, err := control.db.GetDeployBySHA(repo.ID, sha)
	if err != nil || current.Status != db.DeployReady || current.Artifacts["cli"].Status != db.ArtifactFailed {
		t.Fatalf("artifact failure changed preview readiness: %+v, %v", current, err)
	}
	if got := readFile(t, current.Artifacts["cli"].LogPath); !strings.Contains(got, "exit 23") {
		t.Fatalf("artifact log not adopted: %q", got)
	}
	for _, mismatch := range []string{"hash", "name"} {
		t.Run(mismatch, func(t *testing.T) {
			bad := spec
			bad.ExpectedArtifacts = maps.Clone(spec.ExpectedArtifacts)
			if mismatch == "hash" {
				ref := bad.ExpectedArtifacts["cli"]
				ref.Hash = strings.Repeat("0", 64)
				bad.ExpectedArtifacts["cli"] = ref
			} else {
				bad.ExpectedArtifacts["unknown"] = bad.ExpectedArtifacts["cli"]
			}
			if _, err := executor.Execute(ctx, bad, nil, tier); err == nil || runner.count() != 1 {
				t.Fatalf("unmatched artifact spec executed: %v; steps=%d", err, runner.count())
			}
		})
	}
	badResult := *result.Artifacts
	badResult.Artifacts = maps.Clone(result.Artifacts.Artifacts)
	ref := badResult.Artifacts["cli"]
	ref.Hash = strings.Repeat("0", 64)
	badResult.Artifacts["cli"] = ref
	if err := control.q.CompleteArtifactExecution(repo, badResult); err == nil {
		t.Fatal("result for a changed artifact hash was accepted")
	}
}

func TestArtifactExecutionLogsHaveOneTotalBudget(t *testing.T) {
	refs := make(map[string]db.ArtifactRef)
	for _, name := range []string{"a", "b", "c", "d"} {
		path := filepath.Join(t.TempDir(), name+".log")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 300<<10)+"tail-"+name), 0o644); err != nil {
			t.Fatal(err)
		}
		refs[name] = db.ArtifactRef{Hash: name, LogPath: path}
	}
	logs, err := artifactExecutionLogs(refs)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for name, content := range logs {
		total += len(content)
		if !strings.HasSuffix(content, "tail-"+name) {
			t.Fatalf("log %q lost its tail", name)
		}
	}
	if len(logs) != len(refs) || total != 512<<10 {
		t.Fatalf("log output count=%d bytes=%d", len(logs), total)
	}
}

func TestArtifactDispatcherOwnsPostReadyAndRestartWork(t *testing.T) {
	ctx := context.Background()
	source := newFixtureRepo(t)
	appendArtifactSection(t, source)
	dispatched := make(chan int64, 2)
	runner := &countingRunner{delegate: HostRunner{}}
	control := newEnv(t, source, func(q *Queue) {
		q.SetAutoStart(false)
		q.runner = runner
		q.SetArtifactDispatcher(func(_ context.Context, row db.DeployRow) error { dispatched <- row.ID; return nil })
	})
	row := control.deployAndWait(t, "main")
	select {
	case id := <-dispatched:
		if id != row.ID {
			t.Fatalf("dispatched deploy %d, want %d", id, row.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-ready artifacts were not dispatched")
	}
	if row.Artifacts["cli"].Status != db.ArtifactBuilding || runner.count() != 2 {
		t.Fatalf("artifact ran on the control host: status=%s steps=%d", row.Artifacts["cli"].Status, runner.count())
	}
	control.q.process(ctx, row.ID)
	select {
	case id := <-dispatched:
		if id != row.ID {
			t.Fatalf("resumed deploy %d, want %d", id, row.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed artifacts were not dispatched")
	}
	if runner.count() != 2 {
		t.Fatal("resumed artifacts ran on the control host")
	}
}
