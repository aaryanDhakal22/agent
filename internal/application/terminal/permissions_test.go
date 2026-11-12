package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: total}); err != nil {
		t.Fatal(err)
	}
	refund := func(a Actor, tender string, cents int64, pin string) (*PaymentResult, error) {
		return r.svc.AddPayment(r.ctx, a, id, PaymentIn{ClientKey: tk(), Tender: tender, AmountCents: cents, Refund: true, Reason: "cold", ManagerPIN: pin})
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
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: total, Brand: "VISA"}); err != nil {
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
	// A paid order is refunded first (the refund rules), then voided - by
	// anyone (contract 2.10s §5): no manager for the void itself.
	var pf *terminal.PaidFirst
	if _, err := r.svc.Void(r.ctx, mgrTill, id, VoidIn{Reason: "left"}); !errors.As(err, &pf) || pf.PaidCents != total-1000 {
		t.Fatalf("void of a card-paid order: %v", err)
	}
	if _, err := refund(mgrTill, "card", total-1000, ""); err != nil {
		t.Fatal(err)
	}
	if v, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left"}); err != nil || v.RefundedCents != 0 {
		t.Fatalf("cashier void after the refund: %+v %v", v, err)
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

// Goodwill refunds, refunds made outside (Square's dashboard) and the drawer
// kick (contract 2.10n, the owner 2026-11-12).
func TestGoodwillOutsideAndKick(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // cashier, $10 limit
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-good-0001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Payments: []PaymentIn{{Tender: "cash", AmountCents: 2357, TenderedCents: 3000}}})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	g, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 500, Refund: true, Goodwill: true, Reason: "late"})
	if err != nil {
		t.Fatal(err)
	}
	m := g.Order.Order.Money
	if m.Total != total-500 || g.BalanceCents != 0 || !g.Order.Status.Paid || !g.KickDrawer || g.Payment.RefundKind != terminal.RefundGoodwill {
		t.Fatalf("goodwill: total %d (was %d) balance %d kick %v %+v", m.Total, total, g.BalanceCents, g.KickDrawer, g.Payment)
	}
	if d := m.Discounts[len(m.Discounts)-1]; d.Type != terminal.DiscountAdjustment || d.Label != "Goodwill: late" || d.Amount != 500 {
		t.Fatalf("adjustment %+v", m.Discounts)
	}
	if k := journalOf(t, r, terminal.ActionDrawerKick); len(k) != 1 || k[0]["reason"] != "refund" || k[0]["cents"] != float64(500) {
		t.Fatalf("kick journaled: %v", k)
	}
	// Goodwill counts against the cashier's $10 per order like any cash refund.
	var mr *terminal.ManagerRequired
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 501, Refund: true, Goodwill: true, Reason: "more"}); !errors.As(err, &mr) ||
		mr.Requested != 1001 {
		t.Fatalf("goodwill over the limit: %v", err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 1, TipCents: 1, Refund: true, Goodwill: true, Reason: "x"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("goodwill tip: %v", err)
	}

	// Refunded outside (Square's dashboard): a manager's; nothing kicks.
	res, _ = r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-good-0002", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Payments: []PaymentIn{{Tender: "square_app", AmountCents: 2357}}})
	id2 := res.Order.Order.ID
	out := PaymentIn{ClientKey: "k-out-000001", Tender: "square_app", AmountCents: 2357, Refund: true, Outside: true, Reason: "Square dashboard"}
	if _, err := r.svc.AddPayment(r.ctx, till, id2, out); !errors.As(err, &mr) || mr.Action != terminal.NeedCardRefund {
		t.Fatalf("outside by a cashier: %v", err)
	}
	out.ManagerPIN = "9876"
	o, err := r.svc.AddPayment(r.ctx, till, id2, out)
	if err != nil || o.KickDrawer || o.Payment.RefundKind != terminal.RefundOutside || o.Payment.DrawerID != "" {
		t.Fatalf("outside: %+v %v", o, err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 1, Refund: true, Outside: true, Reason: "x"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("outside on cash: %v", err)
	}
	// A void that hands cash back kicks the drawer.
	res, _ = r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-good-0003", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: soda}}}}}})
	sodaTotal := res.Order.Order.Money.Total
	if _, err := r.svc.AddPayment(r.ctx, till, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: sodaTotal}); err != nil {
		t.Fatal(err)
	}
	var pf *terminal.PaidFirst
	if _, err := r.svc.Void(r.ctx, till, res.Order.Order.ID, VoidIn{Reason: "left"}); !errors.As(err, &pf) || pf.PaidCents != sodaTotal {
		t.Fatalf("void of a cash-paid order: %v", err)
	}
	if back := refundAll(t, r, till, res.Order.Order.ID); len(back) != 1 || !back[0].KickDrawer {
		t.Fatalf("the cash refund kicks the drawer: %+v", back)
	}
}

