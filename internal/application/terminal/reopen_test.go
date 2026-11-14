package terminal

import (
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Reopen a check from All checks (contract 2.10ak, batch B): any cashier, the
// open batch only, back on the Pickup list (pickup / counter) as a paid
// takeout, a delivery marked open again; picked up again = off the list.

func reopenRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.main.set(PathTerminalConfig, staffConfig(t, 2,
		terminal.Staff{ID: "drv-1", FirstName: "Ali", Role: terminal.RoleDriver, PinHash: hash(t, "77")}))
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	r.svc.SetReports(fakeReports{r})
	return r
}

func placeFor(t *testing.T, r *rig, key, typ string, pays ...PaymentIn) string {
	t.Helper()
	in := OrderIn{ClientKey: key, Type: typ, Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}, Payments: pays}
	if typ == terminal.TypeDelivery {
		in.Customer = &Customer{Name: "Erik Davis", Phone: "4105550101", Address: &Address{Line1: "12 Main St", City: "Towson"}}
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	return res.Order.Order.ID
}

func TestReopenPaidCounterCheckLandsOnPickupAsPaid(t *testing.T) {
	r := reopenRig(t)
	r.login(t, till, "1234") // a cashier: no manager needed
	id := placeFor(t, r, "k-reo-00001", terminal.TypeCounter, PaymentIn{Tender: "cash", AmountCents: 2357, TenderedCents: 3000})
	if _, ok := pickupIDs(t, r)[id]; ok {
		t.Fatal("a paid counter check is off the Pickup list")
	}
	res, err := r.svc.Reopen(r.ctx, till, id)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Reopened || !res.Check.Reopened || res.Check.ReopenedAt == nil || res.Order.Status.ReopenedAt == nil {
		t.Fatalf("answer: %+v", res.Check)
	}
	if len(res.Payments) != 1 || res.Payments[0].Tender != "cash" || !res.Check.Paid {
		t.Fatalf("the tenders stay: %+v", res.Payments)
	}
	so, ok := pickupIDs(t, r)[id]
	if !ok || !so.Status.Paid || so.Status.CashDueCents != 0 {
		t.Fatalf("back on the Pickup list, paid: %v %+v", ok, so.Status)
	}
	if r.pub.count(terminal.EventOrderReopened) != 1 {
		t.Fatal("terminal.order.reopened")
	}
	d, err := r.svc.CheckByID(r.ctx, till, id)
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, e := range d.Journal {
		seen = seen || (e.Action == terminal.ActionReopen && e.StaffID == "cash-1" && e.DeviceID == till.DeviceID)
	}
	if !seen {
		t.Fatalf("journaled check.reopen under the cashier: %+v", d.Journal)
	}
	// Reopened again: the time moves, the same answer.
	if again, err := r.svc.Reopen(r.ctx, till, id); err != nil || !again.Reopened {
		t.Fatalf("again: %v", err)
	}

	// Picked up = closed again: off the list (finished after the reopen).
	if _, err := r.svc.PickedUp(r.ctx, till, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := pickupIDs(t, r)[id]; ok {
		t.Fatal("picked up again: off the list")
	}
	// Reopen after it was finished: on the list again, and picked up once more.
	if _, err := r.svc.Reopen(r.ctx, till, id); err != nil {
		t.Fatal(err)
	}
	if _, ok := pickupIDs(t, r)[id]; !ok {
		t.Fatal("a finished check reopened is on the list again")
	}
	if pu, err := r.svc.PickedUp(r.ctx, till, id); err != nil || pu.Already {
		t.Fatalf("finished again, not already: %+v %v", pu, err)
	}
	if _, ok := pickupIDs(t, r)[id]; ok {
		t.Fatal("off the list once more")
	}
}

func TestReopenDeliveryAndReadyAt(t *testing.T) {
	r := reopenRig(t)
	r.login(t, till, "1234")
	id := placeFor(t, r, "k-reo-00002", terminal.TypeDelivery, PaymentIn{Tender: "card", AmountCents: 2357, Brand: "visa"})
	if _, err := r.repo.MarkReady(r.ctx, id, false); err != nil {
		t.Fatal(err)
	}
	d, err := r.svc.CheckByID(r.ctx, till, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Check.ReadyAt == nil || d.Check.Reopened {
		t.Fatalf("ready_at set, not reopened: %+v", d.Check)
	}
	res, err := r.svc.Reopen(r.ctx, till, id)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Check.Reopened || res.Check.Type != terminal.TypeDelivery {
		t.Fatalf("a delivery open again: %+v", res.Check)
	}
	if _, ok := pickupIDs(t, r)[id]; ok {
		t.Fatal("a delivery never goes on the Pickup list")
	}
}

func TestReopenRefusals(t *testing.T) {
	r := reopenRig(t)
	r.login(t, till, "1234")
	paid := placeFor(t, r, "k-reo-00003", terminal.TypePickup, PaymentIn{Tender: "cash", AmountCents: 2357, TenderedCents: 2357})
	voided := placeFor(t, r, "k-reo-00004", terminal.TypeCounter)
	if _, err := r.svc.Void(r.ctx, till, voided, VoidIn{Reason: "test", ManagerPIN: "9876"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Reopen(r.ctx, till, voided); !errors.Is(err, terminal.ErrReopenVoided) {
		t.Fatalf("voided: %v", err)
	}
	if _, err := r.svc.Reopen(r.ctx, till, "not-a-uuid"); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("no such check: %v", err)
	}
	// A driver is refused; nobody logged in too.
	r.login(t, till2, "77")
	if _, err := r.svc.Reopen(r.ctx, till2, paid); !errors.Is(err, ErrDriverNotAllowed) {
		t.Fatalf("driver: %v", err)
	}
	if _, err := r.svc.Reopen(r.ctx, till3, paid); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no login: %v", err)
	}
	// Closed batches stay read-only.
	if _, _, err := r.repo.CloseBatch(r.ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Reopen(r.ctx, till, paid); !errors.Is(err, terminal.ErrBatchClosed) {
		t.Fatalf("closed batch: %v", err)
	}
}
