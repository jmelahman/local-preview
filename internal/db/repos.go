package db

import (
	"errors"
	"strings"
)

// ErrConflict is returned when an insert violates a uniqueness constraint.
var ErrConflict = errors.New("already exists")

// Repo is a row in the repos table.
type Repo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"`
	BarePath string `json:"-"`
	// Watch marks the repo for polling: new branch tips deploy
	// automatically. WatchBranches narrows which branches (comma-separated
	// globs, empty = all).
	Watch         bool   `json:"watch"`
	WatchBranches string `json:"watch_branches"`
	CreatedAt     string `json:"created_at"`
}

const repoCols = `id, name, source, bare_path, watch, watch_branches, created_at`

func (r *Repo) scanFields() []any {
	return []any{&r.ID, &r.Name, &r.Source, &r.BarePath, &r.Watch, &r.WatchBranches, &r.CreatedAt}
}

// CreateRepo inserts a repo and returns it. Returns ErrConflict if the name
// is taken.
func (s *Store) CreateRepo(name, source, barePath string) (Repo, error) {
	var r Repo
	err := s.db.QueryRow(
		`INSERT INTO repos (name, source, bare_path) VALUES (?, ?, ?)
		 RETURNING `+repoCols,
		name, source, barePath,
	).Scan(r.scanFields()...)
	if isUniqueErr(err) {
		return Repo{}, ErrConflict
	}
	if err != nil {
		return Repo{}, err
	}
	return r, nil
}

// GetRepoByName returns the repo named name, or ErrNotFound.
func (s *Store) GetRepoByName(name string) (Repo, error) {
	var r Repo
	err := s.db.QueryRow(
		`SELECT `+repoCols+` FROM repos WHERE name = ?`, name,
	).Scan(r.scanFields()...)
	if err != nil {
		return Repo{}, mapNoRows(err)
	}
	return r, nil
}

// ListRepos returns all repos, oldest first.
func (s *Store) ListRepos() ([]Repo, error) {
	rows, err := s.db.Query(`SELECT ` + repoCols + ` FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []Repo{}
	for rows.Next() {
		var r Repo
		if err := rows.Scan(r.scanFields()...); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// SetRepoWatch updates a repo's watch settings and returns the updated row,
// or ErrNotFound.
func (s *Store) SetRepoWatch(id int64, watch bool, branches string) (Repo, error) {
	var r Repo
	err := s.db.QueryRow(
		`UPDATE repos SET watch = ?, watch_branches = ? WHERE id = ?
		 RETURNING `+repoCols,
		watch, branches, id,
	).Scan(r.scanFields()...)
	if err != nil {
		return Repo{}, mapNoRows(err)
	}
	return r, nil
}

// DeleteRepo removes a repo and every row that references it (deploys,
// branch aliases, artifacts, process bookkeeping) in one transaction.
func (s *Store) DeleteRepo(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM process_events WHERE repo_id = ?`,
		`DELETE FROM process_records WHERE repo_id = ?`,
		`DELETE FROM backend_artifacts WHERE repo_id = ?`,
		`DELETE FROM frontend_artifacts WHERE repo_id = ?`,
		`DELETE FROM branch_aliases WHERE repo_id = ?`,
		`DELETE FROM deploys WHERE repo_id = ?`,
		`DELETE FROM repos WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// isUniqueErr reports whether err is a SQLite uniqueness violation.
func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
