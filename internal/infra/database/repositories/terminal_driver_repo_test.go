package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// The terminal driver view's floor transitions (contract/agent.md §2.10b)
// and the auto-close slot (§2.10c).

func ready(t *testing.T, r *OrderRepository, so order.StoredOrder) order.StoredOrder {
	t.Helper()
	got, err := r.MarkReady(context.Background(), so.Order.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return *got
}

// The Dispatch claim: unassigned deliveries past their promised time become his,
// delivered; he never goes out. All or nothing, first come first served.
func TestClaimDeliveredLateUnassignedOnly(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 6)
	// os[0] cooking, os[1] ready, os[2] marked delivered (no driver): unassigned.
	// os[3] ready in b's pool; os[4] out with b; os[5] arrived (never accepted).
	ready(t, r, os[1])
	if _, err := r.MarkDeliveredNoDriver(ctx, os[2].Order.ID, order.MarkedBy{Kind: order.ByTablet, Name: "Front"}); err != nil {
		t.Fatal(err)
	}
	ready(t, r, os[3])
	if _, err := r.PoolOrders(ctx, ids(os[3]), b.ID); err != nil {
		t.Fatal(err)
	}
	ready(t, r, os[4])
	off, _ := r.CreateDriver(ctx, "Cal") // never clocked in
	if _, _, err := r.SendOut(ctx, b.ID, ids(os[4]), false, order.RunMeta{}); err != nil {
		t.Fatal(err)
	}
	fresh := arrive(t, r, "delivery")
	by := order.MarkedBy{Kind: order.ByTerminal, Name: "Ann", DeviceID: "dev-2"}
	// The claim rule: past the order's promised time (estimated_ready_at = accept + prep).
	early := order.ClaimRule{Now: time.Now(), Fallback: 45 * time.Minute}                   // nothing is late yet
	late := order.ClaimRule{Now: time.Now().Add(6 * time.Hour), Fallback: 45 * time.Minute} // everything is

	var tk *order.ErrTaken
	if _, err := r.ClaimDelivered(ctx, a.ID, ids(os[0], os[1]), early, by); !errors.As(err, &tk) || len(tk.IDs) != 2 {
		t.Fatalf("too new: want ErrTaken{os0, os1}, got %v", err)
	}
	_, err := r.ClaimDelivered(ctx, a.ID, ids(os[0], os[3], os[4], fresh), late, by)
	if !errors.As(err, &tk) || len(tk.IDs) != 3 || tk.IDs[0] != os[3].Order.ID || tk.IDs[1] != os[4].Order.ID || tk.IDs[2] != fresh.Order.ID {
		t.Fatalf("pooled / out / unaccepted: want ErrTaken{os3, os4, fresh}, got %v", err)
	}
	if got := get(t, r, os[0].Order.ID); got.Status.State != order.StateAccepted || got.Status.DriverID != "" {
		t.Fatalf("a refused claim changes nothing: %+v", got.Status)
	}
	if _, err := r.ClaimDelivered(ctx, a.ID, []string{"00000000-0000-0000-0000-000000000000"}, late, by); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := r.ClaimDelivered(ctx, a.ID, nil, late, by); !errors.Is(err, order.ErrNoOrders) {
		t.Fatalf("empty claim: %v", err)
	}

	before := len(eventsOfType(t, r, order.FloorEventOrderStatus))
	got, err := r.ClaimDelivered(ctx, a.ID, ids(os[0], os[1], os[2]), late, by)
	if err != nil || len(got) != 3 {
		t.Fatalf("claim: %v %d", err, len(got))
	}
	prev := []order.State{order.StateAccepted, order.StateReady, order.StateAccepted}
	for i, so := range got {
		st := so.Status
		if st.State != order.StateDelivered || st.DriverID != a.ID || st.DriverName != "Ann" || st.DeliveredAt == nil || st.OutAt != nil || so.IsUnassigned() {
			t.Fatalf("claimed %d: %+v", i, st)
		}
		if i < 2 && (st.MarkedDelivered == nil || st.MarkedDelivered.By.Kind != order.ByTerminal || st.MarkedDelivered.PrevState != prev[i]) {
			t.Fatalf("marked by the terminal %d: %+v", i, st.MarkedDelivered)
		}
	}
	if got[2].Status.MarkedDelivered.By.Kind != order.ByTablet {
		t.Fatalf("an Unassigned one keeps how it got there: %+v", got[2].Status.MarkedDelivered)
	}
	if n := len(eventsOfType(t, r, order.FloorEventOrderStatus)) - before; n != 3 {
		t.Fatalf("order.status events = %d, want 3", n)
	}
	if ev := lastStatusEvent(t, r, os[1].Order.ID); ev["status"] != "delivered" || ev["driver_name"] != "Ann" {
		t.Fatalf("main hears delivered by him: %v", ev)
	}
	if len(eventsOfType(t, r, order.FloorEventDeliveryRun)) != 1 { // b's send-out only: a claim is no run
		t.Fatal("a claim must not write a delivery.run")
	}
	ds, _ := r.ListDrivers(ctx)
	for _, d := range ds {
		if d.ID == a.ID && d.Status != order.DriverAvailable {
			t.Fatalf("he never goes out: %+v", d)
		}
	}
	// Again by him: nothing changes; by someone else: taken.
	if again, err := r.ClaimDelivered(ctx, a.ID, ids(os[1]), late, by); err != nil || len(again) != 0 {
		t.Fatalf("his own again: %v %d", err, len(again))
	}
	if _, err := r.ClaimDelivered(ctx, b.ID, ids(os[1]), late, by); !errors.As(err, &tk) {
		t.Fatalf("someone else's: %v", err)
	}
	// A driver who is off may claim too and stays off; the orders are on his report.
	extra := accept(t, r, arrive(t, r, "delivery"))
	if _, err := r.ClaimDelivered(ctx, off.ID, ids(extra), late, by); err != nil {
		t.Fatal(err)
	}
	reps, err := r.DriverReports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, rep := range reps {
		counts[rep.DriverID] = len(rep.Lines)
	}
	if counts[a.ID] != 3 || counts[off.ID] != 1 {
		t.Fatalf("report lines: %v", counts)
	}
	ds, _ = r.ListDrivers(ctx)
	for _, d := range ds {
		if d.ID == off.ID && d.Status != order.DriverOff {
			t.Fatalf("an off driver stays off: %+v", d)
		}
	}
}

