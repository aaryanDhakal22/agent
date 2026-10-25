package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// poolSetup: a repo with available drivers a, b and accepted deliveries.
func poolSetup(t *testing.T, deliveries int) (*OrderRepository, *order.Driver, *order.Driver, []order.StoredOrder) {
	t.Helper()
	ctx := context.Background()
	r := newFloorRepo(t)
	a, _ := r.CreateDriver(ctx, "Ann", "")
	b, _ := r.CreateDriver(ctx, "Bob", "")
	clockIn(t, r, a.ID, b.ID)
	var out []order.StoredOrder
	for range deliveries {
		out = append(out, accept(t, r, arrive(t, r, "delivery")))
	}
	return r, a, b, out
}

func ids(list ...order.StoredOrder) []string {
	out := make([]string, 0, len(list))
	for _, so := range list {
		out = append(out, so.Order.ID)
	}
	return out
}

func get(t *testing.T, r *OrderRepository, id string) order.StoredOrder {
	t.Helper()
	so, err := r.GetByID(context.Background(), uuid.MustParse(id))
	if err != nil {
		t.Fatal(err)
	}
	return *so
}

// poolEvents counts order.status events whose agent_status carries the pool.
func lastStatusEvent(t *testing.T, r *OrderRepository, orderID string) map[string]any {
	t.Helper()
	var last map[string]any
	for _, e := range eventsOf(t, r) {
		if e["type"] == order.FloorEventOrderStatus && e["order_id"] == orderID {
			last = e
		}
	}
	if last == nil {
		t.Fatalf("no order.status event for %s", orderID)
	}
	return last
}

func TestPoolMoveUnassignAndDriverToDriver(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 3)

	got, err := r.PoolOrders(ctx, ids(os[0], os[1]), a.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("pool: %v %d", err, len(got))
	}
	for _, so := range got {
		if so.Status.PoolDriverID != a.ID || so.Status.PooledAt == nil || so.Status.State != order.StateAccepted {
			t.Fatalf("pooled status: %+v", so.Status)
		}
	}
	ev := lastStatusEvent(t, r, os[0].Order.ID)
	if ev["status"] != "accepted" || ev["agent_status"].(map[string]any)["pool_driver_id"] != a.ID {
		t.Fatalf("pool change must be an order.status with the same state + pool: %v", ev)
	}

	// Driver -> driver.
	if _, err := r.PoolOrders(ctx, ids(os[1]), b.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(t, r, os[1].Order.ID); got.Status.PoolDriverID != b.ID {
		t.Fatalf("moved: %+v", got.Status)
	}
	// Unassign.
	if _, err := r.PoolOrders(ctx, ids(os[0]), ""); err != nil {
		t.Fatal(err)
	}
	got0 := get(t, r, os[0].Order.ID)
	if got0.Status.PoolDriverID != "" || got0.Status.PooledAt != nil {
		t.Fatalf("unassigned: %+v", got0.Status)
	}
	if ev := lastStatusEvent(t, r, os[0].Order.ID); ev["agent_status"].(map[string]any)["pool_driver_id"] != nil {
		t.Fatalf("unassign event still names a pool: %v", ev)
	}
	// Ready orders pool too.
	if _, err := r.MarkReady(ctx, os[2].Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PoolOrders(ctx, ids(os[2]), a.ID); err != nil {
		t.Fatalf("ready order should pool: %v", err)
	}
}

func TestPoolRules(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 2)

	if _, err := r.PoolOrders(ctx, nil, a.ID); !errors.Is(err, order.ErrNoOrders) {
		t.Fatalf("empty list: %v", err)
	}
	if _, err := r.PoolOrders(ctx, ids(os[0]), "nobody"); !errors.Is(err, order.ErrDriverNotFound) {
		t.Fatalf("unknown driver: %v", err)
	}
	if _, err := r.PoolOrders(ctx, []string{uuid.NewString()}, a.ID); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	// Off driver.
	off, _ := r.CreateDriver(ctx, "Off", "")
	if _, err := r.PoolOrders(ctx, ids(os[0]), off.ID); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("off driver: %v", err)
	}
	// Out driver.
	ready := accept(t, r, arrive(t, r, "delivery"))
	r.MarkReady(ctx, ready.Order.ID)
	if _, err := r.AssignDriver(ctx, b.ID, ids(ready)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PoolOrders(ctx, ids(os[0]), b.ID); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("out driver: %v", err)
	}
	// Pickup, arrived and on_delivery orders can't be pooled; all or nothing.
	pickup := accept(t, r, arrive(t, r, "pickup"))
	arrived := arrive(t, r, "delivery")
	for _, bad := range []order.StoredOrder{pickup, arrived, ready} {
		if _, err := r.PoolOrders(ctx, ids(os[0], bad), a.ID); !errors.Is(err, order.ErrInvalidState) {
			t.Fatalf("bad order %s: %v", bad.Status.State, err)
		}
	}
	if got := get(t, r, os[0].Order.ID); got.Status.PoolDriverID != "" {
		t.Fatal("all or nothing: the good order must not be pooled")
	}
}

