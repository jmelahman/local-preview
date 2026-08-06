package db

import (
	"errors"
	"strings"
)

// ErrConflict is returned when an insert violates a uniqueness constraint.
var ErrConflict = errors.New("already exists")

// Repo is a row in the repos table.
type Repo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	BarePath  string `json:"-"`
	CreatedAt string `json:"created_at"`
}

// CreateRepo inserts a repo and returns it. Returns ErrConflict if the name
// is taken.
func (s *Store) CreateRepo(name, source, barePath string) (Repo, error) {
	var r Repo
	err := s.db.QueryRow(
		`INSERT INTO repos (name, source, bare_path) VALUES (?, ?, ?)
		 RETURNING id, name, source, bare_path, created_at`,
		name, source, barePath,
	).Scan(&r.ID, &r.Name, &r.Source, &r.BarePath, &r.CreatedAt)
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
		`SELECT id, name, source, bare_path, created_at FROM repos WHERE name = ?`, name,
	).Scan(&r.ID, &r.Name, &r.Source, &r.BarePath, &r.CreatedAt)
	if err != nil {
		return Repo{}, mapNoRows(err)
	}
	return r, nil
}

// ListRepos returns all repos, oldest first.
func (s *Store) ListRepos() ([]Repo, error) {
	rows, err := s.db.Query(`SELECT id, name, source, bare_path, created_at FROM repos ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := []Repo{}
	for rows.Next() {
		var r Repo
		if err := rows.Scan(&r.ID, &r.Name, &r.Source, &r.BarePath, &r.CreatedAt); err != nil {
			return nil, err
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// isUniqueErr reports whether err is a SQLite uniqueness violation.
func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
