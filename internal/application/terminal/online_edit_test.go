package terminal

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// arriveOnline stores a placed online order with two own lines (a $12.00 pie
// with a $1.00 topping, two $3.00 wings) and tax: paid online by card or not.
func arriveOnline(t *testing.T, r *rig, fulfillment string, card bool) order.StoredOrder {
	t.Helper()
	items := []order.LineItem{
		{ID: uuid.NewString(), Name: "Cheese Pizza", Size: "Large", Quantity: 1, UnitPrice: 1200, LineTotal: 1300,
			Children: []order.LineItem{{ID: uuid.NewString(), Name: "Pepperoni", Quantity: 1, UnitPrice: 100, LineTotal: 100}}, Routing: order.RoutingMatched},
		{ID: uuid.NewString(), Name: "Wings", Quantity: 2, UnitPrice: 300, LineTotal: 600, Routing: order.RoutingMatched},
	}
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: "V-" + uuid.NewString()[:6], PlacedAt: time.Now().UTC(), Fulfillment: order.Fulfillment{Type: fulfillment},
		Customer: order.Customer{DisplayName: "Sam", Phone: "4105550100"}, Items: items,
		Money: order.Money{Subtotal: 1900, Tax: 190, Tip: 300, Total: 2390}}
	if fulfillment == "delivery" {
		s.Fulfillment.Address = &order.Address{Line1: "1 Test St", City: "Towson"}
	}
	if card {
		s.Payments = []order.PaymentSummary{{Method: "card", AmountCents: 2390, Last4: "1881"}}
	}
	if _, err := r.repo.UpsertArrived(r.ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := r.repo.ClaimAccept(r.ctx, uuid.MustParse(s.ID), 20, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.repo.RecordAccepted(r.ctx, *got); err != nil {
		t.Fatal(err)
	}
	return *got
}

// The terminal reads an online order's own lines as custom lines.
func onlineCartOf(t *testing.T, r *rig, id string) Cart {
	t.Helper()
	v, err := r.svc.Order(r.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	tm, _ := v.Order.Order.SourceMeta["terminal"].(map[string]any)
	raw, _ := json.Marshal(tm["cart"])
	var c Cart
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestOnlineDeliveryEditedLikeOurs(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	so := arriveOnline(t, r, "delivery", true)

	cart := onlineCartOf(t, r, so.Order.ID)
	if len(cart.Lines) != 2 || !cart.Lines[0].Custom || cart.Lines[0].Name != "Large Cheese Pizza" || cart.Lines[0].Note != "Pepperoni" ||
		*cart.Lines[0].PriceCents != 1300 || cart.Lines[1].Qty != 2 || *cart.Lines[1].PriceCents != 300 {
		t.Fatalf("online cart: %+v", cart.Lines)
	}

	// Add two sodas (our price): the balance is due at the terminal.
	add := cart
	add.Lines = append(append([]pricing.Line(nil), cart.Lines...), pricing.Line{Qty: 2, Selection: pricing.Selection{ItemID: soda}})
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Type: "delivery", OrderID: so.Order.ID, Cart: add})
	if err != nil {
		t.Fatal(err)
	}
	quoted := q.Quote.(pricing.Quote)
	res, err := r.svc.ChangeCart(r.ctx, till, so.Order.ID, ChangeIn{Cart: add, ClientKey: tk()})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Order.Order.Money
	if len(res.Added) != 1 || len(res.Removed) != 0 || m.Total != quoted.TotalCents || m.Tip != 300 || m.Total <= 2390 {
		t.Fatalf("added: %+v money %+v quote %d", res, m, quoted.TotalCents)
	}
	if res.BalanceCents != m.Total-2390 || res.RefundDue != nil || res.Order.Status.RefundOwedCents != 0 {
		t.Fatalf("balance %d (total %d), refund %+v", res.BalanceCents, m.Total, res.RefundDue)
	}
	if res.Order.Order.Source != "online" || res.Order.Order.Fulfillment.Type != "delivery" || len(res.Order.Order.Payments) != 1 {
		t.Fatalf("an online order stays itself: %+v", res.Order.Order)
	}
	// The balance taken in cash at Cash Out.
	pay, err := r.svc.AddPayment(r.ctx, till, so.Order.ID, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: res.BalanceCents})
	if err != nil || pay.BalanceCents != 0 || !pay.Order.Status.Paid {
		t.Fatalf("balance paid: %+v %v", pay, err)
	}

	// Remove the wings (2 x $3.00 + their tax share): owed to the customer.
	rm := onlineCartOf(t, r, so.Order.ID)
	var keep []pricing.Line
	for _, l := range rm.Lines {
		if l.Custom && l.Name == "Wings" {
			continue
		}
		keep = append(keep, l)
	}
	rm.Lines = keep
	var vn *terminal.ManagerRequired // a line of a sent order taken off: a manager's
	if _, err := r.svc.ChangeCart(r.ctx, till, so.Order.ID, ChangeIn{Cart: rm, ClientKey: tk()}); !errors.As(err, &vn) || vn.Action != terminal.ActionVoid {
		t.Fatalf("a cashier takes an online order's line off without a manager: %v", err)
	}
	res2, err := r.svc.ChangeCart(r.ctx, till, so.Order.ID, ChangeIn{Cart: rm, ClientKey: tk(), ManagerPIN: "9876"})
	if err != nil {
		t.Fatal(err)
	}
	wantOwed := int64(600 + 60) // the lines + 10 % tax share
	if res2.RefundDue == nil || res2.RefundDue.Cents != wantOwed || res2.RefundDue.TotalOwed != wantOwed ||
		res2.Order.Status.RefundOwedCents != wantOwed || len(res2.RefundDue.Lines) != 1 || res2.RefundDue.Lines[0].Cents != 600 ||
		res2.RefundDue.Reference != so.Order.SourceEventID || res2.RefundDue.Source != "Online" || res2.RefundDue.DeviceName != "Till 1" ||
		res2.RefundDue.StaffName == "" {
		t.Fatalf("refund due: %+v", res2.RefundDue)
	}
	if res2.BalanceCents != 0 || res2.Order.Order.Money.Total != m.Total-wantOwed {
		t.Fatalf("after removal: balance %d total %d", res2.BalanceCents, res2.Order.Order.Money.Total)
	}
	// The kitchen got a change ticket (REMOVE), main the event with the owed amount.
	if len(r.orders.typeFrom) != 2 || len(r.orders.voidLns) != 1 || r.orders.voidLns[0][0].Name != "Wings" {
		t.Fatalf("change tickets: %d, removed %+v", len(r.orders.typeFrom), r.orders.voidLns)
	}
	evs, _ := r.repo.ListUnsyncedEvents(r.ctx, 1000)
	var seen bool
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if p["type"] == "order.changed" && p["refund_owed_cents"] == float64(wantOwed) && p["refund_due"] != nil {
			seen = true
		}
	}
	if !seen {
		t.Fatal("main's order.changed with the refund owed")
	}
	if len(journalOf(t, r, terminal.ActionRefundOwed)) != 1 {
		t.Fatal("refund.owed journaled")
	}
	// A type change is refused.
	if _, err := r.svc.ChangeCart(r.ctx, till, so.Order.ID, ChangeIn{Cart: rm, ClientKey: tk(), Type: "pickup"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("type change: %v", err)
	}
	// The order as the terminals read it carries what is owed.
	if v, err := r.svc.Order(r.ctx, so.Order.ID); err != nil || v.Order.Status.RefundOwedCents != wantOwed {
		t.Fatalf("order owed: %v", err)
	}
}

