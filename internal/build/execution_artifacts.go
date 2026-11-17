package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/jmelahman/local-preview/internal/db"
)

type ExecutedArtifacts struct {
	SHA       string                    `json:"sha"`
	Artifacts map[string]db.ArtifactRef `json:"artifacts"`
	Logs      map[string]string         `json:"logs,omitempty"`
}

func (q *Queue) executeArtifacts(ctx context.Context, spec ExecutionSpec) (ExecutionResult, error) {
	if spec.Side != "" || spec.Deploy || len(spec.ExpectedArtifacts) == 0 {
		return ExecutionResult{}, errors.New("artifact execution requires expected artifacts and cannot upload or deploy a preview")
	}
	repo, gr, sha, err := q.resolveRepoRef(ctx, spec.RepoName, spec.Ref)
	if err != nil {
		return ExecutionResult{}, err
	}
	deploy, err := q.db.CreateDeploy(repo.ID, sha, commitMeta(ctx, gr, spec.Ref, sha))
	if err != nil {
		return ExecutionResult{}, err
	}
	row, err := q.db.GetDeployByID(deploy.ID)
	if err != nil {
		return ExecutionResult{}, err
	}
	m, projectDir, err := q.loadManifest(ctx, gr, row)
	if err != nil {
		return ExecutionResult{}, err
	}
	env, entries, err := q.hashInputs(ctx, gr, sha, m, projectDir)
	if err != nil {
		return ExecutionResult{}, err
	}
	refs := maps.Clone(spec.ExpectedArtifacts)
	for _, name := range slices.Sorted(maps.Keys(refs)) {
		declared, ok := m.Artifacts[name]
		if !ok {
			return ExecutionResult{}, fmt.Errorf("artifact %q is not declared at %s", name, sha)
		}
		hash, err := artHashOf(m, declared, env, entries)
		if err != nil {
			return ExecutionResult{}, err
		}
		ref := refs[name]
		if ref.Hash != hash {
			return ExecutionResult{}, fmt.Errorf("artifact %q hash changed from %s to %s", name, ref.Hash, hash)
		}
		ref.LogPath = q.logPath(spec.RepoName, "dl", hash)
		refs[name] = ref
	}
	if err := q.db.SetDeployArtifacts(row.ID, refs); err != nil {
		return ExecutionResult{}, err
	}
	row.Artifacts = refs
	q.buildArtifacts(ctx, row, spec.Rebuild)
	if err := ctx.Err(); err != nil {
		return ExecutionResult{}, err
	}
	row, err = q.db.GetDeployByID(row.ID)
	if err != nil {
		return ExecutionResult{}, err
	}
	for _, name := range slices.Sorted(maps.Keys(row.Artifacts)) {
		ref := row.Artifacts[name]
		if ref.Status == db.ArtifactBuilding {
			return ExecutionResult{}, fmt.Errorf("artifact %q did not finish", name)
		}
		if ref.Status != db.ArtifactReady && ref.Status != "" {
			continue
		}
		if !q.files.HasArtifact(spec.RepoName, ref.Hash) {
			q.hydrate(ctx, spec.RepoName, "dl", ref.Hash)
		}
		if !q.files.HasArtifact(spec.RepoName, ref.Hash) {
			return ExecutionResult{}, fmt.Errorf("artifact %q is not available for publication", name)
		}
		if err := q.persistSideContext(ctx, spec.RepoName, "dl", ref.Hash); err != nil {
			return ExecutionResult{}, err
		}
		if ref.Status == "" {
			ref.Status = db.ArtifactReady
			row.Artifacts[name] = ref
		}
	}
	logs, err := artifactExecutionLogs(row.Artifacts)
	if err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{Artifacts: &ExecutedArtifacts{SHA: sha, Artifacts: row.Artifacts, Logs: logs}}, nil
}

func artifactExecutionLogs(refs map[string]db.ArtifactRef) (map[string]string, error) {
	logs := make(map[string]string, len(refs))
	if len(refs) == 0 {
		return logs, nil
	}
	const totalLimit = 512 << 10
	limit := int64(totalLimit / len(refs))
	for _, name := range slices.Sorted(maps.Keys(refs)) {
		file, err := os.Open(refs[name].LogPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		if info.Size() > limit {
			if _, err := file.Seek(-limit, io.SeekEnd); err != nil {
				file.Close()
				return nil, err
			}
		}
		data, readErr := io.ReadAll(io.LimitReader(file, limit))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return nil, err
		}
		logs[name] = string(data)
	}
	return logs, nil
}

func (q *Queue) CompleteArtifactExecution(repo db.Repo, result ExecutedArtifacts) error {
	row, err := q.db.GetDeployBySHA(repo.ID, result.SHA)
	if err != nil {
		return err
	}
	if row.Status != db.DeployReady {
		return errors.New("artifact execution belongs to a deploy that is not ready")
	}
	for name, ref := range result.Artifacts {
		current, ok := row.Artifacts[name]
		if !ok || current.Hash != ref.Hash {
			return fmt.Errorf("artifact %q no longer matches the deploy", name)
		}
		if ref.Status != db.ArtifactReady && ref.Status != db.ArtifactFailed {
			return fmt.Errorf("artifact %q result is not terminal", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(result.Artifacts)) {
		ref := result.Artifacts[name]
		current := row.Artifacts[name]
		current.LogPath = q.logPath(repo.Name, "dl", ref.Hash)
		current.Status = ref.Status
		current.Error = ref.Error
		if ref.LogPath != "" {
			current.Error = strings.ReplaceAll(current.Error, ref.LogPath, current.LogPath)
		}
		if content, ok := result.Logs[name]; ok {
			file, err := openLog(current.LogPath)
			if err != nil {
				return err
			}
			_, writeErr := io.WriteString(file, content)
			if err := errors.Join(writeErr, file.Close()); err != nil {
				return err
			}
		}
		row.Artifacts[name] = current
	}
	return q.db.SetDeployArtifacts(row.ID, row.Artifacts)
}
