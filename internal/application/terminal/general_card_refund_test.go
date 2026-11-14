package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/terminal"
)

// A general refund to a card (batch J, the owner 2026-11-14; contract 2.10x
// "A general refund to a card"): Square first (an unlinked refund to the card
// field's token, or the fallback's linked refund), recorded only after Square
// took it; a manager every time; never in a drawer; the till / Z on the card.

// fakeGeneralSquare plays main's square.general_refund / square.card_payments
// / square.refund.
type fakeGeneralSquare struct {
	fakeSquare
	general []struct {
		nonce, key string
		cents      int64
	}
	generalErr error
	listed     []squareterm.CardPayment
	listErr    error
}

func (f *fakeGeneralSquare) GeneralRefund(_ context.Context, nonce string, cents int64, key, _ string) (squareterm.GeneralRefundResult, error) {
	f.general = append(f.general, struct {
		nonce, key string
		cents      int64
	}{nonce, key, cents})
	if f.generalErr != nil {
		return squareterm.GeneralRefundResult{}, f.generalErr
	}
	return squareterm.GeneralRefundResult{RefundID: "sqref_" + key[:10], Status: "PENDING", Brand: "VISA", Last4: "5858"}, nil
}

func (f *fakeGeneralSquare) CardPayments(_ context.Context, last4 string, cents int64) ([]squareterm.CardPayment, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []squareterm.CardPayment
	for _, p := range f.listed {
		if p.Last4 == last4 && p.RefundableCents >= cents {
			out = append(out, p)
		}
	}
	return out, nil
}

