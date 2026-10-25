package backup

import (
	"context"
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

// POS orders' customers never leave the Pi: the uploaded copy has no name,
// phone or address for a local-only order (online orders are untouched).
func TestNowScrubsPOSCustomers(t *testing.T) {
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
	for _, q := range []string{
		`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id, local_only)
		 VALUES ('pos-1', 't', 'l', 'pos', 'softtouch:1', '{"id":"pos-1","customer":{"display_name":"Pat Secret","phone":"4105550199"},"fulfillment":{"type":"delivery","address":{"line1":"77 Hidden Ln"}},"money":{"total_cents":1430}}', 'accepted', 1, true)`,
		`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id)
		 VALUES ('web-1', 't', 'l', 'online', '1', '{"id":"web-1","customer":{"display_name":"Erik Davis"},"fulfillment":{"type":"delivery","address":{"line1":"12 Main St"}}}', 'accepted', 1)`,
		`INSERT INTO order_geo (order_id, state, lat, lng, updated_at) VALUES ('pos-1', 'located', 39.4, -76.6, 'x')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var got []byte
	s := New(db, path, func(_ context.Context, _ string, body []byte, _ string) (int, []byte, error) {
		got = body
		return 201, nil, nil
	}, &memSettings{}, nil, nil, zerolog.Nop())
	s.TempDir = t.TempDir()
	if _, err := s.Now(ctx); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Pat Secret", "4105550199", "77 Hidden Ln"} {
		if strings.Contains(string(got), secret) {
			t.Fatalf("backup still holds %q", secret)
		}
	}
	if !strings.Contains(string(got), "Erik Davis") || !strings.Contains(string(got), "1430") {
		t.Fatal("scrub touched more than the POS customer")
	}
	// The live database keeps it (wiped at Close batch, not by the backup).
	var snap string
	_ = db.QueryRowContext(ctx, `SELECT snapshot FROM orders WHERE id = 'pos-1'`).Scan(&snap)
	if !strings.Contains(snap, "Pat Secret") {
		t.Fatal("the live order was scrubbed")
	}
}

// The hub feed and the AUTO plans hold POS customer data too: the copy has
// no POS address.confirmed event, no POS customer in an event payload and no
// plan (online orders' events stay).
func TestNowScrubsPOSFeedAndPlans(t *testing.T) {
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
	for _, q := range []string{
		`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id, local_only)
		 VALUES ('pos-1', 't', 'l', 'pos', 'softtouch:1', '{"id":"pos-1"}', 'accepted', 1, true)`,
		`INSERT INTO integration_events (type, payload, created_at) VALUES ('address.confirmed',
		 '{"order_id":"pos-1","ext_ref":"1","how":"candidate","address":{"line1":"88 Secret Rd","city":"Towson"},"lat":39.4111111,"lng":-76.6111111}', 'x')`,
		`INSERT INTO integration_events (type, payload, created_at) VALUES ('address.confirmed',
		 '{"order_id":"pos-1","ext_ref":null,"how":"pin","address":null,"lat":39.4222222,"lng":-76.6222222}', 'x')`,
		`INSERT INTO integration_events (type, payload, created_at) VALUES ('address.confirmed',
		 '{"order_id":"web-1","ext_ref":null,"how":"pin","address":null,"lat":39.4333333,"lng":-76.6333333}', 'x')`,
		`INSERT INTO integration_events (type, payload, created_at) VALUES ('order.accepted',
		 '{"order":{"order":{"id":"pos-2","source":"pos","customer":{"display_name":"Pat Secret","phone":"4105550199"},"fulfillment":{"type":"delivery","address":{"line1":"77 Hidden Ln"}}},"status":{"state":"accepted","ext_source":"softtouch","local_only":true},"geo":{"state":"located","lat":39.4444444}}}', 'x')`,
		`INSERT INTO integration_events (type, payload, created_at) VALUES ('order.accepted',
		 '{"order":{"order":{"id":"web-2","source":"online","customer":{"display_name":"Erik Davis"}},"status":{"state":"accepted"}}}', 'x')`,
		`INSERT INTO auto_plans (id, created_at, plan_json) VALUES ('plan-1', 'x', '{"runs":[{"stops":[{"order_id":"pos-1","lat":39.4555555,"lng":-76.6555555,"address":"99 Plan St"}]}]}')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var got []byte
	s := New(db, path, func(_ context.Context, _ string, body []byte, _ string) (int, []byte, error) {
		got = body
		return 201, nil, nil
	}, &memSettings{}, nil, nil, zerolog.Nop())
	s.TempDir = t.TempDir()
	if _, err := s.Now(ctx); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"88 Secret Rd", "39.4111111", "39.4222222", "Pat Secret", "4105550199", "77 Hidden Ln",
		"39.4444444", "39.4555555", "99 Plan St"} {
		if strings.Contains(string(got), secret) {
			t.Fatalf("backup still holds %q", secret)
		}
	}
	for _, kept := range []string{"39.4333333", "Erik Davis"} {
		if !strings.Contains(string(got), kept) {
			t.Fatalf("scrub removed online data %q", kept)
		}
	}
	// The copy is still a readable database with the online events in it.
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(copyPath, got, 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err := database.Open(ctx, copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var events, plans int
	_ = cp.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_events`).Scan(&events)
	_ = cp.QueryRowContext(ctx, `SELECT COUNT(*) FROM auto_plans`).Scan(&plans)
	if events != 3 || plans != 0 {
		t.Fatalf("copy has %d events, %d plans; want 3, 0", events, plans)
	}
	// The live database keeps everything.
	var live int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM integration_events`).Scan(&live)
	if live != 5 {
		t.Fatalf("live feed touched: %d events", live)
	}
}
