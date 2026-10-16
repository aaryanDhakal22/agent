package migrate

import (
	"context"
	"database/sql"
	"embed"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed sql/*.sql
var migrations embed.FS

// Run applies all pending up migrations against the SQLite file at path. Safe to
// call on every startup — goose tracks applied versions via goose_db_version and
// skips no-ops. The pure-Go modernc driver registers itself as "sqlite".
func Run(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.UpContext(ctx, db, "sql")
}

// RunTo migrates up to (and including) version — for tests that seed data in
// an older schema and then upgrade it.
func RunTo(ctx context.Context, path string, version int64) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()

	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}
	return goose.UpToContext(ctx, db, "sql", version)
}
