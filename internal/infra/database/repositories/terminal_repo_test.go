package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Terminal stage 0d: counter orders (the counter sequence, client_key),
// payments, voids, the journal, snapshots, restore.

func counterOrder(t *testing.T, r *OrderRepository, key string, start, total int64, pays ...terminal.Payment) (order.StoredOrder, bool) {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	snap := order.Snapshot{ID: id, TenantID: "t", LocationID: "l", Source: order.SourceCounter, SourceEventID: key,
		Status: "accepted", Fulfillment: order.Fulfillment{Type: "dine_in"}, Money: order.Money{Subtotal: total, Total: total},
		Items: []order.LineItem{{ID: uuid.NewString(), Name: "Cheese Pizza", Quantity: 1, UnitPrice: total, LineTotal: total}}}
	for _, p := range pays {
		snap.Payments = append(snap.Payments, order.PaymentSummary{Method: terminal.MethodOf(p.Tender), AmountCents: p.AmountCents})
	}
	so, created, err := r.PlaceCounterOrder(context.Background(), terminal.NewCounterOrder{Snapshot: snap, ClientKey: key,
		CounterStart: start, PrepMinutes: 15, Payments: pays, StaffID: "s1", DeviceID: "d1", Now: time.Now(),
		Journal: []terminal.Entry{{StaffID: "s1", DeviceID: "d1", Action: terminal.ActionOrderPlaced, OrderID: id}}})
	if err != nil {
		t.Fatal(err)
	}
	return so, created
}

func pay(tender string, cents int64) terminal.Payment {
	return terminal.Payment{ID: uuid.NewString(), At: time.Now(), Tender: tender, AmountCents: cents, StaffID: "s1", DeviceID: "d1"}
}

func TestCounterSequenceAndIdempotency(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	a, created := counterOrder(t, r, "key-aaaa-1", 500, 1299)
	if !created || a.Status.State != order.StateAccepted || a.Status.KitchenNumber == nil || *a.Status.KitchenNumber != 500 {
		t.Fatalf("first counter order: created=%v %+v", created, a.Status)
	}
	if a.Order.KitchenNumber == nil || *a.Order.KitchenNumber != 500 || a.Status.Paid || a.Status.CashDueCents != 1299 {
		t.Fatalf("snapshot number / money: %+v", a.Status)
	}
	b, _ := counterOrder(t, r, "key-bbbb-2", 500, 800, pay(terminal.TenderCash, 800))
	if *b.Status.KitchenNumber != 501 || !b.Status.Paid || b.Status.CashDueCents != 0 || b.Status.PaidAt == nil {
		t.Fatalf("second: %+v", b.Status)
	}
	// The same key: the same order back, nothing written, no number taken.
	again, created := counterOrder(t, r, "key-aaaa-1", 500, 999)
	if created || again.Order.ID != a.Order.ID {
		t.Fatalf("idempotent key: created=%v id %s want %s", created, again.Order.ID, a.Order.ID)
	}
	if n, _ := r.CounterNext(ctx); n != 502 {
		t.Fatalf("counter next %d", n)
	}
	// One sequence (contract 2.10ae): a website order takes the next number.
	web := accept(t, r, arriveWeb(t, r, "web", "pickup", true))
	if *web.Status.KitchenNumber != 502 {
		t.Fatalf("kitchen number %d", *web.Status.KitchenNumber)
	}
	placed := eventsOfType(t, r, terminal.EventOrderPlaced)
	if len(placed) != 2 || placed[0]["order_id"] != a.Order.ID || placed[0]["snapshot"] == nil || placed[0]["agent_status"] == nil {
		t.Fatalf("order.placed events: %v", placed)
	}
	if snap := placed[1]["snapshot"].(map[string]any); snap["source"] != "counter" || snap["kitchen_order_number"] != float64(501) ||
		snap["fulfillment"].(map[string]any)["type"] != "counter" || snap["source_event_id"] != "key-bbbb-2" || snap["staff_id"] != "s1" ||
		snap["payments"].([]any)[0].(map[string]any)["method"] != "cash" {
		t.Fatalf("placed snapshot: %v", snap)
	}
	// Two placings + the drawer the paid one's terminal opened by itself (2026-11-12).
	if j := eventsOfType(t, r, terminal.EventJournalEntries); len(j) != 3 {
		t.Fatalf("journal.entries events: %d", len(j))
	}
	// No order.status event for a counter order's placing.
	if st := eventsOfType(t, r, order.FloorEventOrderStatus); len(st) != 1 { // the web order's accept only
		t.Fatalf("order.status events: %v", st)
	}

	// A manual close without force is refused over the unpaid counter check
	// (contract 2.10s §4); it lists the check (force follows the store's
	// setting: unpaid_at_close_test.go).
	var un *order.UnsettledError
	if _, _, err := r.CloseBatch(ctx, false); !errors.As(err, &un) || len(un.Checks) != 1 || un.Checks[0].OrderID != a.Order.ID ||
		un.Checks[0].DueCents != 1299 || !un.Checks[0].BlocksClose || un.Checks[0].Type != "counter" || un.Checks[0].OnForce != order.UnpaidWaste {
		t.Fatalf("manual close over an unpaid counter check: %v", err)
	}
	// The auto-close with the carry setting carries it: still open, still on the list, payable.
	closed, _, err := r.CloseBatch(order.WithCloseUnpaid(order.WithCloseKind(ctx, order.CloseAuto), order.UnpaidCarry), true)
	if err != nil || closed.Unsettled == nil || closed.Unsettled.Count != 1 || closed.Unsettled.DueCents != 1299 {
		t.Fatalf("auto close: %+v %v", closed, err)
	}
	// Every Close batch restarts the sequence at the store's start (contract
	// 2.10ae, 2026-11-14; numbering_test.go has the rest).
	c, _ := counterOrder(t, r, "key-cccc-3", 7, 100)
	if *c.Status.KitchenNumber != 7 {
		t.Fatalf("after close: %d", *c.Status.KitchenNumber)
	}
	list, err := r.CounterOrders(ctx)
	if err != nil || len(list) != 2 || list[0].Order.ID != a.Order.ID || !list[0].Status.Carried || list[1].Order.ID != c.Order.ID {
		t.Fatalf("open batch counter orders + the carried one: %v %v", list, err)
	}
	// A payment on the carried check lands in the new batch and settles it.
	_, p, _, err := r.AddCounterPayment(ctx, a.Order.ID, "pay-carried-1", "", func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		p := pay(terminal.TenderCash, 1299)
		p.OrderID = a.Order.ID
		return []terminal.Payment{p}, nil, nil
	})
	if err != nil || p.BatchID != c.Status.BatchID {
		t.Fatalf("carried payment: %+v %v", p, err)
	}
	if got, _ := r.GetByID(ctx, uuid.MustParse(a.Order.ID)); got.Status.Carried || !got.Status.Paid {
		t.Fatalf("settled: %+v", got.Status)
	}
	in, err := r.ZInputs(ctx, c.Status.BatchID)
	if err != nil || len(in.Earlier) != 1 || in.Earlier[0].Order.ID != a.Order.ID {
		t.Fatalf("z inputs of the new batch: earlier %v %v", in.Earlier, err)
	}
	if got, err := r.CounterOrderByKey(ctx, "key-bbbb-2"); err != nil || got == nil || got.Order.ID != b.Order.ID {
		t.Fatalf("by key: %v %v", got, err)
	}
}