func TestDeliverOneReturnsTheDriverOnHisLastStop(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 2)
	ready(t, r, os[0])
	ready(t, r, os[1])
	if _, _, err := r.SendOut(ctx, a.ID, ids(os[0], os[1]), false, order.RunMeta{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.DeliverOne(ctx, b.ID, os[0].Order.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("someone else's order: %v", err)
	}
	so, returned, err := r.DeliverOne(ctx, a.ID, os[0].Order.ID)
	if err != nil || returned || so.Status.State != order.StateDelivered || so.Status.DeliveredAt == nil {
		t.Fatalf("first close: %v returned=%v %+v", err, returned, so)
	}
	if ev := lastStatusEvent(t, r, os[0].Order.ID); ev["status"] != "delivered" {
		t.Fatalf("main hears delivered: %v", ev)
	}
	if _, _, err := r.DeliverOne(ctx, a.ID, os[0].Order.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("closing twice: %v", err)
	}
	_, returned, err = r.DeliverOne(ctx, a.ID, os[1].Order.ID)
	if err != nil || !returned {
		t.Fatalf("last close: %v returned=%v", err, returned)
	}
	ds, _ := r.ListDrivers(ctx)
	for _, d := range ds {
		if d.ID == a.ID && d.Status != order.DriverAvailable {
			t.Fatalf("back in the queue: %+v", d)
		}
	}
}

// A cash delivery on his report: the cash collected for the order only, no tip (cash tips are kept out of the
// system, the owner 2026-11-12; the door tip was retired).
func TestCashDeliveryOnHisReport(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	d, _ := r.CreateDriver(ctx, "Dan")
	clockIn(t, r, d.ID)
	cash := arriveDelivery(t, r, "Erik Davis", 0, 2000, true)
	accept(t, r, cash)
	ready(t, r, cash)
	if _, err := r.AssignDriver(ctx, d.ID, ids(cash)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.DeliverOne(ctx, d.ID, cash.Order.ID); err != nil {
		t.Fatal(err)
	}
	reps, err := r.DriverReports(ctx, 0)
	if err != nil || len(reps) != 1 || len(reps[0].Lines) != 1 {
		t.Fatalf("reports: %v %d", err, len(reps))
	}
	rep := reps[0]
	rep.Finish(0)
	l := rep.Lines[0]
	if l.OrderTipCents != 0 || l.TipCents != 0 || l.CashCents != 2000 || rep.TipCents != 0 || rep.OwedCents != 2000 {
		t.Fatalf("cash on the report: line %+v report %+v", l, rep)
	}
	if ev := eventsOfType(t, r, order.FloorEventOrderTip); len(ev) != 0 {
		t.Fatalf("an order.tip went to main for a cash delivery: %v", ev)
	}
}

func TestDriverLinksAndAutoCloseSlot(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	if id, err := r.TerminalDriverLink(ctx, "s1"); err != nil || id != "" {
		t.Fatalf("no link yet: %q %v", id, err)
	}
	if err := r.LinkTerminalDriver(ctx, "s1", "d1"); err != nil {
		t.Fatal(err)
	}
	// d1 moves to s2: s1 loses it.
	if err := r.LinkTerminalDriver(ctx, "s2", "d1"); err != nil {
		t.Fatal(err)
	}
	all, err := r.TerminalDriverLinks(ctx)
	if err != nil || len(all) != 1 || all["s2"] != "d1" {
		t.Fatalf("links: %v %v", all, err)
	}
	if last, err := r.AutoCloseLast(ctx); err != nil || !last.IsZero() {
		t.Fatalf("no slot yet: %v %v", last, err)
	}
	slot := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	if err := r.SetAutoCloseLast(ctx, slot); err != nil {
		t.Fatal(err)
	}
	if last, err := r.AutoCloseLast(ctx); err != nil || !last.Equal(slot) {
		t.Fatalf("slot: %v %v", last, err)
	}
}
