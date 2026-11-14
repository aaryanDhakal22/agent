package repositories

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// The owner, 2026-11-14 (after 5 unpaid phone pickups were carried past a
// close): "when a pickup order isn't picked up and the close batch command
// is sent, they're supposed to be voided as waste and printed on a not
// picked up slip". Every close (manual or auto) writes an unpaid counter /
// phone pickup with no payment row off as Void waste "Not picked up" (the
// open batch's and the carried ones); a pickup with a payment row is still
// carried; the store's close_unpaid_counter_action carry keeps them all.

func counterPickup(t *testing.T, r *OrderRepository, key string, total int64, pays ...terminal.Payment) order.StoredOrder {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	snap := order.Snapshot{ID: id, TenantID: "t", LocationID: "l", Source: order.SourceCounter, SourceEventID: key,
		Status: "accepted", Fulfillment: order.Fulfillment{Type: "pickup"}, Money: order.Money{Subtotal: total, Total: total},
		Customer: order.Customer{DisplayName: "Sam Lee", Phone: "4105550100"},
		Items:    []order.LineItem{{ID: uuid.NewString(), Name: "Cheese Pizza", Quantity: 1, UnitPrice: total, LineTotal: total}}}
	for _, p := range pays {
		snap.Payments = append(snap.Payments, order.PaymentSummary{Method: terminal.MethodOf(p.Tender), AmountCents: p.AmountCents})
	}
	so, _, err := r.PlaceCounterOrder(context.Background(), terminal.NewCounterOrder{Snapshot: snap, ClientKey: key,
		CounterStart: 100, PrepMinutes: 15, Payments: pays, StaffID: "s1", DeviceID: "d1", Now: time.Now(),
		Journal: []terminal.Entry{{StaffID: "s1", DeviceID: "d1", Action: terminal.ActionOrderPlaced, OrderID: id}}})
	if err != nil {
		t.Fatal(err)
	}
	return so
}

