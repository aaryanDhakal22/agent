package terminal

import (
	"errors"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

func TestChangeCartAfterSend(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // cashier: discount up to 10 %, refunds up to $10
	soda2 := pricing.Line{Qty: 2, Selection: pricing.Selection{ItemID: soda}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-chg-00001", Type: "pickup",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, before := res.Order.Order.ID, res.Order.Order.Money.Total
	tm, _ := res.Order.Order.SourceMeta["terminal"].(map[string]any)
	if tm["cart"] == nil {
		t.Fatalf("the cart is kept on the order: %v", res.Order.Order.SourceMeta)
	}
	pizzaID := res.Order.Order.Items[0].ID
	// Pay it all, then add two sodas: the balance is the sodas.
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: before}); err != nil {
		t.Fatal(err)
	}
	in := ChangeIn{ClientKey: "chg-000001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), soda2}}}}
	ch, err := r.svc.ChangeCart(r.ctx, till, id, in)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Changed || len(ch.Added) != 1 || ch.Added[0] != "2× Soda" || len(ch.Removed) != 0 {
		t.Fatalf("added: %+v", ch)
	}
	if ch.Order.Order.Items[0].ID != pizzaID {
		t.Fatal("the unchanged pizza keeps its line id")
	}
	if ch.BalanceCents != ch.Order.Order.Money.Total-before || ch.Order.Status.Paid || ch.Order.Status.KitchenNumber == nil {
		t.Fatalf("balance: %+v", ch)
	}
	if len(r.orders.addOns) != 1 || len(r.orders.addOns[0]) != 1 || r.orders.addOns[0][0].Quantity != 2 || len(r.orders.voidLns) != 0 {
		t.Fatalf("ADD-ON ticket: %+v", r.orders.addOns)
	}
	if r.pub.count(EventOrderChanged) != 1 {
		t.Fatal("terminal.order.changed")
	}
	// The same key again: the stored answer, nothing printed.
	again, err := r.svc.ChangeCart(r.ctx, till, id, in)
	if err != nil || !again.Duplicate || len(again.Added) != 1 || len(r.orders.addOns) != 1 {
		t.Fatalf("idempotent: %+v %v", again, err)
	}
	// The same cart with no key: nothing changed.
	same, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: in.Cart})
	if err != nil || same.Changed || r.pub.count(EventOrderChanged) != 1 {
		t.Fatalf("no change: %+v %v", same, err)
	}
	// Taking the pizza off would leave less than was paid: refund first.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{soda2}}}}); !errors.Is(err, terminal.ErrRefundFirst) {
		t.Fatalf("refund first: %v", err)
	}
	// Drop one soda (unpaid part): a VOID ticket for it.
	one := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), one}}}})
	if err != nil || len(ch.Removed) != 1 || ch.Removed[0] != "Soda" || len(r.orders.voidLns) != 1 {
		t.Fatalf("removed: %+v %v", ch, err)
	}
	// A discount over the cashier's limit needs a manager.
	disc := ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), one, one}}, Discount: &Discount{Pct: 5, Reason: "x"}}}
	disc.Cart.Discount.Pct = 30
	var mr *terminal.ManagerRequired
	if _, err := r.svc.ChangeCart(r.ctx, till, id, disc); !errors.As(err, &mr) {
		t.Fatalf("manager: %v", err)
	}
	// Bad cart, empty cart, unknown order.
	var ci *terminal.CartInvalid
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: "nope"}}}}}}); !errors.As(err, &ci) {
		t.Fatalf("cart invalid: %v", err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, "0193a000-0000-7000-8000-000000000000", in); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("not found: %v", err)
	}
	// Voided: not editable.
	refundAll(t, r, till, id)
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left", ManagerPIN: "9876"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: in.Cart}); !errors.Is(err, terminal.ErrNotEditable) {
		t.Fatalf("voided: %v", err)
	}
	journal, _ := r.repo.Journal(r.ctx, 50)
	n := 0
	for _, e := range journal {
		if e.Action == terminal.ActionOrderChanged {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("journal order.changed: %d", n)
	}
}

