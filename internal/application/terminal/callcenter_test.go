package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/device"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/rs/zerolog"
)

// The call center (contract §2.10ab): the seat's session, the allowlist,
// the guards, paid lines locked, the store wins, taken by, reprint.

// The fake floor also prints the call center's receipt and reprints
// (optional interfaces of Orders): what it was asked, per rig.
var (
	ccReceipts sync.Map // *fakeOrders -> []order.StoredOrder
	ccReprints sync.Map // *fakeOrders -> []string (order ids)
	ccReprint  sync.Map // *fakeOrders -> error (the next ReprintOn's)
)

func (f *fakeOrders) PrintOrderReceipt(_ context.Context, so order.StoredOrder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list, _ := ccReceipts.Load(f)
	l, _ := list.([]order.StoredOrder)
	ccReceipts.Store(f, append(l, so))
}

func (f *fakeOrders) ReprintOn(_ context.Context, id string) (*order.StoredOrder, string, error) {
	if err, ok := ccReprint.Load(f); ok && err != nil {
		return nil, "", err.(error)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	list, _ := ccReprints.Load(f)
	l, _ := list.([]string)
	ccReprints.Store(f, append(l, id))
	n := int64(7)
	return &order.StoredOrder{Order: order.Snapshot{ID: id}, Status: order.Status{KitchenNumber: &n}}, "Front", nil
}

func receiptsOf(f *fakeOrders) []order.StoredOrder {
	list, _ := ccReceipts.Load(f)
	l, _ := list.([]order.StoredOrder)
	return l
}

const (
	agentID  = "cc-1"
	agentPIN = "4321"
)

func ccStaff(t *testing.T) terminal.Staff {
	return terminal.Staff{ID: agentID, FirstName: "Elena", LastName: "Vega", Role: terminal.RoleCallcenter, PinHash: hash(t, agentPIN),
		// Limits main would never send for the role: ignored anyway.
		Limits: &terminal.Limits{RefundCents: 9000, DiscountPct: 90}}
}

// ccRig is a rig whose config has a call-center agent; seat is one browser.
func ccRig(t *testing.T) (*rig, Actor) {
	t.Helper()
	r := newRig(t)
	r.main.set(PathTerminalConfig, staffConfig(t, 2, ccStaff(t)))
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	return r, CallcenterActor("desk-2", "Elevate desk 2", agentID)
}

func whatOf(err error) string {
	var no *terminal.NotAllowedForCallcenter
	if errors.As(err, &no) {
		return no.What
	}
	if err == nil {
		return "<allowed>"
	}
	return "<" + err.Error() + ">"
}

func cartOf(lines ...pricing.Line) Cart { return Cart{Cart: pricing.Cart{Lines: lines}} }

func sodaLine(qty int) pricing.Line {
	return pricing.Line{Qty: qty, Selection: pricing.Selection{ItemID: soda}}
}

func cents(v int64) *int64 { return &v }

func ccPlace(t *testing.T, r *rig, seat Actor, typ string, pays []PaymentIn, lines ...pricing.Line) *PlaceResult {
	t.Helper()
	in := OrderIn{ClientKey: tk(), Type: typ, Cart: cartOf(lines...), Customer: &Customer{Name: "Ada Test", Phone: "4105550101"}, Payments: pays}
	if typ == terminal.TypeDelivery {
		in.Customer.Address = &Address{Line1: "1 Test St", City: "Towson"}
	}
	res, err := r.svc.PlaceOrder(r.ctx, seat, in)
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	return res
}

func TestCallcenterAllowlist(t *testing.T) {
	allowed := [][2]string{
		{"GET", "menu"}, {"GET", "status"}, {"GET", "session"}, {"POST", "quote"}, {"POST", "orders"}, {"GET", "orders"},
		{"GET", "orders/0199-x"}, {"PUT", "orders/0199-x/cart"}, {"POST", "orders/0199-x/payments"}, {"POST", "square/keyed-payments"},
		{"POST", "orders/0199-x/void"}, {"POST", "orders/0199-x/time"}, {"POST", "orders/0199-x/reprint"}, {"GET", "pickups"},
		{"GET", "checks"}, {"GET", "checks/0199-x"}, {"GET", "drivers"}, {"GET", "floor"}, {"GET", "drafts"}, {"PUT", "drafts/d-12345678"},
		{"DELETE", "drafts/d-12345678"}, {"GET", "customers"}, {"POST", "customers"}, {"PATCH", "customers/c1"},
		{"POST", "customers/c1/addresses"}, {"GET", "customers/c1/orders"}, {"GET", "addresses"}, {"POST", "journal"},
	}
	for _, rt := range allowed {
		if !CallcenterAllowed(rt[0], rt[1]) {
			t.Errorf("%s %s must be allowed", rt[0], rt[1])
		}
	}
	refused := [][2]string{
		{"POST", "login"}, {"POST", "logout"}, {"POST", "manager-check"}, {"POST", "refunds"}, {"POST", "orders/x/waste"},
		{"POST", "orders/x/picked-up"}, {"GET", "unsettled"}, {"GET", "drawer"}, {"POST", "drawer/open"}, {"POST", "drawer/pay-out"},
		{"POST", "drawer/close"}, {"GET", "z"}, {"GET", "reports/till"}, {"GET", "reports/drivers"}, {"POST", "reports/drivers/print"},
		{"POST", "orders/x/payments/p1/tip"}, {"POST", "close"}, {"GET", "journal"}, {"POST", "deliveries/send-out"},
		{"POST", "drivers/d1/returned"}, {"GET", "setup"}, {"POST", "print"}, {"GET", "square/session"}, {"POST", "square/test-refund"},
		{"GET", "app/latest"}, {"GET", "app/apk"}, {"GET", "driver/mine"}, {"POST", "driver/claim"},
		// The right path with another method, and paths that only look right.
		{"DELETE", "orders/x"}, {"POST", "menu"}, {"PUT", "orders"}, {"POST", "floor"}, {"GET", "orders/x/cart"}, {"GET", "orders//cart"},
		{"GET", ""}, {"GET", "orders/../close"}, {"PUT", "orders/x/cart/extra"}, {"GET", "orders/x?open=1"}, {"get", "menu"},
		{"POST", "drafts/d-12345678"}, {"GET", "drafts/d-12345678"}, {"DELETE", "customers/c1"},
	}
	for _, rt := range refused {
		if CallcenterAllowed(rt[0], rt[1]) {
			t.Errorf("%s %q must be refused", rt[0], rt[1])
		}
	}
}

func TestCallcenterSessionIsTheRelaysStaff(t *testing.T) {
	r, seat := ccRig(t)
	if err := r.svc.CallcenterSession(r.ctx, seat); err != nil {
		t.Fatalf("the agent: %v", err)
	}
	v := r.svc.Session(seat)
	if v.Staff == nil || v.Staff.ID != agentID || v.Role != terminal.RoleCallcenter || v.Limits == nil ||
		*v.Limits != (terminal.Limits{}) {
		t.Fatalf("session: staff %+v role %s limits %+v (refund 0, discount 0, never unlimited)", v.Staff, v.Role, v.Limits)
	}
	if seat.DeviceID != "callcenter:desk-2" || !device.IsCallcenterSeat(seat.DeviceID) {
		t.Fatalf("seat id %q", seat.DeviceID)
	}
	// A cashier's or a manager's id is not a call-center session; nor none.
	for _, id := range []string{"cash-1", "mgr-1", ""} {
		a := CallcenterActor("desk-2", "", id)
		if err := r.svc.CallcenterSession(r.ctx, a); !errors.Is(err, terminal.ErrLoginRequired) {
			t.Errorf("staff %q: %v, want login_required", id, err)
		}
		if _, err := r.svc.Quote(r.ctx, a, QuoteIn{Type: "pickup", Cart: cartOf(pizzaLine())}); err != nil {
			t.Errorf("a quote needs no login: %v", err)
		}
		if _, err := r.svc.PlaceOrder(r.ctx, a, OrderIn{ClientKey: tk(), Type: "pickup", Cart: cartOf(pizzaLine())}); !errors.Is(err, terminal.ErrLoginRequired) {
			t.Errorf("staff %q places: %v", id, err)
		}
	}
	// Switched off in main: the next call is login_required.
	off := false
	st := ccStaff(t)
	st.Active = &off
	r.main.set(PathTerminalConfig, staffConfig(t, 3, st))
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.CallcenterSession(r.ctx, seat); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("switched off: %v", err)
	}
}

