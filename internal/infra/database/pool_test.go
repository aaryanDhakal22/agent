package database

import (
	"context"
	"path/filepath"
	"testing"
)

// S-7: the DSN sets synchronous FULL, and quick_check answers ok on a good file.
func TestPragmasAndQuickCheck(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n, err := Synchronous(ctx, db); err != nil || n != 2 {
		t.Fatalf("synchronous = %d (%v), want 2 (FULL)", n, err)
	}
	if _, err := db.Exec(`CREATE TABLE t (a integer)`); err != nil {
		t.Fatal(err)
	}
	if bad, err := QuickCheck(ctx, db); err != nil || bad != "" {
		t.Fatalf("quick_check: %q %v", bad, err)
	}
}
