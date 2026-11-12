package terminal

import (
	"errors"
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
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{Tender: "cash", AmountCents: before}); err != nil {
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
	same, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: in.Cart})
	if err != nil || same.Changed || r.pub.count(EventOrderChanged) != 1 {
		t.Fatalf("no change: %+v %v", same, err)
	}
	// Taking the pizza off would leave less than was paid: refund first.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{soda2}}}}); !errors.Is(err, terminal.ErrRefundFirst) {
		t.Fatalf("refund first: %v", err)
	}
	// Drop one soda (unpaid part): a VOID ticket for it.
	one := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	ch, err = r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), one}}}})
	if err != nil || len(ch.Removed) != 1 || ch.Removed[0] != "Soda" || len(r.orders.voidLns) != 1 {
		t.Fatalf("removed: %+v %v", ch, err)
	}
	// A discount over the cashier's limit needs a manager.
	disc := ChangeIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), one, one}}, Discount: &Discount{Pct: 5, Reason: "x"}}}
	disc.Cart.Discount.Pct = 30
	var mr *terminal.ManagerRequired
	if _, err := r.svc.ChangeCart(r.ctx, till, id, disc); !errors.As(err, &mr) {
		t.Fatalf("manager: %v", err)
	}
	// Bad cart, empty cart, unknown order.
	var ci *terminal.CartInvalid
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: "nope"}}}}}}); !errors.As(err, &ci) {
		t.Fatalf("cart invalid: %v", err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, "0193a000-0000-7000-8000-000000000000", in); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("not found: %v", err)
	}
	// Voided: not editable.
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left", ManagerPIN: "9876"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: in.Cart}); !errors.Is(err, terminal.ErrNotEditable) {
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
