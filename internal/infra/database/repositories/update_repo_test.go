package repositories

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
)

func TestCountOpenBatchOrders(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	if total, active, err := r.CountOpenBatchOrders(ctx); err != nil || total != 0 || active != 0 {
		t.Fatalf("empty batch: %d %d %v", total, active, err)
	}
	arrive(t, r, "pickup")
	accept(t, r, arrive(t, r, "delivery"))
	if total, active, _ := r.CountOpenBatchOrders(ctx); total != 2 || active != 2 {
		t.Fatalf("got %d %d", total, active)
	}
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Right after Close batch: the new open batch is empty.
	if total, active, _ := r.CountOpenBatchOrders(ctx); total != 0 || active != 0 {
		t.Fatalf("after close: %d %d", total, active)
	}
}

func TestUpdateStateAndPrinterMACs(t *testing.T) {
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
	s := NewSettingsRepository(db, zerolog.Nop())
	if j, n, err := s.GetUpdateState(ctx); err != nil || j != "" || n != "" {
		t.Fatalf("fresh %q %q %v", j, n, err)
	}
	_ = s.SetUpdateJob(ctx, `{"id":"x"}`)
	_ = s.SetUpdateNotified(ctx, "x:ok")
	if j, n, _ := s.GetUpdateState(ctx); j != `{"id":"x"}` || n != "x:ok" {
		t.Fatalf("round trip %q %q", j, n)
	}

	p := NewPrinterNetRepository(db)
	at := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC)
	if err := p.SaveMAC(ctx, printer.NetAddress{PrinterID: "p1", MAC: "00:11:62:aa:bb:cc", IP: "192.168.1.40", SeenAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveMAC(ctx, printer.NetAddress{PrinterID: "p1", MAC: "00:11:62:aa:bb:cc", IP: "192.168.1.47", SeenAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m, err := p.ListMACs(ctx)
	if err != nil || len(m) != 1 || m["p1"].IP != "192.168.1.47" || !m["p1"].SeenAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("got %+v %v", m, err)
	}
}