// A person made a minute ago: the snapshot is pulled again once for an id it
// doesn't know (and not again for every wrong id).
func TestCallcenterUnknownAgentRefreshesTheSnapshotOnce(t *testing.T) {
	r := newRig(t) // no call-center agent in the Pi's snapshot yet
	seat := CallcenterActor("desk-1", "", agentID)
	now := time.Now()
	r.svc.now = func() time.Time { return now }
	fetches := func() int { return r.pub.count(EventConfigChanged) }
	before := fetches()
	if err := r.svc.CallcenterSession(r.ctx, seat); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("unknown and main doesn't know them either: %v", err)
	}
	// main has the new person now; within the refresh pause nothing is pulled.
	r.main.set(PathTerminalConfig, staffConfig(t, 2, ccStaff(t)))
	if err := r.svc.CallcenterSession(r.ctx, seat); !errors.Is(err, terminal.ErrLoginRequired) || fetches() != before {
		t.Fatalf("no second pull within %s: %v (config changes %d -> %d)", callcenterRefreshEvery, err, before, fetches())
	}
	now = now.Add(callcenterRefreshEvery + time.Second)
	if err := r.svc.CallcenterSession(r.ctx, seat); err != nil {
		t.Fatalf("after the pull: %v", err)
	}
	if fetches() != before+1 {
		t.Fatalf("pulled once: %d -> %d", before, fetches())
	}
	// A known id of another role never pulls.
	now = now.Add(time.Hour)
	if err := r.svc.CallcenterSession(r.ctx, CallcenterActor("desk-1", "", "cash-1")); !errors.Is(err, terminal.ErrLoginRequired) || fetches() != before+1 {
		t.Fatalf("a cashier's id: %v, pulls %d", err, fetches()-before)
	}
}

// A call-center PIN is refused on a store terminal, and is never a manager.
func TestCallcenterPinRefusedOnAStoreTerminal(t *testing.T) {
	r, _ := ccRig(t)
	if _, err := r.svc.Login(r.ctx, till, agentPIN); !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("login with a call-center PIN: %v, want wrong_pin", err)
	}
	if v := r.svc.Session(till); v.Staff != nil {
		t.Fatalf("nobody is logged in: %+v", v.Staff)
	}
	r.login(t, till, "1234")
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: agentPIN}); !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("manager-check with a call-center PIN: %v", err)
	}
	// An inline manager_pin: a 50 % discount is over the cashier's limit.
	in := OrderIn{ClientKey: tk(), Type: "counter", ManagerPIN: agentPIN,
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}, Discount: &Discount{Pct: 50, Reason: "x"}}}
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.Is(err, terminal.ErrManagerPINWrong) {
		t.Fatalf("manager_pin = a call-center PIN: %v", err)
	}
	// The cached PIN (a first match) is no way around it either.
	if _, err := r.svc.Login(r.ctx, Actor{DeviceID: "dev-9"}, agentPIN); !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("again: %v", err)
	}
}

