package repositories

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/settingslog"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"
)

func TestSettingsLogAddListPrune(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r := NewSettingsLogRepository(db)
	now := time.Date(2026, 10, 29, 21, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	old := settingslog.Entry{At: now.Add(-settingslog.Keep - time.Hour), Section: "times",
		By: settingslog.By{Kind: settingslog.ByWeb, Name: "Pi web page"}, Changes: []settingslog.Change{{Key: "default_pickup_minutes", From: 15, To: 20}}}
	if err := r.Add(ctx, old); err != nil {
		t.Fatal(err)
	}
	add := func(at time.Time, section string, from, to bool) {
		t.Helper()
		if err := r.Add(ctx, settingslog.Entry{At: at, Section: section,
			By:      settingslog.By{Kind: settingslog.ByTablet, Name: "Counter", DeviceID: "dev-1"},
			Changes: []settingslog.Change{{Key: "online_card", From: from, To: to}}}); err != nil {
			t.Fatal(err)
		}
	}
	add(now.Add(-2*time.Hour), "tickets", true, false)
	add(now.Add(-time.Hour), "tickets", false, true)

	got, err := r.List(ctx, now.Add(-settingslog.Keep), 10)
	if err != nil {
		t.Fatal(err)
	}
	// The 91-day-old entry was pruned by the next Add; newest first.
	if len(got) != 2 || got[0].Changes[0].To != true || got[1].Changes[0].To != false {
		t.Fatalf("list: %+v", got)
	}
	if e := got[0]; e.Section != "tickets" || e.By != (settingslog.By{Kind: "tablet", Name: "Counter", DeviceID: "dev-1"}) ||
		!e.At.Equal(now.Add(-time.Hour)) || e.ID == 0 {
		t.Fatalf("entry: %+v", e)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings_log`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows left: %d %v", n, err)
	}
	if got, _ := r.List(ctx, now.Add(-settingslog.Keep), 1); len(got) != 1 || got[0].Changes[0].To != true {
		t.Fatalf("limit: %+v", got)
	}
}
