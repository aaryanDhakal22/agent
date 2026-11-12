package terminal

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Cash drawers (contract 2.10k) and the Z report (2.10l) against a real
// SQLite DB.

func i64(v int64) *int64 { return &v }

func sodaCart(n int) Cart {
	return Cart{Cart: pricing.Cart{Lines: []pricing.Line{{Qty: n, Selection: pricing.Selection{ItemID: soda}}}}}
}

// floorTypes are the unsynced floor events by type (and their subjects).
func floorTypes(t *testing.T, r *rig) (map[string]int, map[string]string) {
	t.Helper()
	evs, err := r.repo.ListUnsyncedEvents(r.ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n, subj := map[string]int{}, map[string]string{}
	for _, e := range evs {
		n[e.Type]++
		var p struct {
			Drawer struct {
				ID string `json:"id"`
			} `json:"drawer"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		if p.Drawer.ID != "" {
			subj[p.Drawer.ID] = e.Type
		}
	}
	return n, subj
}

func TestDrawerLifecycle(t *testing.T) {
	r := newRig(t)
	if _, err := r.svc.OpenDrawer(r.ctx, till, DrawerOpenIn{StartCents: i64(10000)}); !errors.Is(err, terminal.ErrLoginRequired) {
		t.Fatalf("no login: %v", err)
	}
	r.login(t, till, "1234")  // Cara, cashier
	r.login(t, till2, "9876") // Khawar, manager
	if _, err := r.svc.OpenDrawer(r.ctx, till, DrawerOpenIn{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no count: %v", err)
	}
	if _, err := r.svc.OpenDrawer(r.ctx, till, DrawerOpenIn{StartCents: i64(15000),
		Denominations: []terminal.Denomination{{ValueCents: 2000, Count: 5}}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("count mismatch: %v", err)
	}
	d, err := r.svc.OpenDrawer(r.ctx, till, DrawerOpenIn{Denominations: []terminal.Denomination{{ValueCents: 2000, Count: 5}, {ValueCents: 100, Count: 20}}})
	if err != nil || d.StartCents != 12000 || d.State != "open" || d.ExpectedCents != 12000 || d.StaffID != "cash-1" || d.DeviceName != "Till 1" {
		t.Fatalf("open: %+v %v", d, err)
	}
	var open *terminal.DrawerOpen
	if _, err := r.svc.OpenDrawer(r.ctx, till, DrawerOpenIn{StartCents: i64(1)}); !errors.As(err, &open) || open.Drawer.ID != d.ID {
		t.Fatalf("second open on the terminal: %v", err)
	}
	// Cara moves to Till 2 (logged out of Till 1): she still holds Till 1's drawer.
	r.svc.Logout(r.ctx, till2)
	r.login(t, till2, "1234")
	var has *terminal.CashierHasDrawer
	if _, err := r.svc.OpenDrawer(r.ctx, till2, DrawerOpenIn{StartCents: i64(1)}); !errors.As(err, &has) || has.Drawer.ID != d.ID {
		t.Fatalf("cashier with a drawer elsewhere: %v", err)
	}
	v, err := r.svc.Drawer(r.ctx, till2)
	if err != nil || v.Drawer != nil || v.Mine == nil || v.Mine.ID != d.ID || v.PayoutLimitCents == nil || *v.PayoutLimitCents != terminal.DefaultCashierPayoutCents {
		t.Fatalf("view on till 2: %+v %v", v, err)
	}
	r.svc.Logout(r.ctx, till2)
	r.login(t, till2, "9876")

	// A cash sale on Till 1 goes in its drawer (tip too, change not).
	q, _ := r.svc.Quote(r.ctx, till, QuoteIn{Cart: sodaCart(2)})
	total := q.Quote.(pricing.Quote).TotalCents
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "drw-0001", Type: "counter", Cart: sodaCart(2),
		Payments: []PaymentIn{{Tender: "cash", AmountCents: total, TipCents: 100, TenderedCents: 5000}}})
	if err != nil {
		t.Fatal(err)
	}
	pays, _ := r.repo.OrderPayments(r.ctx, res.Order.Order.ID)
	if len(pays) != 1 || pays[0].DrawerID != d.ID {
		t.Fatalf("cash payment's drawer: %+v", pays)
	}
	// A card sale doesn't.
	res2, _ := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "drw-0002", Type: "pickup", Cart: sodaCart(1)})
	q1, _ := r.svc.Quote(r.ctx, till, QuoteIn{Cart: sodaCart(1)})
	if _, err := r.svc.AddPayment(r.ctx, till, res2.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: "card", AmountCents: q1.Quote.(pricing.Quote).TotalCents}); err != nil {
		t.Fatal(err)
	}
	// Pay-in free; pay-out over the cashier's $20 needs a manager; more than
	// the drawer holds is refused.
	if _, err := r.svc.MoveDrawer(r.ctx, till, terminal.MovePayIn, DrawerMoveIn{AmountCents: 500, Reason: "change", ClientKey: "mv-000001"}); err != nil {
		t.Fatal(err)
	}
	dup, err := r.svc.MoveDrawer(r.ctx, till, terminal.MovePayIn, DrawerMoveIn{AmountCents: 500, Reason: "change", ClientKey: "mv-000001"})
	if err != nil || !dup.Duplicate || dup.Drawer.PayInsCents != 500 {
		t.Fatalf("idempotent pay-in: %+v %v", dup, err)
	}
	var need *terminal.ManagerRequired
	if _, err := r.svc.MoveDrawer(r.ctx, till, terminal.MovePayOut, DrawerMoveIn{AmountCents: 2500, Reason: "supplier"}); !errors.As(err, &need) ||
		need.Action != "pay_out" || need.Limit != 2000 || need.Requested != 2500 {
		t.Fatalf("pay-out over the limit: %v", err)
	}
	if _, err := r.svc.MoveDrawer(r.ctx, till, terminal.MovePayOut, DrawerMoveIn{AmountCents: 2500, Reason: "supplier", ManagerPIN: "1234"}); !errors.Is(err, terminal.ErrManagerPINWrong) {
		t.Fatalf("a cashier's PIN: %v", err)
	}
	var short ErrNotEnoughCash
	if _, err := r.svc.MoveDrawer(r.ctx, till, terminal.MovePayOut, DrawerMoveIn{AmountCents: 900_000, Reason: "x", ManagerPIN: "9876"}); !errors.As(err, &short) {
		t.Fatalf("more than the drawer: %v", err)
	}
	mv, err := r.svc.MoveDrawer(r.ctx, till, terminal.MovePayOut, DrawerMoveIn{AmountCents: 2500, Reason: "supplier", ManagerPIN: "9876"})
	if err != nil || mv.Movement.ApprovedBy != "mgr-1" {
		t.Fatalf("approved pay-out: %+v %v", mv, err)
	}
	// No sale: the row carries the drawer, the drawer counts it.
	e, err := r.svc.PostJournal(r.ctx, till, JournalIn{Action: "no_sale"})
	if err != nil || !strings.Contains(string(e.Detail), d.ID) {
		t.Fatalf("no sale: %+v %v", e, err)
	}
	want := int64(12000) + total + 100 + 500 - 2500
	v, _ = r.svc.Drawer(r.ctx, till)
	if v.Drawer.ExpectedCents != want || v.Drawer.CashSalesCents != total || v.Drawer.CashTipsCents != 100 || v.Drawer.NoSales != 1 ||
		len(v.Drawer.Movements) != 2 {
		t.Fatalf("expected %d: %+v", want, v.Drawer)
	}
	// A cash refund from Till 1 comes out of it.
	if _, err := r.svc.AddPayment(r.ctx, till, res.Order.Order.ID, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 50, Refund: true, Reason: "sorry",
		ManagerPIN: "9876"}); err != nil {
		t.Fatal(err)
	}
	want -= 50
	// Khawar (Till 2) can't close Cara's drawer without... he is a manager: he can.
	// Cara counts out on Till 1, $1 short.
	if _, err := r.svc.CloseDrawer(r.ctx, till, DrawerCloseIn{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no count: %v", err)
	}
	closed, err := r.svc.CloseDrawer(r.ctx, till, DrawerCloseIn{CountedCents: i64(want - 100)})
	if err != nil || closed.State != "closed" || closed.ExpectedCents != want || closed.OverShortCents == nil || *closed.OverShortCents != -100 ||
		closed.Flagged || closed.CloseReason != terminal.DrawerClosedCounted || closed.ClosedBy != "cash-1" {
		t.Fatalf("close: %+v %v", closed, err)
	}
	if _, err := r.svc.CloseDrawer(r.ctx, till, DrawerCloseIn{DrawerID: d.ID, CountedCents: i64(1)}); !errors.Is(err, terminal.ErrDrawerClosed) {
		t.Fatalf("closed twice: %v", err)
	}
	if _, err := r.svc.MoveDrawer(r.ctx, till, terminal.MovePayIn, DrawerMoveIn{AmountCents: 1, Reason: "x"}); !errors.Is(err, terminal.ErrNoDrawer) {
		t.Fatalf("no drawer: %v", err)
	}
	n, subj := floorTypes(t, r)
	if n[terminal.EventDrawerOpened] != 1 || n[terminal.EventDrawerMovement] != 2 || n[terminal.EventDrawerClosed] != 1 || subj[d.ID] == "" {
		t.Fatalf("floor events %v", n)
	}
	if r.pub.count(EventDrawerChanged) < 4 {
		t.Fatalf("drawer events %v", r.pub.events)
	}
}

func TestDrawerHandoverOtherCashierAndBatchClose(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	d, err := r.svc.OpenDrawer(r.ctx, till, DrawerOpenIn{StartCents: i64(10000)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.HandoverDrawer(r.ctx, till, DrawerCloseIn{CountedCents: i64(10000)}); !errors.Is(err, terminal.ErrSameCashier) {
		t.Fatalf("hand over to herself: %v", err)
	}
	// Ben takes Till 1 over: counted $99, Cara is $1 short; his starts at $99.
	r.svc.Logout(r.ctx, till)
	r.login(t, till, "55")
	ho, err := r.svc.HandoverDrawer(r.ctx, till, DrawerCloseIn{CountedCents: i64(9900)})
	if err != nil || ho.Closed.ID != d.ID || ho.Closed.CloseReason != terminal.DrawerClosedHandover || *ho.Closed.OverShortCents != -100 ||
		ho.Drawer.StaffID != "cash-2" || ho.Drawer.StartCents != 9900 || ho.Drawer.HandoverFrom != d.ID {
		t.Fatalf("handover: %+v %v", ho, err)
	}
	// Cara on Till 2 opens her own; Ben can't close it without a manager.
	r.login(t, till2, "1234")
	d2, err := r.svc.OpenDrawer(r.ctx, till2, DrawerOpenIn{StartCents: i64(5000)})
	if err != nil {
		t.Fatal(err)
	}
	var need *terminal.ManagerRequired
	if _, err := r.svc.CloseDrawer(r.ctx, till, DrawerCloseIn{DrawerID: d2.ID, CountedCents: i64(5000)}); !errors.As(err, &need) || need.Action != "drawer_close" {
		t.Fatalf("another cashier's drawer: %v", err)
	}
	// A driver has no drawer.
	r.main.set(PathTerminalConfig, staffConfig(t, 2, terminal.Staff{ID: "drv-1", FirstName: "Ali", Role: terminal.RoleDriver, PinHash: hash(t, "77")}))
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	drv := Actor{DeviceID: "dev-3", DeviceName: "Till 3"}
	r.login(t, drv, "77")
	if _, err := r.svc.OpenDrawer(r.ctx, drv, DrawerOpenIn{StartCents: i64(1)}); !errors.Is(err, terminal.ErrDrawerNotAllowed) {
		t.Fatalf("driver: %v", err)
	}
	// Close batch closes both open drawers, not counted, flagged; the Z
	// report lists all three sessions.
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "hand-0001", Type: "counter", Cart: sodaCart(1)}); err != nil {
		t.Fatal(err)
	}
	closed, _, err := r.repo.CloseBatch(order.WithCloseKind(r.ctx, order.CloseAuto), true)
	if err != nil || len(closed.ClosedDrawers) != 2 || !closed.ZPending {
		t.Fatalf("close batch: %+v %v", closed, err)
	}
	r.svc.AfterBatchClose(r.ctx, *closed)
	for _, id := range closed.ClosedDrawers {
		dd, err := r.repo.DrawerByID(r.ctx, id)
		if err != nil || dd.CloseReason != terminal.DrawerClosedBatchClose || !dd.Flagged || dd.CountedCents != nil || dd.OverShortCents != nil {
			t.Fatalf("batch-closed drawer: %+v %v", dd, err)
		}
	}
	z, err := r.svc.ZReport(r.ctx, closed.ID)
	if err != nil || z == nil {
		t.Fatalf("z: %v", err)
	}
	if z.CloseKind != order.CloseAuto || !z.Forced || len(z.Drawers) != 3 || z.OverShort != -100 || z.Counts.Counter != 1 || z.Unpaid.Count != 1 ||
		z.Reconcile.DifferenceCents != 0 || z.BusinessDate == "" || z.FirstOrderAt == nil {
		t.Fatalf("z: %+v", z)
	}
	// Built once: the pending row is done, a second build changes nothing.
	r.svc.BuildPendingZ(r.ctx)
	n, _ := floorTypes(t, r)
	if n[terminal.EventZReport] != 1 || n[terminal.EventDrawerClosed] != 3 {
		t.Fatalf("floor events %v", n)
	}
	if r.pub.count(EventZReport) != 1 {
		t.Fatal("terminal.z.report once")
	}
}

func TestPlacePrepMinutes(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	before := time.Now()
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "prep-0001", Type: "pickup", Cart: sodaCart(1), PrepMinutes: i64(25)})
	if err != nil {
		t.Fatal(err)
	}
	eta := res.Order.Status.EstimatedReadyAt
	if eta == nil || eta.Before(before.Add(25*time.Minute-time.Second)) || eta.After(time.Now().Add(25*time.Minute+time.Second)) {
		t.Fatalf("ready at now + 25 min: %v", eta)
	}
	if _, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "prep-0002", Type: "pickup", Cart: sodaCart(1), PrepMinutes: i64(241)}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("over 240: %v", err)
	}
	// Absent: main's default_pickup_minutes for a pickup.
	cfg := staffConfig(t, 3)
	cfg["settings"].(map[string]any)["default_pickup_minutes"] = 40
	r.main.set(PathTerminalConfig, cfg)
	if _, err := r.svc.store.Refresh(r.ctx, terminal.KindTerminalConfig); err != nil {
		t.Fatal(err)
	}
	r.login(t, till, "1234")
	res, err = r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "prep-0003", Type: "pickup", Cart: sodaCart(1)})
	if err != nil || time.Until(*res.Order.Status.EstimatedReadyAt) < 39*time.Minute {
		t.Fatalf("default pickup minutes: %+v %v", res, err)
	}
	// A counter sale without minutes keeps the Pi's default (15 here).
	res, err = r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "prep-0004", Type: "counter", Cart: sodaCart(1)})
	if err != nil || time.Until(*res.Order.Status.EstimatedReadyAt) > 16*time.Minute {
		t.Fatalf("counter default: %v", err)
	}
}

// TestVoidAfterTenderChangeGivesBackOnlyTheNet: card paid, part given back in
// cash (a tender change): a void returns only what is still held - the
// card's refund shrinks by the cash already given back (found by the Z
// property test: the void used to refund the card in full).
func TestVoidAfterTenderChangeGivesBackOnlyTheNet(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "9876")
	q, _ := r.svc.Quote(r.ctx, till, QuoteIn{Cart: sodaCart(3)})
	total := q.Quote.(pricing.Quote).TotalCents
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: "void-0001", Type: "counter", Cart: sodaCart(3),
		Payments: []PaymentIn{{Tender: "card", AmountCents: total, TipCents: 200}}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Order.Order.ID
	if _, err := r.svc.AddPayment(r.ctx, till, id, PaymentIn{ClientKey: tk(), Tender: "cash", AmountCents: 100, TipCents: 50, Refund: true, Reason: "change"}); err != nil {
		t.Fatal(err)
	}
	// A void takes no money (contract 2.10s §5): 409 refund_first with
	// exactly what is still held - the card's net after the cash given back.
	var pf *terminal.PaidFirst
	if _, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left"}); !errors.As(err, &pf) || pf.PaidCents != total-100+150 {
		t.Fatalf("void of a paid order: %v", err)
	}
	back := refundAll(t, r, till, id)
	if len(back) != 1 || back[0].Payment.Tender != "card" || back[0].Payment.AmountCents != total-100 || back[0].Payment.TipCents != 150 {
		t.Fatalf("the void's split %+v", back)
	}
	if v, err := r.svc.Void(r.ctx, till, id, VoidIn{Reason: "left"}); err != nil || len(v.Refunds) != 0 || v.KickDrawer {
		t.Fatalf("void after the refunds: %+v %v", v, err)
	}
}
