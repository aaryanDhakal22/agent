package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

// 2.5.0 website orders: ADD TIP, the tip lock, CANCEL ORDER, batch.closed.

func arriveWeb(t *testing.T, r *OrderRepository, source, fulfillment string, card bool) order.StoredOrder {
	t.Helper()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
		Source: source, SourceEventID: uuid.NewString()[:6], PlacedAt: time.Now().UTC(),
		Fulfillment: order.Fulfillment{Type: fulfillment}, Money: order.Money{Total: 3000, Tip: 300}}
	if card {
		s.Payments = []order.PaymentSummary{{Method: "card", AmountCents: 3000}}
	}
	if _, err := r.UpsertArrived(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	return get(t, r, s.ID)
}

func eventsOfType(t *testing.T, r *OrderRepository, typ string) []map[string]any {
	var out []map[string]any
	for _, e := range eventsOf(t, r) {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func TestSetAddedTipRules(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	web := accept(t, r, arriveWeb(t, r, "web", "delivery", true))
	so, err := r.SetAddedTip(ctx, web.Order.ID, 450)
	if err != nil || so.Status.AddedTipCents != 450 {
		t.Fatalf("tip: %v %+v", err, so)
	}
	tips := eventsOfType(t, r, order.FloorEventOrderTip)
	if len(tips) != 1 || tips[0]["order_id"] != web.Order.ID || tips[0]["tip_cents"] != float64(450) || tips[0]["at"] == nil {
		t.Fatalf("order.tip events: %v", tips)
	}
	// Unchanged: no second event. Absolute: 0 clears it.
	if _, err := r.SetAddedTip(ctx, web.Order.ID, 450); err != nil {
		t.Fatal(err)
	}
	if so, err = r.SetAddedTip(ctx, web.Order.ID, 0); err != nil || so.Status.AddedTipCents != 0 {
		t.Fatalf("clear: %v %+v", err, so)
	}
	if n := len(eventsOfType(t, r, order.FloorEventOrderTip)); n != 2 {
		t.Fatalf("order.tip events = %d, want 2", n)
	}
	for name, so := range map[string]order.StoredOrder{
		"cash web":    accept(t, r, arriveWeb(t, r, "web", "delivery", false)),
		"online card": accept(t, r, arriveWeb(t, r, "online", "delivery", true)),
	} {
		if _, err := r.SetAddedTip(ctx, so.Order.ID, 100); !errors.Is(err, order.ErrNotTippable) {
			t.Errorf("%s: %v, want ErrNotTippable", name, err)
		}
	}
	if _, err := r.SetAddedTip(ctx, uuid.NewString(), 100); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestTipLockOnClockOutAndCloseBatch(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	d, _ := r.CreateDriver(ctx, "Sam", "")
	other, _ := r.CreateDriver(ctx, "Ana", "")
	clockIn(t, r, d.ID, other.ID)
	a := accept(t, r, arriveWeb(t, r, "web", "delivery", true))
	b := accept(t, r, arriveWeb(t, r, "web", "delivery", true))
	pick := accept(t, r, arriveWeb(t, r, "web", "pickup", true))
	for _, so := range []order.StoredOrder{a, b} {
		if _, err := r.MarkReady(ctx, so.Order.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.AssignDriver(ctx, d.ID, []string{a.Order.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AssignDriver(ctx, other.ID, []string{b.Order.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DriverReturned(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	_, locked, err := r.ClockOut(ctx, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(locked) != 1 || locked[0].Order.ID != a.Order.ID || !locked[0].Status.TipLocked {
		t.Fatalf("clock-out locks the driver's deliveries: %+v", locked)
	}
	if _, err := r.SetAddedTip(ctx, a.Order.ID, 100); !errors.Is(err, order.ErrTipLocked) {
		t.Fatalf("tip after clock-out: %v", err)
	}
	if _, err := r.SetAddedTip(ctx, b.Order.ID, 100); err != nil {
		t.Fatalf("another driver's delivery stays open: %v", err)
	}
	if _, err := r.DriverReturned(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	reps, err := r.DriverReports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reps {
		rep.Finish(0)
		switch rep.DriverID {
		case d.ID:
			if rep.PendingTipCents != 0 || rep.TipCents != 300 || rep.Lines[0].TipPending {
				t.Errorf("Sam: %+v", rep)
			}
		case other.ID:
			if rep.PendingTipCents != 400 || rep.TipCents != 400 || !rep.Lines[0].TipPending || rep.Lines[0].AddedTipCents != 100 {
				t.Errorf("Ana: %+v", rep)
			}
		}
	}
	// Close batch locks everything left and writes batch.closed last.
	closed, _, err := r.CloseBatch(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, so := range []order.StoredOrder{b, pick} {
		if !get(t, r, so.Order.ID).Status.TipLocked {
			t.Errorf("%s not locked after Close batch", so.Order.ID)
		}
	}
	evs := eventsOf(t, r)
	bc := -1
	lastOrder := -1
	for i, e := range evs {
		if e["type"] == order.FloorEventBatchClosed {
			bc = i
			if e["batch_id"] != float64(closed.ID) || e["at"] == nil {
				t.Errorf("batch.closed: %v", e)
			}
		}
		if e["type"] == order.FloorEventOrderStatus {
			lastOrder = i
		}
	}
	if bc < 0 || bc < lastOrder {
		t.Fatalf("batch.closed at %d, last order.status at %d", bc, lastOrder)
	}
}

func TestCancelChannelOrder(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	d, _ := r.CreateDriver(ctx, "Sam", "")
	clockIn(t, r, d.ID)

	// Out on a run alone: cancelled, off the run, the driver is back.
	out := accept(t, r, arriveWeb(t, r, "web", "delivery", true))
	if _, err := r.MarkReady(ctx, out.Order.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AssignDriver(ctx, d.ID, []string{out.Order.ID}); err != nil {
		t.Fatal(err)
	}
	so, prev, returned, err := r.CancelChannelOrder(ctx, out.Order.ID, order.CancelCouldntDeliver)
	if err != nil || so.Status.State != order.StateCancelled || so.Status.CancelReason != order.CancelCouldntDeliver ||
		so.Status.DriverID != "" || prev.Status.State != order.StateOnDelivery || !returned {
		t.Fatalf("cancel on delivery: %v %+v prev=%s returned=%v", err, so.Status, prev.Status.State, returned)
	}
	ds, _ := r.ListDrivers(ctx)
	if ds[0].Status != order.DriverAvailable {
		t.Fatalf("driver back: %+v", ds[0])
	}
	ev := lastStatusEvent(t, r, out.Order.ID)
	if ev["status"] != "cancelled" || ev["reason"] != order.CancelCouldntDeliver {
		t.Fatalf("order.status: %v", ev)
	}
	// Pooled: leaves the pool.
	pooled := accept(t, r, arriveWeb(t, r, "web", "delivery", true))
	if _, err := r.PoolOrders(ctx, []string{pooled.Order.ID}, d.ID); err != nil {
		t.Fatal(err)
	}
	if so, _, _, err = r.CancelChannelOrder(ctx, pooled.Order.ID, order.CancelCustomerAsked); err != nil || so.Status.PoolDriverID != "" {
		t.Fatalf("cancel pooled: %v %+v", err, so)
	}
	// Not allowed: delivered, cancelled, online, POS.
	if _, _, _, err := r.CancelChannelOrder(ctx, out.Order.ID, order.CancelOther); !errors.Is(err, order.ErrNotCancellable) {
		t.Errorf("twice: %v", err)
	}
	online := accept(t, r, arriveWeb(t, r, "online", "pickup", true))
	if _, _, _, err := r.CancelChannelOrder(ctx, online.Order.ID, order.CancelOther); !errors.Is(err, order.ErrNotCancellable) {
		t.Errorf("online: %v", err)
	}
	done := accept(t, r, arriveWeb(t, r, "web", "delivery", true))
	if _, err := r.MarkReady(ctx, done.Order.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AssignDriver(ctx, d.ID, []string{done.Order.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DriverReturned(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.CancelChannelOrder(ctx, done.Order.ID, order.CancelOther); !errors.Is(err, order.ErrNotCancellable) {
		t.Errorf("delivered: %v", err)
	}
	// A plain online cancel (Close batch) carries no reason.
	arrived := arriveWeb(t, r, "online", "pickup", true)
	if _, _, err := r.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if ev := lastStatusEvent(t, r, arrived.Order.ID); ev["status"] != "cancelled" || ev["reason"] != nil {
		t.Fatalf("auto cancel: %v", ev)
	}
}
