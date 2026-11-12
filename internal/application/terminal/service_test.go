package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/infra/database/repositories"
	"quiccpos/agent/internal/migrate"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// The terminal flow against a real (temp) SQLite DB, a fake main and fake
// floor hooks; the menu is the shared pricing case 33 (half-specialties).

const (
	cheese    = "00000000-0000-0000-0000-0000000000a1"
	hawaiian  = "00000000-0000-0000-0000-0000000000a2"
	soda      = "00000000-0000-0000-0000-0000000000a6"
	toppings  = "00000000-0000-0000-0000-0000000000c1"
	pepperoni = "00000000-0000-0000-0000-0000000000d1"
	xl        = "00000000-0000-0000-0000-0000000000b2"
	pizzaPrn  = "11111111-1111-1111-1111-111111111111"
)

type fakeOrders struct {
	mu sync.Mutex
	// crashBeforePrint: the Pi "restarts" before the print finishes (M-11).
	crashBeforePrint bool
	printed          []order.StoredOrder
	voids            []order.StoredOrder
	addOns           [][]order.LineItem
	voidLns          [][]order.LineItem
	// typeFrom: each change ticket's TypeChangedFrom ("" = the type stayed).
	typeFrom []string
	changed  int
	journal  int
	// deliveryUpdates: the orders whose delivery copies were printed again
	// after a payment changed (PrintDeliveryUpdate).
	deliveryUpdates []string
}

func (f *fakeOrders) PrintDeliveryUpdate(_ context.Context, so order.StoredOrder) {
	f.mu.Lock()
	f.deliveryUpdates = append(f.deliveryUpdates, so.Order.ID)
	f.mu.Unlock()
}

func (f *fakeOrders) PrintCounterTickets(_ context.Context, so order.StoredOrder) {
	f.mu.Lock()
	f.printed = append(f.printed, so)
	f.mu.Unlock()
}
func (f *fakeOrders) PrintCounterTicketsThen(ctx context.Context, so order.StoredOrder, done func(context.Context)) {
	f.PrintCounterTickets(ctx, so)
	if done != nil && !f.crashBeforePrint {
		done(ctx)
	}
}
func (f *fakeOrders) PrintVoidSlip(_ context.Context, so order.StoredOrder) {
	f.mu.Lock()
	f.voids = append(f.voids, so)
	f.mu.Unlock()
}
func (f *fakeOrders) PrintChange(_ context.Context, so order.StoredOrder, added, removed []order.LineItem) {
	f.mu.Lock()
	f.typeFrom = append(f.typeFrom, so.Order.TypeChangedFrom)
	if len(added) > 0 {
		f.addOns = append(f.addOns, added)
	}
	if len(removed) > 0 {
		f.voidLns = append(f.voidLns, removed)
	}
	f.mu.Unlock()
}
func (f *fakeOrders) CounterOrderChanged(context.Context, order.StoredOrder, *order.StoredOrder, bool) {
	f.mu.Lock()
	f.changed++
	f.mu.Unlock()
}
func (f *fakeOrders) JournalWritten() {
	f.mu.Lock()
	f.journal++
	f.mu.Unlock()
}
func (f *fakeOrders) PrepMinutesFor(string) int64   { return 15 }
func (f *fakeOrders) StoreIDs() (string, string)    { return "tenant", "location" }
func (f *fakeOrders) StoreLocation() *time.Location { return time.UTC }
func (f *fakeOrders) DevEvent(string)               {}

type fakePub struct {
	mu       sync.Mutex
	events   []string
	payloads []any
}

func (p *fakePub) Publish(typ string, v any) {
	p.mu.Lock()
	p.events = append(p.events, typ)
	p.payloads = append(p.payloads, v)
	p.mu.Unlock()
}

// last is the newest payload published as typ (nil when none).
func (p *fakePub) last(typ string) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.events) - 1; i >= 0; i-- {
		if p.events[i] == typ {
			return p.payloads[i]
		}
	}
	return nil
}

func (p *fakePub) count(typ string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.events {
		if e == typ {
			n++
		}
	}
	return n
}

type fakeMain struct {
	mu     sync.Mutex
	bodies map[string][]byte
	status map[string]int
}

func (m *fakeMain) fetch(_ context.Context, path string) (int, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.status[path]; ok {
		return st, nil, nil
	}
	if b, ok := m.bodies[path]; ok {
		return 200, b, nil
	}
	return 404, nil, nil
}

