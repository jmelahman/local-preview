package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/jmelahman/local-preview/internal/db"
	"github.com/jmelahman/local-preview/internal/gitrepo"
	"github.com/jmelahman/local-preview/internal/store"
	"github.com/jmelahman/local-preview/internal/supervise"
)

type ExecutionSpec struct {
	RepoName          string                    `json:"repo"`
	Source            string                    `json:"source"`
	ProjectDir        string                    `json:"project_dir"`
	Ref               string                    `json:"ref"`
	Side              string                    `json:"side,omitempty"`
	Name              string                    `json:"name,omitempty"`
	Overwrite         bool                      `json:"overwrite"`
	Deploy            bool                      `json:"deploy"`
	ArtifactsOnly     bool                      `json:"artifacts_only,omitempty"`
	ExpectedArtifacts map[string]db.ArtifactRef `json:"expected_artifacts,omitempty"`
	Rebuild           bool                      `json:"rebuild,omitempty"`
	ManifestRefs      []ManifestRef             `json:"manifest_refs"`
	LocalManifest     []byte                    `json:"local_manifest,omitempty"`
}

type ExecutionResult struct {
	Upload    *UploadResult      `json:"upload,omitempty"`
	Deploy    *ExecutedDeploy    `json:"deploy,omitempty"`
	Artifacts *ExecutedArtifacts `json:"artifacts,omitempty"`
}

type ExecutedDeploy struct {
	JobID             string                    `json:"job_id,omitempty"`
	Rebuild           bool                      `json:"rebuild"`
	SHA               string                    `json:"sha"`
	Meta              db.DeployMeta             `json:"meta"`
	FrontendHash      string                    `json:"frontend_hash"`
	BackendHash       string                    `json:"backend_hash"`
	ProjectDir        string                    `json:"project_dir"`
	FrontendRunConfig string                    `json:"frontend_run_config,omitempty"`
	BackendRunConfig  string                    `json:"backend_run_config,omitempty"`
	Ancestors         []string                  `json:"ancestors"`
	Artifacts         map[string]db.ArtifactRef `json:"artifacts,omitempty"`
	Logs              map[string]string         `json:"logs,omitempty"`
}

type Executor struct {
	DataDir         string
	Runner          Runner
	MaxExtractBytes int64
}

func (e Executor) Execute(ctx context.Context, spec ExecutionSpec, archive io.Reader, tier store.ArtifactTier) (ExecutionResult, error) {
	if err := gitrepo.ValidateName(spec.RepoName); err != nil {
		return ExecutionResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(e.DataDir, "jobs"), 0o755); err != nil {
		return ExecutionResult{}, err
	}
	dir, err := os.MkdirTemp(filepath.Join(e.DataDir, "jobs"), "attempt-")
	if err != nil {
		return ExecutionResult{}, err
	}
	defer os.RemoveAll(dir)
	identity := sha256.Sum256([]byte(spec.Source))
	git := gitrepo.NewManager(filepath.Join(e.DataDir, "repos", hex.EncodeToString(identity[:16])))
	gr := git.Open(spec.RepoName)
	if _, err := os.Stat(gr.Path); errors.Is(err, os.ErrNotExist) {
		if gr, err = git.Add(ctx, spec.RepoName, spec.Source, nil); err != nil {
			return ExecutionResult{}, err
		}
	} else if err != nil {
		return ExecutionResult{}, err
	}
	database, err := db.Open(":memory:")
	if err != nil {
		return ExecutionResult{}, err
	}
	defer database.Close()
	repo, err := database.CreateRepoWithProjectDir(spec.RepoName, spec.Source, gr.Path, spec.ProjectDir, db.RepoReady)
	if err != nil {
		return ExecutionResult{}, err
	}
	files := store.New(filepath.Join(dir, "artifacts"), filepath.Join(dir, "state"), filepath.Join(dir, "tmp"))
	files.SetMaxExtractBytes(e.MaxExtractBytes)
	logsDir := filepath.Join(dir, "logs")
	super := supervise.New(database, files, logsDir)
	q := NewQueue(database, git, files, super, logsDir, e.Runner)
	q.SetArtifactTier(tier)
	q.SetAutoStart(false)
	q.SetManifestRefs(spec.ManifestRefs)
	if len(spec.LocalManifest) > 0 {
		manifestDir := filepath.Join(dir, "manifests")
		if err := os.MkdirAll(manifestDir, 0o755); err != nil {
			return ExecutionResult{}, err
		}
		if err := os.WriteFile(filepath.Join(manifestDir, spec.RepoName+".toml"), spec.LocalManifest, 0o600); err != nil {
			return ExecutionResult{}, err
		}
		q.SetLocalManifestDir(manifestDir)
	}
	var result ExecutionResult
	if spec.ArtifactsOnly {
		return q.executeArtifacts(ctx, spec)
	}
	ref := spec.Ref
	if spec.Side != "" {
		if archive == nil {
			return result, errors.New("upload archive is required")
		}
		upload, err := q.Upload(ctx, spec.RepoName, ref, spec.Side, spec.Name, archive, spec.Overwrite)
		if err != nil {
			return result, err
		}
		result.Upload = &upload
		ref = upload.SHA
		side := map[string]string{SideFrontend: "fe", SideBackend: "be", SideArtifact: "dl"}[upload.Side]
		if err := q.persistSideContext(ctx, spec.RepoName, side, upload.Hash); err != nil {
			return result, err
		}
	}
	if !spec.Deploy {
		return result, nil
	}
	_, gr, sha, err := q.resolveRepoRef(ctx, spec.RepoName, ref)
	if err != nil {
		return result, err
	}
	meta := commitMeta(ctx, gr, ref, sha)
	deploy, err := database.CreateDeploy(repo.ID, sha, meta)
	if err != nil {
		return result, err
	}
	row, err := database.GetDeployByID(deploy.ID)
	if err != nil {
		return result, err
	}
	if err := q.buildDeploy(ctx, row, spec.Rebuild); err != nil {
		return result, err
	}
	row, err = database.GetDeployByID(deploy.ID)
	if err != nil {
		return result, err
	}
	for _, side := range []struct{ name, hash string }{{"fe", row.FeHash}, {"be", row.BeHash}} {
		if side.hash == "" {
			continue
		}
		if err := q.persistSideContext(ctx, spec.RepoName, side.name, side.hash); err != nil {
			return result, err
		}
	}
	result.Deploy, err = executedDeploy(ctx, database, repo, row, gr, sha, meta, spec.Rebuild)
	return result, err
}

