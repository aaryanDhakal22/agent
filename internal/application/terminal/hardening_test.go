package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/rs/zerolog"
)

// The hardening round (contract 2.10s): idempotency, kitchen_pending,
// Void waste.

func TestPaymentKeyRequiredAndReusedKey(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-idem-0001", Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{Tender: "cash", AmountCents: 100}); !errors.Is(err, terminal.ErrClientKeyRequired) {
		t.Fatalf("no key: %v", err)
	}
	in := PaymentIn{ClientKey: "pay-idem-0001", Tender: "cash", AmountCents: 1000, TenderedCents: 2000}
	first, err := r.svc.AddPayment(r.ctx, till, id, in)
	if err != nil || first.Duplicate {
		t.Fatal(err)
	}
	// The same tap again (a timeout retry): the stored answer, one row.
	again, err := r.svc.AddPayment(r.ctx, till, id, in)
	if err != nil || !again.Duplicate || again.Payment.ID != first.Payment.ID || again.BalanceCents != first.BalanceCents {
		t.Fatalf("retry: %+v %v", again, err)
	}
	// The key with another amount: 409 key_reused, nothing stored.
	in.AmountCents = 900
	var kr *terminal.KeyReused
	if _, err := r.svc.AddPayment(r.ctx, till, id, in); !errors.As(err, &kr) || kr.ClientKey != "pay-idem-0001" {
		t.Fatalf("reused key: %v", err)
	}
	pays, _ := r.repo.OrderPayments(r.ctx, id)
	if len(pays) != 1 {
		t.Fatalf("payments %d", len(pays))
	}
	// A retried refund isn't a second refund (and asks no manager again).
	ref := PaymentIn{ClientKey: "ref-idem-0001", Tender: "cash", AmountCents: 1000, Refund: true, Reason: "cold"}
	if _, err := r.svc.AddPayment(r.ctx, till, id, ref); err != nil {
		t.Fatal(err)
	}
	if d, err := r.svc.AddPayment(r.ctx, till, id, ref); err != nil || !d.Duplicate {
		t.Fatalf("refund retry: %+v %v", d, err)
	}
	// SEND: the same key with another cart is key_reused; the same cart is
	// the order back with its change.
	send := OrderIn{ClientKey: "k-idem-0002", Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Payments: []PaymentIn{{Tender: "cash", AmountCents: res.Order.Order.Money.Total, TenderedCents: 5000}}}
	p1, err := r.svc.PlaceOrder(r.ctx, till, send)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := r.svc.PlaceOrder(r.ctx, till, send)
	if err != nil || !p2.Duplicate || p2.Order.Order.ID != p1.Order.Order.ID || p2.ChangeCents != p1.ChangeCents || p1.ChangeCents == 0 {
		t.Fatalf("send retry: %+v %v", p2, err)
	}
	send.Note = "extra napkins"
	if _, err := r.svc.PlaceOrder(r.ctx, till, send); !errors.As(err, &kr) {
		t.Fatalf("send with another body: %v", err)
	}
	// A cart change: the key is required; reused with another cart = 409.
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), pizzaLine()}}}}); !errors.Is(err, terminal.ErrClientKeyRequired) {
		t.Fatalf("change without a key: %v", err)
	}
	ch := ChangeIn{ClientKey: "chg-idem-0001", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine(), pizzaLine()}}}}
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ch); err != nil {
		t.Fatal(err)
	}
	ch.Cart = Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ch); !errors.As(err, &kr) {
		t.Fatalf("change with a reused key: %v", err)
	}
}

func TestKitchenPendingPrintsAfterARestart(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	r.orders.crashBeforePrint = true
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-kpend-001", Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}}); err != nil {
		t.Fatal(err)
	}
	if len(r.orders.printed) != 1 {
		t.Fatal("printed once at SEND")
	}
	// "Restart": the flag is still set; the sweep prints once and clears it.
	r.orders.crashBeforePrint = false
	if n := r.svc.ResumeKitchenPrints(r.ctx); n != 1 || len(r.orders.printed) != 2 {
		t.Fatalf("sweep: %d, printed %d", n, len(r.orders.printed))
	}
	if n := r.svc.ResumeKitchenPrints(r.ctx); n != 0 || len(r.orders.printed) != 2 {
		t.Fatalf("second sweep: %d", n)
	}
	// A normal SEND clears its flag at once.
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-kpend-002", Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}}); err != nil {
		t.Fatal(err)
	}
	if n := r.svc.ResumeKitchenPrints(r.ctx); n != 0 {
		t.Fatalf("cleared after the print: %d", n)
	}
}

