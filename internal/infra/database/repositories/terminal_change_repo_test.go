package repositories

import (
	"context"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

func TestChangeCounterOrder(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so, _ := counterOrder(t, r, "key-chg-0001", 1, 2000, pay(terminal.TenderCash, 2000))
	if !so.Status.Paid {
		t.Fatal("paid at placing")
	}
	grow := func(total int64) terminal.ChangeCheck {
		return func(cur order.StoredOrder, paid []terminal.Payment) (*order.Snapshot, []terminal.Entry, error) {
			if len(paid) != 1 {
				t.Fatalf("paid: %v", paid)
			}
			snap := cur.Order
			snap.Items = append(append([]order.LineItem(nil), snap.Items...), order.LineItem{ID: "w", Name: "Wings", Quantity: 1, UnitPrice: total - 2000, LineTotal: total - 2000})
			snap.Money = order.Money{Subtotal: total, Total: total}
			return &snap, []terminal.Entry{{StaffID: "s1", DeviceID: "d1", Action: terminal.ActionOrderChanged, OrderID: cur.Order.ID}}, nil
		}
	}
	got, prev, changed, err := r.ChangeCounterOrder(ctx, so.Order.ID, "s1", "d1", grow(2600))
	if err != nil || !changed || len(prev.Order.Items) != 1 || len(got.Order.Items) != 2 {
		t.Fatalf("change: %v %v", err, changed)
	}
	if got.Status.Paid || got.Status.CashDueCents != 600 || got.Order.Money.Total != 2600 || got.Status.KitchenNumber == nil {
		t.Fatalf("money columns: %+v", got.Status)
	}
	ev := eventsOfType(t, r, terminal.EventOrderChanged)
	if len(ev) != 1 || ev[0]["order_id"] != so.Order.ID || ev[0]["staff_id"] != "s1" {
		t.Fatalf("order.changed: %v", ev)
	}
	snap, _ := ev[0]["snapshot"].(map[string]any)
	if items, _ := snap["items"].([]any); len(items) != 2 || snap["source_event_id"] != "key-chg-0001" {
		t.Fatalf("event snapshot: %v", snap)
	}
	if f, _ := snap["fulfillment"].(map[string]any); f["type"] != terminal.TypeCounter {
		t.Fatalf("fulfillment: %v", snap["fulfillment"])
	}
	if pays, _ := snap["payments"].([]any); len(pays) != 1 {
		t.Fatalf("payments: %v", snap["payments"])
	}
	// nil = nothing written.
	_, _, changed, err = r.ChangeCounterOrder(ctx, so.Order.ID, "s1", "d1", func(order.StoredOrder, []terminal.Payment) (*order.Snapshot, []terminal.Entry, error) {
		return nil, nil, nil
	})
	if err != nil || changed || len(eventsOfType(t, r, terminal.EventOrderChanged)) != 1 {
		t.Fatalf("no change: %v %v", err, changed)
	}
	// Out on a delivery / voided: not editable.
	if _, err := r.db.ExecContext(ctx, `UPDATE orders SET state = 'cancelled' WHERE id = ?`, so.Order.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.ChangeCounterOrder(ctx, so.Order.ID, "s1", "d1", grow(3000)); !errors.Is(err, terminal.ErrNotEditable) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, _, _, err := r.ChangeCounterOrder(ctx, "00000000-0000-0000-0000-000000000000", "s1", "d1", grow(3000)); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// A sent delivery whose address changed (TERMINAL-FLOWS CHG-14) drops main's location of the old address; main
// locates the new one and pushes order.geo again. Only line2 / notes moving keeps it (the same place).
func TestChangedAddressDropsTheOldGeo(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so, _ := counterOrder(t, r, "key-geo-0001", 1, 2000)
	geoRows := func() int {
		var n int
		if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM order_geo WHERE order_id = ?`, so.Order.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	located := func() {
		if _, err := r.UpsertGeo(ctx, order.GeoUpdate{OrderID: so.Order.ID, State: order.GeoLocated, Lat: f64(39.40), Lng: f64(-76.60)}); err != nil {
			t.Fatal(err)
		}
	}
	to := func(a order.Address) terminal.ChangeCheck {
		return func(cur order.StoredOrder, _ []terminal.Payment) (*order.Snapshot, []terminal.Entry, error) {
			snap := cur.Order
			snap.Fulfillment = order.Fulfillment{Type: "delivery", Address: &a}
			return &snap, nil, nil
		}
	}
	a := order.Address{Line1: "12 Sample Road", City: "Towson", State: "MD", PostalCode: "21204"}
	located() // (a counter order has none; set one to see the move to a delivery drop it)
	if _, _, _, err := r.ChangeCounterOrder(ctx, so.Order.ID, "s1", "d1", to(a)); err != nil || geoRows() != 0 {
		t.Fatalf("moved to a delivery: %v, geo rows %d", err, geoRows())
	}
	located()
	b := a
	b.Line2, b.Notes, b.Line1 = "Apt 2", "side door", "12 sample  road"
	if _, _, _, err := r.ChangeCounterOrder(ctx, so.Order.ID, "s1", "d1", to(b)); err != nil || geoRows() != 1 {
		t.Fatalf("same place: %v, geo rows %d", err, geoRows())
	}
	c := a
	c.Line1 = "500 York Road"
	if _, _, _, err := r.ChangeCounterOrder(ctx, so.Order.ID, "s1", "d1", to(c)); err != nil || geoRows() != 0 {
		t.Fatalf("another street: %v, geo rows %d", err, geoRows())
	}
}