// A tip written on a manually entered card's receipt, added afterwards
// (contract 2.10p): the difference is its own payment; lowering it refunds
// the cut; only card payments take one.
func TestTipAddedToACardPayment(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-tip-00001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Payments: []PaymentIn{{Tender: "manual_card", AmountCents: 2356, Last4: "4242"}, {Tender: "cash", AmountCents: 1, TenderedCents: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	pays, _ := r.repo.OrderPayments(r.ctx, id)
	card, cash := pays[0], pays[1]
	tr, err := r.svc.AddTip(r.ctx, till, id, card.ID, PaymentTipIn{TipCents: 300})
	if err != nil || tr.TipCents != 300 || tr.Payment == nil || tr.Payment.AmountCents != 0 || tr.Payment.TipCents != 300 ||
		tr.Payment.Ref != card.ID || tr.Payment.Tender != "manual_card" || tr.Order.Status.TipCents != 300 {
		t.Fatalf("add tip: %+v %v", tr, err)
	}
	tr, err = r.svc.AddTip(r.ctx, till, id, card.ID, PaymentTipIn{TipCents: 100})
	if err != nil || tr.TipCents != 100 || !tr.Payment.Refund || tr.Payment.TipCents != 200 || tr.Order.Status.TipCents != 100 {
		t.Fatalf("lower tip: %+v %v", tr, err)
	}
	if same, err := r.svc.AddTip(r.ctx, till, id, card.ID, PaymentTipIn{TipCents: 100}); err != nil || same.Payment != nil {
		t.Fatalf("same tip: %+v %v", same, err)
	}
	if _, err := r.svc.AddTip(r.ctx, till, id, cash.ID, PaymentTipIn{TipCents: 100}); !errors.Is(err, ErrNotTippable) {
		t.Fatalf("cash: %v", err)
	}
	if j := journalOf(t, r, terminal.ActionPaymentTip); len(j) != 2 || j[0]["to_cents"] == nil {
		t.Fatalf("journal: %v", j)
	}
}

// The same cashier may be logged in on both terminals at once.
func TestSameCashierOnTwoTerminals(t *testing.T) {
	r := newRig(t)
	other := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	r.login(t, till, "1234")
	r.login(t, other, "1234")
	for i, a := range []Actor{till, other} {
		if _, err := r.svc.PlaceOrder(r.ctx, a, OrderIn{ClientKey: fmt.Sprintf("k-two-%05d", i), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
			Payments: []PaymentIn{{Tender: "cash", AmountCents: 2357, TenderedCents: 3000}}}); err != nil {
			t.Fatalf("terminal %d: %v", i, err)
		}
	}
}

// The store's receipt header / footer reach the terminals with main's
// settings (contract 2.10q): login and the menu carry settings.store.
func TestSettingsCarryTheStore(t *testing.T) {
	r := newRig(t)
	r.svc.SetStoreInfo(func() map[string]any {
		return map[string]any{"name": "Lombardi's", "address": "931 Taylor Ave", "phone": "4105550100", "website": "lombardis.com", "footer": "Thank you"}
	})
	res, err := r.svc.Login(r.ctx, till, "1234")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		AutoLock int            `json:"auto_lock_minutes"`
		Store    map[string]any `json:"store"`
	}
	if err := json.Unmarshal(res.Settings, &got); err != nil || got.Store["address"] != "931 Taylor Ave" || got.Store["footer"] != "Thank you" || got.AutoLock != 3 {
		t.Fatalf("login settings: %s %v", res.Settings, err)
	}
	body, _, err := r.svc.MenuView()
	if err != nil || !strings.Contains(string(body), `"website":"lombardis.com"`) {
		t.Fatalf("menu: %v", err)
	}
}
