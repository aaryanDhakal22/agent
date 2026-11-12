package repositories

import (
	"context"
	"encoding/json"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// The hub's callout (0.4.0) reads the feed's order.ready of the terminal's
// orders: source "counter", the counter number, fulfillment, and whether it
// was paid at READY (TERMINAL-PLAN "Callouts": counter when READY, pickup
// when READY and already paid).
func TestCounterOrderReadyOnTheFeed(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	unpaid, _ := counterOrder(t, r, "key-call-001", 100, 1299)
	paid, _ := counterOrder(t, r, "key-call-002", 100, 800, pay(terminal.TenderCash, 800))
	for _, id := range []string{unpaid.Order.ID, paid.Order.ID} {
		if _, err := r.MarkReady(ctx, id, true); err != nil {
			t.Fatal(err)
		}
	}
	evs, _, err := r.ListFeed(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]order.OrderReadyData{}
	for _, e := range evs {
		if e.Type != order.FeedOrderReady {
			continue
		}
		var d order.OrderReadyData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			t.Fatal(err)
		}
		got[d.OrderID] = d
	}
	u, p := got[unpaid.Order.ID], got[paid.Order.ID]
	if u.Source != order.SourceCounter || u.Number == nil || *u.Number != 100 || u.FulfillmentType != "dine_in" || u.Paid || u.CashDueCents != 1299 {
		t.Fatalf("unpaid counter order: %+v", u)
	}
	if p.Number == nil || *p.Number != 101 || !p.Paid || p.CashDueCents != 0 {
		t.Fatalf("paid counter order: %+v", p)
	}
}
