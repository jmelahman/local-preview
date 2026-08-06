package db

// ProcessRecord is crash-recovery bookkeeping for a running backend process.
// The supervisor's in-memory state is authoritative while the orchestrator
// runs; rows left behind at startup identify an unclean exit.
type ProcessRecord struct {
	RepoID    int64
	BeHash    string
	PID       int
	PGID      int
	Port      int
	StartedAt string
}

// UpsertProcessRecord records a started process.
func (s *Store) UpsertProcessRecord(r ProcessRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO process_records (repo_id, be_hash, pid, pgid, port)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (repo_id, be_hash) DO UPDATE SET
		   pid = excluded.pid, pgid = excluded.pgid, port = excluded.port,
		   started_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')`,
		r.RepoID, r.BeHash, r.PID, r.PGID, r.Port)
	return err
}

// DeleteProcessRecord removes the record on clean stop.
func (s *Store) DeleteProcessRecord(repoID int64, beHash string) error {
	_, err := s.db.Exec(
		`DELETE FROM process_records WHERE repo_id = ? AND be_hash = ?`, repoID, beHash)
	return err
}

// ListProcessRecords returns all records (used at startup to reclaim
// orphans from an unclean exit).
func (s *Store) ListProcessRecords() ([]ProcessRecord, error) {
	rows, err := s.db.Query(
		`SELECT repo_id, be_hash, pid, pgid, port, started_at FROM process_records`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProcessRecord{}
	for rows.Next() {
		var r ProcessRecord
		if err := rows.Scan(&r.RepoID, &r.BeHash, &r.PID, &r.PGID, &r.Port, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddProcessEvent appends to the observability trail (start attempts,
// crashes, health timeouts, idle stops).
func (s *Store) AddProcessEvent(repoID int64, beHash, event, detail string) error {
	_, err := s.db.Exec(
		`INSERT INTO process_events (repo_id, be_hash, event, detail) VALUES (?, ?, ?, ?)`,
		repoID, beHash, event, detail)
	return err
}
