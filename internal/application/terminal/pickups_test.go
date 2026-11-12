package terminal

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// The Pi routes the terminal's checkout assumed (contract §2.10i): the Pickup
// list of every source, picked up, payment on online cash pickups, comp needs
// a manager, the Employee meal, shared drafts (hold + TAKE OVER).

// online stores an accepted online order (cash: no payments = the whole
// total is due; card: paid).
func online(t *testing.T, r *rig, fulfillment string, card bool) order.StoredOrder {
	t.Helper()
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: uuid.NewString()[:8], PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: fulfillment},
		Customer: order.Customer{DisplayName: "Sam"}, Money: order.Money{Subtotal: 2000, Total: 2000}}
	if card {
		s.Payments = []order.PaymentSummary{{Method: "card", AmountCents: 2000}}
	}
	if _, err := r.repo.UpsertArrived(r.ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := r.repo.ClaimAccept(r.ctx, uuid.MustParse(s.ID), 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.repo.RecordAccepted(r.ctx, *got); err != nil {
		t.Fatal(err)
	}
	return *got
}

func pickupIDs(t *testing.T, r *rig) map[string]order.StoredOrder {
	t.Helper()
	l, err := r.svc.Pickups(r.ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]order.StoredOrder{}
	for _, so := range l.Orders {
		out[so.Order.ID] = so
	}
	return out
}

func TestOnlineCashPickupPaidAtTheTerminalThenPickedUp(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	cash := online(t, r, "pickup", false)
	card := online(t, r, "pickup", true)
	deliv := online(t, r, "delivery", false)
	counter, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-pick-0001", Type: terminal.TypePickup,
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: soda}}}}}})
	if err != nil {
		t.Fatal(err)
	}

	// Every source that still owes money; no delivery, and the card pickup (paid online) isn't on it (the
	// owner, 2026-11-12: fully paid = off the list).
	list := pickupIDs(t, r)
	if _, ok := list[deliv.Order.ID]; ok || len(list) != 2 {
		t.Fatalf("every pickup source that owes, no delivery: %d", len(list))
	}
	if _, ok := list[counter.Order.Order.ID]; !ok {
		t.Fatal("the unpaid counter pickup is on it")
	}
	if so := list[cash.Order.ID]; so.Status.CashDueCents != 2000 || so.Status.Paid {
		t.Fatalf("cash pickup due: %+v", so.Status)
	}
	if _, ok := list[card.Order.ID]; ok {
		t.Fatal("a paid card pickup is not on the list")
	}
	// Not handed over while money is due.
	var due *terminal.CashDue
	if _, err := r.svc.PickedUp(r.ctx, till, cash.Order.ID); !errors.As(err, &due) || due.BalanceCents != 2000 {
		t.Fatalf("cash due: %v", err)
	}
	// The terminal reads it and takes the cash (same rules as a counter order).
	if v, err := r.svc.Order(r.ctx, cash.Order.ID); err != nil || len(v.Payments) != 0 {
		t.Fatalf("order view: %v", err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, cash.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderCash, AmountCents: 2001}); !errors.Is(err, terminal.ErrOverBalance) {
		t.Fatalf("over the balance: %v", err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, card.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderCash, AmountCents: 1}); !errors.Is(err, terminal.ErrOverBalance) {
		t.Fatalf("a card order owes nothing: %v", err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, deliv.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderCash, AmountCents: 1}); !errors.Is(err, terminal.ErrNotCounterOrder) {
		t.Fatalf("a delivery is the driver's: %v", err)
	}
	pay, err := r.svc.AddPayment(r.ctx, till, cash.Order.ID, PaymentIn{Tender: terminal.TenderCash, AmountCents: 2000, TenderedCents: 2500, ClientKey: "pay-online-01"})
	if err != nil || pay.BalanceCents != 0 || !pay.Order.Status.Paid || pay.Payment.ChangeCents != 500 {
		t.Fatalf("paid: %+v %v", pay, err)
	}
	if pay.Order.Order.Money.Total != 2000 || len(pay.Order.Order.Payments) != 0 {
		t.Fatalf("main's snapshot is never rewritten: %+v", pay.Order.Order)
	}
	if r.pub.count(EventOrderPaid) != 1 {
		t.Fatal("terminal.order.paid")
	}
	// Fully paid: off the Pickup list at once (no Picked Up needed).
	if _, ok := pickupIDs(t, r)[cash.Order.ID]; ok {
		t.Fatal("paid = off the list")
	}
	if len(journalOf(t, r, terminal.ActionPayment)) != 1 {
		t.Fatal("payment journaled")
	}
	// Handed over: finished, off the list, announced; again = already.
	got, err := r.svc.PickedUp(r.ctx, till, cash.Order.ID)
	if err != nil || got.Already || got.Order.Status.FinishedAt == nil || got.Order.Status.FinishedBy != terminal.FinishedByTerminal ||
		got.Order.Status.State != order.StateReady {
		t.Fatalf("picked up: %+v %v", got, err)
	}
	if again, err := r.svc.PickedUp(r.ctx, till, cash.Order.ID); err != nil || !again.Already {
		t.Fatalf("again: %+v %v", again, err)
	}
	if _, ok := pickupIDs(t, r)[cash.Order.ID]; ok {
		t.Fatal("picked up = off the list")
	}
	if r.pub.count(terminal.EventOrderPickedUp) != 1 || len(journalOf(t, r, terminal.ActionPickedUp)) != 1 {
		t.Fatal("event + journal once")
	}
	// The counter pickup (unpaid) and a delivery can't be.
	if _, err := r.svc.PickedUp(r.ctx, till, counter.Order.Order.ID); !errors.As(err, &due) {
		t.Fatalf("unpaid counter pickup: %v", err)
	}
	if _, err := r.svc.PickedUp(r.ctx, till, deliv.Order.ID); !errors.Is(err, terminal.ErrNotPickable) {
		t.Fatalf("delivery: %v", err)
	}
	// A paid card pickup still cooking: picked up = ready + finished at once.
	got, err = r.svc.PickedUp(r.ctx, till, card.Order.ID)
	if err != nil || got.Order.Status.State != order.StateReady || got.Order.Status.ReadyAt == nil {
		t.Fatalf("card pickup: %+v %v", got, err)
	}
	// main hears order.status ready with finished_at in agent_status.
	evs, err := r.repo.ListUnsyncedEvents(r.ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, e := range evs {
		var p struct {
			Type        string `json:"type"`
			OrderID     string `json:"order_id"`
			Status      string `json:"status"`
			AgentStatus struct {
				FinishedAt *time.Time `json:"finished_at"`
			} `json:"agent_status"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Type == order.FloorEventOrderStatus && p.Status == "ready" && p.AgentStatus.FinishedAt != nil {
			finished++
		}
	}
	if finished != 2 {
		t.Fatalf("order.status ready + finished_at: %d", finished)
	}
}

func TestCompByACashierNeedsNoManager(t *testing.T) {
	// The owner, 2026-11-12: comp was manager-only; a cashier comps alone now.
	r := newRig(t)
	r.login(t, till, "1234") // cashier
	soda1 := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{soda1}}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-comp-0001", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	pay, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: terminal.TenderComp, AmountCents: total})
	if err != nil || pay.Payment.ApprovedBy != "" || pay.BalanceCents != 0 {
		t.Fatalf("comp tender by a cashier: %+v %v", pay, err)
	}
	if o := journalOf(t, r, terminal.ActionOverride); len(o) != 0 {
		t.Fatalf("no override: %v", o)
	}
	// A comp tender with the order, and a comp line ($0), the same.
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-comp-0002", Cart: cart,
		Payments: []PaymentIn{{Tender: terminal.TenderComp, AmountCents: total}}}); err != nil {
		t.Fatalf("comp tender at placing: %v", err)
	}
	compCart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{atPrice(soda1, 0)}}}
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: compCart})
	if err != nil || q.NeedsManager || !q.PriceChanges[0].Comp {
		t.Fatalf("quote: %+v %v", q, err)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-comp-0003", Cart: compCart}); err != nil {
		t.Fatalf("comp line: %v", err)
	}
	if c := journalOf(t, r, terminal.ActionComp); len(c) != 1 || c[0]["_staff"] != "cash-1" || c[0]["approved_by"] != nil {
		t.Fatalf("comp journaled: %v", c)
	}
	// A manager logged in, the same.
	mgrTill := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	r.login(t, mgrTill, "9876")
	if _, err := r.svc.PlaceOrder(r.ctx, mgrTill, OrderIn{ClientKey: "k-comp-0004", Cart: compCart}); err != nil {
		t.Fatalf("manager comp line: %v", err)
	}
}

func TestEmployeeMealUpToItsPercent(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // cashier: 10 % typed discounts
	meal := func(pct int, kind string) Cart {
		return Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}, Discount: &Discount{Pct: pct, Reason: "", Kind: kind}}
	}
	// main's settings say nothing: the Pi's fallback, 20 % (the owner's default).
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-meal-0001", Cart: meal(20, KindEmployeeMeal)})
	if err != nil {
		t.Fatalf("employee meal 20 %% by a cashier: %v", err)
	}
	if d := res.Order.Order.Money.Discounts; len(d) != 1 || d[0].Label != "Employee meal 20%" || d[0].Amount != pricing.Pct(2224, 2000) {
		t.Fatalf("discount: %+v", d)
	}
	if j := journalOf(t, r, terminal.ActionEmployeeMeal); len(j) != 1 || j[0]["pct"] != float64(20) || j[0]["reason"] != "Employee meal" {
		t.Fatalf("journal: %v", j)
	}
	var mgr *terminal.ManagerRequired
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-meal-0002", Cart: meal(30, KindEmployeeMeal)}); !errors.As(err, &mgr) ||
		mgr.Action != terminal.ActionEmployeeMeal || mgr.Limit != 20 || mgr.Requested != 30 {
		t.Fatalf("over employee_meal_pct: %v", err)
	}
	typed := meal(50, "")
	typed.Discount.Reason = "friend"
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-meal-0003", Cart: typed}); !errors.As(err, &mgr) || mgr.Action != terminal.ActionDiscount {
		t.Fatalf("a typed 50 %% follows the cashier's 10 %%: %v", err)
	}
	if q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: meal(20, KindEmployeeMeal)}); err != nil || q.NeedsManager {
		t.Fatalf("quote: %+v %v", q, err)
	}
	if _, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: meal(50, "staff")}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unknown kind: %v", err)
	}
	// main's employee_meal_pct moves the line.
	cfg := staffConfig(t, 2)
	cfg["settings"].(map[string]any)["employee_meal_pct"] = 30
	r.main.set(PathTerminalConfig, cfg)
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	r.login(t, till, "1234")
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-meal-0004", Cart: meal(50, KindEmployeeMeal)}); !errors.As(err, &mgr) || mgr.Limit != 30 {
		t.Fatalf("employee_meal_pct 30: %v", err)
	}
}

func draftIn(held bool) DraftIn {
	return DraftIn{Type: terminal.TypeCounter, Name: "Sam", Held: held, Draft: json.RawMessage(`{"lines":[{"item_id":"x"}]}`)}
}

func TestDraftsHoldRaceTakeOverAndCloseBatch(t *testing.T) {
	r := newRig(t)
	till2 := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	r.login(t, till, "1234")
	r.login(t, till2, "55")
	if _, err := r.svc.PutDraft(r.ctx, till, "bad", draftIn(true)); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad id: %v", err)
	}
	// SUSPEND: held, nobody holds it, under the cashier.
	got, err := r.svc.PutDraft(r.ctx, till, "d-held-0001", draftIn(true))
	if err != nil || !got.Draft.Held || got.Draft.Holder != nil || got.Draft.StaffName != "Cara S." {
		t.Fatalf("held: %+v %v", got, err)
	}
	// Both terminals RESUME it at once: exactly one opens it.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, a := range []Actor{till, till2} {
		wg.Add(1)
		go func(i int, a Actor) {
			defer wg.Done()
			_, errs[i] = r.svc.PutDraft(r.ctx, a, "d-held-0001", draftIn(false))
		}(i, a)
	}
	wg.Wait()
	var oe *terminal.OpenElsewhere
	won := -1
	for i, err := range errs {
		switch {
		case err == nil:
			won = i
		case !errors.As(err, &oe):
			t.Fatalf("race: %v", err)
		}
	}
	if won < 0 || (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("one winner: %v", errs)
	}
	winner, loser := till, till2
	if won == 1 {
		winner, loser = till2, till
	}
	if oe.Holder.DeviceID != winner.DeviceID {
		t.Fatalf("holder told: %+v", oe.Holder)
	}
	// The holder keeps mirroring; the other can't delete it.
	if _, err := r.svc.PutDraft(r.ctx, winner, "d-held-0001", draftIn(false)); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.DeleteDraft(r.ctx, loser, "d-held-0001"); !errors.As(err, &oe) {
		t.Fatalf("delete elsewhere: %v", err)
	}
	// TAKE OVER.
	got, err = r.svc.PutDraft(r.ctx, loser, "d-held-0001", DraftIn{Type: "counter", TakeOver: true, Draft: json.RawMessage(`{}`)})
	if err != nil || got.Draft.Holder == nil || got.Draft.Holder.DeviceID != loser.DeviceID {
		t.Fatalf("take over: %+v %v", got, err)
	}
	if r.pub.count(terminal.EventDraftTaken) != 1 || len(journalOf(t, r, terminal.ActionDraftTakeOver)) != 1 {
		t.Fatal("taken event + journal")
	}
	if _, err := r.svc.PutDraft(r.ctx, winner, "d-held-0001", draftIn(false)); !errors.As(err, &oe) {
		t.Fatalf("the old holder's next mirror: %v", err)
	}
	// Not logged in: no writes; the list needs none.
	r.svc.Logout(r.ctx, winner)
	if _, err := r.svc.PutDraft(r.ctx, winner, "d-other-0001", draftIn(true)); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("login: %v", err)
	}
	r.login(t, winner, "1234")
	if _, err := r.svc.PutDraft(r.ctx, winner, "d-other-0001", draftIn(true)); err != nil {
		t.Fatal(err)
	}
	l, err := r.svc.Drafts(r.ctx)
	if err != nil || len(l.Drafts) != 2 {
		t.Fatalf("list: %+v %v", l, err)
	}
	// Sent: deleted by its holder (a missing one is fine).
	if err := r.svc.DeleteDraft(r.ctx, winner, "d-other-0001"); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.DeleteDraft(r.ctx, winner, "d-other-0001"); err != nil || r.pub.count(terminal.EventDraftRemoved) != 1 {
		t.Fatalf("again: %v", err)
	}
	// Close batch clears every draft.
	closed, _, err := r.repo.CloseBatch(r.ctx, true)
	if err != nil || len(closed.ClearedDrafts) != 1 || closed.ClearedDrafts[0] != "d-held-0001" {
		t.Fatalf("close batch: %+v %v", closed, err)
	}
	if l, _ := r.svc.Drafts(r.ctx); len(l.Drafts) != 0 {
		t.Fatal("cleared")
	}
}

func TestDraftDecide(t *testing.T) {
	a, b := terminal.Holder{DeviceID: "a"}, terminal.Holder{DeviceID: "b"}
	h, tk, err := terminal.DraftPut{By: a}.Decide(nil)
	if err != nil || h == nil || h.DeviceID != "a" || tk != nil {
		t.Fatal("new + open")
	}
	if h, _, _ := (terminal.DraftPut{By: a, Held: true}).Decide(nil); h != nil {
		t.Fatal("held = no holder")
	}
	open := &terminal.Draft{Holder: &a}
	if _, _, err := (terminal.DraftPut{By: b}).Decide(open); err == nil {
		t.Fatal("open elsewhere")
	}
	if _, _, err := (terminal.DraftPut{By: b, Held: true}).Decide(open); err == nil {
		t.Fatal("can't hold it from elsewhere either")
	}
	if h, tk, err := (terminal.DraftPut{By: b, TakeOver: true}).Decide(open); err != nil || h.DeviceID != "b" || tk.DeviceID != "a" {
		t.Fatal("take over")
	}
	if h, tk, err := (terminal.DraftPut{By: a, Held: true}).Decide(open); err != nil || h != nil || tk != nil {
		t.Fatal("the holder puts it aside")
	}
	if terminal.CheckDelete(open, "b") == nil || terminal.CheckDelete(open, "a") != nil || terminal.CheckDelete(&terminal.Draft{}, "b") != nil {
		t.Fatal("delete")
	}
}
