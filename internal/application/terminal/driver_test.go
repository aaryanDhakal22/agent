package terminal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	orderApp "quiccpos/agent/internal/application/order"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
)

// The driver view (contract/agent.md §2.10b) against the real floor service
// and a temp SQLite DB. Counter deliveries are placed by the cashier on one
// terminal; drivers log in on others.

type driverRig struct {
	*rig
	floor *orderApp.Service
}

var (
	till2 = Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	till3 = Actor{DeviceID: "dev-3", DeviceName: "Till 3"}
)

func newDriverRig(t *testing.T) *driverRig {
	t.Helper()
	r := newRig(t)
	r.main.set(PathTerminalConfig, staffConfig(t, 2,
		terminal.Staff{ID: "drv-1", FirstName: "Ali", LastName: "Khan", Role: terminal.RoleDriver, PinHash: hash(t, "77")},
		terminal.Staff{ID: "drv-2", FirstName: "Sam", Role: terminal.RoleDriver, PinHash: hash(t, "88")}))
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	meters, _ := observability.NewMeters()
	floor := orderApp.NewService(r.repo, nil, nil, nil, ssebroker.New(), zerolog.Nop(), meters)
	p := order.DefaultDeliveryPrefs
	p.AutoAssign = false
	if err := r.repo.SetDeliveryPrefs(r.ctx, p); err != nil {
		t.Fatal(err)
	}
	r.svc.SetFloor(floor, r.repo)
	return &driverRig{rig: r, floor: floor}
}

