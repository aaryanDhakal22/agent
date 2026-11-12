package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// S-7: sql/schema.sql (what sqlc reads) mirrors the goose migrations by
// hand. Replay the migrations, load schema.sql into another database and
// compare every table's columns and every index (order of columns in a
// table aside: ALTER TABLE adds at the end). Triggers live in the
// migrations only.
func TestSchemaMatchesMigrations(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	migrated := filepath.Join(dir, "migrated.db")
	if err := Run(ctx, migrated); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../sql/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(dir, "schema.db")
	db, err := sql.Open("sqlite", mirror)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("schema.sql: %v", err)
	}
	db.Close()
	a, b := describe(t, migrated), describe(t, mirror)
	for k, v := range a {
		if b[k] != v {
			t.Errorf("%s\n  migrations: %s\n  schema.sql: %s", k, v, b[k])
		}
	}
	for k, v := range b {
		if _, ok := a[k]; !ok {
			t.Errorf("%s only in schema.sql: %s", k, v)
		}
	}
}

// describe lists "table <name>" -> its sorted columns and "index <name>" ->
// its table, uniqueness, partial flag and columns.
func describe(t *testing.T, path string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	out := map[string]string{}
	rows, err := db.Query(`SELECT type, name, tbl_name FROM sqlite_master WHERE type IN ('table', 'index')
		AND name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	type obj struct{ typ, name, tbl string }
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.typ, &o.name, &o.tbl); err != nil {
			t.Fatal(err)
		}
		objs = append(objs, o)
	}
	rows.Close()
	for _, o := range objs {
		switch o.typ {
		case "table":
			cr, err := db.Query(fmt.Sprintf("PRAGMA table_info(%q)", o.name))
			if err != nil {
				t.Fatal(err)
			}
			var cols []string
			for cr.Next() {
				var cid, notnull, pk int
				var name, typ string
				var dflt sql.NullString
				if err := cr.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
					t.Fatal(err)
				}
				cols = append(cols, fmt.Sprintf("%s %s nn=%d pk=%d d=%s", name, strings.ToLower(typ), notnull, pk, dflt.String))
			}
			cr.Close()
			sort.Strings(cols)
			out["table "+o.name] = strings.Join(cols, "; ")
		case "index":
			var unique, partial int
			if err := db.QueryRow(`SELECT "unique", partial FROM pragma_index_list(?) WHERE name = ?`, o.tbl, o.name).Scan(&unique, &partial); err != nil {
				t.Fatal(err)
			}
			ir, err := db.Query(fmt.Sprintf("PRAGMA index_info(%q)", o.name))
			if err != nil {
				t.Fatal(err)
			}
			var cols []string
			for ir.Next() {
				var seq, cid int
				var name sql.NullString
				if err := ir.Scan(&seq, &cid, &name); err != nil {
					t.Fatal(err)
				}
				cols = append(cols, name.String)
			}
			ir.Close()
			out["index "+o.name] = fmt.Sprintf("on %s unique=%d partial=%d (%s)", o.tbl, unique, partial, strings.Join(cols, ", "))
		}
	}
	return out
}
