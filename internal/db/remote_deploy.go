package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type RemoteDeploy struct {
	SHA                                      string
	JobID                                    string
	Meta                                     DeployMeta
	FeHash, BeHash, FeLog, BeLog, ProjectDir string
	Artifacts                                map[string]ArtifactRef
}

func (s *Store) FinalizeRemoteDeploy(repoID int64, result RemoteDeploy) (DeployRow, error) {
	if len(result.SHA) < 7 {
		return DeployRow{}, errors.New("remote deploy commit SHA is too short")
	}
	artifacts := ""
	if len(result.Artifacts) > 0 {
		encoded, err := json.Marshal(result.Artifacts)
		if err != nil {
			return DeployRow{}, err
		}
		artifacts = string(encoded)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return DeployRow{}, err
	}
	defer tx.Rollback()
	var repoName string
	if err := tx.QueryRow(`SELECT name FROM repos WHERE id = ?`, repoID).Scan(&repoName); err != nil {
		return DeployRow{}, mapNoRows(err)
	}
	var id int64
	err = tx.QueryRow(`SELECT id FROM deploys WHERE repo_id = ? AND sha = ?`, repoID, result.SHA).Scan(&id)
	switch {
	case err == nil:
		_, err = tx.Exec(`UPDATE deploys SET fe_hash = ?, be_hash = ?, fe_build_log_path = ?,
			be_build_log_path = ?, project_dir = ?, artifacts = ?, status = 'ready', error = '',
			attempt_count = attempt_count + CASE WHEN ? != '' AND ingestion_job_id != ? THEN 1 ELSE 0 END,
			ingestion_job_id = CASE WHEN ? != '' THEN ? ELSE ingestion_job_id END,
			updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?`, result.FeHash,
			result.BeHash, result.FeLog, result.BeLog, result.ProjectDir, artifacts,
			result.JobID, result.JobID, result.JobID, result.JobID, id)
	case errors.Is(err, sql.ErrNoRows):
		attempts := 0
		if result.JobID != "" {
			attempts = 1
		}
		for n := 7; n <= len(result.SHA); n++ {
			err = tx.QueryRow(`INSERT INTO deploys (repo_id, sha, short_sha, ref, branch, author_name,
				author_email, created_by, fe_hash, be_hash, fe_build_log_path, be_build_log_path,
				project_dir, artifacts, ingestion_job_id, attempt_count, status)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready')
				RETURNING id`, repoID, result.SHA, result.SHA[:n], result.Meta.Ref, result.Meta.Branch,
				result.Meta.AuthorName, result.Meta.AuthorEmail, result.Meta.CreatedBy, result.FeHash,
				result.BeHash, result.FeLog, result.BeLog, result.ProjectDir, artifacts, result.JobID, attempts).Scan(&id)
			if err == nil || !isUniqueErr(err) {
				break
			}
		}
		if err != nil && isUniqueErr(err) {
			return DeployRow{}, fmt.Errorf("could not find a unique short sha for %s", result.SHA)
		}
	}
	if err != nil {
		return DeployRow{}, err
	}
	var hasFeProcess bool
	d, err := scanDeploy(tx.QueryRow(`SELECT `+deployColsD+`, `+hasFeProcessExpr+`
		FROM deploys d WHERE d.id = ?`, id), &hasFeProcess)
	if err != nil {
		return DeployRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeployRow{}, err
	}
	return DeployRow{Deploy: d, RepoName: repoName, HasFeProcess: hasFeProcess}, nil
}
