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

func TestClaimSendsOutReadyUnassignedOnly(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 5)
	// os[0], os[1] ready + unassigned; os[2] ready in b's pool; os[3] cooking; os[4] cooking in a's own pool.
	for _, so := range os[:3] {
		ready(t, r, so)
	}
	if _, err := r.PoolOrders(ctx, ids(os[2]), b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PoolOrders(ctx, ids(os[4]), a.ID); err != nil {
		t.Fatal(err)
	}
	claim := order.RunMeta{Claim: true}

	// Another driver's pool and a cooking order are taken: all or nothing.
	_, _, err := r.SendOut(ctx, a.ID, ids(os[0], os[2], os[3]), false, claim)
	var tk *order.ErrTaken
	if !errors.As(err, &tk) || len(tk.IDs) != 2 || tk.IDs[0] != os[2].Order.ID || tk.IDs[1] != os[3].Order.ID {
		t.Fatalf("want ErrTaken{os2, os3}, got %v", err)
	}
	if got := get(t, r, os[0].Order.ID); got.Status.State != order.StateReady || got.Status.PoolDriverID != "" {
		t.Fatalf("a refused claim must change nothing: %+v", got.Status)
	}
	if _, _, err := r.SendOut(ctx, a.ID, nil, false, claim); !errors.Is(err, order.ErrNoOrders) {
		t.Fatalf("an empty claim: %v", err)
	}

	sent, readied, err := r.SendOut(ctx, a.ID, ids(os[0], os[1]), false, claim)
	if err != nil || len(sent) != 2 || len(readied) != 0 {
		t.Fatalf("claim: %v sent=%d readied=%d", err, len(sent), len(readied))
	}
	for _, so := range sent {
		if so.Status.State != order.StateOnDelivery || so.Status.DriverID != a.ID {
			t.Fatalf("claimed order: %+v", so.Status)
		}
	}
	// His own cooking pool order stayed behind, unassigned.
	if got := get(t, r, os[4].Order.ID); got.Status.State != order.StateAccepted || got.Status.PoolDriverID != "" {
		t.Fatalf("his cooking pool order: %+v", got.Status)
	}
	if n := len(eventsOfType(t, r, order.FloorEventDeliveryRun)); n != 1 {
		t.Fatalf("delivery.run events = %d", n)
	}
	// He is out now: a second claim (b's order is b's anyway) is refused.
	if _, _, err := r.SendOut(ctx, a.ID, ids(os[1]), false, claim); !errors.Is(err, order.ErrDriverNotAvailable) {
		t.Fatalf("claim while out: %v", err)
	}
	// An order claimed first is taken for the next driver.
	_, _, err = r.SendOut(ctx, b.ID, ids(os[0]), false, claim)
	if !errors.As(err, &tk) || tk.IDs[0] != os[0].Order.ID {
		t.Fatalf("second driver: %v", err)
	}
}

func TestDeliverOneReturnsTheDriverOnHisLastStop(t *testing.T) {
	ctx := context.Background()
	r, a, b, os := poolSetup(t, 2)
	ready(t, r, os[0])
	ready(t, r, os[1])
	if _, _, err := r.SendOut(ctx, a.ID, ids(os[0], os[1]), false, order.RunMeta{Claim: true}); err != nil {
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

func TestDoorTipOnHisReport(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	d, _ := r.CreateDriver(ctx, "Dan", "")
	clockIn(t, r, d.ID)
	cash := arriveDelivery(t, r, "Erik Davis", 0, 2000, true)
	accept(t, r, cash)
	ready(t, r, cash)
	if _, err := r.SetDoorTip(ctx, cash.Order.ID, 300); !errors.Is(err, order.ErrNotTippable) {
		t.Fatalf("not out yet: %v", err)
	}
	if _, err := r.AssignDriver(ctx, d.ID, ids(cash)); err != nil {
		t.Fatal(err)
	}
	so, err := r.SetDoorTip(ctx, cash.Order.ID, 300)
	if err != nil || so.Status.DoorTipCents != 300 {
		t.Fatalf("door tip: %v %+v", err, so)
	}
	if _, _, err := r.DeliverOne(ctx, d.ID, cash.Order.ID); err != nil {
		t.Fatal(err)
	}
	if so, err = r.SetDoorTip(ctx, cash.Order.ID, 300); err != nil || so.Status.State != order.StateDelivered { // still tippable once closed
		t.Fatalf("door tip after close: %v", err)
	}
	reps, err := r.DriverReports(ctx, 0)
	if err != nil || len(reps) != 1 || len(reps[0].Lines) != 1 {
		t.Fatalf("reports: %v %d", err, len(reps))
	}
	rep := reps[0]
	rep.Finish(0)
	l := rep.Lines[0]
	if l.DoorTipCents != 300 || l.OrderTipCents != 300 || l.TipCents != 0 || l.CashCents != 2000 || rep.DoorTipCents != 300 || rep.OwedCents != 2000 {
		t.Fatalf("door tip on the report: line %+v report %+v", l, rep)
	}
	if _, err := r.SetDoorTip(ctx, "00000000-0000-0000-0000-000000000000", 1); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
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
