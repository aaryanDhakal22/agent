package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

var counter = order.MarkedBy{Kind: order.ByTablet, Name: "Counter", DeviceID: "dev-1"}

// MARK DELIVERED: an open delivery (cooking, ready or pooled) is delivered
// with no driver, out of its pool, with who / when / who was available;
// Back to deliveries puts it back the way it was.
func TestMarkDeliveredNoDriverAndBack(t *testing.T) {
	ctx := context.Background()
	r, ann, bob, ds := poolSetup(t, 3)
	cooking, ready, pooled := ds[0], ds[1], ds[2]
	if _, err := r.MarkReady(ctx, ready.Order.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PoolOrders(ctx, ids(pooled), ann.ID); err != nil {
		t.Fatal(err)
	}

	for _, so := range []order.StoredOrder{cooking, ready, pooled} {
		got, err := r.MarkDeliveredNoDriver(ctx, so.Order.ID, counter)
		if err != nil {
			t.Fatal(err)
		}
		st := got.Status
		if !got.IsUnassigned() || st.DeliveredAt == nil || st.PoolDriverID != "" || st.DriverID != "" {
			t.Fatalf("not Unassigned: %+v", st)
		}
		md := st.MarkedDelivered
		if md.By != counter || !reflect.DeepEqual(md.Available, []string{ann.Name, bob.Name}) {
			t.Fatalf("marked = %+v", md)
		}
		if ev := lastStatusEvent(t, r, so.Order.ID); ev["status"] != string(order.StateDelivered) {
			t.Fatalf("event = %v", ev)
		}
	}
	if get(t, r, cooking.Order.ID).Status.MarkedDelivered.PrevState != order.StateAccepted {
		t.Fatal("prev state of a cooking order")
	}
	un, err := r.ListUnassigned(ctx, 0)
	if err != nil || len(un) != 3 {
		t.Fatalf("unassigned = %d %v", len(un), err)
	}
	// Twice: it isn't open any more.
	if _, err := r.MarkDeliveredNoDriver(ctx, ready.Order.ID, counter); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("again: %v", err)
	}
	// A pickup can't be.
	pick := accept(t, r, arrive(t, r, "pickup"))
	if _, err := r.MarkDeliveredNoDriver(ctx, pick.Order.ID, counter); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("pickup: %v", err)
	}

	// Back to deliveries: cooking -> accepted, ready -> ready; main hears
	// order.not_delivered retry.
	back, err := r.Undeliver(ctx, cooking.Order.ID)
	if err != nil || back.Status.State != order.StateAccepted || back.Status.MarkedDelivered != nil || back.Status.DeliveredAt != nil {
		t.Fatalf("back = %+v %v", back, err)
	}
	if back, err = r.Undeliver(ctx, ready.Order.ID); err != nil || back.Status.State != order.StateReady {
		t.Fatalf("back ready = %+v %v", back, err)
	}
	var nd int
	for _, e := range eventsOf(t, r) {
		if e["type"] == order.FloorEventNotDelivered && e["action"] == order.NotDeliveredRetry {
			nd++
		}
	}
	if nd != 2 {
		t.Fatalf("not_delivered events = %d", nd)
	}
	if _, err := r.Undeliver(ctx, ready.Order.ID); !errors.Is(err, order.ErrNotUnassigned) {
		t.Fatalf("undeliver a ready one: %v", err)
	}
}

