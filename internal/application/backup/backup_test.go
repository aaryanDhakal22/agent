package backup

import (
	"context"
	"path/filepath"
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
}