func executedDeploy(ctx context.Context, database *db.Store, repo db.Repo, row db.DeployRow, gr gitrepo.Repo, sha string, meta db.DeployMeta, rebuild bool) (*ExecutedDeploy, error) {
	var err error
	resolved := &ExecutedDeploy{SHA: sha, Meta: meta, FrontendHash: row.FeHash, BackendHash: row.BeHash, ProjectDir: row.ProjectDir, Artifacts: row.Artifacts, Rebuild: rebuild}
	if row.FeHash != "" {
		if artifact, err := database.GetFrontendArtifact(repo.ID, row.FeHash); err == nil {
			resolved.FrontendRunConfig = artifact.RunConfig
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, err
		}
	}
	if row.BeHash != "" {
		artifact, err := database.GetBackendArtifact(repo.ID, row.BeHash)
		if err != nil {
			return nil, err
		}
		resolved.BackendRunConfig = artifact.RunConfig
		resolved.Ancestors, err = gr.FirstParentAncestry(ctx, sha, supervise.AncestryLimit)
		if err != nil {
			return nil, err
		}
	}
	resolved.Logs = make(map[string]string)
	for kind, path := range map[string]string{"fe": row.FeBuildLogPath, "be": row.BeBuildLogPath} {
		if path != "" {
			resolved.Logs[kind], err = executionLog(path)
			if err != nil {
				return nil, err
			}
		}
	}
	return resolved, nil
}

func executionLog(path string) (string, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	const limit = 256 << 10
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > limit {
		if _, err := file.Seek(-limit, io.SeekEnd); err != nil {
			return "", err
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	return string(data), err
}

func (q *Queue) CompleteExecution(ctx context.Context, repo db.Repo, result ExecutedDeploy, createdBy string) (db.DeployRow, error) {
	result.Meta.CreatedBy = createdBy
	if current, err := q.db.GetDeployBySHA(repo.ID, result.SHA); err == nil {
		for name, artifact := range result.Artifacts {
			if prior, ok := current.Artifacts[name]; ok && prior.Hash == artifact.Hash && artifact.Status == db.ArtifactBuilding && prior.Status != db.ArtifactBuilding && (!result.Rebuild || current.IngestionJobID == result.JobID) {
				result.Artifacts[name] = prior
			}
		}
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.DeployRow{}, err
	}
	for kind, content := range result.Logs {
		hash := result.FrontendHash
		if kind == "be" {
			hash = result.BackendHash
		}
		if hash == "" {
			continue
		}
		file, err := openLog(q.logPath(repo.Name, kind, hash))
		if err != nil {
			return db.DeployRow{}, err
		}
		_, writeErr := io.WriteString(file, content)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			return db.DeployRow{}, err
		}
	}
	if result.FrontendRunConfig != "" {
		if err := q.db.CreateFrontendArtifact(db.FrontendArtifact{RepoID: repo.ID, FeHash: result.FrontendHash, RunConfig: result.FrontendRunConfig}); err != nil {
			return db.DeployRow{}, err
		}
	}
	if result.BackendHash != "" {
		if err := q.super.ForkOrInitStateFromAncestors(ctx, result.Ancestors, repo.ID, repo.Name, result.BackendHash, result.BackendRunConfig); err != nil {
			return db.DeployRow{}, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(result.Artifacts)) {
		artifact := result.Artifacts[name]
		artifact.LogPath = q.logPath(repo.Name, "dl", artifact.Hash)
		result.Artifacts[name] = artifact
	}
	row, err := q.db.FinalizeRemoteDeploy(repo.ID, db.RemoteDeploy{
		JobID: result.JobID,
		SHA:   result.SHA, Meta: result.Meta, FeHash: result.FrontendHash, BeHash: result.BackendHash,
		FeLog: q.logPath(repo.Name, "fe", result.FrontendHash), BeLog: q.logPath(repo.Name, "be", result.BackendHash),
		ProjectDir: result.ProjectDir, Artifacts: result.Artifacts,
	})
	if err == nil && q.autoStart {
		q.autoStartDeploy(ctx, row.ID)
	}
	return row, err
}
