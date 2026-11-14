package terminal

import (
	"testing"

	"quiccpos/agent/internal/domain/pricing"
)

// Counter delivery holds (contract/agent.md §2.10ao, the owner 2026-11-14):
// a delivery's card taken at the counter is authorized only (a hold), so the
// driver's typed tip goes on it before main captures it.

// heldDelivery places a counter delivery paid in full on a card Square only
// authorized (held: true, Square payment sqID), ready.
func (r *driverRig) heldDelivery(t *testing.T, key, sqID string, held bool) string {
	t.Helper()
	r.login(t, till, "1234")
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Type: "delivery", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: key, Type: "delivery", Cart: cart,
		Customer: &Customer{Name: "Erik Davis", Phone: "4105550101", Address: &Address{Line1: "12 Main St", City: "Towson"}},
		Payments: []PaymentIn{{Tender: "manual_card", AmountCents: q.Quote.(pricing.Quote).TotalCents, SquarePaymentID: sqID,
			Last4: "4242", Brand: "visa", Held: held}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	if _, err := r.floor.Ready(r.ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// The held card is kept (payment_cards), and main hears it in order.payment
// / order.placed (held: true) so it records the hold.
func TestHeldCardIsKeptAndSentToMain(t *testing.T) {
	r := newDriverRig(t)
	id := r.heldDelivery(t, "k-held-0001", "sq-held-1", true)
	pays, err := r.repo.OrderPayments(r.ctx, id)
	if err != nil || len(pays) != 1 || !pays[0].Held || pays[0].SquarePaymentID != "sq-held-1" {
		t.Fatalf("payments: %+v %v", pays, err)
	}
	if w := pays[0].Wire(); w["held"] != true {
		t.Fatalf("order.payment to main: %v (want held: true)", w)
	}
	// A held flag without Square's id is dropped (nothing for main to capture).
	r2 := newDriverRig(t)
	id2 := r2.heldDelivery(t, "k-held-0002", "", true)
	if p, _ := r2.repo.OrderPayments(r2.ctx, id2); len(p) != 1 || p[0].Held {
		t.Fatalf("held without a Square id: %+v", p)
	}
}

// The press of Close batch: a held card's typed tip reaches main (which
// keeps it on the hold) BEFORE the close goes on, so batch.closed's capture
// carries it. Old code asked main in the background: the capture could win.
func TestClosePressHandsAHeldCardsTipToMainFirst(t *testing.T) {
	r := newDriverRig(t)
	sq := &tipSquare{}
	r.svc.SetSquare(sq)
	card := r.heldDelivery(t, "k-held-0003", "sq-held-3", true)
	r.deliverAs(t, till2, "77", card)
	tip := int64(400)
	if _, err := r.svc.Tip(r.ctx, till2, card, TipIn{TipCents: &tip}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.floor.CloseBatch(r.ctx, true); err != nil {
		t.Fatal(err)
	}
	// No waiting: the call was made before CloseBatch returned.
	if c := sq.seen(); len(c) != 1 || c[0].payment != "sq-held-3" || c[0].tip != 400 {
		t.Fatalf("square.tip_adjust before the close went on: %+v", c)
	}
}

// The driver's clock-out: main captures his holds then, so his typed tip on a
// held counter card goes on it at the clock-out (before the clock-out
// event); a completed counter card still waits for the press of Close batch.
func TestClockOutPutsHisTypedTipOnAHeldCounterCard(t *testing.T) {
	r := newDriverRig(t)
	sq := &tipSquare{}
	r.svc.SetSquare(sq)
	held := r.heldDelivery(t, "k-held-0004", "sq-held-4", true)
	done := r.heldDelivery(t, "k-held-0005", "sq-done-5", false)
	d := r.deliverAs(t, till2, "77", held, done)
	for _, id := range []string{held, done} {
		tip := int64(300)
		if _, err := r.svc.Tip(r.ctx, till2, id, TipIn{TipCents: &tip}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.floor.ClockOut(r.ctx, d); err != nil {
		t.Fatal(err)
	}
	if c := sq.seen(); len(c) != 1 || c[0].payment != "sq-held-4" || c[0].tip != 300 {
		t.Fatalf("at the clock-out: %+v (want only the held card's tip)", c)
	}
}

// Which typed card is a hold: a sent order's own fulfilment decides, a cart
// not sent yet the client's order type.
func TestKeyedHoldFollowsTheOrdersFulfilment(t *testing.T) {
	r := newDriverRig(t)
	del := r.heldDelivery(t, "k-held-0006", "sq-held-6", false)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-held-0007", Type: "pickup",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}, Customer: &Customer{Name: "Ann", Phone: "4105550102"}})
	if err != nil {
		t.Fatal(err)
	}
	pick := res.Order.Order.ID
	for _, c := range []struct {
		order, hint string
		want        bool
	}{{del, "pickup", true}, {pick, "delivery", false}, {"", "delivery", true}, {"", "pickup", false}, {"", "", false},
		{"00000000-0000-0000-0000-000000000000", "delivery", true}} {
		if got := r.svc.KeyedHold(r.ctx, c.order, c.hint); got != c.want {
			t.Fatalf("KeyedHold(%q, %q) = %v, want %v", c.order, c.hint, got, c.want)
		}
	}
}