func refundEvents(t *testing.T, r *rig) []map[string]any {
	t.Helper()
	evs, err := r.repo.ListUnsyncedEvents(r.ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range evs {
		if e.Type != terminal.EventRefundGeneral {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(e.Payload, &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, body)
	}
	return out
}

func journalCount(t *testing.T, r *rig, action string) int {
	t.Helper()
	js, err := r.repo.Journal(r.ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range js {
		if e.Action == action {
			n++
		}
	}
	return n
}

func TestGeneralCardRefundUnlinkedIsAManagersAndNeverInTheDrawer(t *testing.T) {
	r := newRig(t)
	sq := &fakeGeneralSquare{}
	r.svc.SetSquare(sq)
	r.login(t, till, "1234") // Cara: cashier
	in := GeneralRefundIn{ClientKey: tk(), Tender: "manual_card", AmountCents: 750, Reason: "Overcharged yesterday",
		CardNonce: "cnon:card-nonce-ok", Brand: "visa", Last4: "1111"}
	// A manager every time: Square is never asked before the PIN.
	var mr *terminal.ManagerRequired
	if _, err := r.svc.GeneralRefund(r.ctx, till, in); !errors.As(err, &mr) || mr.Action != terminal.ActionRefund || mr.Limit != 0 || mr.Requested != 750 {
		t.Fatalf("a cashier alone: %v", err)
	}
	if len(sq.general) != 0 {
		t.Fatal("Square was asked before the manager allowed it")
	}
	in.ManagerPIN = "9876"
	res, err := r.svc.GeneralRefund(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Payment
	if res.Duplicate || res.KickDrawer || p.OrderID != "" || !p.Refund || p.RefundKind != terminal.RefundGeneral || p.Tender != terminal.TenderManualCard ||
		p.AmountCents != 750 || p.ApprovedBy != "mgr-1" || p.DrawerID != "" || p.BatchID == 0 {
		t.Fatalf("refund %+v", res)
	}
	// Square's answer wins: its refund id as ref, the card it went to.
	if len(sq.general) != 1 || sq.general[0].nonce != "cnon:card-nonce-ok" || sq.general[0].key != squareterm.GeneralRefundKey(in.ClientKey) ||
		p.Ref == "" || p.SquareRefundID != p.Ref || p.Brand != terminal.BrandVisa || p.Last4 != "5858" || p.Entry != "keyed" {
		t.Fatalf("square %+v, payment %+v", sq.general, p)
	}
	// The same tap again (a lost answer): the stored refund, Square not asked twice.
	again, err := r.svc.GeneralRefund(r.ctx, till, in)
	if err != nil || !again.Duplicate || again.Payment.ID != p.ID || again.Payment.SquareRefundID != p.SquareRefundID || len(sq.general) != 1 {
		t.Fatalf("again %+v %v (%d calls)", again, err, len(sq.general))
	}
	var kr *terminal.KeyReused
	other := in
	other.AmountCents = 800
	if _, err := r.svc.GeneralRefund(r.ctx, till, other); !errors.As(err, &kr) {
		t.Fatalf("key reused: %v", err)
	}
	// Never the drawer: no cash refund on it (no drawer row may exist at all).
	if d, err := r.repo.DrawerByDevice(r.ctx, till.DeviceID); err != nil || (d != nil && (d.CashRefundsCents != 0 || d.ExpectedCents != d.StartCents)) {
		t.Fatalf("drawer %+v %v", d, err)
	}
	// main: refund.general on the card's method with Square's refund id.
	evs := refundEvents(t, r)
	if len(evs) != 1 {
		t.Fatalf("%d refund.general events", len(evs))
	}
	pw := evs[0]["payment"].(map[string]any)
	if pw["method"] != "manual_card" || pw["amount_cents"].(float64) != -750 || pw["ref"] != p.SquareRefundID || pw["last4"] != "5858" ||
		pw["brand"] != "visa" || evs[0]["kind"] != "general" || evs[0]["reason"] != "Overcharged yesterday" {
		t.Fatalf("event %v", evs[0])
	}
	// The journal: the refund, the manager's override, the Square call; never a drawer kick.
	if journalCount(t, r, terminal.ActionRefund) != 1 || journalCount(t, r, terminal.ActionOverride) != 1 ||
		journalCount(t, r, terminal.ActionSquareGeneralRefund) != 1 || journalCount(t, r, terminal.ActionDrawerKick) != 0 {
		t.Fatal("journal: want 1 refund, 1 override, 1 square.general_refund, no drawer.kick")
	}
	js, _ := r.repo.Journal(r.ctx, 500)
	for _, e := range js {
		if strings.Contains(string(e.Detail), "cnon:") {
			t.Fatalf("the card token reached the journal: %s", e.Detail)
		}
	}
	if r.pub.count(EventGeneralRefund) != 1 {
		t.Fatalf("sse %v", r.pub.events)
	}

	// The till and the Z: a refund on the card (Visa row, manual_card tender), never the cash.
	batch := p.BatchID
	zin, err := r.repo.ZInputs(r.ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	z := terminal.BuildZ(zin)
	var mc terminal.ZTender
	for _, tt := range z.Tenders {
		if tt.Tender == terminal.TenderManualCard {
			mc = tt
		}
		if tt.Tender == terminal.TenderCash && tt.RefundedCents != 0 {
			t.Fatalf("the Z refunded cash %+v", tt)
		}
	}
	if mc.RefundedCents != 750 || z.GeneralRefunds.Count != 1 || z.GeneralRefunds.Cents != 750 || z.Reconcile.GeneralRefundsCents != 750 || z.CashOutside != 0 {
		t.Fatalf("z manual_card %+v, general %+v, reconcile %d, cash outside %d", mc, z.GeneralRefunds, z.Reconcile.GeneralRefundsCents, z.CashOutside)
	}
	tl := terminal.BuildTill(terminal.TillInput{Z: zin})
	if tl.Detail.Cash.Amount != 0 || tl.Detail.GeneralCardRefunds.Count != 1 || tl.Detail.GeneralCardRefunds.Amount != -750 ||
		tl.Detail.GeneralRefunds.Amount != -750 || tl.Summary.Refunds != -750 || tl.Summary.TotalCash != 0 || tl.Summary.TotalCreditCard != -750 {
		t.Fatalf("till cash %+v, card general %+v, general %+v, refunds %d, total cash %d, card %d", tl.Detail.Cash, tl.Detail.GeneralCardRefunds,
			tl.Detail.GeneralRefunds, tl.Summary.Refunds, tl.Summary.TotalCash, tl.Summary.TotalCreditCard)
	}
}

func TestGeneralCardRefundRefusedOrUnknownRecordsNothing(t *testing.T) {
	r := newRig(t)
	sq := &fakeGeneralSquare{}
	r.svc.SetSquare(sq)
	r.login(t, till, "9876") // a manager: no PIN
	in := GeneralRefundIn{ClientKey: tk(), Tender: "manual_card", AmountCents: 500, Reason: "Wrong charge", CardNonce: "cnon:card-nonce-ok"}
	for _, e := range []error{
		&squareterm.Refused{Code: "refund_failed", SquareCode: "NOT_FOUND", Message: "Card nonce not found"},
		&squareterm.Refused{Code: squareterm.CodeUnlinkedUnsupported, Message: "no unlinked refunds"},
		squareterm.ErrUnreachable, // 503: NOT known whether Square refunded
	} {
		sq.generalErr = e
		if _, err := r.svc.GeneralRefund(r.ctx, till, in); !errors.Is(err, e) && !errors.As(err, new(*squareterm.Refused)) {
			t.Fatalf("%v: got %v", e, err)
		}
		if got, err := r.repo.PaymentByKey(r.ctx, "", in.ClientKey); err != nil || got != nil {
			t.Fatalf("%v: recorded %+v %v", e, got, err)
		}
	}
	if len(refundEvents(t, r)) != 0 || journalCount(t, r, terminal.ActionRefund) != 0 || journalCount(t, r, terminal.ActionSquareGeneralRefund) != 3 {
		t.Fatal("a refused / unknown card refund left an event or a refund in the journal")
	}
	// The manager tries again after no answer: the same key = the same Square refund, recorded once.
	sq.generalErr = nil
	res, err := r.svc.GeneralRefund(r.ctx, till, in)
	if err != nil || res.Duplicate {
		t.Fatalf("retry %+v %v", res, err)
	}
	for _, c := range sq.general {
		if c.key != squareterm.GeneralRefundKey(in.ClientKey) {
			t.Fatalf("a retry used another Square key: %+v", sq.general)
		}
	}
	if len(refundEvents(t, r)) != 1 {
		t.Fatal("want one refund.general")
	}
	// No Square on this Pi: 503, nothing recorded.
	r2 := newRig(t)
	r2.login(t, till, "9876")
	if _, err := r2.svc.GeneralRefund(r2.ctx, till, GeneralRefundIn{ClientKey: tk(), Tender: "manual_card", AmountCents: 1, Reason: "x",
		CardNonce: "cnon:card-nonce-ok"}); !errors.Is(err, ErrSquareUnavailable) {
		t.Fatalf("no square: %v", err)
	}
}

func TestGeneralCardRefundFallbackIsALinkedRefund(t *testing.T) {
	r := newRig(t)
	sq := &fakeGeneralSquare{listed: []squareterm.CardPayment{
		{SquarePaymentID: "pay_big", AmountCents: 2000, RefundableCents: 2000, Brand: "MASTERCARD", Last4: "4242", Entry: "chip"},
		{SquarePaymentID: "pay_small", AmountCents: 300, RefundableCents: 300, Brand: "VISA", Last4: "4242", Entry: "keyed"},
	}}
	r.svc.SetSquare(sq)
	r.login(t, till, "1234") // a cashier may list
	got, err := r.svc.GeneralRefundCardPayments(r.ctx, till, "4242", 650)
	if err != nil || len(got) != 1 || got[0].SquarePaymentID != "pay_big" {
		t.Fatalf("listed %+v %v", got, err)
	}
	if _, err := r.svc.GeneralRefundCardPayments(r.ctx, till, "42", 650); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad last4: %v", err)
	}
	in := GeneralRefundIn{ClientKey: tk(), Tender: "card", AmountCents: 650, Reason: "Overcharged", SquarePaymentID: "pay_big",
		Brand: "mastercard", Last4: "4242", ManagerPIN: "9876"}
	res, err := r.svc.GeneralRefund(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Payment
	if len(sq.calls) != 1 || sq.calls[0].id != "pay_big" || sq.calls[0].cents != 650 || sq.calls[0].key != squareterm.GeneralRefundKey(in.ClientKey) ||
		len(sq.general) != 0 {
		t.Fatalf("square calls %+v / %+v", sq.calls, sq.general)
	}
	if p.Tender != terminal.TenderCard || p.SquarePaymentID != "pay_big" || p.SquareRefundID == "" || p.Ref != p.SquareRefundID ||
		p.Brand != terminal.BrandMastercard || p.Last4 != "4242" || p.DrawerID != "" || res.KickDrawer || p.RefundOf != "" {
		t.Fatalf("fallback refund %+v", p)
	}
	// The stored row keeps its card block (the duplicate answer reads it back).
	again, err := r.svc.GeneralRefund(r.ctx, till, in)
	if err != nil || !again.Duplicate || again.Payment.SquarePaymentID != "pay_big" || len(sq.calls) != 1 {
		t.Fatalf("again %+v %v", again, err)
	}
}

func TestGeneralRefundCardBodyChecks(t *testing.T) {
	r := newRig(t)
	r.svc.SetSquare(&fakeGeneralSquare{})
	r.login(t, till, "9876")
	for name, in := range map[string]GeneralRefundIn{
		"card with neither":   {Tender: "manual_card"},
		"card with both":      {Tender: "manual_card", CardNonce: "cnon:x", SquarePaymentID: "pay_x"},
		"cash with a nonce":   {Tender: "cash", CardNonce: "cnon:x"},
		"cash with a payment": {SquarePaymentID: "pay_x"},
		"a square-app card":   {Tender: "square_app", CardNonce: "cnon:x"},
		"comp":                {Tender: "comp"},
	} {
		in.ClientKey, in.AmountCents, in.Reason = tk(), 500, "x"
		if _, err := r.svc.GeneralRefund(r.ctx, till, in); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: %v, want 400", name, err)
		}
	}
}
