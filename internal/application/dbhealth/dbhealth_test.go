package dbhealth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
	_ "modernc.org/sqlite"
)

func TestClassifyText(t *testing.T) {
	for in, want := range map[string]string{
		"attempt to write a readonly database (8)": ReadOnly,
		"database or disk is full":                 Full,
		"disk I/O error":                           IOErr,
		"no such table: x":                         "",
	} {
		if got := Classify(errors.New(in)); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// F-15: a database that can't be written (opened read-only here, as after an
// ext4 read-only remount) raises the sticky problem and pages once; repeats
// within the hour don't page again; a good probe clears it.
func TestReadOnlyDBRaisesOneAlert(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var pages atomic.Int32
	m := New(func(context.Context, string, string) error { pages.Add(1); return nil }, nil, zerolog.Nop())
	m.Probe(ctx, ro)
	m.Probe(ctx, ro)
	Note(ctx, errors.New("attempt to write a readonly database"))
	if st := m.State(); st.Trouble != ReadOnly || st.Since == nil {
		t.Fatalf("state %+v", st)
	}
	deadline := time.Now().Add(2 * time.Second)
	for pages.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if pages.Load() != 1 {
		t.Fatalf("pages = %d, want 1", pages.Load())
	}
	rw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	m.Probe(ctx, rw)
	if m.Trouble() != "" {
		t.Fatalf("not cleared: %+v", m.State())
	}
}
