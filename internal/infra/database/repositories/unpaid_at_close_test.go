package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// The owner, 2026-11-13: a forced close (and the auto-close) follows the
// store's close_unpaid_counter_action: waste (default) writes an unpaid
// counter check off as Void waste "Unpaid at close", carry keeps it open; a
// partly paid check is always carried.

func TestCloseUnpaid_NotForcedSaysWhatForceDoes(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	a, _ := counterOrder(t, r, "key-unpaid-a", 1, 1299)
	b, _ := counterOrder(t, r, "key-unpaid-b", 1, 2000, pay(terminal.TenderCash, 500))
	// The list route says it too (the terminal asks it before the close).
	l, err := r.UnsettledChecks(ctx)
	if err != nil || l.ForceAction != order.UnpaidWaste || l.Force == nil || l.Force.WasteCount != 1 || l.Force.CarryCount != 1 {
		t.Fatalf("unsettled list preview: %+v %v", l, err)
	}
	var un *order.UnsettledError
	if _, _, err := r.CloseBatch(ctx, false); !errors.As(err, &un) {
		t.Fatalf("not forced: %v", err)
	}
	if un.ForceAction != order.UnpaidWaste || un.Force.WasteCount != 1 || un.Force.WasteCents != 1299 ||
		un.Force.CarryCount != 1 || un.Force.CarryDueCents != 1500 || len(un.Checks) != 2 {
		t.Fatalf("preview: %+v %+v", un.ForceAction, un.Force)
	}
	on := map[string]string{}
	for _, c := range un.Checks {
		on[c.OrderID] = c.OnForce
	}
	if on[a.Order.ID] != order.UnpaidWaste || on[b.Order.ID] != order.UnpaidCarry {
		t.Fatalf("on_force: %v", on)
	}
	// The carry setting: both carried.
	_, _, err = r.CloseBatch(order.WithCloseUnpaid(ctx, "carry"), false)
	if !errors.As(err, &un) || un.ForceAction != order.UnpaidCarry || un.Force.WasteCount != 0 || un.Force.CarryCount != 2 {
		t.Fatalf("carry preview: %v %+v", err, un)
	}
	// Nothing changed: both still open, no write-off.
	if got := eventsOfType(t, r, terminal.EventOrderVoid); len(got) != 0 {
		t.Fatalf("a refused close voided: %v", got)
	}
}

func TestCloseUnpaid_ForcedWasteDefault(t *testing.T) {
	r := newFloorRepo(t)
	ctx := order.WithCloseActor(order.WithCloseTrigger(context.Background(), "terminal"),
		order.CloseActor{StaffID: "11111111-1111-4111-8111-111111111111", StaffName: "Mia M.", DeviceID: "term-1", Trigger: "terminal"})
	a, _ := counterOrder(t, r, "key-waste-a", 1, 1299)
	b, _ := counterOrder(t, r, "key-waste-b", 1, 2000, pay(terminal.TenderCash, 500)) // partly paid
	paid, _ := counterOrder(t, r, "key-waste-c", 1, 800, pay(terminal.TenderCash, 800))
	closed, _, err := r.CloseBatch(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	u := closed.UnpaidAtClose
	if u == nil || u.Count != 1 || u.TotalCents != 1299 || u.Checks[0].OrderID != a.Order.ID || u.Reason != "Unpaid at close" ||
		u.ClosedBy.StaffName != "Mia M." || u.PrintedBy != "pi" || u.Auto || len(u.Checks[0].Items) != 1 {
		t.Fatalf("unpaid at close: %+v", u)
	}
	got, _ := r.GetByID(context.Background(), uuid.MustParse(a.Order.ID))
	if got.Status.State != order.StateCancelled || got.Status.CancelReason != terminal.WasteReason || got.Status.Carried {
		t.Fatalf("written off: %+v", got.Status)
	}
	// The partly paid one is carried, still payable; the paid one untouched.
	if got, _ := r.GetByID(context.Background(), uuid.MustParse(b.Order.ID)); !got.Status.Carried || got.Status.State == order.StateCancelled {
		t.Fatalf("partly paid carried: %+v", got.Status)
	}
	if closed.Unsettled == nil || closed.Unsettled.Count != 1 || closed.Unsettled.Checks[0].OrderID != b.Order.ID {
		t.Fatalf("unsettled after the close: %+v", closed.Unsettled)
	}
	if got, _ := r.GetByID(context.Background(), uuid.MustParse(paid.Order.ID)); got.Status.State == order.StateCancelled {
		t.Fatalf("paid check: %+v", got.Status)
	}
	// main hears order.void {kind: waste, waste_reason}; nothing refunded.
	voids := eventsOfType(t, r, terminal.EventOrderVoid)
	if len(voids) != 1 || voids[0]["order_id"] != a.Order.ID || voids[0]["kind"] != "waste" || voids[0]["waste_reason"] != "Unpaid at close" ||
		voids[0]["staff_id"] != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("order.void: %v", voids)
	}
	if pays := eventsOfType(t, r, terminal.EventOrderPayment); len(pays) != 0 {
		for _, p := range pays {
			if p["order_id"] == a.Order.ID {
				t.Fatalf("a write-off moved money: %v", p)
			}
		}
	}
	// The journal: void.waste in the closer's name; the Z's Waste section and balance.
	j, _ := r.Journal(context.Background(), 100)
	var w *terminal.Entry
	for i := range j {
		if j[i].Action == terminal.ActionVoidWaste {
			w = &j[i]
		}
	}
	if w == nil || w.OrderID != a.Order.ID || w.DeviceID != "term-1" || w.StaffID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("journal: %+v", w)
	}
	var d map[string]any
	_ = json.Unmarshal(w.Detail, &d)
	if d["reason"] != "Unpaid at close" || d["at_close"] != true || d["total_cents"] != float64(1299) {
		t.Fatalf("journal detail: %v", d)
	}
	in, err := r.ZInputs(context.Background(), closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	z := terminal.BuildZ(in)
	if z.Waste.Count != 1 || z.Waste.Cents != 1299 || len(z.Waste.Reasons) != 1 || z.Waste.Reasons[0].Reason != "Unpaid at close" {
		t.Fatalf("Z waste: %+v", z.Waste)
	}
	if z.Reconcile.DifferenceCents != 0 || z.Unpaid.Cents != 1500 {
		t.Fatalf("Z balance: unpaid %+v reconcile %+v", z.Unpaid, z.Reconcile)
	}
}

