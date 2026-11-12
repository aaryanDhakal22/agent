package order

import (
	"context"
	"sync"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// A sent delivery whose lines changed (a store terminal's or the call center's
// change after SEND) gets its change tickets AND its delivery copies again,
// marked updated; a pickup gets only the change tickets; a cancelled or a
// delivered delivery prints no copies.
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
	ctx := context.Background()
	added := []order.LineItem{{Name: "Can Coke", Quantity: 1}}
	got := func(so order.StoredOrder) []printer.Mode {
		mu.Lock()
		modes = nil
		mu.Unlock()
		svc.PrintChange(ctx, so, added, nil)
		svc.WaitBackground()
		mu.Lock()
		defer mu.Unlock()
		return append([]printer.Mode(nil), modes...)
	}
	d := order.StoredOrder{Order: order.Snapshot{ID: "o-d", Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "delivery"}},
		Status: order.Status{State: order.StateAccepted}}
	if m := got(d); len(m) != 2 || !((m[0] == printer.ModeChange && m[1] == printer.ModeDeliveryUpdate) ||
		(m[1] == printer.ModeChange && m[0] == printer.ModeDeliveryUpdate)) { // two background prints: any order
		t.Fatalf("a sent delivery: %v", m)
	}
	p := d
	p.Order.Fulfillment.Type = "pickup"
	if m := got(p); len(m) != 1 || m[0] != printer.ModeChange {
		t.Fatalf("a pickup: %v", m)
	}
	for _, st := range []order.State{order.StateCancelled, order.StateDelivered} {
		x := d
		x.Status.State = st
		if m := got(x); len(m) != 1 {
			t.Fatalf("%s: %v", st, m)
		}
	}
	// No line changed and the type stayed: nothing at all.
	mu.Lock()
	modes = nil
	mu.Unlock()
	svc.PrintChange(ctx, d, nil, nil)
	svc.WaitBackground()
	if len(modes) != 0 {
		t.Fatalf("a no-op change printed %v", modes)
	}
}
