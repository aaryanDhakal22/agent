package terminal

import (
	"testing"

	"quiccpos/agent/internal/domain/pricing"
)

// A sent delivery's payments changed (the call center or a store terminal
// charged what was added, a tip set afterwards): its delivery copies print
// again, once per real change; a duplicate (the same key again) prints
// nothing. The summary carries the tip, the tender, the entry and Square's id.
func TestPaymentOnASentDeliveryPrintsTheCopiesAgain(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-dlv-upd-001", Type: "delivery",
		Cart:     Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Customer: &Customer{Name: "Erik Davis", Phone: "4105550101", Address: &Address{Line1: "12 Main St", City: "Towson"}}})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	if n := len(r.orders.deliveryUpdates); n != 0 {
		t.Fatalf("placing prints no update: %d", n)
	}
	in := PaymentIn{ClientKey: "pay-dlv-upd-1", Tender: "manual_card", AmountCents: 1000, TipCents: 300, Last4: "1111", Ref: "sq-pay-1"}
	in.Entry = "keyed"
	p, err := r.svc.AddPayment(r.ctx, till, id, in)
	if err != nil || p.Duplicate {
		t.Fatalf("pay: %+v %v", p, err)
	}
	if got := r.orders.deliveryUpdates; len(got) != 1 || got[0] != id {
		t.Fatalf("one update after the payment: %v", got)
	}
	sum := p.Order.Order.Payments[len(p.Order.Order.Payments)-1]
	if sum.TipCents != 300 || sum.Tender != "manual_card" || sum.Entry != "keyed" || sum.Ref != "sq-pay-1" || sum.EntryLabel() != "MANUALLY ENTERED" {
		t.Fatalf("summary: %+v", sum)
	}
	if again, err := r.svc.AddPayment(r.ctx, till, id, in); err != nil || !again.Duplicate || len(r.orders.deliveryUpdates) != 1 {
		t.Fatalf("a duplicate prints nothing: %+v %v %v", again, err, r.orders.deliveryUpdates)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: total - 1000, TenderedCents: total}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.orders.deliveryUpdates); n != 2 {
		t.Fatalf("the rest paid: a second update, got %d", n)
	}
}