func TestWasteUnpaidOnlyWithAStoreReason(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-waste-001", Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	var bad *BadWasteReason
	if _, err := r.svc.Waste(r.ctx, till, id, WasteIn{Reason: "felt like it"}); !errors.As(err, &bad) || len(bad.Reasons) != 4 {
		t.Fatalf("bad reason: %v", err)
	}
	voidsBefore := len(r.orders.voids)
	w, err := r.svc.Waste(r.ctx, till, id, WasteIn{Reason: "Never picked up"})
	if err != nil || w.Order.Status.State != order.StateCancelled || w.Order.Status.CancelReason != terminal.WasteReason {
		t.Fatalf("waste: %+v %v", w, err)
	}
	if len(r.orders.voids) != voidsBefore {
		t.Fatal("waste prints nothing")
	}
	if again, err := r.svc.Waste(r.ctx, till, id, WasteIn{Reason: "Never picked up"}); err != nil || !again.Already {
		t.Fatalf("again: %+v %v", again, err)
	}
	j := journalOf(t, r, terminal.ActionVoidWaste)
	if len(j) != 1 || j[0]["reason"] != "Never picked up" || j[0]["total_cents"] != float64(res.Order.Order.Money.Total) {
		t.Fatalf("journal: %v", j)
	}
	// A paid order: refund first.
	res, _ = r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-waste-002", Type: "pickup", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Payments: []PaymentIn{{Tender: "cash", AmountCents: 500}}})
	var pf *terminal.PaidFirst
	if _, err := r.svc.Waste(r.ctx, till, res.Order.Order.ID, WasteIn{Reason: "Burnt / dropped"}); !errors.As(err, &pf) || pf.PaidCents != 500 {
		t.Fatalf("paid waste: %v", err)
	}
	// The till report's Waste section.
	z := terminal.WasteOf(journalEntries(t, r))
	if z.Count != 1 || z.Reasons[0].Reason != "Never picked up" {
		t.Fatalf("waste section %+v", z)
	}
}

func journalEntries(t *testing.T, r *rig) []terminal.Entry {
	t.Helper()
	j, err := r.repo.Journal(r.ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// M-15: the soda's menu price goes up between SEND and an edit that adds a
// second soda line: the first keeps $1.99, the new one is $2.49.
func TestEditKeepsRungPrices(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	one := pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: soda}}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-kept-0001", Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{one}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Order.Order.Items[0].UnitPrice != 199 {
		t.Fatalf("rung at %d", res.Order.Order.Items[0].UnitPrice)
	}
	// main raises the soda to $2.49 (a new menu snapshot).
	raw, _ := json.Marshal(caseContext(t))
	var ctxMap map[string]any
	json.Unmarshal(raw, &ctxMap)
	for _, c := range ctxMap["menu"].(map[string]any)["categories"].([]any) {
		for _, it := range c.(map[string]any)["items"].([]any) {
			if m := it.(map[string]any); m["id"] == soda {
				m["price_cents"] = 249
			}
		}
	}
	r.main.set(PathCounterMenu, map[string]any{"menu_version": 13, "build": map[string]any{"view": map[string]any{"kind": "counter"}},
		"pricing": ctxMap, "routes": map[string]any{}})
	if changed, err := r.svc.Store().Refresh(r.ctx, terminal.KindCounterMenu); err != nil || !changed {
		t.Fatalf("menu refresh: %v %v", changed, err)
	}
	// A new soda line: only it is at today's price; no price change, no manager.
	other := pricing.Line{Qty: 1, Note: "no ice", Selection: pricing.Selection{ItemID: soda}}
	ch, err := r.svc.ChangeCart(r.ctx, till, res.Order.Order.ID, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{one, other}}}})
	if err != nil {
		t.Fatal(err)
	}
	items := ch.Order.Order.Items
	if len(items) != 2 || items[0].UnitPrice != 199 || items[1].UnitPrice != 249 {
		t.Fatalf("prices after the edit: %+v", items)
	}
	if j := journalOf(t, r, terminal.ActionPriceChange); len(j) != 0 {
		t.Fatalf("the Pi's own kept price is no price change: %v", j)
	}
}

