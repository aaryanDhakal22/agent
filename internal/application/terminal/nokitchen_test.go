package terminal

import (
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// main's counter menu marks an item "no kitchen ticket" (routes {id:
// {printer_id: "", no_kitchen: true}}): its counter line is routing none
// (no station, not in "Also on this order"; on the receipt only).
func TestCounterLineNoKitchenTicket(t *testing.T) {
	r := newRig(t)
	r.main.set(PathCounterMenu, map[string]any{"menu_version": 13, "build": map[string]any{"view": map[string]any{"kind": "counter"}},
		"pricing": caseContext(t), "routes": map[string]any{cheese: map[string]any{"printer_id": pizzaPrn, "name": "Pizza"},
			soda: map[string]any{"printer_id": "", "no_kitchen": true}}})
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindCounterMenu); err != nil {
		t.Fatal(err)
	}
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-nokit-001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{
		pizzaLine(), {Qty: 1, Selection: pricing.Selection{ItemID: soda}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	items := res.Order.Order.Items
	if items[0].Routing != order.RoutingMatched || items[1].Routing != order.RoutingNone || items[1].PrinterID != "" || items[1].MenuItemID != soda {
		t.Fatalf("routing: %+v", items)
	}
}
