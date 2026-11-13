package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Agent 2.5.3: an online CASH order is voided (or wasted) at a terminal like a counter order: off the
// driver's run and report, an order.void for main; one paid online is ErrPaidOnline, untouched.
func TestVoidOnlineCashOrders(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	d, _ := r.CreateDriver(ctx, "Omar")
	clockIn(t, r, d.ID)
	online := func(pays ...order.PaymentSummary) order.StoredOrder {
		s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
			Source: order.SourceOnline, SourceEventID: uuid.NewString()[:6], PlacedAt: time.Now().UTC(),
			Customer:    order.Customer{DisplayName: "Ann Online"},
			Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{Line1: "12 Main St"}},
			Money:       order.Money{Total: 1500}, Payments: pays}
		if _, err := r.UpsertArrived(ctx, s); err != nil {
			t.Fatal(err)
		}
		so, err := r.GetByID(ctx, uuid.MustParse(s.ID))
		if err != nil {
			t.Fatal(err)
		}
		return accept(t, r, *so)
	}
	none := func(order.StoredOrder, []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) { return nil, nil, nil }

	paid := online(order.PaymentSummary{Method: "card", AmountCents: 1500})
	if _, _, _, _, err := r.VoidCounterOrder(ctx, paid.Order.ID, "x", "", "s1", "d1", none); !errors.Is(err, terminal.ErrPaidOnline) {
		t.Fatalf("paid online: %v", err)
	}

	out := ready(t, r, online())
	if _, err := r.AssignDriver(ctx, d.ID, ids(out)); err != nil {
		t.Fatal(err)
	}
	so, _, _, returned, err := r.VoidCounterOrder(ctx, out.Order.ID, "never answered", "", "s1", "d1", none)
	if err != nil || so.Status.State != order.StateCancelled || so.Status.CancelReason != terminal.VoidReason || !returned {
		t.Fatalf("void after send-out: %v %+v returned=%v", err, so.Status, returned)
	}
	wasted := online()
	if so, _, _, _, err := r.VoidCounterOrder(ctx, wasted.Order.ID, "Burnt", "Burnt", "s1", "d1", none); err != nil ||
		so.Status.CancelReason != terminal.WasteReason {
		t.Fatalf("waste: %v %+v", err, so.Status)
	}
	reps, err := r.DriverReports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reps {
		if rep.DriverName == "Omar" && len(rep.Lines) != 0 {
			t.Fatalf("a voided order on his report: %+v", rep.Lines)
		}
	}
	if ev := eventsOfType(t, r, terminal.EventOrderVoid); len(ev) != 2 {
		t.Fatalf("order.void events: %d", len(ev))
	}
	if cur, _ := r.GetByID(ctx, uuid.MustParse(paid.Order.ID)); cur.Status.State != order.StateAccepted {
		t.Fatalf("the paid-online order moved: %+v", cur.Status)
	}
}