func TestCallcenterGuards(t *testing.T) {
	r, seat := ccRig(t)
	pizza, card := pizzaLine(), []PaymentIn{{Tender: terminal.TenderManualCard, AmountCents: 100}}
	place := func(mut func(*OrderIn)) error {
		in := OrderIn{ClientKey: tk(), Type: "pickup", Cart: cartOf(pizza)}
		mut(&in)
		_, err := r.svc.PlaceOrder(r.ctx, seat, in)
		return err
	}
	priced := pizzaLine()
	priced.PriceCents = cents(500)
	comp := pizzaLine()
	comp.PriceCents = cents(0)
	optPrice := pizzaLine()
	optPrice.Options[0].PriceCents = cents(1)
	cases := []struct {
		name string
		mut  func(*OrderIn)
		want string
	}{
		{"type counter", func(in *OrderIn) { in.Type = "counter" }, terminal.NotAllowedType},
		{"no type (= counter)", func(in *OrderIn) { in.Type = "" }, terminal.NotAllowedType},
		{"a typed discount", func(in *OrderIn) { in.Cart.Discount = &Discount{Pct: 5, Reason: "regular"} }, terminal.NotAllowedDiscount},
		{"an employee meal", func(in *OrderIn) { in.Cart.Discount = &Discount{Pct: 20, Reason: "staff", Kind: KindEmployeeMeal} }, terminal.NotAllowedDiscount},
		{"a custom line", func(in *OrderIn) {
			in.Cart.Lines = append(in.Cart.Lines, pricing.Line{Custom: true, Name: "Extra", Qty: 1, Selection: pricing.Selection{PriceCents: cents(300)}})
		}, terminal.NotAllowedOpenPrice},
		{"a price change", func(in *OrderIn) { in.Cart.Lines = []pricing.Line{priced} }, terminal.NotAllowedPriceChange},
		{"a $0 comp line", func(in *OrderIn) { in.Cart.Lines = []pricing.Line{comp} }, terminal.NotAllowedComp},
		{"a price on an option", func(in *OrderIn) { in.Cart.Lines = []pricing.Line{optPrice} }, terminal.NotAllowedPriceChange},
		{"cash", func(in *OrderIn) { in.Payments = []PaymentIn{{Tender: terminal.TenderCash, AmountCents: 100}} }, terminal.NotAllowedTender},
		{"the reader", func(in *OrderIn) { in.Payments = []PaymentIn{{Tender: terminal.TenderCard, AmountCents: 100}} }, terminal.NotAllowedTender},
		{"Square's app", func(in *OrderIn) { in.Payments = []PaymentIn{{Tender: terminal.TenderSquareApp, AmountCents: 100}} }, terminal.NotAllowedTender},
		{"a comp tender", func(in *OrderIn) { in.Payments = []PaymentIn{{Tender: terminal.TenderComp, AmountCents: 100}} }, terminal.NotAllowedComp},
		{"a refund", func(in *OrderIn) {
			in.Payments = []PaymentIn{{Tender: terminal.TenderManualCard, AmountCents: 100, Refund: true, Reason: "x"}}
		}, terminal.NotAllowedRefund},
		{"a manager's PIN", func(in *OrderIn) { in.ManagerPIN = "9876" }, terminal.NotAllowedManager},
		{"an approval", func(in *OrderIn) { in.Approvals = []string{"ap-1"} }, terminal.NotAllowedManager},
	}
	for _, c := range cases {
		if got := whatOf(place(c.mut)); got != c.want {
			t.Errorf("placing with %s: %s, want %s", c.name, got, c.want)
		}
	}
	if list, _ := r.svc.Orders(r.ctx, false); len(list) != 0 || len(r.orders.printed) != 0 || r.pub.count(EventOrderPlaced) != 0 {
		t.Fatalf("a refused call places and prints nothing: %d orders, %d tickets", len(list), len(r.orders.printed))
	}
	// The same bodies from a cashier's terminal are the cashier's business.
	r.login(t, till, "1234")
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizza}},
		Discount: &Discount{Pct: 5, Reason: "regular"}}}); err != nil {
		t.Fatalf("a cashier's discount: %v", err)
	}

	// A quote changes nothing: only a typed discount is refused on it; a
	// cart that holds a cashier's custom line or price prices as it is.
	if _, err := r.svc.Quote(r.ctx, seat, QuoteIn{Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizza}}, Discount: &Discount{Pct: 5, Reason: "x"}}}); whatOf(err) != terminal.NotAllowedDiscount {
		t.Errorf("quote with a discount: %v", err)
	}
	if _, err := r.svc.Quote(r.ctx, seat, QuoteIn{Type: "pickup", Cart: cartOf(priced, pricing.Line{Custom: true, Name: "Extra", Qty: 1, Selection: pricing.Selection{PriceCents: cents(300)}})}); err != nil {
		t.Errorf("quote of a cart with a price and a custom line: %v", err)
	}

	// Payments on a placed order: a typed card only.
	res := ccPlace(t, r, seat, "pickup", nil, pizza)
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	for _, c := range []struct {
		name string
		in   PaymentIn
		want string
	}{
		{"cash", PaymentIn{Tender: terminal.TenderCash, AmountCents: total}, terminal.NotAllowedTender},
		{"comp", PaymentIn{Tender: terminal.TenderComp, AmountCents: total}, terminal.NotAllowedComp},
		{"refund", PaymentIn{Tender: terminal.TenderManualCard, AmountCents: 100, Refund: true, Reason: "x"}, terminal.NotAllowedRefund},
		{"manager", PaymentIn{Tender: terminal.TenderManualCard, AmountCents: total, ManagerPIN: "9876"}, terminal.NotAllowedManager},
	} {
		c.in.ClientKey = tk()
		if _, err := r.svc.AddPayment(r.ctx, seat, id, c.in); whatOf(err) != c.want {
			t.Errorf("payment %s: %s, want %s", c.name, whatOf(err), c.want)
		}
	}
	if pays, _ := r.repo.OrderPayments(r.ctx, id); len(pays) != 0 {
		t.Fatalf("no payment recorded: %+v", pays)
	}
	paid, err := r.svc.AddPayment(r.ctx, seat, id, PaymentIn{ClientKey: tk(), Tender: terminal.TenderManualCard, AmountCents: total, TipCents: 200,
		Last4: "1881", SquarePaymentID: "sq-1", Entry: "keyed"})
	if err != nil || paid.BalanceCents != 0 || paid.Payment.DeviceID != seat.DeviceID || paid.Payment.StaffID != agentID {
		t.Fatalf("a typed card with its tip: %+v %v", paid, err)
	}
	_ = card
}