func TestCloseUnpaid_ForcedCarry(t *testing.T) {
	r := newFloorRepo(t)
	ctx := order.WithCloseUnpaid(context.Background(), order.UnpaidCarry)
	a, _ := counterOrder(t, r, "key-carry-a", 1, 1299)
	closed, _, err := r.CloseBatch(ctx, true)
	if err != nil || closed.UnpaidAtClose != nil {
		t.Fatalf("carry close: %+v %v", closed, err)
	}
	if closed.Unsettled == nil || closed.Unsettled.Count != 1 || !closed.Unsettled.Checks[0].Carried {
		t.Fatalf("carried: %+v", closed.Unsettled)
	}
	got, _ := r.GetByID(ctx, uuid.MustParse(a.Order.ID))
	if !got.Status.Carried || got.Status.State == order.StateCancelled {
		t.Fatalf("carried check: %+v", got.Status)
	}
	if v := eventsOfType(t, r, terminal.EventOrderVoid); len(v) != 0 {
		t.Fatalf("carry voided: %v", v)
	}
	// The next manual close is refused again over the carried check (it
	// still blocks), and forced with waste it is written off then.
	if _, _, err := r.CloseBatch(context.Background(), false); !errors.As(err, new(*order.UnsettledError)) {
		t.Fatalf("carried check still blocks: %v", err)
	}
	closed, _, err = r.CloseBatch(context.Background(), true)
	if err != nil || closed.UnpaidAtClose == nil || closed.UnpaidAtClose.Count != 1 || !closed.UnpaidAtClose.Checks[0].Carried {
		t.Fatalf("carried check written off later: %+v %v", closed, err)
	}
	if got, _ := r.GetByID(ctx, uuid.MustParse(a.Order.ID)); got.Status.Carried || got.Status.State != order.StateCancelled {
		t.Fatalf("settled by the write-off: %+v", got.Status)
	}
}

func TestCloseUnpaid_AutoFollowsTheSetting(t *testing.T) {
	r := newFloorRepo(t)
	auto := order.WithCloseKind(context.Background(), order.CloseAuto)
	a, _ := counterOrder(t, r, "key-auto-a", 1, 1299)
	closed, _, err := r.CloseBatch(auto, true)
	if err != nil || closed.UnpaidAtClose == nil || !closed.UnpaidAtClose.Auto || closed.UnpaidAtClose.ClosedBy.Trigger != "auto" ||
		closed.UnpaidAtClose.ClosedBy.DeviceID != "pi" || closed.UnpaidAtClose.Checks[0].OrderID != a.Order.ID {
		t.Fatalf("auto close, waste default: %+v %v", closed, err)
	}
	if v := eventsOfType(t, r, terminal.EventOrderVoid); len(v) != 1 || v[0]["staff_id"] != nil {
		t.Fatalf("auto write-off: %v", v)
	}
	b, _ := counterOrder(t, r, "key-auto-b", 1, 700)
	closed, _, err = r.CloseBatch(order.WithCloseUnpaid(auto, "carry"), true)
	if err != nil || closed.UnpaidAtClose != nil || closed.Unsettled.Count != 1 || closed.Unsettled.Checks[0].OrderID != b.Order.ID {
		t.Fatalf("auto close, carry: %+v %v", closed, err)
	}
}

// force_unpaid passes the unpaid counter checks only: the leftovers still
// answer 409 until force.
func TestCloseUnpaid_ForceUnpaidOnlyPassesTheChecks(t *testing.T) {
	r := newFloorRepo(t)
	ctx := order.WithForceUnpaid(context.Background(), true)
	a, _ := counterOrder(t, r, "key-fu-a", 1, 1299)
	web := accept(t, r, arriveWeb(t, r, "web-fu", "pickup", true)) // an online order still cooking
	var lo *order.LeftoversError
	if _, _, err := r.CloseBatch(ctx, false); !errors.As(err, &lo) {
		t.Fatalf("force_unpaid alone over a leftover: %v", err)
	}
	// Nothing written off by the refused close (one transaction).
	if got, _ := r.GetByID(ctx, uuid.MustParse(a.Order.ID)); got.Status.State == order.StateCancelled {
		t.Fatalf("written off by a refused close: %+v", got.Status)
	}
	closed, _, err := r.CloseBatch(ctx, true)
	if err != nil || closed.UnpaidAtClose == nil || closed.UnpaidAtClose.Count != 1 {
		t.Fatalf("forced: %+v %v", closed, err)
	}
	_ = web
}
