package terminal

import (
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Send to phone (contract 2.10z): a phone's Square payment is recorded as the
// cashier who sent the charge, even after her terminal locked; a pair_phone
// code needs a manager.
func TestRecordAsTheSender(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-phone-0001", Type: "pickup",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	if st, err := r.svc.CurrentStaff(till); err != nil || st.ID != "cash-1" {
		t.Fatalf("current: %+v %v", st, err)
	}
	if _, err := r.svc.RequireManager(r.ctx, till, "", nil, ActionPairPhone); !isManagerRequired(err) {
		t.Fatalf("a cashier pairs a phone: %v", err)
	}
	if mgr, err := r.svc.RequireManager(r.ctx, till, "9876", nil, ActionPairPhone); err != nil || mgr.ID != "mgr-1" {
		t.Fatalf("with the manager's PIN: %+v %v", mgr, err)
	}
	r.svc.Logout(r.ctx, till)
	if _, err := r.svc.CurrentStaff(till); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("logged out: %v", err)
	}
	in := PaymentIn{ClientKey: "ph-key-0001", Tender: terminal.TenderSquareApp, AmountCents: total, TipCents: 150, Ref: "sqpay-1", Last4: "1111", Brand: "visa"}
	p, err := r.svc.RecordAs(r.ctx, till, "cash-1", id, in)
	if err != nil || p.Payment.StaffID != "cash-1" || p.Payment.DeviceID != till.DeviceID || p.BalanceCents != 0 || p.Payment.TipCents != 150 {
		t.Fatalf("recorded: %+v %v", p, err)
	}
	if again, err := r.svc.RecordAs(r.ctx, till, "cash-1", id, in); err != nil || !again.Duplicate {
		t.Fatalf("again: %+v %v", again, err)
	}
	// A staff member main dropped since still gets the payment by id.
	if _, err := r.svc.RecordAs(r.ctx, till, "gone-1", id, PaymentIn{ClientKey: "ph-key-0002", Tender: terminal.TenderSquareApp, AmountCents: 1}); !errors.Is(err, terminal.ErrOverBalance) {
		t.Fatalf("over balance: %v", err)
	}
	if _, err := r.svc.RecordAs(r.ctx, till, "cash-1", id, PaymentIn{ClientKey: "ph-key-0003", Tender: "cash", AmountCents: 1, Refund: true, Reason: "x"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a refund: %v", err)
	}
}

func isManagerRequired(err error) bool {
	var mr *terminal.ManagerRequired
	return errors.As(err, &mr)
}

// An unsent cart (the owner, 2026-11-12): checked when the phone charge is
// made (the manager's PIN then), placed with the payment only when paid; the
// station tickets print then, never before.
func TestPrecheckThenPlaceAs(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // cashier: discounts up to 10 %
	in := OrderIn{ClientKey: "k-cart-0001", Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}},
		Discount: &Discount{Pct: 30, Reason: "regular"}}}
	if _, err := r.svc.PrecheckOrder(r.ctx, till, in); !isManagerRequired(err) {
		t.Fatalf("30 %% needs a manager: %v", err)
	}
	in.ManagerPIN = "9876"
	chk, err := r.svc.PrecheckOrder(r.ctx, till, in)
	if err != nil || chk.ApprovedBy != "mgr-1" || chk.TotalCents <= 0 {
		t.Fatalf("precheck: %+v %v", chk, err)
	}
	if all, _ := r.svc.Orders(r.ctx, false); len(all) != 0 || len(r.orders.printed) != 0 {
		t.Fatalf("nothing placed or printed: %d orders, %d printed", len(all), len(r.orders.printed))
	}
	if _, err := r.svc.PrecheckOrder(r.ctx, till, OrderIn{ClientKey: "k-cart-0002", Type: "pickup",
		Payments: []PaymentIn{{Tender: "cash", AmountCents: 100}}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("payments in a charge's cart: %v", err)
	}
	r.svc.Logout(r.ctx, till) // her terminal locked before the customer tapped
	pay := PaymentIn{Tender: terminal.TenderSquareApp, AmountCents: chk.TotalCents, Ref: "sqpay-9", Last4: "1111", Brand: "visa"}
	in.Payments = []PaymentIn{pay}
	res, err := r.svc.PlaceAs(r.ctx, till, "cash-1", chk.ApprovedBy, in)
	if err != nil || res.Duplicate || !res.Order.Status.Paid {
		t.Fatalf("placed: %+v %v", res, err)
	}
	view, err := r.svc.OrderByKey(r.ctx, "k-cart-0001")
	if err != nil || view == nil || len(view.Payments) != 1 || view.Payments[0].StaffID != "cash-1" || view.Payments[0].Ref != "sqpay-9" {
		t.Fatalf("by key: %+v %v", view, err)
	}
	r.login(t, till, "1234")
	if _, err := r.svc.PrecheckOrder(r.ctx, till, OrderIn{ClientKey: "k-cart-0001", Type: "pickup"}); err == nil {
		t.Fatal("a placed key prechecks again")
	} else if ap := (*AlreadyPlaced)(nil); !errors.As(err, &ap) || ap.OrderID != res.Order.Order.ID {
		t.Fatalf("already placed: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.orders.mu.Lock()
		n := len(r.orders.printed)
		r.orders.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the station tickets printed %d times", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if o, _ := r.svc.OrderByKey(r.ctx, "k-nope-0001"); o != nil {
		t.Fatal("an unknown key has an order")
	}
}