// A cart change may not bring a new custom line or price; what the stored
// order already holds rides along.
func TestCallcenterChangeKeepsWhatTheOrderHolds(t *testing.T) {
	r, seat := ccRig(t)
	r.login(t, till, "9876") // a manager: any price
	custom := pricing.Line{Custom: true, Name: "Extra sauce", Qty: 1, Selection: pricing.Selection{PriceCents: cents(150)}}
	lowered := pizzaLine()
	lowered.PriceCents = cents(1200) // the store's own price change
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "pickup", Cart: cartOf(lowered, custom),
		Customer: &Customer{Name: "Ada Test", Phone: "4105550101"}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	// The same lines + a soda: allowed (how the sender writes an empty list, the note and the quantity don't matter).
	same := custom
	same.Options, same.Note = []pricing.Pick{}, "on the side"
	ch, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(lowered, same, sodaLine(1))})
	if err != nil || !ch.Changed || len(ch.Added) == 0 {
		t.Fatalf("the order's own custom line and price ride along: %+v %v", ch, err)
	}
	for name, c := range map[string]struct {
		cart Cart
		want string
	}{
		"another custom line":   {cartOf(lowered, custom, pricing.Line{Custom: true, Name: "Tip jar", Qty: 1, Selection: pricing.Selection{PriceCents: cents(100)}}), terminal.NotAllowedOpenPrice},
		"the custom line twice": {cartOf(lowered, custom, custom), terminal.NotAllowedOpenPrice},
		"its price changed":     {cartOf(lowered, pricing.Line{Custom: true, Name: "Extra sauce", Qty: 1, Selection: pricing.Selection{PriceCents: cents(50)}}), terminal.NotAllowedOpenPrice},
		"another price":         {cartOf(func() pricing.Line { l := pizzaLine(); l.PriceCents = cents(900); return l }(), custom), terminal.NotAllowedPriceChange},
		"a discount":            {Cart{Cart: pricing.Cart{Lines: []pricing.Line{lowered, custom}}, Discount: &Discount{AmountCents: 100, Reason: "x"}}, terminal.NotAllowedDiscount},
	} {
		if _, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: c.cart}); whatOf(err) != c.want {
			t.Errorf("%s: %s, want %s", name, whatOf(err), c.want)
		}
	}
	if _, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(lowered, custom), ManagerPIN: "9876"}); whatOf(err) != terminal.NotAllowedManager {
		t.Errorf("manager_pin on a change: %v", err)
	}
	// An online order's own lines are custom lines on the terminal: they ride along too.
	so := arriveOnline(t, r, "pickup", false)
	own := onlineCartOf(t, r, so.Order.ID)
	own.Lines = append(own.Lines, sodaLine(1))
	if ch, err := r.svc.ChangeCart(r.ctx, seat, so.Order.ID, ChangeIn{ClientKey: tk(), Cart: own}); err != nil || !ch.Changed {
		t.Fatalf("adding to an online order: %+v %v", ch, err)
	}
	own.Lines = append(own.Lines, pricing.Line{Custom: true, Name: "Made up", Qty: 1, Selection: pricing.Selection{PriceCents: cents(1)}})
	if _, err := r.svc.ChangeCart(r.ctx, seat, so.Order.ID, ChangeIn{ClientKey: tk(), Cart: own}); whatOf(err) != terminal.NotAllowedOpenPrice {
		t.Fatalf("a new custom line on an online order: %v", err)
	}
}

func TestCallcenterPaidLinesLocked(t *testing.T) {
	r, seat := ccRig(t)
	// Unpaid: cashier rules (a line may go).
	res := ccPlace(t, r, seat, "pickup", nil, pizzaLine(), sodaLine(2))
	id := res.Order.Order.ID
	if ch, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine(), sodaLine(1))}); err != nil || len(ch.Removed) != 1 {
		t.Fatalf("an unpaid order: %+v %v", ch, err)
	}
	// A typed card on it: adding is allowed ...
	if _, err := r.svc.AddPayment(r.ctx, seat, id, PaymentIn{ClientKey: tk(), Tender: terminal.TenderManualCard, AmountCents: 500, Last4: "1881"}); err != nil {
		t.Fatal(err)
	}
	more, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine(), sodaLine(3))})
	if err != nil || !more.Changed || len(more.Added) != 1 || more.Added[0] != "2× Soda" || len(more.Removed) != 0 {
		t.Fatalf("raising a quantity on a paid order: %+v %v", more, err)
	}
	withWings, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine(), sodaLine(3), pizzaLine())})
	if err != nil || len(withWings.Added) != 1 {
		t.Fatalf("a new line on a paid order: %+v %v", withWings, err)
	}
	prints, changes := len(r.orders.addOns)+len(r.orders.voidLns), r.pub.count(EventOrderChanged)
	before, _ := r.svc.Order(r.ctx, id)
	// ... removing, lowering or changing what was there is not.
	noted := pizzaLine()
	noted.Note = "well done"
	plain := pizzaLine()
	plain.Options = nil
	for name, cart := range map[string]Cart{
		"a line removed":     cartOf(pizzaLine(), pizzaLine()),
		"a quantity lowered": cartOf(pizzaLine(), sodaLine(2), pizzaLine()),
		"a note changed":     cartOf(noted, sodaLine(3), pizzaLine()),
		"an option removed":  cartOf(plain, sodaLine(3), pizzaLine()),
		"a line swapped":     cartOf(pizzaLine(), sodaLine(3), sodaLine(1)),
	} {
		_, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cart})
		var pl *terminal.PaidLocked
		if !errors.As(err, &pl) || len(pl.Lines) == 0 {
			t.Errorf("%s: %v, want paid_locked {lines}", name, err)
			continue
		}
		if name == "a quantity lowered" && (len(pl.Lines) != 1 || pl.Lines[0] != "Soda") {
			t.Errorf("%s: lines %v", name, pl.Lines)
		}
	}
	after, _ := r.svc.Order(r.ctx, id)
	if len(after.Order.Order.Items) != len(before.Order.Order.Items) || after.Order.Order.Money.Total != before.Order.Order.Money.Total ||
		len(r.orders.addOns)+len(r.orders.voidLns) != prints || r.pub.count(EventOrderChanged) != changes {
		t.Fatalf("a locked change changes and prints nothing")
	}
	// The store's own terminal still changes it under its rules (refund first when the total drops below what was paid).
	r.login(t, till, "1234")
	if ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine(), sodaLine(3))}); err != nil || len(ch.Removed) != 1 {
		t.Fatalf("a cashier removes a line of a part-paid order: %+v %v", ch, err)
	}
	// Moving takeout <-> delivery stays as for a cashier.
	if ch, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine(), sodaLine(3)), Type: "delivery",
		Customer: &Customer{Name: "Ada Test", Address: &Address{Line1: "1 Test St", City: "Towson"}}}); err != nil || !ch.TypeChanged {
		t.Fatalf("pickup -> delivery on a paid order: %+v %v", ch, err)
	}

	// An online order that arrived paid: its lines are locked; one that
	// pays cash at the store is edited under cashier rules.
	paidOnline := arriveOnline(t, r, "pickup", true)
	cart := onlineCartOf(t, r, paidOnline.Order.ID)
	var pl *terminal.PaidLocked
	if _, err := r.svc.ChangeCart(r.ctx, seat, paidOnline.Order.ID, ChangeIn{ClientKey: tk(), Cart: cartOf(cart.Lines[0])}); !errors.As(err, &pl) {
		t.Fatalf("a line off an online order paid by card: %v", err)
	}
	if ch, err := r.svc.ChangeCart(r.ctx, seat, paidOnline.Order.ID, ChangeIn{ClientKey: tk(), Cart: cartOf(cart.Lines[0], cart.Lines[1], sodaLine(1))}); err != nil || len(ch.Added) != 1 {
		t.Fatalf("adding to it: %+v %v", ch, err)
	}
	cashOnline := arriveOnline(t, r, "pickup", false)
	cart = onlineCartOf(t, r, cashOnline.Order.ID)
	if ch, err := r.svc.ChangeCart(r.ctx, seat, cashOnline.Order.ID, ChangeIn{ClientKey: tk(), Cart: cartOf(cart.Lines[0])}); err != nil || len(ch.Removed) != 1 {
		t.Fatalf("a line off an unpaid online order: %+v %v", ch, err)
	}
}

