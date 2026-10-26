package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	_ "modernc.org/sqlite"
)

func newFloorRepo(t *testing.T) *OrderRepository {
	t.Helper()
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
	return NewOrderRepository(db, zerolog.Nop())
}

func arrive(t *testing.T, r *OrderRepository, fulfillment string) order.StoredOrder {
	t.Helper()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
		Source: "test", SourceEventID: uuid.NewString()[:6], PlacedAt: time.Now().UTC(),
		Fulfillment: order.Fulfillment{Type: fulfillment}}
	if _, err := r.UpsertArrived(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // distinct arrival_date ordering
	so, err := r.GetByID(context.Background(), uuid.MustParse(s.ID))
	if err != nil {
		t.Fatal(err)
	}
	return *so
}

func accept(t *testing.T, r *OrderRepository, so order.StoredOrder) order.StoredOrder {
	t.Helper()
	got, err := r.ClaimAccept(context.Background(), uuid.MustParse(so.Order.ID), 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordAccepted(context.Background(), *got, false); err != nil {
		t.Fatal(err)
	}
	return *got
}

func clockIn(t *testing.T, r *OrderRepository, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := r.ClockIn(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

func eventsOf(t *testing.T, r *OrderRepository) []map[string]any {
	t.Helper()
	evs, err := r.ListUnsyncedEvents(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatal(err)
		}
		if m["event_id"] != e.ID || m["type"] != e.Type {
			t.Fatalf("payload must carry its own event_id + type: %v", m)
		}
		out = append(out, m)
	}
	return out
}

// TestMigrationFrom00003KeepsOrders upgrades a DB holding pre-floor orders.
func TestMigrationFrom00003KeepsOrders(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.RunTo(ctx, path, 3); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"arrived", "accepted"} {
		snap, _ := json.Marshal(order.Snapshot{ID: uuid.NewString()})
		_, err := raw.Exec(`INSERT INTO orders (id, tenant_id, location_id, source, source_event_id, snapshot, state, printed_date)
			VALUES (?, 't', 'l', 'online', ?, ?, ?, CASE WHEN ? = 'accepted' THEN '2026-10-01T10:00:00.000Z' END)`,
			uuid.NewString(), i, string(snap), state, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewOrderRepository(db, zerolog.Nop())
	b, err := r.OpenBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ListByBatch(ctx, b.ID, 0, 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("old orders should join the first batch: %v %d", err, len(got))
	}
	for _, so := range got {
		if so.Status.State == order.StateAccepted && so.Status.AcceptedAt == nil {
			t.Fatal("accepted_at should be backfilled from printed_date")
		}
	}
	fs, _ := r.FloorSettings(ctx)
	if fs.KitchenNext != 1100 || fs.LastPrepMinutes != 20 {
		t.Fatalf("floor settings defaults: %+v", fs)
	}
}

func TestKitchenNumbers(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	a, b := arrive(t, r, "pickup"), arrive(t, r, "pickup")

	ga := accept(t, r, a)
	if ga.Status.KitchenNumber == nil || *ga.Status.KitchenNumber != 1100 {
		t.Fatalf("first kitchen number = %v", ga.Status.KitchenNumber)
	}
	if ga.Status.EstimatedReadyAt == nil || ga.Status.EstimatedReadyAt.Sub(*ga.Status.AcceptedAt) != 20*time.Minute {
		t.Fatalf("estimated ready should be accepted + 20m: %+v", ga.Status)
	}

	// A reverted claim keeps its number on re-accept; the next order gets 1101.
	gb, err := r.ClaimAccept(ctx, uuid.MustParse(b.Order.ID), 30, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RevertAccept(ctx, uuid.MustParse(b.Order.ID)); err != nil {
		t.Fatal(err)
	}
	again, err := r.ClaimAccept(ctx, uuid.MustParse(b.Order.ID), 30, false)
	if err != nil {
		t.Fatal(err)
	}
	if *gb.Status.KitchenNumber != 1101 || *again.Status.KitchenNumber != 1101 {
		t.Fatalf("retry must keep 1101: %d / %d", *gb.Status.KitchenNumber, *again.Status.KitchenNumber)
	}
	fs, _ := r.FloorSettings(ctx)
	if fs.KitchenNext != 1102 || fs.LastPrepMinutes != 30 {
		t.Fatalf("settings after accepts: %+v", fs)
	}
}

func TestPickupLifecycleAndEvents(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so := accept(t, r, arrive(t, r, "pickup"))

	if _, err := r.MarkReady(ctx, uuid.NewString()); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("ready on missing order: %v", err)
	}
	ready, err := r.MarkReady(ctx, so.Order.ID)
	if err != nil || ready.Status.State != order.StateReady || ready.Status.ReadyAt == nil {
		t.Fatalf("ready: %v %+v", err, ready)
	}
	if _, err := r.MarkReady(ctx, so.Order.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("second ready: %v", err)
	}
	evs := eventsOf(t, r)
	if len(evs) != 2 || evs[0]["status"] != "accepted" || evs[1]["status"] != "ready" {
		t.Fatalf("events = %v", evs)
	}
	if evs[0]["kitchen_number"].(float64) != 1100 || evs[0]["prep_minutes"].(float64) != 20 || evs[0]["estimated_ready_at"] == nil {
		t.Fatalf("accepted event must carry kitchen # + prep: %v", evs[0])
	}
}

func TestDeliveryFlow(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	d1, err := r.CreateDriver(ctx, "Sam", "")
	if err != nil {
		t.Fatal(err)
	}
	d2, _ := r.CreateDriver(ctx, "Ana", "")
	if _, err := r.AssignDriver(ctx, d1.ID, []string{uuid.NewString()}); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("a clocked-out driver can't be sent out: %v", err)
	}
	clockIn(t, r, d1.ID, d2.ID)
	del1 := accept(t, r, arrive(t, r, "delivery"))
	del2 := accept(t, r, arrive(t, r, "delivery"))
	pick := accept(t, r, arrive(t, r, "pickup"))

	// Not ready yet → can't go out.
	if _, err := r.AssignDriver(ctx, d1.ID, []string{del1.Order.ID}); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("assign before ready: %v", err)
	}
	for _, so := range []order.StoredOrder{del1, del2, pick} {
		if _, err := r.MarkReady(ctx, so.Order.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Pickup can't go out; the whole assignment rolls back.
	if _, err := r.AssignDriver(ctx, d1.ID, []string{del1.Order.ID, pick.Order.ID}); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("assign pickup: %v", err)
	}
	if got, _ := r.GetByID(ctx, uuid.MustParse(del1.Order.ID)); got.Status.State != order.StateReady {
		t.Fatal("failed assignment must roll back")
	}
	if _, err := r.AssignDriver(ctx, "nope", []string{del1.Order.ID}); !errors.Is(err, order.ErrDriverNotFound) {
		t.Fatalf("unknown driver: %v", err)
	}

	out, err := r.AssignDriver(ctx, d1.ID, []string{del1.Order.ID, del2.Order.ID})
	if err != nil || len(out) != 2 || out[0].Status.State != order.StateOnDelivery || out[0].Status.DriverName != "Sam" {
		t.Fatalf("assign: %v %+v", err, out)
	}
	ds, _ := r.ListDrivers(ctx)
	if ds[0].ID != d1.ID || ds[0].Status != order.DriverOut || ds[0].Deliveries != 2 {
		t.Fatalf("driver after assign: %+v", ds)
	}

	back, err := r.DriverReturned(ctx, d1.ID)
	if err != nil || len(back) != 2 || back[0].Status.State != order.StateDelivered || back[0].Status.DeliveredAt == nil {
		t.Fatalf("returned: %v %+v", err, back)
	}
	ds, _ = r.ListDrivers(ctx)
	if ds[0].ID != d2.ID || ds[1].ID != d1.ID || ds[1].Status != order.DriverAvailable {
		t.Fatalf("returned driver goes to the back of the queue: %+v", ds)
	}
	// Status events carry the driver name; roster events the whole roster.
	var outEv, rosterEv map[string]any
	for _, e := range eventsOf(t, r) {
		if e["status"] == "on_delivery" && outEv == nil {
			outEv = e
		}
		if e["type"] == "drivers.roster" {
			rosterEv = e
		}
	}
	if outEv["driver_name"] != "Sam" || len(rosterEv["drivers"].([]any)) != 2 {
		t.Fatalf("events: out=%v roster=%v", outEv, rosterEv)
	}
}

func TestDriversRoster(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	a, _ := r.CreateDriver(ctx, "A", "")
	b, _ := r.CreateDriver(ctx, "B", "")
	c, _ := r.CreateDriver(ctx, "C", "")
	if err := r.ReorderDrivers(ctx, []string{c.ID, a.ID}); err != nil {
		t.Fatal(err)
	}
	ds, _ := r.ListDrivers(ctx)
	if ds[0].ID != c.ID || ds[1].ID != a.ID || ds[2].ID != b.ID {
		t.Fatalf("reorder: %+v", ds)
	}
	if err := r.RenameDriver(ctx, b.ID, "Bee"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RemoveDriver(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RemoveDriver(ctx, a.ID); !errors.Is(err, order.ErrDriverNotFound) {
		t.Fatalf("double remove: %v", err)
	}
	ds, _ = r.ListDrivers(ctx)
	if len(ds) != 2 || ds[1].Name != "Bee" {
		t.Fatalf("after rename/remove: %+v", ds)
	}
	if err := r.ReorderDrivers(ctx, []string{a.ID}); !errors.Is(err, order.ErrDriverNotFound) {
		t.Fatalf("reorder with removed driver: %v", err)
	}
}

func TestCloseBatch(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	d, _ := r.CreateDriver(ctx, "Sam", "")
	clockIn(t, r, d.ID)
	done := accept(t, r, arrive(t, r, "pickup"))
	r.MarkReady(ctx, done.Order.ID)
	waiting := arrive(t, r, "pickup")                 // never accepted → cancelled
	cooking := accept(t, r, arrive(t, r, "delivery")) // accepted, not ready
	out := accept(t, r, arrive(t, r, "delivery"))
	r.MarkReady(ctx, out.Order.ID)
	if _, err := r.AssignDriver(ctx, d.ID, []string{out.Order.ID}); err != nil {
		t.Fatal(err)
	}
	first, _ := r.OpenBatch(ctx)

	_, _, err := r.CloseBatch(ctx, false)
	var lo *order.LeftoversError
	if !errors.As(err, &lo) || len(lo.Orders) != 3 {
		t.Fatalf("want 3 leftovers, got %v", err)
	}
	if b, _ := r.OpenBatch(ctx); b.ID != first.ID {
		t.Fatal("a refused close must not close the batch")
	}

	closed, finished, err := r.CloseBatch(ctx, true)
	if err != nil || closed.ID != first.ID || len(finished) != 3 {
		t.Fatalf("force close: %v %+v %d", err, closed, len(finished))
	}
	states := map[string]order.State{}
	for _, so := range finished {
		states[so.Order.ID] = so.Status.State
	}
	if states[waiting.Order.ID] != order.StateCancelled || states[cooking.Order.ID] != order.StateDelivered ||
		states[out.Order.ID] != order.StateDelivered {
		t.Fatalf("auto-finish states: %v", states)
	}
	next, _ := r.OpenBatch(ctx)
	fs, _ := r.FloorSettings(ctx)
	ds, _ := r.ListDrivers(ctx)
	if next.ID == first.ID || fs.KitchenNext != 1100 || ds[0].Status != order.DriverAvailable || ds[0].Deliveries != 0 {
		t.Fatalf("after close: batch=%d kitchen=%d drivers=%+v", next.ID, fs.KitchenNext, ds)
	}
	auto, cancelled := 0, 0
	for _, e := range eventsOf(t, r) {
		if e["auto"] == true {
			auto++
			if e["status"] == "cancelled" {
				cancelled++
			}
		}
	}
	if cancelled != 1 {
		t.Fatalf("cancel events = %d", cancelled)
	}
	got, _ := r.GetByID(ctx, uuid.MustParse(waiting.Order.ID))
	if got.Status.CancelledAt == nil || got.Status.KitchenNumber != nil || got.Status.PrintedDate != nil {
		t.Fatalf("cancelled order: %+v", got.Status)
	}
	if auto != 3 {
		t.Fatalf("auto-finish events = %d", auto)
	}
	// New arrivals land in the new batch; the old batch is still listed.
	fresh := arrive(t, r, "pickup")
	if fresh.Status.BatchID != next.ID {
		t.Fatalf("new order batch = %d, want %d", fresh.Status.BatchID, next.ID)
	}
	if got := accept(t, r, fresh); *got.Status.KitchenNumber != 1100 {
		t.Fatalf("kitchen numbers restart at 1100, got %d", *got.Status.KitchenNumber)
	}
}

func TestTrimBatchesKeepsOpenPlusN(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		arrive(t, r, "pickup")
		if _, _, err := r.CloseBatch(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	arrive(t, r, "pickup") // in the open batch
	n, err := r.TrimBatches(ctx, 3, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("trim deleted %d (%v), want the 2 oldest batches' orders", n, err)
	}
	bs, _ := r.ListBatches(ctx, 10)
	if len(bs) != 4 || bs[0].ClosedAt != nil {
		t.Fatalf("want open + 3 closed, got %+v", bs)
	}
	// Unsynced events are never trimmed; synced ones past the cutoff are.
	left, _ := r.CountUnsyncedEvents(ctx)
	if left == 0 {
		t.Fatal("unsynced events must survive the trim")
	}
}

func TestEventSyncBookkeeping(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	accept(t, r, arrive(t, r, "pickup"))
	evs, _ := r.ListUnsyncedEvents(ctx, 10)
	if len(evs) != 1 {
		t.Fatalf("events = %d", len(evs))
	}
	if err := r.MarkEventFailed(ctx, evs[0].ID, "boom"); err != nil {
		t.Fatal(err)
	}
	evs, _ = r.ListUnsyncedEvents(ctx, 10)
	if evs[0].Attempts != 1 {
		t.Fatalf("attempts = %d", evs[0].Attempts)
	}
	if err := r.MarkEventSynced(ctx, evs[0].ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.CountUnsyncedEvents(ctx); n != 0 {
		t.Fatalf("unsynced = %d", n)
	}
	if _, err := r.TrimBatches(ctx, 3, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestOverdueArrived(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so := arrive(t, r, "pickup")
	accept(t, r, arrive(t, r, "pickup")) // accepted: never overdue

	if got, _ := r.ListOverdueArrived(ctx, time.Now().Add(-time.Minute)); len(got) != 0 {
		t.Fatal("a fresh order isn't overdue")
	}
	later := time.Now().Add(3 * time.Minute)
	got, _ := r.ListOverdueArrived(ctx, later.Add(-2*time.Minute))
	if len(got) != 1 || got[0].Order.ID != so.Order.ID {
		t.Fatalf("overdue = %+v", got)
	}
	r.MarkAlerted(ctx, so.Order.ID, later)
	if got, _ := r.ListOverdueArrived(ctx, later.Add(time.Minute-2*time.Minute)); len(got) != 0 {
		t.Fatal("alerted < 2 min ago: not again yet")
	}
	if got, _ := r.ListOverdueArrived(ctx, later.Add(2*time.Minute+time.Second)); len(got) != 1 {
		t.Fatal("2 min after the last alert: alert again")
	}
}

func TestClockInOutAndShifts(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	a, _ := r.CreateDriver(ctx, "Abdul", "")
	b, _ := r.CreateDriver(ctx, "Juma", "")
	if a.Status != order.DriverOff {
		t.Fatalf("new drivers start clocked out, got %s", a.Status)
	}
	// Queue = clock-in order: Juma first, then Abdul.
	clockIn(t, r, b.ID, a.ID)
	ds, _ := r.ListDrivers(ctx)
	var queue []string
	for _, d := range ds {
		if d.Status == order.DriverAvailable {
			queue = append(queue, d.Name)
			if d.ClockedInAt == nil {
				t.Fatal("clocked-in drivers show their clock-in time")
			}
		}
	}
	if len(queue) != 2 || queue[0] != "Juma" {
		t.Fatalf("queue = %v", queue)
	}
	if _, err := r.ClockIn(ctx, a.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("double clock-in: %v", err)
	}

	// Out on a run → can't clock out.
	so := accept(t, r, arrive(t, r, "delivery"))
	r.MarkReady(ctx, so.Order.ID)
	if _, err := r.AssignDriver(ctx, b.ID, []string{so.Order.ID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ClockOut(ctx, b.ID); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("clock out while out: %v", err)
	}

	got, _, err := r.ClockOut(ctx, a.ID)
	if err != nil || got.Status != order.DriverOff || got.ClockedInAt != nil {
		t.Fatalf("clock out: %v %+v", err, got)
	}
	shifts, _ := r.ListShifts(ctx, time.Now().Add(-time.Hour))
	if len(shifts) != 2 {
		t.Fatalf("shifts = %+v", shifts)
	}
	for _, sh := range shifts {
		if sh.DriverID == a.ID && (sh.ClockOut == nil || sh.AutoOut) {
			t.Fatalf("Abdul's shift should be closed by hand: %+v", sh)
		}
		if sh.DriverID == b.ID && sh.ClockOut != nil {
			t.Fatalf("Juma is still on shift: %+v", sh)
		}
	}
	n := 0
	for _, e := range eventsOf(t, r) {
		if e["type"] == "driver.shift" {
			n++
		}
	}
	if n != 3 { // 2 clock-ins + 1 clock-out
		t.Fatalf("driver.shift events = %d", n)
	}
}

func TestAutoClockOut(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	a, _ := r.CreateDriver(ctx, "Abdul", "")
	b, _ := r.CreateDriver(ctx, "Juma", "")
	clockIn(t, r, a.ID, b.ID)
	so := accept(t, r, arrive(t, r, "delivery"))
	r.MarkReady(ctx, so.Order.ID)
	if _, err := r.AssignDriver(ctx, b.ID, []string{so.Order.ID}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)

	n, _, err := r.AutoClockOut(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("auto clock-out: %d %v (only the available driver)", n, err)
	}
	rs, _ := r.GetDriverReset(ctx)
	if rs.Time != "10:00" || rs.LastRun == nil {
		t.Fatalf("reset setting: %+v", rs)
	}
	// Juma was out during the reset: clocked out on return.
	if _, err := r.DriverReturned(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	ds, _ := r.ListDrivers(ctx)
	for _, d := range ds {
		if d.Status != order.DriverOff {
			t.Fatalf("%s should be off after the reset: %s", d.Name, d.Status)
		}
	}
	shifts, _ := r.ListShifts(ctx, time.Now().Add(-time.Hour))
	for _, sh := range shifts {
		if sh.ClockOut == nil || !sh.AutoOut {
			t.Fatalf("every shift auto-closed: %+v", sh)
		}
	}
}

// TestMigrationTo00006KeepsDrivers upgrades a 00005 DB that already has
// drivers (statuses available / out) and an open batch: drivers keep their
// status and queue, new drivers default to off, clock-in works after.
func TestMigrationTo00006KeepsDrivers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.RunTo(ctx, path, 5); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range []string{"available", "out"} {
		if _, err := raw.Exec(`INSERT INTO drivers (id, name, position, status) VALUES (?, ?, ?, ?)`,
			uuid.NewString(), []string{"Abdul", "Juma"}[i], i+1, st); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatalf("upgrade to 00006: %v", err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := NewOrderRepository(db, zerolog.Nop())
	ds, err := r.ListDrivers(ctx)
	if err != nil || len(ds) != 2 || ds[0].Name != "Abdul" || ds[0].Status != order.DriverAvailable || ds[1].Status != order.DriverOut {
		t.Fatalf("drivers after upgrade: %v %+v", err, ds)
	}
	rs, _ := r.GetDriverReset(ctx)
	if rs.Time != "10:00" || rs.LastRun != nil {
		t.Fatalf("reset defaults: %+v", rs)
	}
	nd, _ := r.CreateDriver(ctx, "Deep", "")
	if nd.Status != order.DriverOff {
		t.Fatalf("new driver status %s", nd.Status)
	}
	if _, err := r.ClockIn(ctx, nd.ID); err != nil {
		t.Fatalf("clock in after upgrade: %v", err)
	}
}

func TestDriverGuards(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	out, _ := r.CreateDriver(ctx, "Out", "")
	in, _ := r.CreateDriver(ctx, "In", "")
	clockIn(t, r, out.ID, in.ID)
	so := accept(t, r, arrive(t, r, "delivery"))
	r.MarkReady(ctx, so.Order.ID)
	// Duplicate ids in one assignment are fine.
	if _, err := r.AssignDriver(ctx, out.ID, []string{so.Order.ID, so.Order.ID}); err != nil {
		t.Fatalf("assign with a duplicated id: %v", err)
	}
	if _, err := r.RemoveDriver(ctx, out.ID); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("removing a driver out on a run: %v", err)
	}
	if _, err := r.RemoveDriver(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	shifts, _ := r.ListShifts(ctx, time.Now().Add(-time.Hour))
	for _, sh := range shifts {
		if sh.DriverID == in.ID && sh.ClockOut == nil {
			t.Fatal("removing a clocked-in driver closes their shift")
		}
	}
	if _, err := r.DriverReturned(ctx, out.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DriverReturned(ctx, out.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("returned twice: %v", err)
	}
}

func TestCloseBatchClocksOutDriversWhoWereOutDuringTheReset(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	d, _ := r.CreateDriver(ctx, "Sam", "")
	clockIn(t, r, d.ID)
	so := accept(t, r, arrive(t, r, "delivery"))
	r.MarkReady(ctx, so.Order.ID)
	r.AssignDriver(ctx, d.ID, []string{so.Order.ID})
	time.Sleep(2 * time.Millisecond)
	r.AutoClockOut(ctx, time.Now()) // Sam is out: skipped
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	ds, _ := r.ListDrivers(ctx)
	if ds[0].Status != order.DriverOff {
		t.Fatalf("Sam's shift began before the reset — close batch clocks him out, got %s", ds[0].Status)
	}
}

// TestInterruptedAcceptIsFound: a claim without a recorded accept (the agent
// died mid-print) is listed for the startup sweep; a recorded one isn't, and
// neither is a restored order.
func TestInterruptedAcceptIsFound(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	done := accept(t, r, arrive(t, r, "pickup")) // claim + record
	cut := arrive(t, r, "pickup")
	if _, err := r.ClaimAccept(ctx, uuid.MustParse(cut.Order.ID), 20, false); err != nil {
		t.Fatal(err)
	}
	ids, err := r.ListInterruptedAccepts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != cut.Order.ID {
		t.Fatalf("interrupted %v, want only %s (not %s)", ids, cut.Order.ID, done.Order.ID)
	}
	if err := r.RevertAccept(ctx, uuid.MustParse(cut.Order.ID)); err != nil {
		t.Fatal(err)
	}
	if ids, _ := r.ListInterruptedAccepts(ctx); len(ids) != 0 {
		t.Fatalf("after revert: %v", ids)
	}
	got, _ := r.GetByID(ctx, uuid.MustParse(cut.Order.ID))
	if got.Status.State != order.StateArrived || got.Status.KitchenNumber == nil {
		t.Fatalf("reverted order %+v (kitchen number is kept for the re-accept)", got.Status)
	}
}
