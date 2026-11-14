package terminal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
)

// Batch F (contract/agent.md §2.10am, the owner 2026-11-14 #25 / #26 / #27).

// tipSquare records the tip adjustments main is asked for.
type tipSquare struct {
	mu    sync.Mutex
	calls []fakeTipCall
	err   error
}

type fakeTipCall struct {
	payment string
	tip     int64
}

func (f *tipSquare) Refund(context.Context, string, int64, string, string) (squareterm.RefundResult, error) {
	return squareterm.RefundResult{}, errors.New("not in this test")
}

func (f *tipSquare) TipAdjust(_ context.Context, id string, tip int64, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeTipCall{id, tip})
	if f.err != nil {
		return "", f.err
	}
	return "APPROVED", nil
}

func (f *tipSquare) seen() []fakeTipCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeTipCall(nil), f.calls...)
}

// cardDelivery places a counter delivery paid in full on the card reader
// (Square payment sqID, no tip), ready.
func (r *driverRig) cardDelivery(t *testing.T, key, sqID string) string {
	t.Helper()
	r.login(t, till, "1234")
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Type: "delivery", Cart: cart})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: key, Type: "delivery", Cart: cart,
		Customer: &Customer{Name: "Erik Davis", Phone: "4105550101", Address: &Address{Line1: "12 Main St", City: "Towson"}},
		Payments: []PaymentIn{{Tender: "card", AmountCents: q.Quote.(pricing.Quote).TotalCents, SquarePaymentID: sqID, Last4: "4242", Brand: "visa"}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	if _, err := r.floor.Ready(r.ctx, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// deliverAs logs the driver in on a, sends the orders out with him and has
// him close them (delivered by him). Returns his driver id.
func (r *driverRig) deliverAs(t *testing.T, a Actor, pin string, ids ...string) string {
	t.Helper()
	res, err := r.svc.Login(r.ctx, a, pin)
	if err != nil {
		t.Fatal(err)
	}
	d := res.Driver.ID
	if res.Driver.Status == order.DriverOff {
		if _, err := r.floor.ClockIn(r.ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.floor.SendOut(r.ctx, d, orderApp.SendOutRequest{Include: ids}); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := r.svc.CloseOrder(r.ctx, a, id); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func lineOf(t *testing.T, rep *order.DriverReport, id string) order.DriverReportLine {
	t.Helper()
	for _, l := range rep.Lines {
		if l.OrderID == id {
			return l
		}
	}
	t.Fatalf("no line %s on %+v", id, rep.Lines)
	return order.DriverReportLine{}
}

// #25: a card tip typed for a delivery is on his report at once and comes
// off his handout, but nothing is charged or written as a payment before
// Close batch. Old code: POST …/tip answered not_tippable for a counter card
// delivery and the report never saw the tip.
func TestTypedCardTipOnReportAtOnceAndOffTheHandout(t *testing.T) {
	r := newDriverRig(t)
	cash := r.delivery(t, "k-tipcash-1", false, true)
	card := r.cardDelivery(t, "k-tipcard-1", "sq-pay-1")
	r.deliverAs(t, till2, "77", cash, card)
	before, err := r.svc.Report(r.ctx, till2)
	if err != nil {
		t.Fatal(err)
	}
	if l := lineOf(t, before, card); !l.Tippable || l.TipState != order.TipStateNone || before.TipsToType != 1 {
		t.Fatalf("a card delivery waits for its tip: %+v (to type %d)", l, before.TipsToType)
	}
	if l := lineOf(t, before, cash); l.Tippable {
		t.Fatalf("a cash delivery takes no tip: %+v", l)
	}
	pays, _ := r.repo.OrderPayments(r.ctx, card)
	tip := int64(400)
	res, err := r.svc.Tip(r.ctx, till2, card, TipIn{TipCents: &tip})
	if err != nil || res.TipState != order.TipStateTipped || res.Ticket.TypedTipCents != 400 || res.Ticket.TipKind != "card" {
		t.Fatalf("tip: %+v %v", res, err)
	}
	after, _ := r.svc.Report(r.ctx, till2)
	l := lineOf(t, after, card)
	if l.TipCents != 400 || l.TypedTipCents != 400 || l.TipState != order.TipStateTipped || after.TypedTipCents != 400 || after.TipsToType != 0 {
		t.Fatalf("on the report at once: %+v %+v", l, after)
	}
	// Handout = before − the tip after the 6 % tip tax ($4.00 → $3.76).
	if after.TipCents != before.TipCents+400 || after.OwedCents != before.OwedCents-376 || after.CashCents != before.CashCents {
		t.Fatalf("handout: before %+v after %+v", before, after)
	}
	// Saved, not closed: no payment yet, still editable.
	if now, _ := r.repo.OrderPayments(r.ctx, card); len(now) != len(pays) {
		t.Fatalf("a payment before Close batch: %+v", now)
	}
	tip = 250
	if _, err := r.svc.Tip(r.ctx, till2, card, TipIn{TipCents: &tip}); err != nil {
		t.Fatal(err)
	}
	if again, _ := r.svc.Report(r.ctx, till2); lineOf(t, again, card).TipCents != 250 {
		t.Fatalf("a corrected tip replaces the first: %+v", lineOf(t, again, card))
	}
	if _, err := r.svc.Tip(r.ctx, till2, cash, TipIn{TipCents: &tip}); !errors.Is(err, order.ErrNotTippable) {
		t.Fatalf("cash: %v", err)
	}
	if r.pub.count(EventDriverTip) != 2 {
		t.Fatalf("events: %v", r.pub.events)
	}
}

// #26: the Tips tab's pile = his card deliveries with nothing typed; Add tip
// or No tip takes one off the pile.
func TestTipsTabPileAddTipAndNoTipLeaveIt(t *testing.T) {
	r := newDriverRig(t)
	a := r.cardDelivery(t, "k-pile-0001", "sq-pay-a")
	b := r.cardDelivery(t, "k-pile-0002", "sq-pay-b")
	c := r.delivery(t, "k-pile-0003", false, true) // cash: never on the pile
	r.deliverAs(t, till2, "77", a, b, c)
	v, err := r.svc.Tips(r.ctx, till2)
	if err != nil || len(v.Pile) != 2 || len(v.Typed) != 0 || v.Report == nil || v.Report.TipsToType != 2 {
		t.Fatalf("pile: %+v %v", v, err)
	}
	if _, err := r.svc.NoTip(r.ctx, till2, a); err != nil {
		t.Fatal(err)
	}
	tip := int64(300)
	if _, err := r.svc.Tip(r.ctx, till2, b, TipIn{TipCents: &tip}); err != nil {
		t.Fatal(err)
	}
	v, _ = r.svc.Tips(r.ctx, till2)
	if len(v.Pile) != 0 || len(v.Typed) != 2 || v.Report.TypedTipCents != 300 || v.Report.TipsToType != 0 {
		t.Fatalf("after Add tip / No tip: %+v", v)
	}
	// Another driver can't tip his orders.
	if _, err := r.svc.Login(r.ctx, till3, "88"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.NoTip(r.ctx, till3, b); err == nil {
		t.Fatal("someone else's order")
	}
}

// #25 (Quibase): a manager corrects the tip until Close batch; after the
// close it is read-only (batch_closed).
func TestManagerCorrectsTypedTipUntilClose(t *testing.T) {
	r := newDriverRig(t)
	card := r.cardDelivery(t, "k-mgr-00001", "sq-pay-m")
	d := r.deliverAs(t, till2, "77", card)
	tip := int64(1000)
	if _, err := r.svc.Tip(r.ctx, till2, card, TipIn{TipCents: &tip}); err != nil {
		t.Fatal(err)
	}
	five := int64(500)
	res, err := r.svc.ManagerTip(r.ctx, TipByQuibase, "tablet-1", card, ManagerTipIn{TipState: order.TipStateTipped, TipCents: &five})
	if err != nil || res.TypedTipCents != 500 || res.DriverID != d || res.Report == nil || lineOf(t, res.Report, card).TipCents != 500 {
		t.Fatalf("manager edit: %+v %v", res, err)
	}
	if _, err := r.svc.ManagerTip(r.ctx, TipByQuibase, "tablet-1", card, ManagerTipIn{TipState: "lots"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad state: %v", err)
	}
	// Back to none: on his pile again, nothing counted.
	res, err = r.svc.ManagerTip(r.ctx, TipByQuibase, "tablet-1", card, ManagerTipIn{TipState: order.TipStateNone})
	if err != nil || lineOf(t, res.Report, card).TipCents != 0 || res.Report.TipsToType != 1 {
		t.Fatalf("set back to none: %+v %v", res, err)
	}
	if _, err := r.svc.ManagerTip(r.ctx, TipByWeb, "web", card, ManagerTipIn{TipState: order.TipStateTipped, TipCents: &five}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.floor.CloseBatch(r.ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ManagerTip(r.ctx, TipByQuibase, "tablet-1", card, ManagerTipIn{TipState: order.TipStateNoTip}); !errors.Is(err, order.ErrTipBatchClosed) {
		t.Fatalf("after the close: %v", err)
	}
}

// #25: the press of Close batch puts the typed tip on the card payment (a
// 2.10p tip payment) and asks Square for the whole tip; the closed batch's
// report counts it once.
func TestClosePressPutsTypedTipsOnTheCard(t *testing.T) {
	r := newDriverRig(t)
	sq := &tipSquare{}
	r.svc.SetSquare(sq)
	card := r.cardDelivery(t, "k-press-001", "sq-pay-p")
	d := r.deliverAs(t, till2, "77", card)
	tip := int64(400)
	if _, err := r.svc.Tip(r.ctx, till2, card, TipIn{TipCents: &tip}); err != nil {
		t.Fatal(err)
	}
	closed, _, err := r.floor.CloseBatch(r.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	pays, _ := r.repo.OrderPayments(r.ctx, card)
	var tipPay int64
	for _, p := range pays {
		if p.AmountCents == 0 && !p.Refund && p.Ref != "" {
			tipPay += p.TipCents
		}
	}
	if tipPay != 400 {
		t.Fatalf("the tip payment: %+v", pays)
	}
	waitTip(t, "square adjusted", func() bool { return len(sq.seen()) == 1 })
	if c := sq.seen()[0]; c.payment != "sq-pay-p" || c.tip != 400 {
		t.Fatalf("square call: %+v", c)
	}
	waitTip(t, "square state saved", func() bool {
		m, _ := r.repo.DriverTipsOfBatch(r.ctx, closed.ID)
		return m[card].SquareState == order.TipSquareAdjusted && m[card].AppliedCents == 400
	})
	reps, err := r.floor.DriverReports(r.ctx, closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rep := range reps {
		if rep.DriverID == d && (rep.TipCents != 400 || lineOf(t, &rep, card).TipCents != 400) {
			t.Fatalf("counted once after the close: %+v", rep)
		}
	}
}

// #25: the press applies even when the close is then refused (leftovers);
// a correction after it goes on as the difference at the next press, and
// Square gets the new whole tip. A completed payment Square won't change is
// kept as not_adjustable (the driver is still owed the tip).
func TestClosePressRefusedStillAppliesAndCorrectionsFollow(t *testing.T) {
	r := newDriverRig(t)
	sq := &tipSquare{}
	r.svc.SetSquare(sq)
	card := r.cardDelivery(t, "k-press-002", "sq-pay-q")
	r.deliverAs(t, till2, "77", card)
	r.delivery(t, "k-leftover1", false, false) // still cooking: the close asks to confirm
	tip := int64(500)
	if _, err := r.svc.Tip(r.ctx, till2, card, TipIn{TipCents: &tip}); err != nil {
		t.Fatal(err)
	}
	var lo *order.LeftoversError
	if _, _, err := r.floor.CloseBatch(r.ctx, false); !errors.As(err, &lo) {
		t.Fatalf("leftovers: %v", err)
	}
	waitTip(t, "first adjust", func() bool { return len(sq.seen()) == 1 })
	rep, _ := r.svc.Report(r.ctx, till2)
	if lineOf(t, rep, card).TipCents != 500 || rep.TipCents != 500 {
		t.Fatalf("applied, not doubled: %+v", rep)
	}
	sq.mu.Lock()
	sq.err = &squareterm.Refused{Code: "not_adjustable", Message: "completed"}
	sq.mu.Unlock()
	tip = 300
	if _, err := r.svc.Tip(r.ctx, till2, card, TipIn{TipCents: &tip}); err != nil {
		t.Fatal(err)
	}
	if rep, _ := r.svc.Report(r.ctx, till2); rep.TipCents != 300 {
		t.Fatalf("lowered before the next press: %+v", rep)
	}
	closed, _, err := r.floor.CloseBatch(r.ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	waitTip(t, "second adjust", func() bool { return len(sq.seen()) == 2 })
	if c := sq.seen()[1]; c.tip != 300 {
		t.Fatalf("square gets the new whole tip: %+v", c)
	}
	waitTip(t, "not adjustable kept", func() bool {
		m, _ := r.repo.DriverTipsOfBatch(r.ctx, closed.ID)
		return m[card].SquareState == order.TipSquareNotAdjustable && m[card].AppliedCents == 300
	})
	reps, _ := r.floor.DriverReports(r.ctx, closed.ID)
	for _, rp := range reps {
		if len(rp.Lines) > 0 && rp.TipCents != 300 {
			t.Fatalf("closed report: %+v", rp)
		}
	}
}

// #27: a delivery still not delivered past the store's setting (60 min)
// shows for every driver, whoever has it, and stays there; a claim makes it
// the claimer's and delivered. Old code: a delivery out with another driver
// (or pooled) never showed on anyone's list.
func TestOldUndeliveredDeliveryShowsForEveryDriver(t *testing.T) {
	r := newDriverRig(t)
	paid := r.delivery(t, "k-old-00001", true, true)
	sam, err := r.svc.Login(r.ctx, till3, "88")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.floor.ClockIn(r.ctx, sam.Driver.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.floor.SendOut(r.ctx, sam.Driver.ID, orderApp.SendOutRequest{Include: []string{paid}}); err != nil {
		t.Fatal(err)
	}
	ali, err := r.svc.Login(r.ctx, till2, "77")
	if err != nil {
		t.Fatal(err)
	}
	r.later(59 * time.Minute)
	if list, _ := r.svc.Available(r.ctx, till2); len(list) != 0 {
		t.Fatalf("out with Sam, 59 min: %+v", list)
	}
	var tk *order.ErrTaken
	if _, err := r.svc.Claim(r.ctx, till2, ClaimIn{OrderIDs: []string{paid}}); !errors.As(err, &tk) {
		t.Fatalf("claim before 60 min: %v", err)
	}
	r.later(61 * time.Minute)
	for _, a := range []Actor{till2, till3} { // every driver, Sam too
		list, err := r.svc.Available(r.ctx, a)
		if err != nil || len(list) != 1 || list[0].OrderID != paid || !list[0].Old || list[0].DriverName != "Sam" ||
			list[0].State != string(order.StateOnDelivery) {
			t.Fatalf("old for %s: %+v %v", a.DeviceID, list, err)
		}
	}
	cl, err := r.svc.Claim(r.ctx, till2, ClaimIn{OrderIDs: []string{paid}})
	if err != nil || len(cl.Orders) != 1 {
		t.Fatalf("claim: %+v %v", cl, err)
	}
	so := cl.Orders[0].Status
	if so.State != order.StateDelivered || so.DriverID != ali.Driver.ID || so.DeliveredAt == nil || so.PoolDriverID != "" {
		t.Fatalf("his and delivered: %+v", so)
	}
	if list, _ := r.svc.Available(r.ctx, till3); len(list) != 0 {
		t.Fatalf("gone once claimed: %+v", list)
	}
	rep, _ := r.svc.Report(r.ctx, till2)
	if rep.Deliveries != 1 {
		t.Fatalf("on the claimer's report: %+v", rep)
	}
}

func waitTip(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