func TestCounterPaymentsAndVoid(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so, _ := counterOrder(t, r, "key-pay-0001", 1, 2000, pay(terminal.TenderCard, 500))
	if so.Status.CashDueCents != 1500 || so.Status.Paid {
		t.Fatalf("partly paid: %+v", so.Status)
	}
	check := func(p terminal.Payment) terminal.MoneyCheck {
		return func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
			if len(paid) != 1 {
				t.Fatalf("paid so far: %v", paid)
			}
			return []terminal.Payment{p}, []terminal.Entry{{StaffID: "s1", Action: terminal.ActionPayment, OrderID: cur.Order.ID}}, nil
		}
	}
	cash := pay(terminal.TenderCash, 1500)
	cash.OrderID, cash.ClientKey, cash.TipCents = so.Order.ID, "pay-key-0001", 200
	got, p, dup, err := r.AddCounterPayment(ctx, so.Order.ID, cash.ClientKey, "", check(cash))
	if err != nil || dup || p.ID != cash.ID {
		t.Fatalf("payment: %v dup=%v", err, dup)
	}
	// The tip stays on the payment (main's rule), not in the order's money.
	if !got.Status.Paid || got.Status.CashDueCents != 0 || got.Status.TipCents != 200 || got.Order.Money.Total != 2000 {
		t.Fatalf("paid in full + tip: %+v money %+v", got.Status, got.Order.Money)
	}
	// The same payment key again: the stored payment, nothing new.
	_, p2, dup, err := r.AddCounterPayment(ctx, so.Order.ID, cash.ClientKey, "", check(pay(terminal.TenderCash, 1)))
	if err != nil || !dup || p2.ID != cash.ID {
		t.Fatalf("duplicate payment: %v dup=%v %s", err, dup, p2.ID)
	}
	if ev := eventsOfType(t, r, terminal.EventOrderPayment); len(ev) != 1 || ev[0]["paid"] != true ||
		ev[0]["payment"].(map[string]any)["method"] != "cash" || ev[0]["payment"].(map[string]any)["amount_cents"] != float64(1500) {
		t.Fatalf("order.payment events: %v", ev)
	}

	// Void: refunds whatever check says, cancels, order.void.
	refund := pay(terminal.TenderCash, 1500)
	refund.Refund, refund.TipCents, refund.OrderID = true, 200, so.Order.ID
	voided, prev, refunds, _, err := r.VoidCounterOrder(ctx, so.Order.ID, "customer left", "", "s1", "d1",
		func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
			if len(paid) != 2 {
				t.Fatalf("paid: %v", paid)
			}
			return []terminal.Payment{refund}, []terminal.Entry{{Action: terminal.ActionVoid, OrderID: cur.Order.ID}}, nil
		})
	if err != nil || prev.Status.State != order.StateAccepted || len(refunds) != 1 {
		t.Fatalf("void: %v %+v", err, prev.Status)
	}
	if voided.Status.State != order.StateCancelled || voided.Status.CancelReason != terminal.VoidReason {
		t.Fatalf("voided: %+v", voided.Status)
	}
	ev := eventsOfType(t, r, terminal.EventOrderVoid)
	if len(ev) != 1 || ev[0]["reason"] != "customer left" || ev[0]["staff_id"] != "s1" {
		t.Fatalf("order.void: %v", ev)
	}
	// The refund went to main first as a negative order.payment (tip folded in).
	pe := eventsOfType(t, r, terminal.EventOrderPayment)
	if len(pe) != 2 || pe[1]["payment"].(map[string]any)["amount_cents"] != float64(-1700) {
		t.Fatalf("refund payment event: %v", pe)
	}
	if st := eventsOfType(t, r, order.FloorEventOrderStatus); len(st) != 0 {
		t.Fatalf("a counter void writes no order.status: %v", st)
	}
	// Again: already voided.
	if _, _, _, _, err := r.VoidCounterOrder(ctx, so.Order.ID, "x", "", "s1", "d1", nil); !errors.Is(err, terminal.ErrAlreadyVoided) {
		t.Fatalf("second void: %v", err)
	}
	// No payment on a voided order.
	if _, _, _, err := r.AddCounterPayment(ctx, so.Order.ID, "", "", check(pay(terminal.TenderCash, 1))); !errors.Is(err, terminal.ErrNotPayable) {
		t.Fatalf("pay a voided order: %v", err)
	}
	pays, _ := r.OrderPayments(ctx, so.Order.ID)
	if len(pays) != 3 || !pays[2].Refund {
		t.Fatalf("payments: %+v", pays)
	}
	// Every order from main takes money at a terminal now (§2.10aa: online
	// deliveries too, their balance after an edit); the service decides
	// what (application/terminal pickups_test).
}