// The Drivers tab: delivered orders move between drivers and to / from
// Unassigned; an out order moves to another driver's run (the first driver
// is back in the queue when it was his last stop, an available one goes
// out); nothing moves to a clocked-out driver.
func TestReassignDeliveries(t *testing.T) {
	ctx := context.Background()
	r, ann, bob, ds := poolSetup(t, 3)
	for _, so := range ds {
		if _, err := r.MarkReady(ctx, so.Order.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	// Ann takes two out and comes back; then the third goes out with her.
	if _, err := r.AssignDriver(ctx, ann.ID, ids(ds[0], ds[1])); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DriverReturned(ctx, ann.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AssignDriver(ctx, ann.ID, ids(ds[2])); err != nil {
		t.Fatal(err)
	}

	// Delivered #0 -> Bob.
	moved, changed, err := r.ReassignDeliveries(ctx, ids(ds[0]), bob.ID, counter)
	if err != nil || len(moved) != 1 || changed || moved[0].Status.DriverID != bob.ID || moved[0].Status.DriverName != "Bob" {
		t.Fatalf("moved = %+v changed=%v %v", moved, changed, err)
	}
	if lastStatusEvent(t, r, ds[0].Order.ID)["driver_name"] != "Bob" {
		t.Fatal("main not told")
	}
	// Bob's again: nothing moves.
	if moved, _, err := r.ReassignDeliveries(ctx, ids(ds[0]), bob.ID, counter); err != nil || len(moved) != 0 {
		t.Fatalf("same driver: %+v %v", moved, err)
	}

	// Delivered #1 -> Unassigned (from Ann), then on to Bob.
	moved, _, err = r.ReassignDeliveries(ctx, ids(ds[1]), "", counter)
	if err != nil || len(moved) != 1 || !moved[0].IsUnassigned() || moved[0].Status.MarkedDelivered.FromDriver != "Ann" {
		t.Fatalf("to unassigned = %+v %v", moved, err)
	}
	moved, _, _ = r.ReassignDeliveries(ctx, ids(ds[1]), bob.ID, counter)
	if moved[0].IsUnassigned() || moved[0].Status.MarkedDelivered == nil {
		t.Fatalf("assigned keeps the record: %+v", moved[0].Status)
	}

	// An out order can't go to Unassigned.
	if _, _, err := r.ReassignDeliveries(ctx, ids(ds[2]), "", counter); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("out to unassigned: %v", err)
	}
	// Out #2 (Ann's last stop) -> Bob (available): Bob is out, Ann back.
	moved, changed, err = r.ReassignDeliveries(ctx, ids(ds[2]), bob.ID, counter)
	if err != nil || !changed || moved[0].Status.State != order.StateOnDelivery || moved[0].Status.DriverID != bob.ID {
		t.Fatalf("out move = %+v changed=%v %v", moved, changed, err)
	}
	drivers, _ := r.ListDrivers(ctx)
	status := map[string]string{}
	for _, d := range drivers {
		status[d.Name] = d.Status
	}
	if status["Ann"] != order.DriverAvailable || status["Bob"] != order.DriverOut {
		t.Fatalf("drivers = %v", status)
	}
	if moved[0].Status.RunSeq == nil || *moved[0].Status.RunSeq != 1 {
		t.Fatalf("run seq = %v", moved[0].Status.RunSeq)
	}

	// Ann clocks out: nothing moves to her.
	if _, _, err := r.ClockOut(ctx, ann.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ReassignDeliveries(ctx, ids(ds[0]), ann.ID, counter); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("to clocked out: %v", err)
	}
	// ... unless the POS says so (the record follows the POS).
	if moved, _, err := r.ReassignDeliveries(ctx, ids(ds[0]), ann.ID, order.MarkedBy{Kind: order.ByPOS, Name: "POS"}); err != nil || len(moved) != 1 {
		t.Fatalf("POS to clocked out: %+v %v", moved, err)
	}
	// A cooking order isn't delivered or out.
	cook := accept(t, r, arrive(t, r, "delivery"))
	if _, _, err := r.ReassignDeliveries(ctx, ids(cook), bob.ID, counter); !errors.Is(err, order.ErrNotOutOrDelivered) {
		t.Fatalf("cooking: %v", err)
	}
}

// A POS delivery's move goes to the hub (order.driver_changed with from /
// to); one the POS itself moved doesn't.
func TestReassignPOSFeed(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	ann, _ := r.CreateDriver(ctx, "Ann", "11")
	bob, _ := r.CreateDriver(ctx, "Bob", "12")
	clockIn(t, r, ann.ID, bob.ID)
	so := upsertPOS(t, r, "921", kitchenCheck("delivery", "h1", pizza)).Order
	if _, err := r.MarkDeliveredNoDriver(ctx, so.Order.ID, counter); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ReassignDeliveries(ctx, ids(so), ann.ID, counter); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ReassignDeliveries(ctx, ids(so), bob.ID, order.MarkedBy{Kind: order.ByPOS, Name: "POS"}); err != nil {
		t.Fatal(err)
	}
	evs, _, err := r.ListFeed(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var got []order.OrderDriverChangedData
	for _, e := range evs {
		if e.Type == order.FeedOrderDriverChanged {
			var d order.OrderDriverChangedData
			if err := json.Unmarshal(e.Data, &d); err != nil {
				t.Fatal(err)
			}
			got = append(got, d)
		}
	}
	if len(got) != 1 || got[0].From != nil || got[0].To == nil || *got[0].To.PosNumber != "11" || got[0].ExtRef == nil || *got[0].ExtRef != "921" {
		t.Fatalf("feed = %+v", got)
	}
}
