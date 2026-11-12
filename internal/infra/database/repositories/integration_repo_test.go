package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// 2.1.0: the hub feed, future orders, close batch.

func TestFeedCursorAndPrune(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	set := order.DefaultIntegrationSettings()
	for i := 0; i < 3; i++ {
		set.Times.DefaultPickupMinutes = int64(10 + i)
		if err := r.SetIntegrationSettings(ctx, set); err != nil {
			t.Fatal(err)
		}
	}
	evs, maxSeq, err := r.ListFeed(ctx, 0, 2)
	if err != nil || len(evs) != 2 || maxSeq != 3 || evs[0].Seq != 1 || evs[1].Seq != 2 || evs[0].Type != order.FeedSettingsChanged {
		t.Fatalf("page 1 = %+v max %d %v", evs, maxSeq, err)
	}
	var got order.IntegrationSettings
	if err := json.Unmarshal(evs[1].Data, &got); err != nil || got.Times.DefaultPickupMinutes != 11 {
		t.Fatalf("data = %s", evs[1].Data)
	}
	if evs, _, _ = r.ListFeed(ctx, 2, 10); len(evs) != 1 || evs[0].Seq != 3 {
		t.Fatalf("after 2 = %+v", evs)
	}
	if back, err := r.IntegrationSettings(ctx); err != nil || back.Times.DefaultPickupMinutes != 12 {
		t.Fatalf("stored = %+v %v", back, err)
	}
	// Kept 3 days; seq never reused after a prune.
	if _, err := r.db.ExecContext(ctx, `UPDATE integration_events SET created_at = ? WHERE seq <= 2`, fmtTS(time.Now().Add(-73*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.TrimBatches(ctx, 3, time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if evs, _, _ = r.ListFeed(ctx, 0, 10); len(evs) != 1 || evs[0].Seq != 3 {
		t.Fatalf("after prune = %+v", evs)
	}
	if err := r.SetIntegrationSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	if _, maxSeq, _ = r.ListFeed(ctx, 0, 10); maxSeq != 4 {
		t.Fatalf("next seq = %d, want 4", maxSeq)
	}
}

func TestScheduleAndRaiseDue(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so := arrive(t, r, "delivery")
	ring := time.Now().Add(time.Hour)
	got, err := r.Schedule(ctx, so.Order.ID, 25, ring)
	if err != nil || got.Status.State != order.StateScheduled || *got.Status.ScheduledLeadMinutes != 25 || !got.Status.RingAt.Equal(ring.Truncate(time.Millisecond)) {
		t.Fatalf("schedule = %+v %v", got, err)
	}
	if ev := lastStatusEvent(t, r, so.Order.ID); ev["status"] != "scheduled" {
		t.Fatalf("event = %v", ev)
	}
	// Not in Deliveries / pools.
	open, _ := r.ListOpenDeliveries(ctx)
	if len(open) != 0 {
		t.Fatalf("open deliveries = %d", len(open))
	}
	if out, _ := r.RaiseDue(ctx, ring.Add(-time.Second)); len(out) != 0 {
		t.Fatal("raised early")
	}
	out, err := r.RaiseDue(ctx, ring.Add(time.Second))
	if err != nil || len(out) != 1 || out[0].Status.State != order.StateArrived || !out[0].Status.Rerung {
		t.Fatalf("raise = %+v %v", out, err)
	}
	// Escalation counts from the ring, not the first arrival.
	if od, _ := r.ListOverdueArrived(ctx, ring.Add(time.Second).Add(-2*time.Minute)); len(od) != 0 {
		t.Fatal("escalated at once")
	}
	if od, _ := r.ListOverdueArrived(ctx, ring.Add(2*time.Minute)); len(od) != 1 {
		t.Fatal("no escalation 2 min after the ring")
	}
	if _, err := r.Schedule(ctx, uuid.NewString(), 10, ring); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

// 00014 rebuilds orders: an upgraded DB keeps its orders (and pools).
func TestMigrationTo00014KeepsOrders(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.RunTo(ctx, path, 13); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id, pool_seq)
		VALUES ('0198b4a6-0000-7000-8000-000000000001', 't', 'l', 'online', '1', '{"id":"0198b4a6-0000-7000-8000-000000000001","fulfillment":{"type":"delivery"}}', 'ready', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewOrderRepository(db, zerolog.Nop())
	so := get(t, r, "0198b4a6-0000-7000-8000-000000000001")
	if so.Status.State != order.StateReady || so.Status.PoolSeq == nil || *so.Status.PoolSeq != 2 {
		t.Fatalf("migrated = %+v", so.Status)
	}
	if set, err := r.IntegrationSettings(ctx); err != nil || !reflect.DeepEqual(set, order.DefaultIntegrationSettings()) {
		t.Fatalf("settings = %+v %v", set, err)
	}
}

func arriveAt(t *testing.T, r *OrderRepository, fulfillment string, promised time.Time) order.StoredOrder {
	t.Helper()
	p := promised.UTC()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
		Source: "test", SourceEventID: uuid.NewString()[:6], PlacedAt: time.Now().UTC(),
		Fulfillment: order.Fulfillment{Type: fulfillment, PromisedAt: &p}}
	if _, err := r.UpsertArrived(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	return get(t, r, s.ID)
}

// A future order not accepted yet (never scheduled, or rung again) whose
// promised time is ahead moves into the next batch at Close batch, even a
// forced one; a past-due one is cancelled as before.
func TestCloseBatchMovesWaitingFutureOrders(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	now := time.Now()
	fresh := arriveAt(t, r, "pickup", now.Add(3*time.Hour))
	if !fresh.Status.Future {
		t.Fatal("not a future order")
	}
	rerung := arriveAt(t, r, "delivery", now.Add(30*time.Minute))
	if _, err := r.Schedule(ctx, rerung.Order.ID, 40, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	pastDue := arriveAt(t, r, "pickup", now.Add(-5*time.Minute))
	if _, err := r.Schedule(ctx, pastDue.Order.ID, 20, now.Add(-25*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if raised, err := r.RaiseDue(ctx, now); err != nil || len(raised) != 2 {
		t.Fatalf("raise = %d %v", len(raised), err)
	}
	soon := arrive(t, r, "pickup") // an ordinary order nobody accepted
	old, _ := r.OpenBatch(ctx)

	// Only the past-due and the ordinary order are leftovers.
	_, _, err := r.CloseBatch(ctx, false)
	var lo *order.LeftoversError
	if !errors.As(err, &lo) || len(lo.Orders) != 2 {
		t.Fatalf("leftovers = %v", err)
	}
	for _, so := range lo.Orders {
		if so.Order.ID != pastDue.Order.ID && so.Order.ID != soon.Order.ID {
			t.Fatalf("leftover %s", so.Order.ID)
		}
	}
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	nb, _ := r.OpenBatch(ctx)
	if nb.ID == old.ID {
		t.Fatal("no new batch")
	}
	for _, id := range []string{fresh.Order.ID, rerung.Order.ID} {
		got := get(t, r, id)
		if got.Status.State != order.StateArrived || got.Status.BatchID != nb.ID {
			t.Fatalf("waiting future order %s = %s in batch %d (new %d)", id, got.Status.State, got.Status.BatchID, nb.ID)
		}
	}
	if got := get(t, r, rerung.Order.ID); !got.Status.Rerung {
		t.Fatal("lost rerung")
	}
	for _, id := range []string{pastDue.Order.ID, soon.Order.ID} {
		if got := get(t, r, id); got.Status.State != order.StateCancelled || got.Status.BatchID != old.ID {
			t.Fatalf("order %s = %s in batch %d", id, got.Status.State, got.Status.BatchID)
		}
	}
}

// 00030 (H3): the POS checks still on the Pi go with their side rows, every
// online order and driver stays, and the POS switches leave the settings.
func TestMigration00030DropsSoftTouch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.RunTo(ctx, path, 29); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const online, pos = "0198b4a6-0000-7000-8000-000000000011", "0198b4a6-0000-7000-8000-000000000012"
	for _, q := range []string{
		`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id)
		 VALUES ('` + online + `', 't', 'l', 'online', '1', '{"id":"` + online + `","fulfillment":{"type":"delivery"}}', 'accepted', 1)`,
		`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, batch_id, ext_source, ext_ref, local_only, display_number)
		 VALUES ('` + pos + `', 't', 'l', 'pos', 'softtouch:7', '{"id":"` + pos + `","fulfillment":{"type":"delivery"}}', 'accepted', 1, 'softtouch', '7', true, 57)`,
		`INSERT INTO order_geo (order_id, state, updated_at) VALUES ('` + pos + `', 'pending', '2026-11-12T00:00:00Z')`,
		`INSERT INTO order_times (a, b, seconds) VALUES ('` + online + `', '` + pos + `', 60)`,
		`INSERT INTO drivers (id, name, pos_number) VALUES ('d1', 'Ali', '12')`,
		`UPDATE settings SET features = '{"pos_pull":true,"order_insert":false,"driver_writeback":true,"address_heal":true,"future_orders":false,"paid_wave":true}',
		 ticket_toggles = '{"pos_delivery":true,"pos_takeout":false,"pos_counter":false,"online_cash_pickup":false,"online_cash_delivery":true,"online_card":true}'
		 WHERE id = 1`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewOrderRepository(db, zerolog.Nop())
	if so := get(t, r, online); so.Status.State != order.StateAccepted {
		t.Fatalf("online order = %+v", so.Status)
	}
	if _, err := r.GetByID(ctx, uuid.MustParse(pos)); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("the POS check is still there: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM order_times`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("order_times left = %d %v", n, err)
	}
	ds, err := r.ListDrivers(ctx)
	if err != nil || len(ds) != 1 || ds[0].Name != "Ali" {
		t.Fatalf("drivers = %+v %v", ds, err)
	}
	set, err := r.IntegrationSettings(ctx)
	if err != nil || set.Features.FutureOrders || !set.Features.PaidWave || set.Tickets.OnlineCashPickup || !set.Tickets.OnlineCashDelivery {
		t.Fatalf("settings = %+v %v", set, err)
	}
	var features, tickets string
	if err := db.QueryRowContext(ctx, `SELECT features, ticket_toggles FROM settings WHERE id = 1`).Scan(&features, &tickets); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"pos_pull", "order_insert", "driver_writeback", "address_heal", "pos_delivery", "pos_takeout", "pos_counter"} {
		if strings.Contains(features+tickets, k) {
			t.Fatalf("%s is still in the settings: %s %s", k, features, tickets)
		}
	}
}