// A cash online pickup: removing a line lowers what is due, nothing is owed.
func TestOnlineCashPickupRemovalLowersTheBalance(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	so := arriveOnline(t, r, "pickup", false)
	c := onlineCartOf(t, r, so.Order.ID)
	c.Lines[1].Qty = 1 // one of the two wings
	res, err := r.svc.ChangeCart(r.ctx, till, so.Order.ID, ChangeIn{Cart: c, ClientKey: tk(), ManagerPIN: "9876"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RefundDue != nil || res.BalanceCents != 2390-330 || res.Order.Order.Money.Total != 2390-330 {
		t.Fatalf("cash pickup: %+v balance %d", res.RefundDue, res.BalanceCents)
	}
}

// A refund on an earlier day's order: a cashier needs a manager; with one it
// lands in today's batch.
func TestEarlierDayRefundWithAManager(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: sodaCart(2),
		Payments: []PaymentIn{{Tender: "cash", AmountCents: totalOf(t, r, sodaCart(2)), TenderedCents: 5000}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	if _, _, err := r.repo.CloseBatch(r.ctx, true); err != nil {
		t.Fatal(err)
	}
	var need *terminal.ManagerRequired
	_, err = r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 100, Refund: true, Reason: "cold"})
	if !errors.As(err, &need) || need.Action != terminal.NeedRefundEarlierDay || need.Requested != 100 {
		t.Fatalf("cashier: %v", err)
	}
	// A payment (not a refund) stays refused.
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 1}); !errors.Is(err, terminal.ErrNotPayable) {
		t.Fatalf("payment on an earlier day: %v", err)
	}
	pay, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 100, Refund: true, Reason: "cold",
		ManagerPIN: "9876"})
	if err != nil || pay.Payment.ApprovedBy != "mgr-1" {
		t.Fatalf("with a manager: %+v %v", pay, err)
	}
	if pay.Payment.BatchID == res.Order.Status.BatchID {
		t.Fatalf("the refund is today's: batch %d", pay.Payment.BatchID)
	}
	evs, _ := r.repo.ListUnsyncedEvents(r.ctx, 1000)
	var flagged bool
	for _, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		if p["type"] == "order.payment" && p["earlier_day"] == true {
			flagged = true
		}
	}
	if !flagged {
		t.Fatal("main hears earlier_day")
	}
}

// Tips are card only.
func TestCashTipRefused(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "9876") // a manager can't either
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "pickup", Cart: sodaCart(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 100, TipCents: 50}); !errors.Is(err, terminal.ErrTipCardOnly) {
		t.Fatalf("cash tip: %v", err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 100, TipCents: 50}); err != nil {
		t.Fatalf("card tip: %v", err)
	}
}

func totalOf(t *testing.T, r *rig, c Cart) int64 {
	t.Helper()
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: c})
	if err != nil {
		t.Fatal(err)
	}
	return q.Quote.(pricing.Quote).TotalCents
}