func TestJournalAndSnapshots(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	if err := r.AddJournal(ctx, []terminal.Entry{{StaffID: "s1", DeviceID: "d1", Action: "no_sale"},
		{StaffID: "s1", DeviceID: "d1", Action: "drawer.open", Detail: json.RawMessage(`{"why":"change"}`)}}); err != nil {
		t.Fatal(err)
	}
	list, err := r.Journal(ctx, 10)
	if err != nil || len(list) != 2 || string(list[0].Detail) != `{"why":"change"}` && string(list[1].Detail) != `{"why":"change"}` {
		t.Fatalf("journal: %+v %v", list, err)
	}
	ev := eventsOfType(t, r, terminal.EventJournalEntries)
	if len(ev) != 1 || len(ev[0]["entries"].([]any)) != 2 {
		t.Fatalf("one journal.entries event for the batch: %v", ev)
	}

	if s, err := r.GetTerminalSnapshot(ctx, terminal.KindCounterMenu); err != nil || s != nil {
		t.Fatalf("no snapshot yet: %v %v", s, err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, v := range []int64{3, 4} {
		if err := r.PutTerminalSnapshot(ctx, terminal.Snapshot{Kind: terminal.KindCounterMenu, Version: v, ETag: "e", Body: []byte(`{}`), FetchedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	s, err := r.GetTerminalSnapshot(ctx, terminal.KindCounterMenu)
	if err != nil || s.Version != 4 || !s.FetchedAt.Equal(now) {
		t.Fatalf("snapshot: %+v %v", s, err)
	}
}

// The sequence rides in agent.state and survives a restore on a new Pi
// without ever repeating a number (one sequence since batch E: the restore's
// kitchen_next is the number the next order gets).
func TestCounterNextInStateAndRestore(t *testing.T) {
	a := newFloorRepo(t)
	ctx := context.Background()
	c1, _ := counterOrder(t, a, "key-rest-0001", 4000, 500)
	counterOrder(t, a, "key-rest-0002", 4000, 600)
	st, err := a.AgentState(ctx)
	if err != nil || st.CounterNext != 4002 {
		t.Fatalf("state counter_next %d %v", st.CounterNext, err)
	}
	b := newFloorRepo(t)
	st.CounterNext = 0 // an older report without it: the orders still count
	res, err := b.Restore(ctx, order.RestoreData{State: &st, Orders: []order.RestoreOrder{{Order: c1.Order, AgentStatus: &c1.Status}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.CounterNext != 4001 || res.KitchenNext != 4001 {
		t.Fatalf("restore: %+v", res)
	}
	got, err := b.CounterOrderByKey(ctx, "key-rest-0001")
	if err != nil || got == nil || got.Order.ID != c1.Order.ID {
		t.Fatalf("restored client_key: %v %v", got, err)
	}
}
