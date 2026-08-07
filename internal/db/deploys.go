package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Deploy build-outcome statuses. Process runtime status is a separate,
// supervisor-owned concept merged into API views at read time.
const (
	DeployQueued   = "queued"
	DeployBuilding = "building"
	DeployReady    = "ready"
	DeployFailed   = "failed"
	DeployEvicted  = "evicted"
)

// ArtifactRef locates one named downloadable artifact of a deploy: its
// content hash and build log path. Stored as JSON in the deploys.artifacts
// column, keyed by artifact name.
type ArtifactRef struct {
	Hash    string `json:"hash"`
	LogPath string `json:"log_path"`
}

// Deploy is a row in the deploys table.
type Deploy struct {
	ID             int64                  `json:"id"`
	RepoID         int64                  `json:"-"`
	SHA            string                 `json:"sha"`
	ShortSHA       string                 `json:"short_sha"`
	Ref            string                 `json:"ref,omitempty"`
	Branch         string                 `json:"branch,omitempty"`
	AuthorName     string                 `json:"author_name,omitempty"`
	AuthorEmail    string                 `json:"author_email,omitempty"`
	FeHash         string                 `json:"fe_hash,omitempty"`
	BeHash         string                 `json:"be_hash,omitempty"`
	Artifacts      map[string]ArtifactRef `json:"-"`
	Status         string                 `json:"status"`
	Error          string                 `json:"error,omitempty"`
	AttemptCount   int64                  `json:"attempt_count"`
	FeBuildLogPath string                 `json:"-"`
	BeBuildLogPath string                 `json:"-"`
	CreatedAt      string                 `json:"created_at"`
	UpdatedAt      string                 `json:"updated_at"`
}

// DeployRow is a deploy joined with its repo name.
type DeployRow struct {
	Deploy
	RepoName string `json:"repo"`
}

// deployCols is the scan-ordered column list; deployColsD is the same list
// qualified for joins (repos shares column names like created_at).
const deployCols = `id, repo_id, sha, short_sha, ref, branch, author_name, ` +
	`author_email, fe_hash, be_hash, artifacts, status, error, attempt_count, ` +
	`fe_build_log_path, be_build_log_path, created_at, updated_at`

var deployColsD = "d." + strings.ReplaceAll(deployCols, ", ", ", d.")

func scanDeploy(row interface{ Scan(...any) error }, extra ...any) (Deploy, error) {
	var d Deploy
	var artifacts string
	dest := []any{&d.ID, &d.RepoID, &d.SHA, &d.ShortSHA, &d.Ref, &d.Branch,
		&d.AuthorName, &d.AuthorEmail, &d.FeHash, &d.BeHash, &artifacts,
		&d.Status, &d.Error, &d.AttemptCount, &d.FeBuildLogPath, &d.BeBuildLogPath,
		&d.CreatedAt, &d.UpdatedAt}
	dest = append(dest, extra...)
	if err := row.Scan(dest...); err != nil {
		return Deploy{}, err
	}
	if artifacts != "" {
		if err := json.Unmarshal([]byte(artifacts), &d.Artifacts); err != nil {
			return Deploy{}, fmt.Errorf("deploy %d: parse artifacts: %w", d.ID, err)
		}
	}
	return d, nil
}

// DeployMeta is the commit metadata captured when a deploy is created. All
// fields are optional; Branch and the author fields are best-effort.
type DeployMeta struct {
	Ref         string
	Branch      string
	AuthorName  string
	AuthorEmail string
}

// CreateDeploy inserts a queued deploy, computing the shortest sha prefix
// (>=7 chars) unique among the repo's deploys. The UNIQUE(repo_id,
// short_sha) constraint backstops races: on collision the prefix grows.
func (s *Store) CreateDeploy(repoID int64, sha string, meta DeployMeta) (Deploy, error) {
	for n := 7; n <= len(sha); n++ {
		d, err := s.insertDeploy(repoID, sha, sha[:n], meta)
		if err == nil {
			return d, nil
		}
		if !isUniqueErr(err) {
			return Deploy{}, err
		}
		// Same sha already deployed → caller should have checked; surface
		// as conflict. Short-sha collision with a different sha → grow.
		if _, gerr := s.GetDeployBySHA(repoID, sha); gerr == nil {
			return Deploy{}, ErrConflict
		}
	}
	return Deploy{}, fmt.Errorf("could not find a unique short sha for %s", sha)
}