func TestNotPickedUp_ManualCloseWastesUnpaidPickups(t *testing.T) {
	r := newFloorRepo(t)
	staff := "11111111-1111-4111-8111-111111111111"
	ctx := order.WithCloseActor(order.WithCloseTrigger(context.Background(), "terminal"),
		order.CloseActor{StaffID: staff, StaffName: "Mia M.", DeviceID: "term-1", Trigger: "terminal"})
	// Day 1 under the carry setting: the unpaid pickup is carried (today's
	// behaviour kept by the setting).
	old := counterPickup(t, r, "key-np-old", 1483)
	closed, _, err := r.CloseBatch(order.WithCloseUnpaid(ctx, order.UnpaidCarry), true)
	if err != nil || closed.NotPickedUp != nil || closed.Unsettled == nil || closed.Unsettled.Count != 1 || !closed.Unsettled.Checks[0].Carried {
		t.Fatalf("carry setting: %+v %v", closed, err)
	}
	if got, _ := r.GetByID(ctx, uuid.MustParse(old.Order.ID)); !got.Status.Carried || got.Status.State == order.StateCancelled {
		t.Fatalf("carried under carry: %+v", got.Status)
	}
	// Day 2: a fresh unpaid pickup, a partly paid one, a paid one, and the
	// carried pickup from day 1 (force: the partly paid pickup and the web
	// order are unfinished leftovers; TestNotPickedUp_NeverBlocks has the
	// plain close).
	fresh := counterPickup(t, r, "key-np-fresh", 4289)
	part := counterPickup(t, r, "key-np-part", 2000, pay(terminal.TenderCash, 500))
	paid := counterPickup(t, r, "key-np-paid", 800, pay(terminal.TenderCash, 800))
	web := accept(t, r, arriveWeb(t, r, "web", "pickup", false)) // a website cash pickup: out of scope
	_ = web
	closed, _, err = r.CloseBatch(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if closed.UnpaidAtClose != nil {
		t.Fatalf("pickups on the Unpaid at close list: %+v", closed.UnpaidAtClose)
	}
	np := closed.NotPickedUp
	if np == nil || np.Count != 2 || np.TotalCents != 1483+4289 || np.Reason != "Not picked up" || np.PrintedBy != "pi" ||
		np.ClosedBy.StaffName != "Mia M." || np.Auto {
		t.Fatalf("not picked up: %+v", np)
	}
	ids := map[string]order.UnsettledCheck{}
	for _, c := range np.Checks {
		ids[c.OrderID] = c
	}
	if c, ok := ids[old.Order.ID]; !ok || !c.Carried || c.Type != "pickup" || c.Customer.Name != "Sam Lee" || c.Customer.Phone != "4105550100" ||
		len(c.Items) != 1 {
		t.Fatalf("carried pickup on the slip: %+v", c)
	}
	if _, ok := ids[fresh.Order.ID]; !ok {
		t.Fatalf("open batch pickup missing: %+v", np.Checks)
	}
	for _, id := range []string{old.Order.ID, fresh.Order.ID} {
		got, _ := r.GetByID(ctx, uuid.MustParse(id))
		if got.Status.State != order.StateCancelled || got.Status.CancelReason != terminal.WasteReason || got.Status.Carried {
			t.Fatalf("written off: %+v", got.Status)
		}
	}
	// The partly paid pickup is carried, still payable; the paid one untouched.
	if got, _ := r.GetByID(ctx, uuid.MustParse(part.Order.ID)); !got.Status.Carried || got.Status.State == order.StateCancelled {
		t.Fatalf("partly paid carried: %+v", got.Status)
	}
	if got, _ := r.GetByID(ctx, uuid.MustParse(paid.Order.ID)); got.Status.State == order.StateCancelled {
		t.Fatalf("paid pickup: %+v", got.Status)
	}
	if closed.Unsettled == nil {
		t.Fatal("no unsettled list")
	}
	for _, c := range closed.Unsettled.Checks {
		if c.OrderID == old.Order.ID || c.OrderID == fresh.Order.ID {
			t.Fatalf("a written-off pickup is still on the unsettled list: %+v", c)
		}
	}
	// The Pickup list no longer has them.
	carried, _ := r.CarriedOrders(ctx)
	for _, so := range carried {
		if so.Order.ID == old.Order.ID || so.Order.ID == fresh.Order.ID {
			t.Fatalf("still carried: %s", so.Order.ID)
		}
	}
	// main hears order.void {kind: waste, waste_reason: "Not picked up"} in the closer's name.
	voids := map[string]map[string]any{}
	for _, v := range eventsOfType(t, r, terminal.EventOrderVoid) {
		voids[v["order_id"].(string)] = v
	}
	if len(voids) != 2 {
		t.Fatalf("order.void events: %v", voids)
	}
	for _, id := range []string{old.Order.ID, fresh.Order.ID} {
		v := voids[id]
		if v["kind"] != "waste" || v["waste_reason"] != "Not picked up" || v["reason"] != "Not picked up" || v["staff_id"] != staff {
			t.Fatalf("order.void: %v", v)
		}
	}
	// No money moved for a write-off.
	for _, p := range eventsOfType(t, r, terminal.EventOrderPayment) {
		if p["order_id"] == old.Order.ID || p["order_id"] == fresh.Order.ID {
			t.Fatalf("a write-off moved money: %v", p)
		}
	}
	// Journal void.waste {at_close, closed_by} in the closer's name.
	j, _ := r.Journal(ctx, 200)
	n := 0
	for _, e := range j {
		if e.Action != terminal.ActionVoidWaste {
			continue
		}
		n++
		var d map[string]any
		_ = json.Unmarshal(e.Detail, &d)
		by, _ := d["closed_by"].(map[string]any)
		if e.StaffID != staff || e.DeviceID != "term-1" || d["reason"] != "Not picked up" || d["at_close"] != true ||
			by["staff_name"] != "Mia M." || by["trigger"] != "terminal" {
			t.Fatalf("journal: %+v %v", e, d)
		}
	}
	if n != 2 {
		t.Fatalf("void.waste rows: %d", n)
	}
	// The Z's Waste section (both on the batch they were written off in).
	in, err := r.ZInputs(ctx, closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	z := terminal.BuildZ(in)
	if z.Waste.Count != 2 || z.Waste.Cents != 1483+4289 || len(z.Waste.Reasons) != 1 || z.Waste.Reasons[0].Reason != "Not picked up" {
		t.Fatalf("Z waste: %+v", z.Waste)
	}
	if z.Reconcile.DifferenceCents != 0 {
		t.Fatalf("Z balance: %+v", z.Reconcile)
	}
}

// Counter checks keep their own rule: a manual close without force is
// still refused over an unpaid counter check (pickups are not on the 409),
// and forced, the counter check goes to Unpaid at close, the pickup to Not
// picked up.
func TestNotPickedUp_CounterChecksKeepTheirRule(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	check, _ := counterOrder(t, r, "key-npc-check", 1, 1299)
	pick := counterPickup(t, r, "key-npc-pick", 2299)
	var un *order.UnsettledError
	_, _, err := r.CloseBatch(ctx, false)
	if !asUnsettled(err, &un) || len(un.Checks) != 1 || un.Checks[0].OrderID != check.Order.ID || un.Force.WasteCount != 1 {
		t.Fatalf("refused over the counter check only: %v %+v", err, un)
	}
	if got, _ := r.GetByID(ctx, uuid.MustParse(pick.Order.ID)); got.Status.State == order.StateCancelled {
		t.Fatal("a refused close wrote the pickup off")
	}
	closed, _, err := r.CloseBatch(order.WithForceUnpaid(ctx, true), false)
	if err != nil {
		t.Fatal(err)
	}
	if closed.UnpaidAtClose == nil || closed.UnpaidAtClose.Count != 1 || closed.UnpaidAtClose.Checks[0].OrderID != check.Order.ID ||
		closed.UnpaidAtClose.Reason != "Unpaid at close" {
		t.Fatalf("unpaid at close: %+v", closed.UnpaidAtClose)
	}
	if closed.NotPickedUp == nil || closed.NotPickedUp.Count != 1 || closed.NotPickedUp.Checks[0].OrderID != pick.Order.ID {
		t.Fatalf("not picked up: %+v", closed.NotPickedUp)
	}
	in, _ := r.ZInputs(ctx, closed.ID)
	z := terminal.BuildZ(in)
	reasons := map[string]int64{}
	for _, wr := range z.Waste.Reasons {
		reasons[wr.Reason] = wr.Cents
	}
	if reasons["Unpaid at close"] != 1299 || reasons["Not picked up"] != 2299 || z.Reconcile.DifferenceCents != 0 {
		t.Fatalf("Z waste reasons: %v %+v", reasons, z.Reconcile)
	}
}

// A pickup alone never blocks a manual close (no force needed).
func TestNotPickedUp_NeverBlocks(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	pick := counterPickup(t, r, "key-npb-pick", 999)
	l, err := r.UnsettledChecks(ctx)
	if err != nil || l.Blocking != 0 || l.Count != 1 {
		t.Fatalf("list: %+v %v", l, err)
	}
	closed, _, err := r.CloseBatch(ctx, false)
	if err != nil || closed.NotPickedUp == nil || closed.NotPickedUp.Checks[0].OrderID != pick.Order.ID {
		t.Fatalf("plain close: %+v %v", closed, err)
	}
}

// The auto-close follows the setting too: waste (default) writes the
// pickup off in the Pi's name, carry carries it.
func TestNotPickedUp_AutoClose(t *testing.T) {
	r := newFloorRepo(t)
	auto := order.WithCloseKind(context.Background(), order.CloseAuto)
	a := counterPickup(t, r, "key-npa-a", 1299)
	closed, _, err := r.CloseBatch(auto, true)
	if err != nil || closed.NotPickedUp == nil || !closed.NotPickedUp.Auto || closed.NotPickedUp.ClosedBy.Trigger != "auto" ||
		closed.NotPickedUp.ClosedBy.DeviceID != "pi" || closed.NotPickedUp.Checks[0].OrderID != a.Order.ID || closed.NotPickedUp.PrintedBy != "pi" {
		t.Fatalf("auto close, waste default: %+v %v", closed, err)
	}
	if v := eventsOfType(t, r, terminal.EventOrderVoid); len(v) != 1 || v[0]["staff_id"] != nil || v[0]["waste_reason"] != "Not picked up" {
		t.Fatalf("auto write-off: %v", v)
	}
	b := counterPickup(t, r, "key-npa-b", 700)
	closed, _, err = r.CloseBatch(order.WithCloseUnpaid(auto, order.UnpaidCarry), true)
	if err != nil || closed.NotPickedUp != nil || closed.Unsettled.Count != 1 || closed.Unsettled.Checks[0].OrderID != b.Order.ID {
		t.Fatalf("auto close, carry: %+v %v", closed, err)
	}
}

// A terminal that asks print_not_picked_up_here prints the slip itself.
func TestNotPickedUp_PrintedByTheTerminal(t *testing.T) {
	r := newFloorRepo(t)
	ctx := order.WithCloseActor(context.Background(), order.CloseActor{StaffName: "Mia M.", DeviceID: "term-1", Trigger: "terminal",
		PrintsHere: true, PrintsNotPickedUpHere: true})
	counterPickup(t, r, "key-npt-a", 1299)
	closed, _, err := r.CloseBatch(ctx, true)
	if err != nil || closed.NotPickedUp == nil || closed.NotPickedUp.PrintedBy != "terminal" {
		t.Fatalf("printed by: %+v %v", closed, err)
	}
	// An older terminal (print_unpaid_here only) gets the Pi's print.
	ctx = order.WithCloseActor(context.Background(), order.CloseActor{DeviceID: "term-1", Trigger: "terminal", PrintsHere: true})
	counterPickup(t, r, "key-npt-b", 1299)
	closed, _, err = r.CloseBatch(ctx, true)
	if err != nil || closed.NotPickedUp == nil || closed.NotPickedUp.PrintedBy != "pi" {
		t.Fatalf("older terminal: %+v %v", closed, err)
	}
}

func asUnsettled(err error, un **order.UnsettledError) bool {
	e, ok := err.(*order.UnsettledError)
	if ok {
		*un = e
	}
	return ok
}
