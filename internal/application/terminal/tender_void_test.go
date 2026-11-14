package terminal

import (
	"encoding/json"
	"errors"
	"testing"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Batch C (contract §2.10al): a cashier voids a tender from Cash Out. Cash
// back out of the drawer, a card refunded on Square for what Square charged,
// due = total - the live tenders, never under the cashier's refund limit,
// idempotent by key, already-voided answered as such.
func TestVoidTenderCashAndCard(t *testing.T) {
	r := newRig(t)
	sq := &fakeSquare{}
	r.svc.SetSquare(sq)
	r.login(t, till, "1234") // Cara, a cashier: refund limit 1000
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "pickup",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	if total <= 1300 {
		t.Fatalf("the test wants a total over the cashier's limit + 300, got %d", total)
	}
	cash, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: total - 300, TenderedCents: total})
	if err != nil {
		t.Fatal(err)
	}
	card, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 300, TipCents: 100,
		Brand: "visa", Last4: "4242", SquarePaymentID: "pay_R1", Entry: "chip"})
	if err != nil || card.BalanceCents != 0 {
		t.Fatalf("card: %+v %v", card, err)
	}
	// A tip added afterwards rides the void's tip, not Square's refund (Square holds the charge only).
	if _, err := r.svc.AddTip(r.ctx, till, id, card.Payment.ID, PaymentTipIn{TipCents: 250, ClientKey: tk()}); err != nil {
		t.Fatal(err)
	}

	// The card: Square first for 400, then recorded with its refund id; any cashier, no manager.
	key := tk()
	v, err := r.svc.VoidTender(r.ctx, till, id, card.Payment.ID, TenderVoidIn{ClientKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(sq.calls) != 1 || sq.calls[0].id != "pay_R1" || sq.calls[0].cents != 400 {
		t.Fatalf("square calls %+v", sq.calls)
	}
	rf := v.Refund
	if !rf.Refund || rf.RefundKind != terminal.RefundTenderVoid || rf.RefundOf != card.Payment.ID || rf.SquareRefundID == "" ||
		rf.AmountCents != 300 || rf.TipCents != 250 || rf.Last4 != "4242" || rf.Reason != terminal.DefaultTenderVoidReason {
		t.Fatalf("card void %+v", rf)
	}
	if v.KickDrawer || v.RefundInSquare || v.BalanceCents != 300 || v.Voided.ID != card.Payment.ID {
		t.Fatalf("card void answer %+v", v)
	}
	if w := rf.Wire(); w["kind"] != terminal.RefundTenderVoid || w["refund_of"] != card.Payment.ID || w["amount_cents"] != int64(-550) {
		t.Fatalf("wire %v", w)
	}
	// The same key again: the stored void, Square not asked again; another key: already.
	again, err := r.svc.VoidTender(r.ctx, till, id, card.Payment.ID, TenderVoidIn{ClientKey: key})
	if err != nil || !again.Duplicate || again.Refund.ID != rf.ID || len(sq.calls) != 1 {
		t.Fatalf("again %+v %v", again, err)
	}
	other, err := r.svc.VoidTender(r.ctx, till, id, card.Payment.ID, TenderVoidIn{ClientKey: tk()})
	if err != nil || !other.Already || other.Refund.ID != rf.ID || len(sq.calls) != 1 {
		t.Fatalf("already %+v %v", other, err)
	}
	// The key with another body: 409 key_reused.
	var kr *terminal.KeyReused
	if _, err := r.svc.VoidTender(r.ctx, till, id, cash.Payment.ID, TenderVoidIn{ClientKey: key}); !errors.As(err, &kr) {
		t.Fatalf("key reused: %v", err)
	}

	// The cash, far over Cara's 1000 limit: allowed, out of the drawer, journaled.
	cv, err := r.svc.VoidTender(r.ctx, till, id, cash.Payment.ID, TenderVoidIn{ClientKey: tk(), Reason: "Wrong tender"})
	if err != nil {
		t.Fatal(err)
	}
	if !cv.KickDrawer || cv.Refund.Tender != "cash" || cv.Refund.AmountCents != total-300 || cv.BalanceCents != total || cv.Refund.Reason != "Wrong tender" {
		t.Fatalf("cash void %+v", cv)
	}
	if cv.Order.Status.Paid {
		t.Fatal("the order still reads paid")
	}
	j, err := r.svc.Journal(r.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var voids, kicks int
	for _, e := range j {
		switch e.Action {
		case terminal.ActionTenderVoid:
			voids++
			var d map[string]any
			_ = json.Unmarshal(e.Detail, &d)
			if e.StaffID != "cash-1" || d["refund_payment_id"] == nil {
				t.Fatalf("journal %+v %v", e, d)
			}
		case terminal.ActionDrawerKick:
			kicks++
		}
	}
	if voids != 2 || kicks != 1 {
		t.Fatalf("journal: %d tender.void, %d drawer.kick", voids, kicks)
	}

	// Paid again in cash: Cara's own cash refund of 800 needs no manager (the voids aren't under her limit).
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: total}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 800, Refund: true, Reason: "x"}); err != nil {
		t.Fatalf("a refund under the limit after two voids: %v", err)
	}

	// Refused: a refund is not a tender, an unknown id, an order that isn't there.
	ov, _ := r.svc.Order(r.ctx, id)
	refundID := ov.Payments[len(ov.Payments)-1].ID
	if _, err := r.svc.VoidTender(r.ctx, till, id, refundID, TenderVoidIn{ClientKey: tk()}); !errors.Is(err, terminal.ErrNotVoidableTender) {
		t.Fatalf("a refund voided: %v", err)
	}
	if _, err := r.svc.VoidTender(r.ctx, till, id, "nope", TenderVoidIn{ClientKey: tk()}); !errors.Is(err, terminal.ErrTenderNotFound) {
		t.Fatalf("unknown tender: %v", err)
	}
	if _, err := r.svc.VoidTender(r.ctx, till, id, cash.Payment.ID, TenderVoidIn{}); !errors.Is(err, terminal.ErrClientKeyRequired) {
		t.Fatalf("no key: %v", err)
	}
}