func journalActions(t *testing.T, r *rig) map[string][]terminal.Entry {
	t.Helper()
	list, err := r.svc.Journal(r.ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]terminal.Entry{}
	for _, e := range list {
		out[e.Action] = append(out[e.Action], e)
	}
	return out
}

// The store wins: a seat never takes over what a store terminal holds; a
// store terminal takes over a seat's; two seats take over as terminals do.
func TestCallcenterTheStoreWins(t *testing.T) {
	r, seat := ccRig(t)
	seat2 := CallcenterActor("desk-3", "Elevate desk 3", agentID)
	r.login(t, till, "1234")
	var oe *terminal.OpenElsewhere

	// 1. The store holds it.
	if _, err := r.svc.PutDraft(r.ctx, till, "d-store-0001", draftIn(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PutDraft(r.ctx, seat, "d-store-0001", draftIn(false)); !errors.As(err, &oe) || oe.Holder.DeviceID != till.DeviceID || oe.TakeOverRefused {
		t.Fatalf("open on the store's terminal: %v", err)
	}
	take := draftIn(false)
	take.TakeOver = true
	taken := r.pub.count(terminal.EventDraftTaken)
	if _, err := r.svc.PutDraft(r.ctx, seat, "d-store-0001", take); !errors.As(err, &oe) || oe.Holder.DeviceID != till.DeviceID || !oe.TakeOverRefused {
		t.Fatalf("take_over from the call center must be refused the same way: %v", err)
	}
	if err := r.svc.DeleteDraft(r.ctx, seat, "d-store-0001"); !errors.As(err, &oe) {
		t.Fatalf("delete of the store's draft: %v", err)
	}
	ds, _ := r.svc.Drafts(r.ctx)
	if len(ds.Drafts) != 1 || ds.Drafts[0].Holder == nil || ds.Drafts[0].Holder.DeviceID != till.DeviceID || r.pub.count(terminal.EventDraftTaken) != taken {
		t.Fatalf("the store still holds it: %+v", ds.Drafts)
	}
	refused := journalActions(t, r)[terminal.ActionDraftTakeOverRefused]
	if len(refused) != 1 || refused[0].DeviceID != seat.DeviceID || refused[0].StaffID != agentID ||
		!strings.Contains(string(refused[0].Detail), `"holder_device_id":"dev-1"`) {
		t.Fatalf("journal draft.take_over_refused: %+v", refused)
	}

	// 2. A seat holds it: the store takes over; the seat is told, with its seat id.
	if _, err := r.svc.PutDraft(r.ctx, seat, "d-seat-00001", draftIn(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PutDraft(r.ctx, till, "d-seat-00001", draftIn(false)); !errors.As(err, &oe) || oe.Holder.DeviceID != seat.DeviceID {
		t.Fatalf("open on the seat: %v", err)
	}
	got, err := r.svc.PutDraft(r.ctx, till, "d-seat-00001", take)
	if err != nil || got.Draft.Holder == nil || got.Draft.Holder.DeviceID != till.DeviceID {
		t.Fatalf("the store takes over the call center's: %+v %v", got, err)
	}
	ev, _ := r.pub.last(terminal.EventDraftTaken).(map[string]any)
	if ev == nil || ev["device_id"] != seat.DeviceID || ev["id"] != "d-seat-00001" {
		t.Fatalf("terminal.draft.taken carries the seat id in device_id: %+v", ev)
	}

	// 3. Two seats: the usual take-over.
	if _, err := r.svc.PutDraft(r.ctx, seat, "d-seats-0001", draftIn(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PutDraft(r.ctx, seat2, "d-seats-0001", draftIn(false)); !errors.As(err, &oe) {
		t.Fatalf("open on another seat: %v", err)
	}
	if got, err := r.svc.PutDraft(r.ctx, seat2, "d-seats-0001", take); err != nil || got.Draft.Holder.DeviceID != seat2.DeviceID {
		t.Fatalf("a seat takes over another seat's: %+v %v", got, err)
	}

	// 4. An order open on a store terminal: the call center can't change or cancel it.
	res := ccPlace(t, r, seat, "pickup", nil, pizzaLine())
	id := res.Order.Order.ID
	edit := draftIn(false)
	edit.OrderID = id
	if _, err := r.svc.PutDraft(r.ctx, till, "d-edit-00001", edit); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine(), sodaLine(1))}); !errors.As(err, &oe) || oe.Holder.DeviceID != till.DeviceID {
		t.Fatalf("a change of an order the store has open: %v", err)
	}
	if _, err := r.svc.Void(r.ctx, seat, id, VoidIn{Reason: "customer called back"}); !errors.As(err, &oe) {
		t.Fatalf("a cancel of an order the store has open: %v", err)
	}
	// Once the store lets go (held), the call center goes on.
	edit.Held = true
	if _, err := r.svc.PutDraft(r.ctx, till, "d-edit-00001", edit); err != nil {
		t.Fatal(err)
	}
	if ch, err := r.svc.ChangeCart(r.ctx, seat, id, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine(), sodaLine(1))}); err != nil || !ch.Changed {
		t.Fatalf("after the store let go: %+v %v", ch, err)
	}
	// A void needs a reason for the kind (as for everyone), and is the everyday rule: unpaid only.
	if _, err := r.svc.Void(r.ctx, seat, id, VoidIn{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a void without a reason: %v", err)
	}
	if v, err := r.svc.Void(r.ctx, seat, id, VoidIn{Reason: "customer called back"}); err != nil || v.Order.Status.State != order.StateCancelled {
		t.Fatalf("cancel an unpaid order: %+v %v", v, err)
	}
	paid := ccPlace(t, r, seat, "pickup", []PaymentIn{{Tender: terminal.TenderManualCard, AmountCents: 500, Last4: "1881"}}, pizzaLine())
	var pf *terminal.PaidFirst
	if _, err := r.svc.Void(r.ctx, seat, paid.Order.Order.ID, VoidIn{Reason: "changed their mind"}); !errors.As(err, &pf) {
		t.Fatalf("cancel a paid order: %v, want refund_first", err)
	}
}

func TestCallcenterOrderIsTakenByTheAgent(t *testing.T) {
	r, seat := ccRig(t)
	r.svc.SetReports(fakeReports{r})
	res := ccPlace(t, r, seat, "delivery", []PaymentIn{{Tender: terminal.TenderManualCard, AmountCents: 300, Last4: "1881"}}, pizzaLine())
	so := res.Order
	if so.Order.TakenBy != "Call center - Elena V." || so.Order.DeviceKind != "callcenter" {
		t.Fatalf("taken_by %q device_kind %q", so.Order.TakenBy, so.Order.DeviceKind)
	}
	// Placed like a phone order: accepted at once with a number, tickets at once, and the Pi prints the customer's paper.
	if so.Status.State != order.StateAccepted || so.Status.KitchenNumber == nil || len(r.orders.printed) != 1 {
		t.Fatalf("accepted at once: %s, tickets %d", so.Status.State, len(r.orders.printed))
	}
	if got := receiptsOf(r.orders); len(got) != 1 || got[0].Order.ID != so.Order.ID || got[0].Order.TakenBy != so.Order.TakenBy {
		t.Fatalf("the Pi prints the receipt / delivery ticket: %d", len(got))
	}
	raw, _ := json.Marshal(so.Order)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["taken_by"] != "Call center - Elena V." || wire["device_kind"] != "callcenter" {
		t.Fatalf("on the order's JSON: %v %v", wire["taken_by"], wire["device_kind"])
	}
	tm, _ := so.Order.SourceMeta["terminal"].(map[string]any)
	if tm["device_id"] != device.CallcenterID || tm["seat_id"] != seat.DeviceID || tm["staff_id"] != agentID {
		t.Fatalf("source_metadata.terminal: %+v", tm)
	}
	// order.placed to main: device_kind + staff_id (the per-agent report), device_id = the store's call-center device.
	events, err := r.repo.ListUnsyncedEvents(r.ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	for _, e := range events {
		if e.Type == terminal.EventOrderPlaced {
			var p struct {
				Snapshot map[string]any `json:"snapshot"`
			}
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			snap = p.Snapshot
		}
	}
	if snap == nil || snap["device_kind"] != "callcenter" || snap["staff_id"] != agentID || snap["device_id"] != device.CallcenterID ||
		snap["seat_id"] != seat.DeviceID || snap["taken_by"] != "Call center - Elena V." {
		t.Fatalf("order.placed's snapshot: device_kind %v staff_id %v device_id %v seat_id %v taken_by %v",
			snap["device_kind"], snap["staff_id"], snap["device_id"], snap["seat_id"], snap["taken_by"])
	}
	// On the checks list; ?device_id=callcenter finds every seat's orders.
	r.login(t, till, "1234")
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: cartOf(pizzaLine())}); err != nil {
		t.Fatal(err)
	}
	all, err := r.svc.Checks(r.ctx, till, ChecksQuery{Batch: "current", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var mine, store int
	for _, c := range all.Checks {
		switch c.OrderID {
		case so.Order.ID:
			mine++
			if c.TakenBy != "Call center - Elena V." || c.StaffID != agentID || c.DeviceID != seat.DeviceID || c.DeviceName != device.CallcenterName {
				t.Fatalf("the check: taken_by %q staff %q device %q / %q", c.TakenBy, c.StaffID, c.DeviceID, c.DeviceName)
			}
		default:
			store++
			if c.TakenBy != "" {
				t.Fatalf("a store order has no taken_by: %q", c.TakenBy)
			}
		}
	}
	if mine != 1 || store != 1 {
		t.Fatalf("checks: %d call-center, %d store", mine, store)
	}
	only, err := r.svc.Checks(r.ctx, till, ChecksQuery{Batch: "current", Limit: 50, DeviceID: device.CallcenterID})
	if err != nil || len(only.Checks) != 1 || only.Checks[0].OrderID != so.Order.ID {
		t.Fatalf("?device_id=callcenter: %+v %v", only, err)
	}
	// A store terminal's order carries neither field.
	if list, _ := r.svc.Orders(r.ctx, false); len(list) != 2 {
		t.Fatalf("orders: %d", len(list))
	} else {
		for _, o := range list {
			if o.Order.ID != so.Order.ID && (o.Order.TakenBy != "" || o.Order.DeviceKind != "") {
				t.Fatalf("a store order: %q %q", o.Order.TakenBy, o.Order.DeviceKind)
			}
		}
	}
}

func TestTerminalReprint(t *testing.T) {
	r, seat := ccRig(t)
	if _, err := r.svc.Reprint(r.ctx, till, "0199c0de-0000-7000-8000-000000000001"); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no login: %v", err)
	}
	r.login(t, till, "1234")
	for _, a := range []Actor{till, seat} { // every terminal kind
		res, err := r.svc.Reprint(r.ctx, a, "0199c0de-0000-7000-8000-000000000001")
		if err != nil || !res.OK || res.PrintedOn != "Front" {
			t.Fatalf("%s: %+v %v", a.DeviceID, res, err)
		}
	}
	got := journalActions(t, r)[terminal.ActionReprint]
	if len(got) != 2 || !strings.Contains(string(got[0].Detail), `"number":7`) || got[0].DeviceID != seat.DeviceID || got[1].DeviceID != till.DeviceID {
		t.Fatalf("journal reprint {number}: %+v", got)
	}
	ccReprint.Store(r.orders, order.ErrNotFound)
	if _, err := r.svc.Reprint(r.ctx, till, "0199c0de-0000-7000-8000-000000000002"); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("an unknown order: %v", err)
	}
}

// The shared rule table's call-center rows (contract/money-rules.json
// "callcenter"), through the Pi's own service.
func TestMoneyRulesCallcenter(t *testing.T) {
	raw := readRules(t)
	var tab struct {
		People map[string]struct {
			Role string `json:"role"`
		} `json:"people"`
		Callcenter []struct {
			ID     string          `json:"id"`
			Note   string          `json:"note"`
			By     string          `json:"by"`
			Action string          `json:"action"`
			Pi     json.RawMessage `json:"pi"`
		} `json:"callcenter"`
	}
	if err := json.Unmarshal(raw, &tab); err != nil {
		t.Fatal(err)
	}
	if len(tab.Callcenter) == 0 {
		t.Fatal("the table has no callcenter rows")
	}
	answer := func(err error) string {
		var no *terminal.NotAllowedForCallcenter
		var pl *terminal.PaidLocked
		switch {
		case err == nil:
			return `"allow"`
		case errors.As(err, &no):
			return `{"refused":"not_allowed_for_callcenter","what":"` + no.What + `"}`
		case errors.As(err, &pl):
			return `{"refused":"paid_locked"}`
		}
		return "error: " + err.Error()
	}
	norm := func(raw json.RawMessage) string {
		var v any
		_ = json.Unmarshal(raw, &v)
		b, _ := json.Marshal(v)
		return string(b)
	}
	for _, c := range tab.Callcenter {
		if tab.People[c.By].Role != terminal.RoleCallcenter {
			t.Fatalf("%s: %s is not a call-center agent in the table", c.ID, c.By)
		}
		r, seat := ccRig(t)
		var err error
		switch c.Action {
		case "refund":
			res := ccPlace(t, r, seat, "pickup", []PaymentIn{{Tender: terminal.TenderManualCard, AmountCents: 500, Last4: "1881"}}, pizzaLine())
			_, err = r.svc.AddPayment(r.ctx, seat, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderManualCard, AmountCents: 100, Refund: true, Reason: "x"})
		case "general_refund", "pay_out", "close":
			route := map[string][2]string{"general_refund": {"POST", "refunds"}, "pay_out": {"POST", "drawer/pay-out"}, "close": {"POST", "close"}}[c.Action]
			if !CallcenterAllowed(route[0], route[1]) {
				err = notAllowed(terminal.NotAllowedRoute)
			}
		case "comp":
			res := ccPlace(t, r, seat, "pickup", nil, pizzaLine())
			_, err = r.svc.AddPayment(r.ctx, seat, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderComp, AmountCents: 100})
		case "comp_line":
			l := pizzaLine()
			l.PriceCents = cents(0)
			_, err = r.svc.PlaceOrder(r.ctx, seat, OrderIn{ClientKey: tk(), Type: "pickup", Cart: cartOf(l)})
		case "discount":
			_, err = r.svc.PlaceOrder(r.ctx, seat, OrderIn{ClientKey: tk(), Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}, Discount: &Discount{Pct: 1, Reason: "x"}}})
		case "employee_meal":
			_, err = r.svc.PlaceOrder(r.ctx, seat, OrderIn{ClientKey: tk(), Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}, Discount: &Discount{Pct: 1, Reason: "x", Kind: KindEmployeeMeal}}})
		case "price_drop", "price_raise":
			l := pizzaLine()
			l.PriceCents = cents(map[string]int64{"price_drop": 100, "price_raise": 99900}[c.Action])
			_, err = r.svc.PlaceOrder(r.ctx, seat, OrderIn{ClientKey: tk(), Type: "pickup", Cart: cartOf(l)})
		case "open_price":
			_, err = r.svc.PlaceOrder(r.ctx, seat, OrderIn{ClientKey: tk(), Type: "pickup",
				Cart: cartOf(pizzaLine(), pricing.Line{Custom: true, Name: "Extra", Qty: 1, Selection: pricing.Selection{PriceCents: cents(100)}})})
		case "cash_payment":
			res := ccPlace(t, r, seat, "pickup", nil, pizzaLine())
			_, err = r.svc.AddPayment(r.ctx, seat, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderCash, AmountCents: 100})
		case "typed_card_payment":
			res := ccPlace(t, r, seat, "pickup", nil, pizzaLine())
			_, err = r.svc.AddPayment(r.ctx, seat, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderManualCard, AmountCents: 100, Last4: "1881"})
		case "counter_order":
			_, err = r.svc.PlaceOrder(r.ctx, seat, OrderIn{ClientKey: tk(), Type: "counter", Cart: cartOf(pizzaLine())})
		case "manager_pin":
			_, err = r.svc.PlaceOrder(r.ctx, seat, OrderIn{ClientKey: tk(), Type: "pickup", Cart: cartOf(pizzaLine()), ManagerPIN: "9876"})
		case "void_unpaid":
			res := ccPlace(t, r, seat, "pickup", nil, pizzaLine())
			_, err = r.svc.Void(r.ctx, seat, res.Order.Order.ID, VoidIn{Reason: "customer called back"})
		case "add_to_paid", "remove_paid_line":
			res := ccPlace(t, r, seat, "pickup", []PaymentIn{{Tender: terminal.TenderManualCard, AmountCents: 500, Last4: "1881"}}, pizzaLine(), sodaLine(1))
			cart := cartOf(pizzaLine(), sodaLine(2))
			if c.Action == "remove_paid_line" {
				cart = cartOf(pizzaLine())
			}
			_, err = r.svc.ChangeCart(r.ctx, seat, res.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: cart})
		case "remove_unpaid_line":
			res := ccPlace(t, r, seat, "pickup", nil, pizzaLine(), sodaLine(1))
			_, err = r.svc.ChangeCart(r.ctx, seat, res.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: cartOf(pizzaLine())})
		default:
			t.Fatalf("%s: the Pi's runner doesn't know the action %q", c.ID, c.Action)
		}
		if got, want := answer(err), norm(c.Pi); got != want {
			t.Errorf("%s (%s): the Pi answers %s, the table says %s", c.ID, c.Note, got, want)
		}
	}
}

