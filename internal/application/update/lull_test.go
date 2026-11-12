package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/migrate"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// TestForcedUpdateRunsInALull (00019): mid-shift the open batch keeps ready
// pickups and deliveries. Once the online pickup is finished (N minutes
// after ready) and the delivery is delivered, the gate's open count is 0 and
// a forced update is handed to the updater - no hand-written request.
func TestForcedUpdateRunsInALull(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := repositories.NewOrderRepository(db, zerolog.Nop())

	// An online pickup, accepted and ready.
	snap := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "test",
		SourceEventID: "o1", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "pickup"}}
	if _, err := repo.UpsertArrived(ctx, snap); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ClaimAccept(ctx, uuid.MustParse(snap.ID), 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordAccepted(ctx, *got); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkReady(ctx, snap.ID, true); err != nil {
		t.Fatal(err)
	}
	// A delivery, ready, still waiting for a driver.
	dlv := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "test",
		SourceEventID: "o2", PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: "delivery"}}
	if _, err := repo.UpsertArrived(ctx, dlv); err != nil {
		t.Fatal(err)
	}
	if got, err = repo.ClaimAccept(ctx, uuid.MustParse(dlv.ID), 20, false); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordAccepted(ctx, *got); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.MarkReady(ctx, dlv.ID, true); err != nil {
		t.Fatal(err)
	}

	g := newRig(t)
	r := New(Config{Dir: g.dir, Supported: true, Version: "2.2.2", Env: "prod", Now: func() time.Time { return g.now }},
		repo.CountOpenBatchOrders, g.store, func(context.Context, string, any) error { return nil },
		func(context.Context, string, string) error { return nil }, nil, zerolog.Nop())
	r.Load(ctx)
	if rep := r.Request(ctx, ActionUpdate, true, "admin", "lull1"); !rep.Accepted || rep.State != StateWaiting {
		t.Fatalf("forced request = %+v, want waiting (two ready orders open)", rep)
	}

	// 30 minutes on: the online pickup finishes; the delivery goes.
	if _, err := repo.FinishDueOnlinePickups(ctx, time.Now().Add(time.Second), time.Now()); err != nil {
		t.Fatal(err)
	}
	g.now = g.now.Add(31 * time.Second)
	r.Tick(ctx)
	if _, err := os.Stat(filepath.Join(g.dir, RequestFile)); err == nil {
		t.Fatal("request written while the delivery is still open")
	}
	if _, err := repo.MarkDeliveredNoDriver(ctx, dlv.ID, order.MarkedBy{Kind: order.ByTablet, Name: "Counter"}); err != nil {
		t.Fatal(err)
	}
	g.now = g.now.Add(31 * time.Second)
	r.Tick(ctx)
	b, err := os.ReadFile(filepath.Join(g.dir, RequestFile))
	if err != nil {
		t.Fatalf("no update.request in a lull: %v", err)
	}
	var req map[string]any
	_ = json.Unmarshal(b, &req)
	if req["id"] != "lull1" {
		t.Fatalf("request = %v", req)
	}
}
