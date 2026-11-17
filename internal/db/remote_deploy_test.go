package db

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestFinalizeRemoteDeployRecoversOnlyCompleteRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := jobTestRepo(t, s)
	for _, event := range []string{"INSERT", "UPDATE"} {
		if _, err := s.db.Exec(`CREATE TRIGGER reject_remote_` + event + ` BEFORE ` + event + ` ON deploys
			WHEN NEW.fe_hash = 'rejected' BEGIN SELECT RAISE(ABORT, 'injected metadata failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	sha := strings.Repeat("a", 40)
	if _, err := s.FinalizeRemoteDeploy(repo.ID, RemoteDeploy{SHA: sha, FeHash: "rejected", BeHash: "be"}); err == nil {
		t.Fatal("injected metadata failure was ignored")
	}
	if _, err := s.GetDeployBySHA(repo.ID, sha); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed finalization left a deploy: %v", err)
	}
	want := RemoteDeploy{SHA: sha, Meta: DeployMeta{Ref: sha, Branch: "main", CreatedBy: "ci"}, FeHash: "fe", BeHash: "be", FeLog: "/logs/fe", BeLog: "/logs/be", ProjectDir: "app", Artifacts: map[string]ArtifactRef{"cli": {Hash: "artifact", Status: ArtifactReady}}}
	if _, err := s.FinalizeRemoteDeploy(repo.ID, want); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	unfinished, err := s.ListUnfinishedDeployIDs()
	if err != nil || len(unfinished) != 0 {
		t.Fatalf("restart resumed remote builds: %v, %v", unfinished, err)
	}
	d, err := s.GetDeployBySHA(repo.ID, sha)
	if err != nil || d.Status != DeployReady || d.FeHash != want.FeHash || d.BeHash != want.BeHash || d.ProjectDir != want.ProjectDir || d.Artifacts["cli"].Hash != "artifact" {
		t.Fatalf("recovered remote deploy = %+v, %v", d, err)
	}
}

func TestFinalizeRemoteDeployCollisionAndIdempotency(t *testing.T) {
	s := newTestStore(t)
	repo := jobTestRepo(t, s)
	firstSHA := "abcdefa1" + strings.Repeat("a", 32)
	secondSHA := "abcdefa2" + strings.Repeat("b", 32)
	first, err := s.CreateDeploy(repo.ID, firstSHA, DeployMeta{Ref: "original", Branch: "original-branch", AuthorName: "original-author", CreatedBy: "original-actor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBackendArtifact(BackendArtifact{RepoID: repo.ID, BeHash: "backend", StateDir: "/state", RunConfig: `{}`}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBackendInitDone(repo.ID, "backend"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFrontendArtifact(FrontendArtifact{RepoID: repo.ID, FeHash: "frontend", RunConfig: `{}`}); err != nil {
		t.Fatal(err)
	}
	result := RemoteDeploy{SHA: secondSHA, Meta: DeployMeta{CreatedBy: "remote-actor"}, FeHash: "frontend", BeHash: "backend", FeLog: "/fe", BeLog: "/be", Artifacts: map[string]ArtifactRef{"cli": {Hash: "artifact", Status: ArtifactReady}}}
	second, err := s.FinalizeRemoteDeploy(repo.ID, result)
	if err != nil || second.ShortSHA != secondSHA[:8] || second.Status != DeployReady || !second.HasFeProcess || second.RepoName != repo.Name {
		t.Fatalf("colliding remote SHA = %+v, %v", second, err)
	}
	if again, err := s.FinalizeRemoteDeploy(repo.ID, result); err != nil || again.ID != second.ID || again.CreatedBy != second.CreatedBy {
		t.Fatalf("duplicate finalization = %+v, %v", again, err)
	}
	result.SHA = firstSHA
	result.Meta = DeployMeta{Ref: "new-ref", Branch: "new-branch", AuthorName: "new-author", CreatedBy: "new-actor"}
	updated, err := s.FinalizeRemoteDeploy(repo.ID, result)
	if err != nil || updated.ID != first.ID || updated.Ref != "original" || updated.Branch != "original-branch" || updated.AuthorName != "original-author" || updated.CreatedBy != "original-actor" || updated.FeHash != "frontend" || updated.BeHash != "backend" {
		t.Fatalf("existing deploy metadata = %+v, %v", updated, err)
	}
	artifact, err := s.GetBackendArtifact(repo.ID, "backend")
	if err != nil || artifact.InitDoneAt == "" {
		t.Fatalf("finalization reset backend initialization: %+v, %v", artifact, err)
	}
}

func TestRemoteDeployGenerationIsDurableAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "generations.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := jobTestRepo(t, s)
	result := RemoteDeploy{SHA: strings.Repeat("b", 40), JobID: "first-job", FeHash: "first-frontend", BeHash: "backend"}
	first, err := s.FinalizeRemoteDeploy(repo.ID, result)
	if err != nil || first.AttemptCount != 1 || first.IngestionJobID != result.JobID {
		t.Fatalf("first generation = %+v, %v", first, err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_generation BEFORE UPDATE ON deploys
		WHEN NEW.fe_hash = 'rejected' BEGIN SELECT RAISE(ABORT, 'injected generation failure'); END`); err != nil {
		t.Fatal(err)
	}
	result.JobID, result.FeHash = "second-job", "rejected"
	if _, err := s.FinalizeRemoteDeploy(repo.ID, result); err == nil {
		t.Fatal("injected finalization failure ignored")
	}
	unchanged, err := s.GetDeployBySHA(repo.ID, result.SHA)
	if err != nil || unchanged.AttemptCount != 1 || unchanged.IngestionJobID != "first-job" || unchanged.FeHash != "first-frontend" {
		t.Fatalf("failed finalization changed generation = %+v, %v", unchanged, err)
	}
	result.FeHash = "second-frontend"
	second, err := s.FinalizeRemoteDeploy(repo.ID, result)
	if err != nil || second.AttemptCount != 2 || second.IngestionJobID != "second-job" {
		t.Fatalf("new generation = %+v, %v", second, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for range 2 {
		replayed, err := s.FinalizeRemoteDeploy(repo.ID, result)
		if err != nil || replayed.AttemptCount != 2 || replayed.IngestionJobID != "second-job" {
			t.Fatalf("replayed generation = %+v, %v", replayed, err)
		}
	}
	result.JobID = ""
	legacy, err := s.FinalizeRemoteDeploy(repo.ID, result)
	if err != nil || legacy.AttemptCount != 2 || legacy.IngestionJobID != "second-job" {
		t.Fatalf("legacy finalization changed generation = %+v, %v", legacy, err)
	}
}

func TestOpenMigratesRemoteDeployGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo := jobTestRepo(t, s)
	if _, err := s.db.Exec(`ALTER TABLE deploys DROP COLUMN ingestion_job_id`); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("c", 40)
	if _, err := s.db.Exec(`INSERT INTO deploys (repo_id, sha, short_sha) VALUES (?, ?, ?)`, repo.ID, sha, sha[:7]); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	legacy, err := s.GetDeployBySHA(repo.ID, sha)
	if err != nil || legacy.IngestionJobID != "" {
		t.Fatalf("legacy generation = %+v, %v", legacy, err)
	}
	current, err := s.FinalizeRemoteDeploy(repo.ID, RemoteDeploy{SHA: sha, JobID: "first-remote-job"})
	if err != nil || current.IngestionJobID != "first-remote-job" || current.AttemptCount != 1 {
		t.Fatalf("migrated generation = %+v, %v", current, err)
	}
}