func (s *Store) insertDeploy(repoID int64, sha, shortSHA string, meta DeployMeta) (Deploy, error) {
	return scanDeploy(s.db.QueryRow(
		`INSERT INTO deploys (repo_id, sha, short_sha, ref, branch, author_name, author_email)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 RETURNING `+deployCols,
		repoID, sha, shortSHA, meta.Ref, meta.Branch, meta.AuthorName, meta.AuthorEmail))
}

// GetDeployBySHA returns the deploy for (repo, sha), or ErrNotFound.
func (s *Store) GetDeployBySHA(repoID int64, sha string) (Deploy, error) {
	d, err := scanDeploy(s.db.QueryRow(
		`SELECT `+deployColsD+` FROM deploys d WHERE d.repo_id = ? AND d.sha = ?`, repoID, sha))
	if err != nil {
		return Deploy{}, mapNoRows(err)
	}
	return d, nil
}

// GetDeployByID returns a deploy joined with its repo name, or ErrNotFound.
func (s *Store) GetDeployByID(id int64) (DeployRow, error) {
	var name string
	d, err := scanDeploy(s.db.QueryRow(
		`SELECT `+deployColsD+`, r.name FROM deploys d
		 JOIN repos r ON r.id = d.repo_id WHERE d.id = ?`, id), &name)
	if err != nil {
		return DeployRow{}, mapNoRows(err)
	}
	return DeployRow{Deploy: d, RepoName: name}, nil
}

// DeployFilter narrows ListDeploys; zero-value fields don't filter.
type DeployFilter struct {
	// Repo and Branch match exactly.
	Repo   string
	Branch string
	// Author is a case-insensitive substring of the author name or email.
	Author string
	// Status matches the build status exactly (queued/building/ready/...).
	Status string
	// Query is a free-text search: a case-insensitive prefix of the commit
	// sha, or a case-insensitive substring of the repo name, branch, ref,
	// author name, or author email.
	Query string
}

// IsDeployStatus reports whether s is one of the deploy build statuses.
func IsDeployStatus(s string) bool {
	switch s {
	case DeployQueued, DeployBuilding, DeployReady, DeployFailed, DeployEvicted:
		return true
	}
	return false
}

