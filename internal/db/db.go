// Package db wraps the SQLite store. The schema lives in schema.sql and is
// applied idempotently on every Open.
package db

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is a handle to the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and applies
// the schema. Pass ":memory:" for an ephemeral in-memory database.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		// Shared cache so the in-memory DB survives across pooled
		// connections; combined with MaxOpenConns(1) below it behaves like a
		// single persistent connection.
		dsn = "file::memory:?cache=shared"
	}
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc.org/sqlite serializes writes; a single connection avoids
	// SQLITE_BUSY under concurrent handlers at the cost of throughput this
	// application doesn't need.
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(schema); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: sqlDB}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}