func (m *fakeMain) set(path string, v any) {
	b, _ := json.Marshal(v)
	m.mu.Lock()
	m.bodies[path] = b
	m.mu.Unlock()
}

// hashes: one bcrypt hash per PIN, as main keeps a staff member's pin_hash
// until the PIN changes (a new hash ends their session, §2.10u).
var hashes sync.Map

func hash(t *testing.T, pin string) string {
	if h, ok := hashes.Load(pin); ok {
		return h.(string)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := hashes.LoadOrStore(pin, string(h))
	return got.(string)
}

func caseContext(t *testing.T) pricing.Context {
	raw, err := os.ReadFile("../../domain/pricing/testdata/cases/33_one_specialty_on_a_half.json")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Context pricing.Context `json:"context"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.Context
}

type rig struct {
	svc    *Service
	repo   *repositories.OrderRepository
	main   *fakeMain
	orders *fakeOrders
	pub    *fakePub
	ctx    context.Context
}

func staffConfig(t *testing.T, version int64, extra ...terminal.Staff) map[string]any {
	staff := []terminal.Staff{
		{ID: "cash-1", FirstName: "Cara", LastName: "Smith", Role: terminal.RoleCashier, PinHash: hash(t, "1234")},
		{ID: "mgr-1", FirstName: "Khawar", LastName: "Ali", Role: terminal.RoleManager, PinHash: hash(t, "9876")},
		{ID: "cash-2", FirstName: "Ben", Role: terminal.RoleCashier, PinHash: hash(t, "55"), Limits: &terminal.Limits{RefundCents: 5000, DiscountPct: 50}},
	}
	staff = append(staff, extra...)
	return map[string]any{"version": version, "staff": staff, "settings": map[string]any{"counter_start": 300, "auto_lock_minutes": 3,
		"role_limits": map[string]any{"cashier": map[string]any{"refund_cents": 1000, "discount_pct": 10}}}}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, path); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repo := repositories.NewOrderRepository(db, zerolog.Nop())
	fm := &fakeMain{bodies: map[string][]byte{}, status: map[string]int{}}
	pctx := caseContext(t)
	fm.set(PathCounterMenu, map[string]any{"menu_version": 12, "build": map[string]any{"view": map[string]any{"kind": "counter"}},
		"pricing": pctx, "routes": map[string]any{cheese: map[string]any{"printer_id": pizzaPrn, "name": "Pizza"}, hawaiian: map[string]any{"printer_id": pizzaPrn}}})
	fm.set(PathTerminalConfig, staffConfig(t, 1))
	pub, orders := &fakePub{}, &fakeOrders{}
	store := NewStore(repo, fm.fetch, pub, zerolog.Nop())
	svc := NewService(store, repo, orders, pub, zerolog.Nop())
	for _, k := range []string{terminal.KindCounterMenu, terminal.KindTerminalConfig} {
		if changed, err := store.Refresh(ctx, k); err != nil || !changed {
			t.Fatalf("refresh %s: %v %v", k, changed, err)
		}
	}
	return &rig{svc: svc, repo: repo, main: fm, orders: orders, pub: pub, ctx: ctx}
}

var till = Actor{DeviceID: "dev-1", DeviceName: "Till 1"}

func (r *rig) login(t *testing.T, a Actor, pin string) {
	t.Helper()
	if _, err := r.svc.Login(r.ctx, a, pin); err != nil {
		t.Fatalf("login %s: %v", pin, err)
	}
}

func pizzaLine() pricing.Line {
	return pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: cheese, SizeID: xl,
		Halves:  []pricing.Half{{ItemID: hawaiian, Half: 2}},
		Options: []pricing.Pick{{GroupID: toppings, OptionID: pepperoni, Portion: "first"}}}}
}

func TestStoreRefreshLoadAndCommands(t *testing.T) {
	r := newRig(t)
	if r.pub.count(EventMenuChanged) != 1 || r.pub.count(EventConfigChanged) != 1 {
		t.Fatalf("events %v", r.pub.events)
	}
	// The same body again: nothing changes, nothing announced.
	if changed, err := r.svc.store.Refresh(r.ctx, terminal.KindCounterMenu); err != nil || changed {
		t.Fatalf("same menu: %v %v", changed, err)
	}
	// A new Pi process reads the saved ones (offline).
	st := NewStore(r.repo, func(context.Context, string) (int, []byte, error) { return 0, nil, errors.New("offline") }, nil, zerolog.Nop())
	st.Load(r.ctx)
	if m, s := st.Menu(); m == nil || s.Version != 12 {
		t.Fatalf("loaded menu %v", s)
	}
	if c, _ := st.Config(); c == nil || c.CounterStart() != 300 {
		t.Fatal("loaded config")
	}
	// main without the route.
	r.main.status[PathTerminalConfig] = 404
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); !errors.Is(err, ErrMainUnsupported) {
		t.Fatalf("404: %v", err)
	}
	// A menu.changed for the version in use is a no-op; another kicks.
	if v, _ := r.svc.store.HandleMenuChanged(r.ctx, json.RawMessage(`{"version":12}`)); v != nil || len(r.svc.store.kick[terminal.KindCounterMenu]) != 0 {
		t.Fatalf("current version: no reply, no kick: %v", v)
	}
	if _, err := r.svc.store.HandleMenuChanged(r.ctx, json.RawMessage(`{"version":13}`)); err != nil || len(r.svc.store.kick[terminal.KindCounterMenu]) != 1 {
		t.Fatal("menu.changed must kick a refetch")
	}
}

func TestLoginThrottleAndSessions(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.Login(r.ctx, till, "12a"); !errors.Is(err, terminal.ErrBadPIN) {
		t.Fatalf("bad pin: %v", err)
	}
	res, err := r.svc.Login(r.ctx, till, "1234")
	if err != nil || res.Staff.ID != "cash-1" || res.Role != terminal.RoleCashier || res.Limits.DiscountPct != 10 || res.Staff.Name != "Cara S." {
		t.Fatalf("login: %+v %v", res, err)
	}
	if v := r.svc.Session(till); v.Staff == nil || v.Staff.ID != "cash-1" || v.MenuVersion != 12 || v.ConfigVersion != 1 {
		t.Fatalf("session %+v", v)
	}
	if res, _ := r.svc.Login(r.ctx, Actor{DeviceID: "dev-2"}, "9876"); res == nil || !res.Limits.Unlimited {
		t.Fatal("a manager is unlimited")
	}
	// Five wrong PINs, then a wait (also for the right one).
	other := Actor{DeviceID: "dev-3"}
	for i := 0; i < pinFreeMisses; i++ {
		if _, err := r.svc.Login(r.ctx, other, "0000"); !errors.Is(err, terminal.ErrWrongPIN) {
			t.Fatalf("miss %d: %v", i, err)
		}
	}
	var thr *terminal.Throttled
	if _, err := r.svc.Login(r.ctx, other, "1234"); !errors.As(err, &thr) || thr.Wait <= 0 {
		t.Fatalf("throttled: %v", err)
	}
	// Staff removed in main: their session ends.
	r.main.set(PathTerminalConfig, map[string]any{"version": 2, "staff": []terminal.Staff{{ID: "mgr-1", Role: terminal.RoleManager, PinHash: hash(t, "9876")}}})
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-removed-1"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("removed staff: %v", err)
	}
	r.svc.Logout(r.ctx, Actor{DeviceID: "dev-2"})
	journal, _ := r.repo.Journal(r.ctx, 50)
	counts := map[string]int{}
	for _, e := range journal {
		counts[e.Action]++
	}
	if counts[terminal.ActionLogin] != 2 || counts[terminal.ActionLoginFailed] != 5 || counts[terminal.ActionLogout] != 1 {
		t.Fatalf("journal: %v", counts)
	}
}

func TestQuoteAndPlaceOrder(t *testing.T) {
	r := newRig(t)
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), {Qty: 2, Selection: pricing.Selection{ItemID: soda}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	quote := q.Quote.(pricing.Quote)
	if len(quote.Errors) != 0 || quote.Lines[0].UnitCents != 2224 || quote.SubtotalCents != 2224+398 || q.MenuVersion != 12 {
		t.Fatalf("quote %+v", quote)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-nologin-1"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no login: %v", err)
	}
	r.login(t, till, "1234")
	in := OrderIn{ClientKey: "k-order-0001", Type: "counter", Note: "extra napkins",
		Cart:     Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), {Qty: 2, Selection: pricing.Selection{ItemID: soda}}}}},
		Payments: []PaymentIn{{Tender: "cash", AmountCents: quote.TotalCents, TenderedCents: 4000}}}
	res, err := r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	so := res.Order
	if res.Number != 300 || res.Duplicate || res.ChangeCents != 4000-quote.TotalCents || so.Status.State != order.StateAccepted {
		t.Fatalf("placed: %+v %+v", res, so.Status)
	}
	if so.Order.Source != order.SourceCounter || so.Order.Fulfillment.Type != "dine_in" || !so.Status.Paid || so.Status.CashDueCents != 0 {
		t.Fatalf("snapshot: %+v / %+v", so.Order, so.Status)
	}
	pizza := so.Order.Items[0]
	if pizza.Name != "Cheese Pizza" || pizza.PrinterID != pizzaPrn || pizza.Routing != order.RoutingMatched || !pizza.HasHalves() {
		t.Fatalf("pizza line: %+v", pizza)
	}
	if so.Order.Items[1].Routing != order.RoutingUnassigned || so.Order.Items[1].MenuItemID != soda || so.Order.Money.Total != quote.TotalCents {
		t.Fatalf("soda line / money: %+v %+v", so.Order.Items[1], so.Order.Money)
	}
	var spec, pep bool
	for _, c := range pizza.Children {
		spec = spec || (c.Mode == order.ModeSpecialty && c.Portion == order.PortionSecond)
		pep = pep || (c.Name == "Pepperoni (1st half)" && c.Portion == order.PortionFirst)
	}
	if !spec || !pep {
		t.Fatalf("halves: %+v", pizza.Children)
	}
	if len(r.orders.printed) != 1 || r.pub.count(EventOrderPlaced) != 1 {
		t.Fatal("printed + announced once")
	}
	// The same key: the same order, nothing printed again.
	again, err := r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil || !again.Duplicate || again.Order.Order.ID != so.Order.ID || len(r.orders.printed) != 1 {
		t.Fatalf("idempotent: %+v %v", again, err)
	}
	// Overpaid.
	in.ClientKey, in.Payments = "k-order-0002", []PaymentIn{{Tender: "card", AmountCents: quote.TotalCents + 1}}
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.Is(err, terminal.ErrOverBalance) {
		t.Fatalf("overpaid: %v", err)
	}
	// An unknown item.
	in.Payments, in.Cart = nil, Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: 1, Selection: pricing.Selection{ItemID: "nope"}}}}}
	var ci *terminal.CartInvalid
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.As(err, &ci) {
		t.Fatalf("cart invalid: %v", err)
	}
	// A delivery needs an address.
	in.Type, in.Cart = "delivery", Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("delivery address: %v", err)
	}
	in.Customer = &Customer{Name: "Pat", Phone: "4105550100", Address: &Address{Line1: "1 Main St", City: "Towson"}}
	d, err := r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil || d.Number != 301 || d.Order.Order.Fulfillment.Address == nil || d.Order.Status.CashDueCents == 0 {
		t.Fatalf("delivery: %+v %v", d, err)
	}
	open, _ := r.svc.Orders(r.ctx, true)
	if len(open) != 2 { // the paid counter order is not picked up yet, the delivery is unpaid
		t.Fatalf("open: %d", len(open))
	}
}

func TestDiscountLimitNeedsManager(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // cashier: discount up to 10 %
	cart := Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}, Discount: &Discount{Pct: 20, Reason: "regular"}}
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: cart})
	if err != nil || !q.NeedsManager || q.DiscountPct != 20 {
		t.Fatalf("quote: %+v %v", q, err)
	}
	quote := q.Quote.(pricing.Quote)
	if d := quote.Discounts[len(quote.Discounts)-1]; d.Type != DiscountManual || d.AmountCents != pricing.Pct(2224, 2000) {
		t.Fatalf("manual discount %+v", quote.Discounts)
	}
	in := OrderIn{ClientKey: "k-disc-0001", Cart: cart}
	var mr *terminal.ManagerRequired
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.As(err, &mr) || mr.Limit != 10 || mr.Requested != 20 {
		t.Fatalf("over the limit: %v", err)
	}
	in.ManagerPIN = "55" // a cashier's PIN is no manager's
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); !errors.Is(err, terminal.ErrManagerPINWrong) {
		t.Fatalf("not a manager: %v", err)
	}
	in.ManagerPIN = "9876"
	res, err := r.svc.PlaceOrder(r.ctx, till, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Order.Order.Money.Total != quote.TotalCents {
		t.Fatalf("total %d want %d", res.Order.Order.Money.Total, quote.TotalCents)
	}
	journal, _ := r.repo.Journal(r.ctx, 20)
	var override, discount bool
	for _, e := range journal {
		override = override || (e.Action == terminal.ActionOverride && string(e.Detail) != "{}")
		discount = discount || e.Action == terminal.ActionDiscount
	}
	if !override || !discount {
		t.Fatalf("journal %+v", journal)
	}
	// Within the limit: no manager.
	r.login(t, till, "55") // Ben: his own 50 %
	in.ClientKey, in.ManagerPIN = "k-disc-0002", ""
	if _, err := r.svc.PlaceOrder(r.ctx, till, in); err != nil {
		t.Fatalf("within Ben's limit: %v", err)
	}
}

func TestPaymentsRefundsAndVoid(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234") // cashier: refunds up to $10
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-pay-00001", Type: "pickup",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id, total := res.Order.Order.ID, res.Order.Order.Money.Total
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: total + 1}); !errors.Is(err, terminal.ErrOverBalance) {
		t.Fatalf("over balance: %v", err)
	}
	p, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{Tender: "card", AmountCents: 1000, ClientKey: "pay-00000001"})
	if err != nil || p.BalanceCents != total-1000 || p.Duplicate {
		t.Fatalf("part payment: %+v %v", p, err)
	}
	p, err = r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: total - 1000, TenderedCents: 5000})
	if err != nil || p.BalanceCents != 0 || !p.Order.Status.Paid || p.Payment.ChangeCents != 5000-(total-1000) {
		t.Fatalf("rest: %+v %v", p, err)
	}
	// A refund over $10 needs a manager.
	var mr *terminal.ManagerRequired
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 1500, Refund: true, Reason: "cold"}); !errors.As(err, &mr) {
		t.Fatalf("refund over the limit: %v", err)
	}
	p, err = r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 500, Refund: true, Reason: "cold"})
	if err != nil || p.BalanceCents != 500 || p.Order.Status.Paid {
		t.Fatalf("small refund: %+v %v", p, err)
	}
	// Void of an order that holds money: refund first (contract 2.10s §5).
	var pf *terminal.PaidFirst
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "customer left"}); !errors.As(err, &pf) || pf.PaidCents != total-500 {
		t.Fatalf("void of a paid order: %v", err)
	}
	if back := refundAll(t, r, till, id); len(back) != 2 {
		t.Fatalf("refunds by tender: %+v", back)
	}
	v, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "customer left", ManagerPIN: "9876"})
	if err != nil || v.Already || v.RefundedCents != 0 || len(v.Refunds) != 0 || v.Order.Status.State != order.StateCancelled {
		t.Fatalf("void: %+v %v", v, err)
	}
	if len(r.orders.voids) != 1 || r.pub.count(EventOrderVoided) != 1 {
		t.Fatal("VOID slip + event")
	}
	again, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "again"})
	if err != nil || !again.Already {
		t.Fatalf("again: %+v %v", again, err)
	}
	// An unpaid void takes no money, and a manager (the order was sent): the
	// cashier with an approval for "void", the approver on the void.
	res, _ = r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-pay-00002", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	var vn *terminal.ManagerRequired
	if _, err := r.svc.Void(r.ctx, till, res.Order.Order.ID, VoidIn{Reason: "mistake"}); !errors.As(err, &vn) || vn.Action != terminal.ActionVoid ||
		vn.Requested != res.Order.Order.Money.Total {
		t.Fatalf("a cashier's void without a manager: %v", err)
	}
	ap, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.ActionVoid})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := r.svc.Void(r.ctx, till, res.Order.Order.ID, VoidIn{Reason: "mistake", Approvals: []string{ap.ApprovalID}}); err != nil || v.RefundedCents != 0 {
		t.Fatalf("void with the approval: %+v %v", v, err)
	}
	if j := journalOf(t, r, terminal.ActionVoid); len(j) == 0 || j[0]["approved_by"] != "mgr-1" {
		t.Fatalf("the void names its approver: %v", j)
	}
	// The journal: a terminal-only action, and a Pi-only one refused.
	if _, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: "no_sale"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: terminal.ActionVoid}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("reserved action: %v", err)
	}
}