// ListDeploys returns deploys newest first, narrowed by the filter.
func (s *Store) ListDeploys(f DeployFilter) ([]DeployRow, error) {
	q := `SELECT ` + deployColsD + `, r.name FROM deploys d JOIN repos r ON r.id = d.repo_id`
	where := []string{}
	args := []any{}
	if f.Repo != "" {
		where = append(where, `r.name = ?`)
		args = append(args, f.Repo)
	}
	if f.Branch != "" {
		where = append(where, `d.branch = ?`)
		args = append(args, f.Branch)
	}
	if f.Author != "" {
		// instr instead of LIKE so % and _ in the query aren't wildcards.
		where = append(where,
			`(instr(lower(d.author_name), lower(?)) > 0 OR instr(lower(d.author_email), lower(?)) > 0)`)
		args = append(args, f.Author, f.Author)
	}
	if f.Status != "" {
		where = append(where, `d.status = ?`)
		args = append(args, f.Status)
	}
	if f.Query != "" {
		// instr = 1 is a wildcard-safe prefix match (shas are stored
		// lowercase); short_sha needs no clause of its own, being a prefix of
		// sha itself.
		where = append(where, `(instr(d.sha, lower(?)) = 1
			OR instr(lower(d.branch), lower(?)) > 0
			OR instr(lower(d.ref), lower(?)) > 0
			OR instr(lower(r.name), lower(?)) > 0
			OR instr(lower(d.author_name), lower(?)) > 0
			OR instr(lower(d.author_email), lower(?)) > 0)`)
		args = append(args, f.Query, f.Query, f.Query, f.Query, f.Query, f.Query)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY d.id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployRow{}
	for rows.Next() {
		var name string
		d, err := scanDeploy(rows, &name)
		if err != nil {
			return nil, err
		}
		out = append(out, DeployRow{Deploy: d, RepoName: name})
	}
	return out, rows.Err()
}

var hexRE = regexp.MustCompile(`^[0-9a-f]+$`)

// DeploysBySHAPrefix returns the repo's deploys whose sha starts with
// prefix. The prefix must be lowercase hex (routing labels are validated
// before they reach the database).
func (s *Store) DeploysBySHAPrefix(repoID int64, prefix string) ([]Deploy, error) {
	if !hexRE.MatchString(prefix) {
		return nil, fmt.Errorf("invalid sha prefix %q", prefix)
	}
	rows, err := s.db.Query(
		`SELECT `+deployColsD+` FROM deploys d
		 WHERE d.repo_id = ? AND d.sha LIKE ? || '%' ORDER BY d.id DESC`, repoID, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deploy{}
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListUnfinishedDeployIDs returns deploys stuck in queued/building — used
// at startup to resume work interrupted by a shutdown mid-build.
func (s *Store) ListUnfinishedDeployIDs() ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT id FROM deploys WHERE status IN (?, ?) ORDER BY id`,
		DeployQueued, DeployBuilding)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResetDeploy re-queues a failed or evicted deploy for another attempt.
func (s *Store) ResetDeploy(id int64) error {
	return s.updateDeploy(id,
		`status = ?, error = '', attempt_count = attempt_count + 1`, DeployQueued)
}

// SetDeployBuilding marks the deploy as building.
func (s *Store) SetDeployBuilding(id int64) error {
	return s.updateDeploy(id, `status = ?`, DeployBuilding)
}

// SetDeployHashes records the computed partition hashes and log paths as
// soon as they're known — visible even if the build later fails.
func (s *Store) SetDeployHashes(id int64, feHash, beHash, feLog, beLog string) error {
	return s.updateDeploy(id,
		`fe_hash = ?, be_hash = ?, fe_build_log_path = ?, be_build_log_path = ?`,
		feHash, beHash, feLog, beLog)
}

// SetDeployArtifacts records the deploy's named downloadable artifacts —
// like the partition hashes, written as soon as they're known so they're
// visible even if the build later fails. An empty map clears the column.
func (s *Store) SetDeployArtifacts(id int64, refs map[string]ArtifactRef) error {
	encoded := ""
	if len(refs) > 0 {
		b, err := json.Marshal(refs)
		if err != nil {
			return err
		}
		encoded = string(b)
	}
	return s.updateDeploy(id, `artifacts = ?`, encoded)
}

// SetDeployReady marks the deploy ready to serve.
func (s *Store) SetDeployReady(id int64) error {
	return s.updateDeploy(id, `status = ?, error = ''`, DeployReady)
}

// SetDeployFailed marks the deploy failed with a short error summary (full
// detail lives in the build logs).
func (s *Store) SetDeployFailed(id int64, msg string) error {
	return s.updateDeploy(id, `status = ?, error = ?`, DeployFailed, msg)
}

// SetDeployEvicted marks a deploy whose artifacts were garbage-collected.
func (s *Store) SetDeployEvicted(id int64) error {
	return s.updateDeploy(id, `status = ?`, DeployEvicted)
}

// DeleteDeploy removes a single deploy row and any branch alias pointing at
// it. Artifact and process bookkeeping keyed by hash is shared across
// deploys, so it is reclaimed separately by the caller once a hash is fully
// orphaned (see supervise.Manager.GCDeploy).
func (s *Store) DeleteDeploy(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM branch_aliases WHERE deploy_id = ?`,
		`DELETE FROM deploys WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) updateDeploy(id int64, set string, args ...any) error {
	args = append(args, id)
	res, err := s.db.Exec(
		`UPDATE deploys SET `+set+`, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = ?`,
		args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// mapNoRows converts sql.ErrNoRows into the package's ErrNotFound.
func mapNoRows(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