// delivery places a counter delivery (cash due unless paid) and, ready,
// marks it ready.
func (r *driverRig) delivery(t *testing.T, key string, paid, isReady bool) string {
	t.Helper()
	r.login(t, till, "1234")
	in := OrderIn{ClientKey: key, Type: "delivery", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Customer: &Customer{Name: "Erik Davis", Phone: "4105550101", Address: &Address{Line1: "12 Main St, Apt 3", City: "Towson", Notes: "ring twice"}}}
	if paid {
		q, err := r.svc.Quote(r.ctx, till, QuoteIn{Type: "delivery", Cart: in.Cart})
		if err != nil {
			t.Fatal(err)
		}
		in.Payments = []PaymentIn{{Tender: "square_app", AmountCents: q.Quote.(pricing.Quote).TotalCents}}
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	if isReady {
		if _, err := r.floor.Ready(r.ctx, res.Order.Order.ID); err != nil {
			t.Fatal(err)
		}
	}
	return res.Order.Order.ID
}

func TestDriverLinkAtLogin(t *testing.T) {
	r := newDriverRig(t)
	// A roster driver with his name exists: he is linked to it.
	existing, err := r.floor.CreateDriver(r.ctx, "Ali Khan")
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.Login(r.ctx, till2, "77")
	if err != nil || res.Driver == nil || res.Driver.ID != existing.ID {
		t.Fatalf("login links the roster driver: %+v %v", res, err)
	}
	// Sam has none: created as "Sam".
	res, err = r.svc.Login(r.ctx, till3, "88")
	if err != nil || res.Driver == nil || res.Driver.Name != "Sam" || res.Driver.Status != order.DriverOff {
		t.Fatalf("login creates a driver: %+v %v", res, err)
	}
	// Linked by staff id: a second login (even after a rename) is the same driver.
	if err := r.floor.RenameDriver(r.ctx, res.Driver.ID, "Sammy"); err != nil {
		t.Fatal(err)
	}
	again, _ := r.svc.Login(r.ctx, till3, "88")
	if again.Driver.ID != res.Driver.ID {
		t.Fatalf("relinked: %+v", again.Driver)
	}
	// A cashier gets 403 on the driver routes; no login is 401.
	r.login(t, till, "1234")
	if _, err := r.svc.Available(r.ctx, till); !errors.Is(err, terminal.ErrDriverOnly) {
		t.Fatalf("cashier: %v", err)
	}
	if _, err := r.svc.Mine(r.ctx, Actor{DeviceID: "nobody"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no login: %v", err)
	}
	if r.pub.count(EventDriverLinked) != 2 {
		t.Fatalf("linked events: %v", r.pub.events)
	}
}

// later moves the service's clock (the claim rule's clock is injectable).
func (r *driverRig) later(d time.Duration) {
	r.svc.now = func() time.Time { return time.Now().Add(d) }
}

func TestDriverClaimMineTipCloseReport(t *testing.T) {
	r := newDriverRig(t)
	cooking := r.delivery(t, "k-cooking-1", false, false)
	cash := r.delivery(t, "k-cash-0001", false, true)
	paid := r.delivery(t, "k-paid-0001", true, true)
	res, err := r.svc.Login(r.ctx, till2, "77")
	if err != nil {
		t.Fatal(err)
	}
	me := res.Driver.ID
	// Nothing is past its promised time (SEND + 15 minutes here) yet.
	if list, err := r.svc.Available(r.ctx, till2); err != nil || len(list) != 0 {
		t.Fatalf("available before 45 min: %+v %v", list, err)
	}
	var tk *order.ErrTaken
	if _, err := r.svc.Claim(r.ctx, till2, ClaimIn{OrderIDs: []string{cash}}); !errors.As(err, &tk) {
		t.Fatalf("claim before the promised time: %v", err)
	}
	// 46 minutes later: every unassigned one, cooking too, longest past its promised time first.
	r.later(46 * time.Minute)
	list, err := r.svc.Available(r.ctx, till2)
	if err != nil || len(list) != 3 || list[0].OrderID != cooking || list[1].OrderID != cash || list[2].OrderID != paid {
		t.Fatalf("available after 45 min: %+v %v", list, err)
	}
	if list[0].State != string(order.StateAccepted) || list[1].State != string(order.StateReady) || list[0].WaitingMinutes < 45 ||
		list[1].Payment != PayCashDue || list[1].CashDueCents == 0 || list[1].Street != "12 Main St" || list[2].Payment != PayPaid {
		t.Fatalf("entries: %+v", list)
	}
	// A manager sent "paid" out with the other driver meanwhile: it is taken, all or nothing.
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
	if _, err := r.svc.Claim(r.ctx, till2, ClaimIn{OrderIDs: []string{cash, paid}}); !errors.As(err, &tk) || len(tk.IDs) != 1 || tk.IDs[0] != paid {
		t.Fatalf("taken: %v", err)
	}
	// Claim = his and delivered; he never goes out (he stays off).
	cl, err := r.svc.Claim(r.ctx, till2, ClaimIn{OrderIDs: []string{cash, cooking}})
	if err != nil || len(cl.Orders) != 2 || cl.Driver.Status != order.DriverOff {
		t.Fatalf("claim: %+v %v", cl, err)
	}
	for _, so := range cl.Orders {
		if so.Status.State != order.StateDelivered || so.Status.DriverID != me || so.Status.OutAt != nil {
			t.Fatalf("claimed: %+v", so.Status)
		}
	}
	if list, _ := r.svc.Available(r.ctx, till3); len(list) != 0 {
		t.Fatalf("gone from the list: %+v", list)
	}
	if _, err := r.svc.Claim(r.ctx, till3, ClaimIn{OrderIDs: []string{cash}}); !errors.As(err, &tk) {
		t.Fatalf("second driver: %v", err)
	}
	mine, err := r.svc.Mine(r.ctx, till2)
	if err != nil || len(mine.Out) != 0 || len(mine.Delivered) != 2 || mine.Delivered[0].TipKind != "door" || mine.Delivered[0].Phone != "4105550101" {
		t.Fatalf("mine: %+v %v", mine, err)
	}
	// Tip: a counter order's is a door tip on his report.
	tip := int64(400)
	tr, err := r.svc.Tip(r.ctx, till2, cash, TipIn{TipCents: &tip})
	if err != nil || tr.Kind != "door" || tr.Ticket.DoorTipCents != 400 {
		t.Fatalf("tip: %+v %v", tr, err)
	}
	// Another driver can't touch his orders; a claimed order is closed already.
	if _, err := r.svc.CloseOrder(r.ctx, till3, cash); !errors.Is(err, terminal.ErrNotYourOrder) {
		t.Fatalf("not his: %v", err)
	}
	if _, err := r.svc.CloseOrder(r.ctx, till2, cash); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("close a claimed one: %v", err)
	}
	// Close still works for an order he was sent out with: Sam's last stop returns him.
	c, err := r.svc.CloseOrder(r.ctx, till3, paid)
	if err != nil || !c.Returned || c.Driver.Status != order.DriverAvailable || c.Ticket.State != string(order.StateDelivered) {
		t.Fatalf("close: %+v %v", c, err)
	}
	rep, err := r.svc.Report(r.ctx, till2)
	if err != nil || rep.Deliveries != 2 || rep.DoorTipCents != 400 || rep.CashCents == 0 {
		t.Fatalf("report: %+v %v", rep, err)
	}
	if _, err := r.svc.PrintReport(r.ctx, till2); !errors.Is(err, orderApp.ErrPrintingUnavailable) {
		t.Fatalf("no printer wired: %v", err)
	}
	// Journal: link x2, claim, tip, close.
	j, _ := r.svc.Journal(r.ctx, 100)
	seen := map[string]int{}
	for _, e := range j {
		seen[e.Action]++
	}
	if seen[terminal.ActionDriverClaim] != 1 || seen[terminal.ActionDriverTip] != 1 || seen[terminal.ActionDriverClose] != 1 || seen[terminal.ActionDriverLinked] != 2 {
		t.Fatalf("journal: %v", seen)
	}
	if r.pub.count(EventDriverClaimed) != 1 || r.pub.count(EventDriverClosed) != 1 || r.pub.count(EventDriverTip) != 1 {
		t.Fatalf("events: %v", r.pub.events)
	}
}

// The claim rule (the owner, 2026-11-12): past the order's promised time on
// the Pi, not a fixed wait; main's old driver_claim_after_minutes is ignored.
func TestDriverClaimAtPromisedTime(t *testing.T) {
	r := newDriverRig(t)
	r.login(t, till, "1234")
	thirty := int64(30)
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-prom-0001", Type: "delivery", PrepMinutes: &thirty,
		Cart:     Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Customer: &Customer{Name: "Erik Davis", Phone: "4105550101", Address: &Address{Line1: "12 Main St", City: "Towson"}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	cfg := staffConfig(t, 3, terminal.Staff{ID: "drv-1", FirstName: "Ali", LastName: "Khan", Role: terminal.RoleDriver, PinHash: hash(t, "77")})
	cfg["settings"].(map[string]any)["driver_claim_after_minutes"] = 10 // retired: no effect
	r.main.set(PathTerminalConfig, cfg)
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Login(r.ctx, till2, "77"); err != nil {
		t.Fatal(err)
	}
	r.later(29 * time.Minute)
	if list, _ := r.svc.Available(r.ctx, till2); len(list) != 0 {
		t.Fatalf("29 of 30 minutes: %+v", list)
	}
	var tk *order.ErrTaken
	if _, err := r.svc.Claim(r.ctx, till2, ClaimIn{OrderIDs: []string{id}}); !errors.As(err, &tk) {
		t.Fatalf("claim before the promised time: %v", err)
	}
	r.later(32 * time.Minute)
	list, _ := r.svc.Available(r.ctx, till2)
	if len(list) != 1 || list[0].OrderID != id || list[0].LateMinutes < 1 || list[0].WaitingMinutes < 31 ||
		!list[0].PromisedAt.Equal(res.Order.Status.EstimatedReadyAt.UTC()) {
		t.Fatalf("past the promised time: %+v", list)
	}
	if c, err := r.svc.Claim(r.ctx, till2, ClaimIn{OrderIDs: []string{id}}); err != nil || len(c.Orders) != 1 {
		t.Fatalf("claim: %+v %v", c, err)
	}
	// An order with no promised time counts the Pi's default delivery minutes (15 here).
	so := order.StoredOrder{}
	so.Order.Fulfillment.Type = "delivery"
	at := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	so.Status.AcceptedAt = &at
	if got := order.PromisedAt(so, 15*time.Minute); !got.Equal(at.Add(15 * time.Minute)) {
		t.Fatalf("fallback: %v", got)
	}
	later := at.Add(time.Hour)
	so.Order.Fulfillment.PromisedAt = &later // a customer's later promised time wins
	est := at.Add(20 * time.Minute)
	so.Status.EstimatedReadyAt = &est
	if got := order.PromisedAt(so, 15*time.Minute); !got.Equal(later) {
		t.Fatalf("promised_at: %v", got)
	}
}

// Two drivers race for the same order: exactly one gets it, the other 409s.
func TestDriverClaimRace(t *testing.T) {
	r := newDriverRig(t)
	id := r.delivery(t, "k-race-0001", false, true)
	r.later(time.Hour)
	for _, a := range []struct {
		actor Actor
		pin   string
	}{{till2, "77"}, {till3, "88"}} {
		if _, err := r.svc.Login(r.ctx, a.actor, a.pin); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, a := range []Actor{till2, till3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.svc.Claim(context.Background(), a, ClaimIn{OrderIDs: []string{id}})
		}()
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range errs {
		var tk *order.ErrTaken
		switch {
		case err == nil:
			won++
		case errors.As(err, &tk):
			lost++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("won %d lost %d", won, lost)
	}
}

func TestAutoCloseSettingFromConfig(t *testing.T) {
	var nilCfg *terminal.Config
	if nilCfg.AutoCloseAt() != "" {
		t.Fatal("no config = off")
	}
	for body, want := range map[string]string{
		`{"version":1,"staff":[],"settings":{}}`:                              terminal.DefaultAutoCloseAt,
		`{"version":1,"staff":[],"settings":{"auto_close_batch_at":""}}`:      "",
		`{"version":1,"staff":[],"settings":{"auto_close_batch_at":"03:30"}}`: "03:30",
	} {
		c, err := terminal.ParseConfig([]byte(body))
		if err != nil || c.AutoCloseAt() != want {
			t.Fatalf("%s: %q %v", body, c.AutoCloseAt(), err)
		}
	}
}

var _ DriverLinks = (*repositories.OrderRepository)(nil)
