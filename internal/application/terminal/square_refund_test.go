package terminal

import (
	"context"
	"errors"
	"testing"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// fakeSquare plays main's square.refund.
type fakeSquare struct {
	calls []struct {
		id    string
		cents int64
		key   string
	}
	err error
}

func (f *fakeSquare) Refund(_ context.Context, id string, cents int64, key, _ string) (squareterm.RefundResult, error) {
	f.calls = append(f.calls, struct {
		id    string
		cents int64
		key   string
	}{id, cents, key})
	if f.err != nil {
		return squareterm.RefundResult{}, f.err
	}
	return squareterm.RefundResult{RefundID: "ref-" + key[:8], Status: "PENDING"}, nil
}

// Contract 2.10r §3 + §4: Square's fields are kept on the payment; a
// refund of a reader / manual-entry payment goes through Square first and is
// recorded only when Square took it; the same key again never asks twice.
func TestSquareCardFieldsAndRefunds(t *testing.T) {
	r := newRig(t)
	sq := &fakeSquare{}
	r.svc.SetSquare(sq)
	r.login(t, till, "9876") // a manager: card refunds are a manager's
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "pickup",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	keyed, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "manual_card", AmountCents: 1000, TipCents: 200,
		Brand: "VISA", Last4: "1111", Ref: "pay_K1", SquarePaymentID: "pay_K1", SquareReceiptNumber: "K1rc", AuthCode: "A1B2C3", Entry: "keyed"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: total - 1000,
		Brand: "visa", Last4: "4242", SquarePaymentID: "pay_R1", AuthCode: "777", Entry: "contactless",
		EMV: map[string]string{"application_label": "VISA CREDIT", "aid": "A0000000031010", "cvm": "none"}})
	if err != nil || reader.BalanceCents != 0 {
		t.Fatalf("reader: %+v %v", reader, err)
	}
	ov, err := r.svc.Order(r.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]terminal.Payment{}
	for _, p := range ov.Payments {
		got[p.Tender] = p
	}
	if c := got["card"]; c.SquarePaymentID != "pay_R1" || c.AuthCode != "777" || c.Entry != "contactless" || c.EMV["aid"] != "A0000000031010" {
		t.Fatalf("reader payment kept %+v", c.CardInfo)
	}
	if c := got["manual_card"]; c.SquareReceiptNumber != "K1rc" || c.Entry != "keyed" {
		t.Fatalf("keyed payment kept %+v", c.CardInfo)
	}
	// main hears the auth code.
	if w := got["card"].Wire(); w["auth_code"] != "777" {
		t.Fatalf("wire %v", w)
	}

	// A refund of the typed card: Square first, then recorded with its refund id.
	key := tk()
	ref, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: key, Tender: "manual_card", AmountCents: 500, Refund: true, Reason: "Wrong order"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sq.calls) != 1 || sq.calls[0].id != "pay_K1" || sq.calls[0].cents != 500 || len(sq.calls[0].key) > 45 {
		t.Fatalf("square calls %+v", sq.calls)
	}
	if p := ref.Payment; p.SquareRefundID == "" || p.RefundOf != keyed.Payment.ID || p.SquarePaymentID != "pay_K1" || p.Last4 != "1111" {
		t.Fatalf("refund %+v", p)
	}
	w := ref.Payment.Wire()
	if w["ref"] != ref.Payment.SquareRefundID || w["refund_of"] != keyed.Payment.ID || w["amount_cents"] != int64(-500) {
		t.Fatalf("refund wire %v", w)
	}
	// The same key again: the stored refund, Square not asked again.
	again, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: key, Tender: "manual_card", AmountCents: 500, Refund: true, Reason: "Wrong order"})
	if err != nil || !again.Duplicate || len(sq.calls) != 1 || again.Payment.SquareRefundID != ref.Payment.SquareRefundID {
		t.Fatalf("again %+v %v (%d calls)", again, err, len(sq.calls))
	}
	// More than the typed card still holds (1200 - 500): refused before Square.
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "manual_card", AmountCents: 701, Refund: true, Reason: "x"}); !errors.Is(err, terminal.ErrRefundTooBig) {
		t.Fatalf("over the card: %v", err)
	}
	// Square refuses: nothing recorded. No answer: nothing recorded either.
	sq.err = &squareterm.Refused{Code: "refund_failed", Message: "no", SquareCode: "REFUND_DECLINED"}
	before, _ := r.svc.Order(r.ctx, id)
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 300, Refund: true, Reason: "x"}); err == nil {
		t.Fatal("a refused Square refund was recorded")
	}
	sq.err = squareterm.ErrUnreachable
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 300, Refund: true, Reason: "x"}); !errors.Is(err, squareterm.ErrUnreachable) {
		t.Fatalf("no answer: %v", err)
	}
	after, _ := r.svc.Order(r.ctx, id)
	if len(after.Payments) != len(before.Payments) {
		t.Fatalf("payments %d -> %d", len(before.Payments), len(after.Payments))
	}
	// Naming another order's Square payment: 400.
	sq.err = nil
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 100, Refund: true, Reason: "x",
		SquarePaymentID: "pay_elsewhere"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown square payment: %v", err)
	}
	// Naming the reader payment refunds on it.
	n := len(sq.calls)
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 100, Refund: true, Reason: "x",
		SquarePaymentID: "pay_R1"}); err != nil || len(sq.calls) != n+1 || sq.calls[n].id != "pay_R1" {
		t.Fatalf("reader refund: %v %+v", err, sq.calls)
	}
	// Bad Square fields: 400.
	for _, in := range []PaymentIn{{Entry: "magic"}, {EMV: map[string]string{"tvr": "x"}}, {AuthCode: "012345678901234567890123456789012"}} {
		in.ClientKey, in.Tender, in.AmountCents = tk(), "card", 1
		if _, err := r.svc.AddPayment(r.ctx, till, id, in); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

// Without a Square payment on the order a card refund is recorded as before.
func TestCardRefundWithoutSquareIsRecorded(t *testing.T) {
	r := newRig(t)
	sq := &fakeSquare{}
	r.svc.SetSquare(sq)
	r.login(t, till, "9876")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Payments: []PaymentIn{{Tender: "card", AmountCents: 500, Last4: "4242"}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.svc.AddPayment(r.ctx, till, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 500, Refund: true, Reason: "x"})
	if err != nil || len(sq.calls) != 0 || p.Payment.SquareRefundID != "" {
		t.Fatalf("plain card refund: %+v %v (%d calls)", p, err, len(sq.calls))
	}
}
