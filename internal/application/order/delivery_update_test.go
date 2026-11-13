package order

import (
	"context"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// A sent delivery whose lines changed (a store terminal's or the call center's
// change after SEND) gets its change tickets at once and ONE updated pair of
// delivery copies (the owner's choice A): at the payment that follows (the
// difference charged), else when the wait runs out; a second change while it
// waits still makes one pair. A pickup gets only the change tickets; a
// cancelled or a delivered delivery prints no copies.
func TestChangeOfASentDeliveryPrintsUpdatedCopies(t *testing.T) {
	svc, _, _, _ := newTestService(t, false)
	var mu sync.Mutex
	var modes []printer.Mode
	svc.print = func(_ context.Context, _ order.Snapshot, m printer.Mode) (printer.Outcome, error) {
		mu.Lock()
		modes = append(modes, m)
		mu.Unlock()
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketDeliveryCopies, Station: "Front", PrintedOn: "Front"}}}, nil
	}
	took := func() []printer.Mode {
		svc.WaitBackground()
		mu.Lock()
		defer mu.Unlock()
		out := append([]printer.Mode(nil), modes...)
		modes = nil
		return out
	}
	count := func(ms []printer.Mode, m printer.Mode) int {
		n := 0
		for _, x := range ms {
			if x == m {
				n++
			}
		}
		return n
	}
	ctx := context.Background()
	added := []order.LineItem{{Name: "Can Coke", Quantity: 1}}
	d := order.StoredOrder{Order: order.Snapshot{ID: "o-d", Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "delivery"}},
		Status: order.Status{State: order.StateAccepted}}

	// Change, change again, then the charge: the change tickets twice, one pair after the charge.
	svc.SetDeliveryUpdateWait(time.Hour)
	svc.PrintChange(ctx, d, added, nil)
	svc.PrintChange(ctx, d, added, nil)
	if m := took(); count(m, printer.ModeChange) != 2 || count(m, printer.ModeDeliveryUpdate) != 0 || !svc.DeliveryUpdatePending("o-d") {
		t.Fatalf("before the charge: %v (pending %v)", m, svc.DeliveryUpdatePending("o-d"))
	}
	svc.PrintDeliveryUpdate(ctx, d)
	if m := took(); len(m) != 1 || m[0] != printer.ModeDeliveryUpdate || svc.DeliveryUpdatePending("o-d") {
		t.Fatalf("after the charge: %v", m)
	}

	// Nothing charged: the pair prints when the wait runs out (once).
	svc.SetDeliveryUpdateWait(30 * time.Millisecond)
	svc.PrintChange(ctx, d, added, nil)
	printed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return count(modes, printer.ModeDeliveryUpdate) > 0
	}
	for end := time.Now().Add(5 * time.Second); !printed() && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
	}
	if m := took(); count(m, printer.ModeChange) != 1 || count(m, printer.ModeDeliveryUpdate) != 1 {
		t.Fatalf("unpaid difference: %v", m)
	}

	p := d
	p.Order.Fulfillment.Type = "pickup"
	svc.PrintChange(ctx, p, added, nil)
	if m := took(); len(m) != 1 || m[0] != printer.ModeChange || svc.DeliveryUpdatePending("o-d") {
		t.Fatalf("a pickup: %v", m)
	}
	for _, st := range []order.State{order.StateCancelled, order.StateDelivered} {
		x := d
		x.Status.State = st
		svc.PrintChange(ctx, x, added, nil)
		if m := took(); len(m) != 1 || svc.DeliveryUpdatePending("o-d") {
			t.Fatalf("%s: %v", st, m)
		}
	}
	// No line changed and the type stayed: nothing at all.
	svc.PrintChange(ctx, d, nil, nil)
	if m := took(); len(m) != 0 {
		t.Fatalf("a no-op change printed %v", m)
	}
}