// The events forwarder: only while main wants them, batched, capped, dropped
// (never queued) when main is unreachable.
func TestCallcenterEventsForwarded(t *testing.T) {
	src := make(chan RelayEvent, 512)
	var subs, cancels int
	var mu sync.Mutex
	var reports [][]RelayEvent
	fail := false
	f := NewEventForwarder(func() (<-chan RelayEvent, func()) {
		mu.Lock()
		subs++
		mu.Unlock()
		return src, func() { mu.Lock(); cancels++; mu.Unlock() }
	}, func(_ context.Context, typ string, payload any) error {
		mu.Lock()
		defer mu.Unlock()
		if typ != MsgTerminalEvents {
			t.Errorf("report type %q", typ)
		}
		if fail {
			return errors.New("main is unreachable")
		}
		reports = append(reports, payload.(map[string]any)["events"].([]RelayEvent))
		return nil
	}, zerolog.Nop())
	f.every = 5 * time.Millisecond
	count := func() (n, total int) {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range reports {
			total += len(b)
		}
		return len(reports), total
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	if f.Wanted() {
		t.Fatal("nothing is forwarded until main asks")
	}
	// main wants them (seconds over the limit are cut to 300).
	if _, err := f.HandleWant(context.Background(), json.RawMessage(`{"seconds": 9000}`)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	left := time.Until(f.until)
	f.mu.Unlock()
	if left > EventsWantMaxSeconds*time.Second || left < (EventsWantMaxSeconds-5)*time.Second {
		t.Fatalf("wanted for %s, want <= 300 s", left)
	}
	for i := 0; i < 250; i++ {
		src <- RelayEvent{Event: "terminal.order.placed", Data: json.RawMessage(`{"n":1}`), At: time.Now()}
	}
	waitFor("250 events in reports of at most 100", func() bool { _, total := count(); return total == 250 })
	mu.Lock()
	for _, b := range reports {
		if len(b) > eventsBatchMax {
			t.Errorf("a report of %d events (max %d)", len(b), eventsBatchMax)
		}
	}
	if b, _ := json.Marshal(map[string]any{"events": reports[0][:1]}); !strings.Contains(string(b), `{"events":[{"event":"terminal.order.placed","data":{"n":1},"at":"`) {
		t.Errorf("wire shape: %s", b)
	}
	// main unreachable: what is waiting is dropped, never sent later.
	fail = true
	mu.Unlock()
	n0, _ := count()
	for i := 0; i < 30; i++ {
		src <- RelayEvent{Event: "terminal.order.paid", Data: json.RawMessage(`{}`), At: time.Now()}
	}
	waitFor("the tap drained", func() bool { return len(src) == 0 })
	time.Sleep(4 * f.every) //nolint: an absence check: a few batch ticks pass with main down
	mu.Lock()
	fail = false
	mu.Unlock()
	src <- RelayEvent{Event: "terminal.order.voided", Data: json.RawMessage(`{}`), At: time.Now()}
	waitFor("the next event after main is back", func() bool { n, _ := count(); return n > n0 })
	mu.Lock()
	for _, b := range reports[n0:] {
		for _, e := range b {
			if e.Event == "terminal.order.paid" {
				t.Errorf("an event from while main was down was sent later")
			}
		}
	}
	mu.Unlock()
	// main stops listening: the tap is dropped and nothing more is sent.
	if _, err := f.HandleWant(context.Background(), json.RawMessage(`{"seconds": 0}`)); err != nil {
		t.Fatal(err)
	}
	waitFor("the tap cancelled", func() bool { mu.Lock(); defer mu.Unlock(); return cancels == 1 })
	if f.Wanted() || subs != 1 {
		t.Fatalf("wanted %v, taps %d", f.Wanted(), subs)
	}
}

func readRules(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../domain/terminal/testdata/money-rules.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The call center has no drawer: a typed card from a seat (with the order or
// afterwards) never opens or joins a drawer session, and the day's Z still
// balances with its payments in the tenders only.
func TestCallcenterPaymentOpensNoDrawer(t *testing.T) {
	r, seat := ccRig(t)
	r.svc.SetReports(fakeReports{r})
	with := ccPlace(t, r, seat, "pickup", []PaymentIn{{Tender: terminal.TenderManualCard, AmountCents: 500, TipCents: 100, Last4: "1881"}}, pizzaLine())
	later := ccPlace(t, r, seat, "delivery", nil, pizzaLine())
	total := later.Order.Order.Money.Total
	paid, err := r.svc.AddPayment(r.ctx, seat, later.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderManualCard, AmountCents: total, TipCents: 300, Last4: "1881"})
	if err != nil || paid.Payment.DrawerID != "" {
		t.Fatalf("a seat's payment has no drawer: %+v %v", paid, err)
	}
	if ds, err := r.svc.Drawers(r.ctx, 0); err != nil || len(ds) != 0 {
		t.Fatalf("no drawer session for the call center: %+v %v", ds, err)
	}
	for _, id := range []string{with.Order.Order.ID, later.Order.Order.ID} {
		pays, _ := r.repo.OrderPayments(r.ctx, id)
		for _, p := range pays {
			if p.DrawerID != "" || p.DeviceID != seat.DeviceID {
				t.Fatalf("payment %+v", p)
			}
		}
	}
	// The store's own terminal still gets its drawer at its first payment.
	r.login(t, till, "1234")
	cash, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: cartOf(sodaLine(1))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.AddPayment(r.ctx, till, cash.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: terminal.TenderCash, AmountCents: cash.Order.Order.Money.Total}); err != nil {
		t.Fatal(err)
	}
	ds, err := r.svc.Drawers(r.ctx, 0)
	if err != nil || len(ds) != 1 || ds[0].DeviceID != till.DeviceID {
		t.Fatalf("the till's drawer only: %+v %v", ds, err)
	}
	// Close the day: one drawer (the till's), the call center's cards in the tenders, the identity holds.
	closed, _, err := r.repo.CloseBatch(order.WithCloseKind(r.ctx, order.CloseAuto), true)
	if err != nil {
		t.Fatal(err)
	}
	r.svc.AfterBatchClose(r.ctx, *closed)
	z, err := r.svc.ZReport(r.ctx, closed.ID)
	if err != nil || z == nil {
		t.Fatalf("z: %v", err)
	}
	var typed int64
	for _, tn := range z.Tenders {
		if tn.Tender == terminal.TenderManualCard {
			typed = tn.CollectedCents
		}
	}
	if len(z.Drawers) != 1 || z.Drawers[0].ID != ds[0].ID || z.Flagged || z.Reconcile.DifferenceCents != 0 ||
		z.Reconcile.DrawersDifferences != 0 || typed == 0 {
		t.Fatalf("z: drawers %d, flagged %v, difference %d, drawers difference %d, typed cards %d\n%+v", len(z.Drawers), z.Flagged,
			z.Reconcile.DifferenceCents, z.Reconcile.DrawersDifferences, typed, z.Reconcile)
	}
}
