package repositories

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

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

// An unpaid READY pickup is called out when it gets paid (the owner,
// 2026-11-12): the payment writes order.ready again with trigger "paid".
func TestPickupPaidAfterReadyOnTheFeed(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	id := uuid.Must(uuid.NewV7()).String()
	so, _, err := r.PlaceCounterOrder(ctx, terminal.NewCounterOrder{Snapshot: order.Snapshot{ID: id, TenantID: "t", LocationID: "l",
		Source: order.SourceCounter, SourceEventID: "key-call-003", Status: "accepted", Fulfillment: order.Fulfillment{Type: "pickup"},
		Money: order.Money{Subtotal: 1299, Total: 1299}, Items: []order.LineItem{{ID: uuid.NewString(), Name: "Cheese Pizza", Quantity: 1,
			UnitPrice: 1299, LineTotal: 1299}}}, ClientKey: "key-call-003", CounterStart: 100, PrepMinutes: 15, StaffID: "s1", DeviceID: "d1",
		Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarkReady(ctx, so.Order.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.AddCounterPayment(ctx, so.Order.ID, "", "", func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		p := pay(terminal.TenderCash, 1299)
		p.OrderID = so.Order.ID
		return []terminal.Payment{p}, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	evs, _, _ := r.ListFeed(ctx, 0, 100)
	var readies []order.OrderReadyData
	for _, e := range evs {
		if e.Type == order.FeedOrderReady {
			var d order.OrderReadyData
			_ = json.Unmarshal(e.Data, &d)
			readies = append(readies, d)
		}
	}
	if len(readies) != 2 || readies[0].Paid || readies[0].Trigger != "" || !readies[1].Paid || readies[1].Trigger != order.ReadyTriggerPaid {
		t.Fatalf("readies: %+v", readies)
	}
}
