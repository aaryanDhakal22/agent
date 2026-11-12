package terminal

import (
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// The owner's permissions (2026-11-12) through the real Pi: a cashier's
// refunds add up against the cashier limit (per order), a card refund needs
// a manager, cash back for a card sale needs a manager, a manager logged in
// acts without a PIN; every one journaled.
func TestRefundPermissions(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // Cara: cashier, refunds up to $10 (role_limits)
	place := func(key string) (string, int64) {
		t.Helper()
		res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: key, Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Order.Order.ID, res.Order.Order.Money.Total
	}
	id, total := place("k-perm-0001")
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{Tender: "cash", AmountCents: total}); err != nil {
		t.Fatal(err)
	}
	refund := func(a Actor, tender string, cents int64, pin string) (*PaymentResult, error) {
		return r.svc.AddPayment(r.ctx, a, id, PaymentIn{Tender: tender, AmountCents: cents, Refund: true, Reason: "cold", ManagerPIN: pin})
	}
	// $6 + $4 = the $10 limit; one more cent needs a manager (per order, not per refund).
	for _, c := range []int64{600, 400} {
		if _, err := refund(till, "cash", c, ""); err != nil {
			t.Fatalf("refund %d: %v", c, err)
		}
	}
	var mr *terminal.ManagerRequired
	if _, err := refund(till, "cash", 1, ""); !errors.As(err, &mr) || mr.Action != terminal.ActionRefund || mr.Limit != 1000 || mr.Requested != 1001 {
		t.Fatalf("the refunds add up: %v", err)
	}
	if p, err := refund(till, "cash", 100, "9876"); err != nil || p.Payment.ApprovedBy != "mgr-1" {
		t.Fatalf("with a manager: %+v %v", p, err)
	}
	// A manager logged in: no PIN, and his refund doesn't count against the cashiers.
	mgrTill := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	r.login(t, mgrTill, "9876")
	if p, err := refund(mgrTill, "cash", 300, ""); err != nil || p.Payment.ApprovedBy != "" || p.Payment.StaffID != "mgr-1" {
		t.Fatalf("manager logged in: %+v %v", p, err)
	}
	if _, err := refund(till, "cash", 1, ""); !errors.As(err, &mr) || mr.Requested != 1001 {
		t.Fatalf("still $10 by cashiers: %v", err)
	}

	// A card sale: a card refund needs a manager (any amount), cash back for it too.
	id, total = place("k-perm-0002")
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{Tender: "card", AmountCents: total, Brand: "VISA"}); err != nil {
		t.Fatal(err)
	}
	if _, err := refund(till, "card", 1, ""); !errors.As(err, &mr) || mr.Action != terminal.NeedCardRefund {
		t.Fatalf("card refund: %v", err)
	}
	if _, err := refund(till, "cash", 100, ""); !errors.As(err, &mr) || mr.Action != terminal.NeedCashForCard || mr.Limit != 0 {
		t.Fatalf("cash for a card sale: %v", err)
	}
	if p, err := refund(till, "card", 500, "9876"); err != nil || p.Payment.ApprovedBy != "mgr-1" {
		t.Fatalf("card refund with a manager: %+v %v", p, err)
	}
	if p, err := refund(mgrTill, "card", 500, ""); err != nil {
		t.Fatalf("card refund by a manager logged in: %+v %v", p, err)
	}
	// Void of a card-paid order: a manager; a manager logged in just voids.
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left"}); !errors.As(err, &mr) || mr.Action != terminal.NeedCardRefund {
		t.Fatalf("void by card: %v", err)
	}
	if v, err := r.svc.Void(r.ctx, mgrTill, id, VoidIn{Reason: "left"}); err != nil || v.RefundedCents != total-1000 {
		t.Fatalf("manager void: %+v %v", v, err)
	}
	// A cash void within the cashier limit needs no one (unpaid is free anyway).
	id, total = place("k-perm-0003")
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{Tender: "cash", AmountCents: total}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left"}); total > 1000 && (!errors.As(err, &mr) || mr.Action != terminal.ActionVoid || mr.Limit != 1000) {
		t.Fatalf("cash void over $10: %v", err)
	}
	if o := journalOf(t, r, terminal.ActionOverride); len(o) != 2 {
		t.Fatalf("overrides journaled: %v", o)
	}
	// The card's brand is kept on the payment.
	pays, _ := r.repo.OrderPayments(r.ctx, mustOrderID(t, r, "k-perm-0002"))
	if pays[0].Brand != terminal.BrandVisa {
		t.Fatalf("brand %+v", pays[0])
	}
}

func mustOrderID(t *testing.T, r *rig, key string) string {
	t.Helper()
	so, err := r.repo.CounterOrderByKey(r.ctx, key)
	if err != nil || so == nil {
		t.Fatalf("order %s: %v", key, err)
	}
	return so.Order.ID
}
