package terminal

import (
	"errors"
	"testing"

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
