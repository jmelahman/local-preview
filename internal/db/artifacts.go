package db

// BackendArtifact records a provisioned backend: its state dir and the run
// contract (backend section of preview.toml as JSON). Row existence means
// the state dir is fully provisioned.
type BackendArtifact struct {
	RepoID     int64
	BeHash     string
	ForkedFrom string
	StateDir   string
	RunConfig  string
	// InitDoneAt is empty until the manifest's init steps have succeeded
	// against this artifact's state dir (or forever, if none are declared).
	InitDoneAt string
	CreatedAt  string
}

// CreateBackendArtifact records a provisioned backend artifact. Idempotent:
// an existing row is left untouched.
func (s *Store) CreateBackendArtifact(a BackendArtifact) error {
	_, err := s.db.Exec(
		`INSERT INTO backend_artifacts (repo_id, be_hash, forked_from, state_dir, run_config)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, be_hash) DO NOTHING`,
		a.RepoID, a.BeHash, a.ForkedFrom, a.StateDir, a.RunConfig)
	return err
}

// UpdateBackendArtifactRunConfig refreshes the stored run contract for an
// already-provisioned artifact (run-time-only fields like `networks` don't
// feed the hash, so they land via update rather than a new row).
func (s *Store) UpdateBackendArtifactRunConfig(repoID int64, beHash, runConfig string) error {
	_, err := s.db.Exec(
		`UPDATE backend_artifacts SET run_config = ? WHERE repo_id = ? AND be_hash = ?`,
		runConfig, repoID, beHash)
	return err
}

// GetBackendArtifact returns the artifact row, or ErrNotFound.
func (s *Store) GetBackendArtifact(repoID int64, beHash string) (BackendArtifact, error) {
	var a BackendArtifact
	err := s.db.QueryRow(
		`SELECT repo_id, be_hash, forked_from, state_dir, run_config, init_done_at, created_at
		 FROM backend_artifacts WHERE repo_id = ? AND be_hash = ?`, repoID, beHash,
	).Scan(&a.RepoID, &a.BeHash, &a.ForkedFrom, &a.StateDir, &a.RunConfig, &a.InitDoneAt, &a.CreatedAt)
	if err != nil {
		return BackendArtifact{}, mapNoRows(err)
	}
	return a, nil
}

// MarkBackendInitDone records that the artifact's init steps completed
// successfully; the supervisor skips init on every later start.
func (s *Store) MarkBackendInitDone(repoID int64, beHash string) error {
	_, err := s.db.Exec(
		`UPDATE backend_artifacts
		 SET init_done_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
		 WHERE repo_id = ? AND be_hash = ?`, repoID, beHash)
	return err
}
