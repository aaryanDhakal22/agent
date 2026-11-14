package order

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// Ticket numbers through the service, the way the store sees them (contract
// 2.10ae, the owner 2026-11-14): online orders placed on arrival take the
// next number of the one sequence from 100; a future order prints its early
// tickets with its 5-digit placeholder, keeps it across Close batch, and is
// placed at its ring time with the next real number - which the tickets, the
// hub's callout (order.ready) and main's mirror (the accepted event) carry.
func TestFutureOrderPlaceholderOnEarlyTicketsRealNumberWhenItFires(t *testing.T) {
	ctx := context.Background()
	svc, repo, events, prints, _ := newIntegService(t)
	svc.legacyArrival = false
	svc.SetPrepSource(func(string) int64 { return 15 })
	svc.WaitBackground()
	drain(events)
	svc.UpdateIntegrationSettings(ctx, func(x *order.IntegrationSettings) error {
		x.Tickets.FutureKitchen, x.Tickets.FutureReceipt = true, true
		return nil
	})

	first := onlineSnap("pickup")
	if err := svc.OnArrival(ctx, first); err != nil {
		t.Fatal(err)
	}
	if n := storedOf(t, svc, first.ID).Status.KitchenNumber; n == nil || *n != 100 {
		t.Fatalf("the first online order of a batch = %v, want 100 (no more 1100)", n)
	}

	// A future delivery: scheduled on arrival with a placeholder.
	promised := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Millisecond)
	fut := onlineSnap("delivery")
	fut.Fulfillment.PromisedAt = &promised
	if err := svc.OnArrival(ctx, fut); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	sched := storedOf(t, svc, fut.ID)
	ph := sched.Status.KitchenNumber
	if sched.Status.State != order.StateScheduled || ph == nil || !sched.Status.NumberPlaceholder || *ph < 10_000 || *ph > 99_999 {
		t.Fatalf("future order on arrival: %+v", sched.Status)
	}
	var early *printCall
	for _, p := range prints.all() {
		if p.OrderID == fut.ID && p.Mode == printer.ModeFutureEarly {
			early = &p
		}
	}
	if early == nil || early.Number != *ph {
		t.Fatalf("the early tickets print the placeholder %d: %+v", *ph, early)
	}
	evs := statusEvents(t, repo, fut.ID)
	if len(evs) == 0 || evs[0].Status != order.StateScheduled || evs[0].KitchenNumber == nil || *evs[0].KitchenNumber != *ph {
		t.Fatalf("main's mirror gets the placeholder with the scheduled event: %+v", evs)
	}

	// Close batch: the sequence restarts, the future order keeps its placeholder.
	second := onlineSnap("pickup")
	if err := svc.OnArrival(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Ready(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CloseBatch(ctx, true); err != nil {
		t.Fatal(err)
	}
	if n := storedOf(t, svc, fut.ID).Status.KitchenNumber; n == nil || *n != *ph {
		t.Fatalf("carried across the close: %v, want the placeholder %d", n, *ph)
	}
	third := onlineSnap("pickup")
	if err := svc.OnArrival(ctx, third); err != nil {
		t.Fatal(err)
	}
	if n := storedOf(t, svc, third.ID).Status.KitchenNumber; *n != 100 {
		t.Fatalf("after Close batch: %d, want 100", *n)
	}

	// It fires at its ring time: placed with the next number (101).
	if _, err := svc.RaiseDue(ctx, promised); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	placed := storedOf(t, svc, fut.ID)
	if placed.Status.State != order.StateAccepted || placed.Status.KitchenNumber == nil || *placed.Status.KitchenNumber != 101 ||
		placed.Status.NumberPlaceholder {
		t.Fatalf("fired future order: %+v, want accepted #101", placed.Status)
	}
	p := prints.all()
	if last := p[len(p)-1]; last.OrderID != fut.ID || last.Number != 101 {
		t.Fatalf("the placed tickets print #101: %+v", last)
	}
	evs = statusEvents(t, repo, fut.ID)
	if last := evs[len(evs)-1]; last.Status != order.StateAccepted || *last.KitchenNumber != 101 {
		t.Fatalf("main's mirror gets the real number: %+v", last)
	}
	// READY: the hub's callout reads the real number.
	if _, err := svc.Ready(ctx, fut.ID); err != nil {
		t.Fatal(err)
	}
	page, err := svc.Feed(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var called int64 = -1
	for _, ev := range page.Events {
		if ev.Type != order.FeedOrderReady {
			continue
		}
		var d struct {
			OrderID string `json:"order_id"`
			Number  int64  `json:"number"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if d.OrderID == fut.ID {
			called = d.Number
		}
	}
	if called != 101 {
		t.Fatalf("the hub's order.ready number = %d, want 101", called)
	}
}
