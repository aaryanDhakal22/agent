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

func TestCountOpenFloor(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	counts := func() map[string]int64 {
		t.Helper()
		rows, err := r.CountOpenFloor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int64{}
		for _, c := range rows {
			src := "online"
			if c.POS {
				src = "pos"
			}
			m[string(c.State)+"/"+src] += c.N
		}
		return m
	}
	if m := counts(); len(m) != 0 {
		t.Fatalf("empty batch: %v", m)
	}
	arrive(t, r, "pickup")
	accept(t, r, arrive(t, r, "delivery"))
	ready := accept(t, r, arrive(t, r, "pickup"))
	if _, err := r.MarkReady(ctx, ready.Order.ID); err != nil {
		t.Fatal(err)
	}
	future := arrive(t, r, "pickup")
	if _, err := r.Schedule(ctx, future.Order.ID, 20, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	upsertPOS(t, r, "1", posOrder("delivery", "a"))
	m := counts()
	want := map[string]int64{"arrived/online": 1, "accepted/online": 1, "ready/online": 1, "scheduled/online": 1, "accepted/pos": 1}
	if len(m) != len(want) {
		t.Fatalf("got %v, want %v", m, want)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("got %v, want %v", m, want)
		}
	}
	// Close batch: the leftovers are cancelled, the scheduled order moves on.
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if m := counts(); len(m) != 1 || m["scheduled/online"] != 1 {
		t.Fatalf("after close: %v", m)
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

	if at, err := s.GetUpdateLastSuccess(ctx); err != nil || !at.IsZero() {
		t.Fatalf("fresh last success %v %v", at, err)
	}
	ok := time.Date(2026, 10, 26, 3, 4, 5, 0, time.UTC)
	if err := s.SetUpdateLastSuccess(ctx, ok); err != nil {
		t.Fatal(err)
	}
	if at, err := s.GetUpdateLastSuccess(ctx); err != nil || !at.Equal(ok) {
		t.Fatalf("last success round trip %v %v", at, err)
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
