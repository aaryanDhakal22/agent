package repositories

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// A card tip taken with the payment placed at SEND (a terminal's keyed card, the call center's)
// reaches the driver report (the flow's D008 / D011, 2.5.2): the order's tip_cents counts the
// payments placed with it; a second payment's tip and a refund's tip back follow; a restored Pi
// keeps them.
func TestCardTipsReachTheDriverReport(t *testing.T) {
	ctx := context.Background()
	r := newFloorRepo(t)
	d, _ := r.CreateDriver(ctx, "Dan")
	clockIn(t, r, d.ID)

	place := func(key string, total int64, pays ...terminal.Payment) order.StoredOrder {
		id := uuid.Must(uuid.NewV7()).String()
		snap := order.Snapshot{ID: id, TenantID: "t", LocationID: "l", Source: order.SourceCounter, SourceEventID: key,
			Status: "accepted", Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{Line1: "1 Main St"}},
			Customer: order.Customer{DisplayName: "Erik Davis"}, Money: order.Money{Subtotal: total, Total: total},
			Items: []order.LineItem{{ID: uuid.NewString(), Name: "Cheese Pizza", Quantity: 1, UnitPrice: total, LineTotal: total}}}
		for _, p := range pays {
			snap.Payments = append(snap.Payments, p.Summary())
		}
		so, _, err := r.PlaceCounterOrder(ctx, terminal.NewCounterOrder{Snapshot: snap, ClientKey: key, CounterStart: 1,
			PrepMinutes: 15, Payments: pays, StaffID: "s1", DeviceID: "d1", Now: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		return so
	}
	keyed := pay(terminal.TenderManualCard, 2046)
	keyed.TipCents, keyed.Last4 = 517, "4242"
	a := place("key-tip-at-send", 2046, keyed) // card + tip at SEND
	if a.Status.TipCents != 517 {
		t.Fatalf("tip_cents at SEND: %d", a.Status.TipCents)
	}
	b := place("key-tip-later", 1500, pay(terminal.TenderCard, 1000)) // part card at SEND, the rest + a tip later
	addPay := func(so order.StoredOrder, p terminal.Payment) {
		p.OrderID, p.ClientKey = so.Order.ID, uuid.NewString()
		if _, _, _, err := r.AddCounterPayment(ctx, so.Order.ID, p.ClientKey, "", func(order.StoredOrder, []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
			return []terminal.Payment{p}, nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	second := pay(terminal.TenderCard, 500)
	second.TipCents = 300
	addPay(b, second)
	back := pay(terminal.TenderCard, 0) // a refund that gives $1 of the tip back
	back.Refund, back.TipCents = true, 100
	addPay(b, back)

	for _, so := range []order.StoredOrder{a, b} { // a counter order is accepted at SEND
		ready(t, r, so)
	}
	if _, err := r.AssignDriver(ctx, d.ID, ids(a, b)); err != nil {
		t.Fatal(err)
	}
	for _, so := range []order.StoredOrder{a, b} {
		if _, _, err := r.DeliverOne(ctx, d.ID, so.Order.ID); err != nil {
			t.Fatal(err)
		}
	}
	reps, err := r.DriverReports(ctx, 0)
	if err != nil || len(reps) != 1 || len(reps[0].Lines) != 2 {
		t.Fatalf("reports: %v %+v", err, reps)
	}
	rep := reps[0]
	rep.Finish(600, 300)
	tips := map[string]int64{}
	for _, l := range rep.Lines {
		tips[l.OrderID] = l.TipCents
	}
	if tips[a.Order.ID] != 517 || tips[b.Order.ID] != 200 {
		t.Fatalf("line tips: %v", tips)
	}
	for _, l := range rep.Lines {
		if l.OrderID == a.Order.ID && (l.SlipTotalCents != 2563 || l.Cash) {
			t.Fatalf("the card + tip line: %+v", l)
		}
	}
	// tips 7.17 - 6% = 6.74; no cash; pay 2 x 3.00 → the store owes 12.74
	if rep.TipCents != 717 || rep.TipAfterTaxCents != 674 || rep.OwedCents != -1274 {
		t.Fatalf("report: tip %d a/t %d owed %d", rep.TipCents, rep.TipAfterTaxCents, rep.OwedCents)
	}
}
