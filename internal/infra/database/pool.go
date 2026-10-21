// Package database opens the agent's pure-Go SQLite database (modernc.org/sqlite,
// CGO-free). There is no otelpgx-style automatic query tracing here; the repos
// wrap their calls in manual spans instead (see repositories/span.go).
package database

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open opens the SQLite file at path with WAL journalling, a busy timeout (so
// concurrent writers wait rather than error), and foreign keys on. The
// connection pool is capped at one open connection: the agent is low-volume and
// a single writer sidesteps SQLite's "database is locked" corner cases entirely.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return db, nil
}