// Takeout <-> delivery after SEND (terminal owner pass 2026-11-12): re-quoted for the new type, the
// fulfillment and source_metadata follow, the kitchen gets a change ticket naming the old type,
// journaled; a delivery needs an address; a delivery with a driver is locked; counter never moves.
func TestChangeCartType(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-type-00001", Type: "pickup",
		Customer: &Customer{Name: "Ada Test", Phone: "4105550101"},
		Cart:     Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	// No address: refused.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "delivery"}); err == nil {
		t.Fatal("a delivery without an address")
	}
	// Counter is not a type to move to.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "counter"}); err == nil {
		t.Fatal("pickup -> counter")
	}
	addr := &Address{Line1: "1 Test Street", City: "Towson", Zip: "21286"}
	ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: cart, Type: "delivery", ClientKey: "chg-type-0001",
		Customer: &Customer{Name: "Ada Test", Phone: "4105550101", Address: addr}})
	if err != nil {
		t.Fatal(err)
	}
	snap := ch.Order.Order
	if !ch.Changed || !ch.TypeChanged || snap.Fulfillment.Type != "delivery" || snap.Fulfillment.Address == nil || snap.Fulfillment.Address.Line1 != "1 Test Street" {
		t.Fatalf("moved: %+v", ch)
	}
	if orderType(snap) != "delivery" || len(ch.Added) != 0 || len(ch.Removed) != 0 {
		t.Fatalf("type in source_metadata / no line moved: %v %+v", snap.SourceMeta, ch)
	}
	if n := len(r.orders.typeFrom); n == 0 || r.orders.typeFrom[n-1] != "pickup" {
		t.Fatalf("the change ticket names the old type: %v", r.orders.typeFrom)
	}
	if r.pub.count(EventOrderChanged) != 1 {
		t.Fatal("terminal.order.changed")
	}
	journal, _ := r.repo.Journal(r.ctx, 50)
	found := false
	for _, e := range journal {
		if e.Action == terminal.ActionOrderChanged && strings.Contains(string(e.Detail), `"type_to":"delivery"`) && strings.Contains(string(e.Detail), `"type_from":"pickup"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("journaled with type_from / type_to: %+v", journal)
	}
	// Back to a pickup: the address goes; the same type again changes nothing.
	ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "pickup"})
	if err != nil || !ch.TypeChanged || ch.Order.Order.Fulfillment.Type != "pickup" || ch.Order.Order.Fulfillment.Address != nil {
		t.Fatalf("back to pickup: %+v %v", ch, err)
	}
	same, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cart, Type: "pickup"})
	if err != nil || same.Changed || same.TypeChanged {
		t.Fatalf("same type: %+v %v", same, err)
	}
	// A counter order never changes type.
	c, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-type-00002", Type: "counter", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, c.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: cart, Type: "pickup"}); err == nil {
		t.Fatal("counter -> pickup")
	}
}

func TestTypeChangeRules(t *testing.T) {
	addr := &Customer{Address: &Address{Line1: "1 Test Street", City: "Towson"}}
	for _, c := range []struct {
		cur, to string
		cust    *Customer
		want    string
		bad     bool
	}{
		{"pickup", "", nil, "", false},
		{"pickup", "pickup", nil, "", false},
		{"pickup", "delivery", addr, "delivery", false},
		{"pickup", "delivery", nil, "", true},
		{"pickup", "delivery", &Customer{Address: &Address{Line1: "1 Test Street"}}, "", true},
		{"delivery", "pickup", nil, "pickup", false},
		{"counter", "pickup", nil, "", true},
		{"pickup", "counter", nil, "", true},
	} {
		got, err := typeChange(c.cur, ChangeIn{Type: c.to, Customer: c.cust})
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("%s -> %s: %q %v", c.cur, c.to, got, err)
		}
	}
}