type pages struct {
	mu   sync.Mutex
	msgs []string
}

func (p *pages) Send(_ context.Context, msg, _ string) error {
	p.mu.Lock()
	p.msgs = append(p.msgs, msg)
	p.mu.Unlock()
	return nil
}

func (p *pages) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.msgs...)
}

// X-2: a right PIN doesn't reset the misses; the throttle survives a
// restart; 10 misses in 10 minutes page the owner once; a store-wide count
// stops a second terminal from going on.
func TestPinThrottleIsNotResetByALogin(t *testing.T) {
	r := newRig(t)
	pg := &pages{}
	r.svc.SetPager(pg)
	r.svc.SetPinRepo(r.ctx, r.repo)
	clock := time.Now()
	r.svc.now = func() time.Time { return clock }
	r.login(t, till, "1234")
	for i := 0; i < pinFreeMisses-1; i++ {
		if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "0000"}); !errors.Is(err, terminal.ErrWrongPIN) {
			t.Fatalf("miss %d: %v", i, err)
		}
	}
	r.login(t, till, "1234") // own login: the old rule cleared the count here
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "0000"}); !errors.Is(err, terminal.ErrWrongPIN) {
		t.Fatalf("fifth miss: %v", err)
	}
	var thr *terminal.Throttled
	if _, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876"}); !errors.As(err, &thr) {
		t.Fatalf("still throttled after the own login: %v", err)
	}
	// A restart (a new service on the same DB) keeps the lock.
	svc2 := NewService(r.svc.Store(), r.repo, r.orders, r.pub, zerolog.Nop())
	svc2.now = r.svc.now
	svc2.SetPinRepo(r.ctx, r.repo)
	if _, err := svc2.Login(r.ctx, till, "1234"); !errors.As(err, &thr) {
		t.Fatalf("restart cleared the lock: %v", err)
	}
	// Another terminal: 5 more misses reach the store's 10 -> paged once,
	// and every terminal pauses.
	t2 := Actor{DeviceID: "dev-2", DeviceName: "Till 2"}
	for i := 0; i < pinFreeMisses; i++ {
		if _, err := r.svc.Login(r.ctx, t2, "0000"); err != nil && !errors.Is(err, terminal.ErrWrongPIN) && !errors.As(err, &thr) {
			t.Fatalf("till 2 miss %d: %v", i, err)
		}
	}
	waitFor(t, func() bool { return len(pg.all()) >= 1 })
	if m := pg.all(); len(m) != 1 || !strings.Contains(m[0], "wrong PINs") {
		t.Fatalf("pages %v", m)
	}
	t3 := Actor{DeviceID: "dev-3", DeviceName: "Till 3"}
	if _, err := r.svc.Login(r.ctx, t3, "1234"); !errors.As(err, &thr) {
		t.Fatalf("the store-wide count pauses a fresh terminal too: %v", err)
	}
	if p := r.svc.Problems(r.ctx); len(p) == 0 || p[len(p)-1] != "pin_lockout" {
		t.Fatalf("problems %v", p)
	}
	if j := journalOf(t, r, terminal.ActionPinLockout); len(j) == 0 {
		t.Fatal("lockout journaled")
	}
	// After the window the misses age out.
	clock = clock.Add(pinWindow + time.Minute)
	if _, err := r.svc.Login(r.ctx, t3, "1234"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200 && !ok(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
}

// L-1: one structured line per money action, the stable messages Grafana
// matches, the fields who / how much / which order, never a PIN.
func TestMoneyActionsAreLogged(t *testing.T) {
	r := newRig(t)
	var buf syncBuf
	r.svc.logger = zerolog.New(&buf)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-log-00001", Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 1000}); err != nil {
		t.Fatal(err)
	}
	// A wrong inline manager PIN on a card refund (a manager's; asked while the
	// order still holds money: a refund it can't hold is refused before any
	// PIN, money-rules R23).
	r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 1, Refund: true, Reason: "x", ManagerPIN: "0000"})
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 1000, Refund: true, Reason: "cold",
		ManagerPIN: "0000"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, m := range []string{"Counter order placed", "Counter payment taken", "Counter refund given", "Drawer kicked", "PIN failed", "Counter order voided"} {
		if !strings.Contains(out, `"message":"`+m+`"`) {
			t.Errorf("no %q line in\n%s", m, out)
		}
	}
	for _, f := range []string{`"order_id":"` + id + `"`, `"staff_id":"cash-1"`, `"amount_cents":1000`, `"tender":"cash"`, `"device_id":"dev-1"`} {
		if !strings.Contains(out, f) {
			t.Errorf("no field %s", f)
		}
	}
	if strings.Contains(out, `"pin"`) || strings.Contains(out, `"manager_pin"`) || strings.Contains(out, "0000") {
		t.Errorf("a PIN reached the log:\n%s", out)
	}
	if j := journalOf(t, r, terminal.ActionManagerCheck); len(j) == 0 || j[0]["inline"] != true {
		t.Fatalf("the inline manager miss is journaled: %v", j)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The terminal's B1 fields (contract 2.10s §9): single-use approvals, the
// draft deleted with the SEND, a reopened order's quote, journal dedupe and
// admin_mode without a login.
func TestApprovalsDraftsQuoteAndJournalKeys(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-appr-0001", Type: "counter", Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}},
		Payments: []PaymentIn{{Tender: "card", AmountCents: 1000}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	// A manager's PIN checked once gives a single-use approval for a card refund.
	mc, err := r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.NeedCardRefund, AmountCents: 500})
	if err != nil || mc.ApprovalID == "" {
		t.Fatalf("approval: %+v %v", mc, err)
	}
	ref := PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 400, Refund: true, Reason: "x", Approvals: []string{mc.ApprovalID}}
	p, err := r.svc.AddPayment(r.ctx, till, id, ref)
	if err != nil || p.Payment.ApprovedBy != "mgr-1" {
		t.Fatalf("refund with the approval: %+v %v", p, err)
	}
	ref.ClientKey = tk()
	var mr *terminal.ManagerRequired
	if _, err := r.svc.AddPayment(r.ctx, till, id, ref); !errors.As(err, &mr) {
		t.Fatalf("an approval is single use: %v", err)
	}
	// Bound to the amount: a bigger refund isn't covered.
	mc, _ = r.svc.ManagerCheck(r.ctx, till, ManagerCheckIn{PIN: "9876", Action: terminal.NeedCardRefund, AmountCents: 100})
	big := PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: 300, Refund: true, Reason: "x", Approvals: []string{mc.ApprovalID}}
	if _, err := r.svc.AddPayment(r.ctx, till, id, big); !errors.As(err, &mr) {
		t.Fatalf("over the approved amount: %v", err)
	}
	// draft_id: the shared draft goes with the SEND.
	if _, err := r.svc.PutDraft(r.ctx, till, "d-send-0001", draftIn(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "k-appr-0002", Type: "counter", DraftID: "d-send-0001",
		Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}}); err != nil {
		t.Fatal(err)
	}
	if l, _ := r.svc.Drafts(r.ctx); len(l.Drafts) != 0 {
		t.Fatalf("the draft is gone with the SEND: %+v", l.Drafts)
	}
	// A reopened order's quote: priced like PUT .../cart.
	q, err := r.svc.Quote(r.ctx, till, QuoteIn{Type: "counter", OrderID: id, Cart: Cart{Cart: pricing.Cart{Lines: []pricing.Line{pizzaLine()}}}})
	if err != nil || q.Quote.(pricing.Quote).TotalCents != res.Order.Order.Money.Total {
		t.Fatalf("quote of a reopened order: %+v %v", q, err)
	}
	// Journal: the same detail.client_key twice is one row; drawer_kick ok.
	in := JournalIn{Action: "drawer_kick", Detail: json.RawMessage(`{"client_key":"jr-0001","reason":"cash_tender"}`)}
	e1, dup1, err := r.svc.PostJournalDedup(r.ctx, till, in)
	if err != nil || dup1 {
		t.Fatal(err)
	}
	e2, dup2, err := r.svc.PostJournalDedup(r.ctx, till, in)
	if err != nil || !dup2 || e2.ID != e1.ID {
		t.Fatalf("dedupe: %+v %v %v", e2, dup2, err)
	}
	// admin_mode with nobody logged in.
	r.svc.Logout(r.ctx, till)
	if _, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: "admin_mode"}); err != nil {
		t.Fatalf("admin_mode without a login: %v", err)
	}
	if _, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: "no_sale"}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("other actions still need a login: %v", err)
	}
}
