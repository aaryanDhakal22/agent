// Package database opens the agent's pure-Go SQLite database (modernc.org/sqlite,
// CGO-free). There is no otelpgx-style automatic query tracing here; the repos
// wrap their calls in manual spans instead (see repositories/span.go).
package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens the SQLite file at path with WAL journalling, a busy timeout (so
// concurrent writers wait rather than error), and foreign keys on. The
// connection pool is capped at one open connection: the agent is low-volume and
// a single writer sidesteps SQLite's "database is locked" corner cases entirely.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		// synchronous(FULL) on purpose (S-7): it is SQLite's default, set
		// here so nobody "optimises" it to NORMAL - a power cut on the SD
		// card must not lose a committed payment.
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=synchronous(FULL)",
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

// QuickCheck runs PRAGMA quick_check (S-7, at start): "" = ok, else what
// SQLite reported (the first lines).
func QuickCheck(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var out []string
	for rows.Next() && len(out) < 5 {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		if line != "ok" {
			out = append(out, line)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(out, "; "), nil
}

// Synchronous is the connection's PRAGMA synchronous (2 = FULL).
func Synchronous(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&n)
	return n, err
}