func TestSendOutNotReadyVsMarkReady(t *testing.T) {
	ctx := context.Background()
	r, a, _, os := poolSetup(t, 2)
	if _, err := r.MarkReady(ctx, os[0].Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.SendOut(ctx, a.ID, nil, false, order.RunMeta{}); !errors.Is(err, order.ErrNoOrders) {
		t.Fatalf("empty pool: %v", err)
	}
	if _, err := r.PoolOrders(ctx, ids(os...), a.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err := r.SendOut(ctx, a.ID, nil, false, order.RunMeta{})
	var nr *order.ErrNotReady
	if !errors.As(err, &nr) || len(nr.IDs) != 1 || nr.IDs[0] != os[1].Order.ID {
		t.Fatalf("want not_ready for the cooking order: %v", err)
	}
	if got := get(t, r, os[0].Order.ID); got.Status.State != order.StateReady || got.Status.PoolDriverID != a.ID {
		t.Fatalf("refused send-out must change nothing: %+v", got.Status)
	}

	sent, readied, err := r.SendOut(ctx, a.ID, nil, true, order.RunMeta{PlanID: "p1", SuggestedIDs: ids(os[1])})
	if err != nil || len(sent) != 2 || len(readied) != 1 {
		t.Fatalf("mark_ready send-out: %v sent=%d readied=%d", err, len(sent), len(readied))
	}
	for _, so := range sent {
		if so.Status.State != order.StateOnDelivery || so.Status.DriverID != a.ID || so.Status.PoolDriverID != "" {
			t.Fatalf("sent: %+v", so.Status)
		}
	}
	ds, _ := r.ListDrivers(ctx)
	for _, d := range ds {
		if d.ID == a.ID && d.Status != order.DriverOut {
			t.Fatalf("driver should be out: %s", d.Status)
		}
	}
	// Events: ready for the cooking one, on_delivery for both, a delivery.run.
	var readyEv, runEv bool
	for _, e := range eventsOf(t, r) {
		if e["type"] == order.FloorEventOrderStatus && e["order_id"] == os[1].Order.ID && e["status"] == "ready" {
			readyEv = true
		}
		if e["type"] == order.FloorEventDeliveryRun {
			runEv = true
			if e["driver_id"] != a.ID || e["plan_id"] != "p1" || len(e["sent_ids"].([]any)) != 2 ||
				len(e["suggested_ids"].([]any)) != 1 || len(e["stops"].([]any)) != 2 || e["driver_name"] != "Ann" {
				t.Fatalf("delivery.run: %v", e)
			}
		}
	}
	if !readyEv || !runEv {
		t.Fatalf("events: ready=%v run=%v", readyEv, runEv)
	}
}

func TestSendOutIncludeAndRules(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 3)
	for _, so := range os {
		r.MarkReady(ctx, so.Order.ID)
	}
	if _, err := r.PoolOrders(ctx, ids(os[0]), a.ID); err != nil {
		t.Fatal(err)
	}
	// include pulls an unassigned order and one from another driver's pool.
	if _, err := r.PoolOrders(ctx, ids(os[2]), b.ID); err != nil {
		t.Fatal(err)
	}
	sent, _, err := r.SendOut(ctx, a.ID, ids(os[1], os[2]), false, order.RunMeta{})
	if err != nil || len(sent) != 3 {
		t.Fatalf("include: %v %d", err, len(sent))
	}
	// Driver now out: send-out again is refused.
	if _, _, err := r.SendOut(ctx, a.ID, nil, false, order.RunMeta{}); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("out driver: %v", err)
	}
	// A bad include rolls everything back.
	extra := accept(t, r, arrive(t, r, "delivery"))
	r.PoolOrders(ctx, ids(extra), b.ID)
	pickup := accept(t, r, arrive(t, r, "pickup"))
	if _, _, err := r.SendOut(ctx, b.ID, ids(pickup), true, order.RunMeta{}); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("bad include: %v", err)
	}
	if got := get(t, r, extra.Order.ID); got.Status.State != order.StateAccepted {
		t.Fatalf("refused send-out must not mark ready: %+v", got.Status)
	}
}

