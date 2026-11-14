package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
)

type memSettings struct{ last string }

func (m *memSettings) GetLastBackup(context.Context) (string, error) { return m.last, nil }
func (m *memSettings) SetLastBackup(_ context.Context, at string) error {
	m.last = at
	return nil
}

func TestDue(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	st := &memSettings{}
	s := New(nil, "", nil, st, nil, func() *time.Location { return ny }, zerolog.Nop())
	ctx := context.Background()
	at := func(h, m int) time.Time { return time.Date(2026, 10, 17, h, m, 0, 0, ny) }
	if !s.due(ctx, at(1, 0)) {
		t.Fatal("never backed up → due")
	}
	st.last = at(4, 5).UTC().Format(time.RFC3339)
	if s.due(ctx, at(12, 0)) || s.due(ctx, at(23, 59)) {
		t.Fatal("already backed up today")
	}
	if s.due(ctx, at(4, 5).Add(23*time.Hour)) { // 03:05 next day, before the slot
		t.Fatal("not before 04:00")
	}
	if !s.due(ctx, at(4, 5).Add(24*time.Hour)) {
		t.Fatal("04:05 next day → due")
	}
	st.last = at(3, 0).Add(-27 * time.Hour).UTC().Format(time.RFC3339)
	if !s.due(ctx, at(3, 0)) {
		t.Fatal("more than 26 h old → due at any hour")
	}
	off := New(nil, "", nil, &memSettings{}, func() bool { return false }, nil, zerolog.Nop())
	if off.due(ctx, at(5, 0)) {
		t.Fatal("unconfigured → never due")
	}
}

func TestNowUploadsASQLiteFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got []byte
	st := &memSettings{}
	s := New(db, path, func(_ context.Context, p string, body []byte, ct string) (int, []byte, error) {
		if p != "/store/v1/agent/backup" || ct != "application/vnd.sqlite3" {
			t.Fatalf("upload %s %s", p, ct)
		}
		got = body
		return 201, []byte(`{}`), nil
	}, st, nil, nil, zerolog.Nop())
	tmp := t.TempDir()
	s.TempDir = tmp
	n, err := s.Now(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 || string(got[:16]) != "SQLite format 3\x00" {
		t.Fatalf("not a SQLite file (%d bytes)", n)
	}
	if st.last == "" {
		t.Fatal("last backup not recorded")
	}
	// The copy is made in the temp dir (RAM on the Pi), never next to the
	// database, and removed afterwards.
	for _, dir := range []string{filepath.Dir(path), tmp} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.Contains(e.Name(), "backup") {
				t.Fatalf("left %s in %s", e.Name(), dir)
			}
		}
	}
}

// F-7: no backup while orders are in play, nor around the auto-close minute.
func TestHeldWhenBusyOrAtAutoClose(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	busy := true
	s := New(nil, "", nil, &memSettings{}, nil, func() *time.Location { return ny }, zerolog.Nop())
	s.SetQuiet(func(context.Context) bool { return !busy })
	s.SetAvoid(func() (int, int, bool) { return 4, 0, true })
	ctx := context.Background()
	at := func(h, m int) time.Time { return time.Date(2026, 10, 17, h, m, 0, 0, ny) }
	if s.due(ctx, at(19, 0)) {
		t.Fatal("never backed up, but orders are in play: must wait")
	}
	busy = false
	if !s.due(ctx, at(19, 0)) {
		t.Fatal("quiet → due")
	}
	for _, m := range []int{0, 10} {
		if s.due(ctx, at(4, m)) {
			t.Fatalf("04:%02d is the auto-close's window", m)
		}
	}
	if s.due(ctx, at(3, 56)) {
		t.Fatal("03:56 is inside the 5 minutes before the auto-close")
	}
	if !s.due(ctx, at(4, 16)) {
		t.Fatal("04:16 → due")
	}
}

// F-7: the copy runs on its own read-only connection, so a write on the
// agent's one connection isn't held up while a big file is copied.
func TestBackupDoesNotBlockWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE pad(b BLOB)`); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 1<<20)
	for i := 0; i < 24; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO pad(b) VALUES (?)`, blob); err != nil {
			t.Fatal(err)
		}
	}
	usedRO := false
	s := New(db, path, func(context.Context, string, []byte, string) (int, []byte, error) { return 201, nil, nil },
		&memSettings{}, nil, nil, zerolog.Nop())
	s.TempDir = t.TempDir()
	s.OpenRO = func(p string) (*sql.DB, error) { usedRO = true; return openReadOnly(p) }
	defer s.Close()
	done := make(chan error, 1)
	go func() { _, err := s.Now(ctx); done <- err }()
	var worst time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		if _, err := db.ExecContext(ctx, `UPDATE settings SET id = id WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		worst = max(worst, time.Since(start))
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !usedRO {
		t.Fatal("the backup didn't use its own read-only connection")
	}
	// 200 ms on an idle box; a loaded gate or GitHub's shared runners (CI on master, 2026-11-14: 201.6 ms) get 1 s:
	// a backup that really blocked writes holds them for the whole copy (seconds), well past either.
	limit := 200 * time.Millisecond
	if os.Getenv("GATE_PARALLEL") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
		limit = time.Second
	}
	if worst > limit {
		t.Fatalf("a write waited %v during the backup (limit %v)", worst, limit)
	}
}