// A Square-app card (or one recorded with no Square id): recorded, nothing
// asked of Square, the answer says to give it back in Square's app; a card
// Square refuses is not recorded.
func TestVoidTenderSquareAppAndRefused(t *testing.T) {
	r := newRig(t)
	sq := &fakeSquare{}
	r.svc.SetSquare(sq)
	r.login(t, till, "55") // Ben, a cashier
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	app, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "square_app", AmountCents: 500})
	if err != nil {
		t.Fatal(err)
	}
	keyed, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "manual_card", AmountCents: total - 500,
		Last4: "1111", SquarePaymentID: "pay_K1", Entry: "keyed"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := r.svc.VoidTender(r.ctx, till, id, app.Payment.ID, TenderVoidIn{ClientKey: tk()})
	if err != nil || !v.RefundInSquare || v.KickDrawer || len(sq.calls) != 0 || v.BalanceCents != 500 {
		t.Fatalf("square app void %+v %v (%d calls)", v, err, len(sq.calls))
	}
	sq.err = &squareterm.Refused{Code: "refund_failed", Message: "no", SquareCode: "REFUND_DECLINED"}
	if _, err := r.svc.VoidTender(r.ctx, till, id, keyed.Payment.ID, TenderVoidIn{ClientKey: tk()}); err == nil {
		t.Fatal("a refused Square refund was recorded")
	}
	sq.err = squareterm.ErrUnreachable
	if _, err := r.svc.VoidTender(r.ctx, till, id, keyed.Payment.ID, TenderVoidIn{ClientKey: tk()}); !errors.Is(err, squareterm.ErrUnreachable) {
		t.Fatalf("no answer: %v", err)
	}
	ov, _ := r.svc.Order(r.ctx, id)
	if len(ov.Payments) != 3 || ov.Order.Status.CashDueCents != 500 {
		t.Fatalf("after the refusals: %d payments, due %d", len(ov.Payments), ov.Order.Status.CashDueCents)
	}
	// A partial Square refund made under the refund rules: the tender can't be voided whole any more.
	sq.err = nil
	r.login(t, till, "9876")
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "manual_card", AmountCents: 100, Refund: true, Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.VoidTender(r.ctx, till, id, keyed.Payment.ID, TenderVoidIn{ClientKey: tk()}); !errors.Is(err, terminal.ErrTenderRefunded) {
		t.Fatalf("partly refunded: %v", err)
	}
	// Nobody logged in: login required.
	r.svc.Logout(r.ctx, till)
	if _, err := r.svc.VoidTender(r.ctx, till, id, keyed.Payment.ID, TenderVoidIn{ClientKey: tk()}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("logged out: %v", err)
	}
}