func TestPoolClearedOnClockOutRemoveResetCloseAndAssign(t *testing.T) {
	ctx := context.Background()
	unpooled := func(t *testing.T, r *OrderRepository, id string) {
		t.Helper()
		if got := get(t, r, id); got.Status.PoolDriverID != "" {
			t.Fatalf("pool not cleared: %+v", got.Status)
		}
		if ev := lastStatusEvent(t, r, id); ev["agent_status"].(map[string]any)["pool_driver_id"] != nil {
			t.Fatalf("last event still pooled: %v", ev)
		}
	}

	t.Run("clock-out", func(t *testing.T) {
		r, a, _, os := poolSetup(t, 1)
		r.PoolOrders(ctx, ids(os...), a.ID)
		_, cleared, err := r.ClockOut(ctx, a.ID)
		if err != nil || len(cleared) != 1 || cleared[0].Status.PoolDriverID != "" {
			t.Fatalf("clock-out: %v %+v", err, cleared)
		}
		unpooled(t, r, os[0].Order.ID)
	})
	t.Run("remove", func(t *testing.T) {
		r, a, _, os := poolSetup(t, 1)
		r.PoolOrders(ctx, ids(os...), a.ID)
		cleared, err := r.RemoveDriver(ctx, a.ID)
		if err != nil || len(cleared) != 1 {
			t.Fatalf("remove: %v %d", err, len(cleared))
		}
		unpooled(t, r, os[0].Order.ID)
	})
	t.Run("daily reset", func(t *testing.T) {
		r, a, b, os := poolSetup(t, 2)
		r.PoolOrders(ctx, ids(os[0]), a.ID)
		r.PoolOrders(ctx, ids(os[1]), b.ID)
		n, cleared, err := r.AutoClockOut(ctx, time.Now())
		if err != nil || n != 2 || len(cleared) != 2 {
			t.Fatalf("reset: %v n=%d cleared=%d", err, n, len(cleared))
		}
		unpooled(t, r, os[0].Order.ID)
		unpooled(t, r, os[1].Order.ID)
	})
	t.Run("close batch", func(t *testing.T) {
		r, a, _, os := poolSetup(t, 1)
		r.PoolOrders(ctx, ids(os...), a.ID)
		_, finished, err := r.CloseBatch(ctx, true)
		if err != nil || len(finished) != 1 || finished[0].Status.PoolDriverID != "" {
			t.Fatalf("close: %v %+v", err, finished)
		}
		unpooled(t, r, os[0].Order.ID)
	})
	t.Run("old assign", func(t *testing.T) {
		r, a, b, os := poolSetup(t, 1)
		r.MarkReady(ctx, os[0].Order.ID)
		r.PoolOrders(ctx, ids(os...), a.ID)
		sent, err := r.AssignDriver(ctx, b.ID, ids(os...))
		if err != nil || sent[0].Status.PoolDriverID != "" || sent[0].Status.DriverID != b.ID {
			t.Fatalf("assign: %v %+v", err, sent)
		}
		unpooled(t, r, os[0].Order.ID)
	})
	t.Run("cancel", func(t *testing.T) {
		// Only never-accepted orders are cancelled (by close batch); they can't
		// be pooled, so the pool is empty by construction — still cleared.
		r, a, _, os := poolSetup(t, 1)
		r.PoolOrders(ctx, ids(os...), a.ID)
		waiting := arrive(t, r, "delivery")
		_, finished, err := r.CloseBatch(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, so := range finished {
			if so.Order.ID == waiting.Order.ID && (so.Status.State != order.StateCancelled || so.Status.PoolDriverID != "") {
				t.Fatalf("cancelled: %+v", so.Status)
			}
		}
	})
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestUpsertGeoAndTimesBothDirections(t *testing.T) {
	ctx := context.Background()
	r, _, _, os := poolSetup(t, 2)
	a, b := os[0].Order.ID, os[1].Order.ID

	if _, err := r.UpsertGeo(ctx, order.GeoUpdate{OrderID: uuid.NewString(), State: order.GeoPending}); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
	so, err := r.UpsertGeo(ctx, order.GeoUpdate{OrderID: a, State: order.GeoLocated, Lat: f64(40.66), Lng: f64(-74.11),
		Quality: "exact", DistanceM: 3700, DurationS: 420, Polyline: "abc", StoreToS: i64(420), ToStoreS: i64(430),
		Times: []order.GeoTime{{Other: b, ToS: i64(300), FromS: i64(310)}}})
	if err != nil {
		t.Fatal(err)
	}
	if so.Geo == nil || !so.Geo.Located() || *so.Geo.Lat != 40.66 || so.Geo.Quality != "exact" || so.Geo.DurationS != 420 ||
		*so.Geo.StoreToS != 420 || *so.Geo.ToStoreS != 430 || so.Geo.Polyline != "abc" {
		t.Fatalf("geo: %+v", so.Geo)
	}
	times, err := r.DriveTimes(ctx, []string{a, b})
	if err != nil || times[a][b] != 300 || times[b][a] != 310 {
		t.Fatalf("times: %v %v", err, times)
	}
	// B located later overwrites the pair with its own figures (both ways).
	if _, err := r.UpsertGeo(ctx, order.GeoUpdate{OrderID: b, State: order.GeoLocated, Lat: f64(40.67), Lng: f64(-74.12),
		Quality: "approx", Times: []order.GeoTime{{Other: a, ToS: i64(305), FromS: i64(299)}}}); err != nil {
		t.Fatal(err)
	}
	times, _ = r.DriveTimes(ctx, []string{a, b})
	if times[b][a] != 305 || times[a][b] != 299 {
		t.Fatalf("times after B: %v", times)
	}
	// Geo rides along with the order everywhere it is read.
	if got := get(t, r, a); got.Geo == nil || got.Geo.State != order.GeoLocated {
		t.Fatalf("GetByID geo: %+v", got.Geo)
	}
	list, _ := r.ListPage(ctx, 0, 10)
	n := 0
	for _, so := range list {
		if so.Geo != nil {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("ListPage geo on %d orders", n)
	}
	// not_found: no coordinates, quality empty.
	so, err = r.UpsertGeo(ctx, order.GeoUpdate{OrderID: a, State: order.GeoNotFound})
	if err != nil || so.Geo.Located() || so.Geo.Lat != nil || so.Geo.Quality != "" {
		t.Fatalf("not_found: %v %+v", err, so.Geo)
	}
}

func TestMissingGeoReadsPendingThenUnavailable(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	fresh := arrive(t, r, "delivery")
	if fresh.Geo == nil || fresh.Geo.State != order.GeoPending || fresh.Geo.Lat != nil {
		t.Fatalf("fresh delivery: %+v", fresh.Geo)
	}
	if b, _ := json.Marshal(fresh); !strings.Contains(string(b), `"geo":{"state":"pending"}`) {
		t.Fatalf("agent-made geo must be just the state: %s", b)
	}
	old := arrive(t, r, "delivery")
	r.db.Exec(`UPDATE orders SET arrival_date = ? WHERE id = ?`, fmtTS(time.Now().Add(-order.GeoPendingWindow-time.Minute)), old.Order.ID)
	if got := get(t, r, old.Order.ID); got.Geo == nil || got.Geo.State != order.GeoUnavailable {
		t.Fatalf("old delivery: %+v", got.Geo)
	}
	pickup := arrive(t, r, "pickup")
	list, _ := r.ListPage(ctx, 0, 10)
	for _, so := range list {
		switch so.Order.ID {
		case pickup.Order.ID:
			if so.Geo != nil {
				t.Fatalf("pickup geo must stay omitted: %+v", so.Geo)
			}
		case old.Order.ID:
			if so.Geo.State != order.GeoUnavailable {
				t.Fatalf("list old: %+v", so.Geo)
			}
		}
	}
	// A row from main wins, whatever the age.
	so, _ := r.UpsertGeo(ctx, order.GeoUpdate{OrderID: old.Order.ID, State: order.GeoNotFound})
	if so.Geo.State != order.GeoNotFound {
		t.Fatalf("main's row: %+v", so.Geo)
	}
}

func TestAutoPlanStoreAndConfirm(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 4)
	now := time.Now()
	if err := r.SaveAutoPlan(ctx, "plan-1", now, []byte(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if raw, err := r.GetAutoPlan(ctx, "plan-1"); err != nil || string(raw) != `{"x":1}` {
		t.Fatalf("get plan: %v %s", err, raw)
	}
	if _, err := r.GetAutoPlan(ctx, "nope"); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing plan: %v", err)
	}
	// os[1] got pooled to b meanwhile; os[3] was sent; b's run is fine.
	r.PoolOrders(ctx, ids(os[1]), b.ID)
	r.MarkReady(ctx, os[3].Order.ID)
	r.AssignDriver(ctx, b.ID, ids(os[3])) // b is out now
	applied, skipped, err := r.ConfirmAutoPlan(ctx, "plan-1", []order.PlanRun{
		{DriverID: a.ID, SuggestedIDs: ids(os[0], os[1], os[3])},
		{DriverID: b.ID, SuggestedIDs: ids(os[2])},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0].Order.ID != os[0].Order.ID || applied[0].Status.PoolDriverID != a.ID {
		t.Fatalf("applied: %+v", applied)
	}
	want := map[string]string{os[1].Order.ID: order.SkipAlreadyPooled, os[3].Order.ID: order.SkipInvalidState,
		os[2].Order.ID: order.SkipDriverNotAvailable}
	if len(skipped) != 3 {
		t.Fatalf("skipped: %+v", skipped)
	}
	for _, s := range skipped {
		if want[s.OrderID] != s.Reason {
			t.Fatalf("skip %s: %s (want %s)", s.OrderID, s.Reason, want[s.OrderID])
		}
	}
	if _, _, err := r.ConfirmAutoPlan(ctx, "nope", nil, now); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("confirm missing: %v", err)
	}
}

func TestDeliveryPrefsRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	p, err := r.DeliveryPrefs(ctx)
	if err != nil || p != order.DefaultDeliveryPrefs {
		t.Fatalf("defaults: %v %+v", err, p)
	}
	if err := r.SetDeliveryPrefs(ctx, order.DeliveryPrefs{SoonReadyMinutes: 0, DeliveryTargetMinutes: 60}); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.DeliveryPrefs(ctx); p.SoonReadyMinutes != 0 || p.DeliveryTargetMinutes != 60 {
		t.Fatalf("saved: %+v", p)
	}
}

func TestTrimDropsOrphanGeo(t *testing.T) {
	ctx := context.Background()
	r, _, _, os := poolSetup(t, 1)
	r.UpsertGeo(ctx, order.GeoUpdate{OrderID: os[0].Order.ID, State: order.GeoLocated, Lat: f64(1), Lng: f64(2),
		Times: []order.GeoTime{{Other: uuid.NewString(), ToS: i64(5)}}})
	for range 2 {
		if _, _, err := r.CloseBatch(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.TrimBatches(ctx, 0, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var n int
	r.db.QueryRow(`SELECT (SELECT count(*) FROM order_geo) + (SELECT count(*) FROM order_times)`).Scan(&n)
	if n != 0 {
		t.Fatalf("orphan geo/times left: %d", n)
	}
}

func TestEstimateStops(t *testing.T) {
	dep := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	stops := order.EstimateStops([]string{"a", "b", "c"}, []string{"b", "a"}, dep,
		map[string]int64{"b": 300}, map[string]map[string]int64{"b": {"a": 60}})
	if len(stops) != 3 || stops[0].OrderID != "b" || stops[1].OrderID != "a" || stops[2].OrderID != "c" {
		t.Fatalf("order: %+v", stops)
	}
	if !stops[0].ETA.Equal(dep.Add(300*time.Second)) || !stops[1].ETA.Equal(dep.Add(480*time.Second)) || stops[2].ETA != nil {
		t.Fatalf("etas: %v %v %v", stops[0].ETA, stops[1].ETA, stops[2].ETA)
	}
}

func TestSetPoolSeqEventsAndClearing(t *testing.T) {
	ctx := context.Background()
	r, a, _, os := poolSetup(t, 2)
	r.PoolOrders(ctx, ids(os...), a.ID)
	changed, err := r.SetPoolSeq(ctx, a.ID, ids(os[1], os[0]))
	if err != nil || len(changed) != 2 || *changed[0].Status.PoolSeq != 1 || changed[0].Order.ID != os[1].Order.ID {
		t.Fatalf("seq: %v %+v", err, changed)
	}
	if ev := lastStatusEvent(t, r, os[0].Order.ID); ev["agent_status"].(map[string]any)["pool_seq"] != float64(2) {
		t.Fatalf("agent_status must carry pool_seq: %v", ev)
	}
	if again, _ := r.SetPoolSeq(ctx, a.ID, ids(os[1], os[0])); len(again) != 0 {
		t.Fatalf("unchanged numbers write nothing: %d", len(again))
	}
	// Another driver's id never numbers this pool; leaving the pool clears it.
	if other, _ := r.SetPoolSeq(ctx, "someone", ids(os[0])); len(other) != 0 {
		t.Fatal("wrong driver must not renumber")
	}
	r.PoolOrders(ctx, ids(os[0]), "")
	if got := get(t, r, os[0].Order.ID); got.Status.PoolSeq != nil || got.Status.PoolPlanID != "" {
		t.Fatalf("unassigned: %+v", got.Status)
	}
}
