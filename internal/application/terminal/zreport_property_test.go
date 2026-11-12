package terminal

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// TestZReportMoneyReconciles is the property test (contract 2.10l): random
// days of counter orders, payments (every tender, tips, change), refunds,
// voids, drawer sessions (open, pay-in / pay-out, count out, handover, left
// open for Close batch) on two terminals always give a Z report that
// balances, and its numbers match an independent model of the same day:
//
//   - tenders collected - refunds + unpaid = sales total + tips + on cancelled
//   - each tender's collected / refunded / tips = the model's
//   - each drawer's expected = start + cash in - cash out + pay-ins - pay-outs
//     for the payments the model put in it, and the drawers' cash + the cash
//     outside them = the cash tender's net
//
// Days: Z_PROPERTY_DAYS (default 25), seed Z_PROPERTY_SEED (default random,
// logged on failure).
func TestZReportMoneyReconciles(t *testing.T) {
	days := 25
	if v, err := strconv.Atoi(os.Getenv("Z_PROPERTY_DAYS")); err == nil && v > 0 {
		days = v
	}
	seed := rand.Int63()
	if v, err := strconv.ParseInt(os.Getenv("Z_PROPERTY_SEED"), 10, 64); err == nil {
		seed = v
	}
	t.Logf("seed %d (Z_PROPERTY_SEED=%d to replay)", seed, seed)
	rng := rand.New(rand.NewSource(seed))
	r := newRig(t)
	for day := 0; day < days; day++ {
		m := runDay(t, r, rng, day)
		// Auto-close: never refused over unpaid counter checks (they are
		// carried, contract 2.10s §4; the model doesn't touch them again).
		closed, _, err := r.repo.CloseBatch(order.WithCloseKind(r.ctx, order.CloseAuto), true)
		if err != nil {
			t.Fatalf("seed %d day %d: close batch: %v", seed, day, err)
		}
		r.svc.AfterBatchClose(r.ctx, *closed)
		z, err := r.svc.ZReport(r.ctx, closed.ID)
		if err != nil || z == nil {
			t.Fatalf("seed %d day %d: z: %v", seed, day, err)
		}
		if z.Reconcile.DifferenceCents != 0 {
			dumpZ(t, r, closed.ID)
		}
		m.check(t, fmt.Sprintf("seed %d day %d", seed, day), z)
		// The next day starts logged in again (Close batch keeps sessions).
	}
}

type mPay struct {
	tender      string
	amount, tip int64
	refund      bool
	drawer      string
}

type mOrder struct {
	id       string
	typ      string
	total    int64
	voided   bool
	pays     []mPay
	terminal Actor
}

func (o *mOrder) net() (amt, tip int64) {
	for _, p := range o.pays {
		if p.refund {
			amt, tip = amt-p.amount, tip-p.tip
		} else {
			amt, tip = amt+p.amount, tip+p.tip
		}
	}
	return
}

func (o *mOrder) netByTender() map[string][2]int64 {
	out := map[string][2]int64{}
	for _, p := range o.pays {
		v := out[p.tender]
		if p.refund {
			v[0], v[1] = v[0]-p.amount, v[1]-p.tip
		} else {
			v[0], v[1] = v[0]+p.amount, v[1]+p.tip
		}
		out[p.tender] = v
	}
	return out
}

type mDrawer struct {
	id               string
	start, in, out   int64 // cash in (sales + tips), cash out (refunds)
	payIns, payOuts  int64
	counted          *int64
	open             bool
	reason           string
	staff            string
	terminalDeviceID string
}

func (d *mDrawer) expected() int64 { return d.start + d.in - d.out + d.payIns - d.payOuts }

type model struct {
	orders  []*mOrder
	drawers map[string]*mDrawer
	open    map[string]*mDrawer // by terminal
	// openOnPi: the Pi's open session on a terminal ("" = none).
	openOnPi func(deviceID string) string
}

func (m *model) record(o *mOrder, a Actor, pays ...terminal.Payment) {
	for _, p := range pays {
		if m.open[a.DeviceID] == nil {
			// The owner, 2026-11-12: a terminal's session opens by itself
			// with its first payment (start 0).
			id := m.openOnPi(a.DeviceID)
			if id == "" {
				panic(fmt.Sprintf("payment %s on %s: no session opened by itself", p.ID, a.DeviceID))
			}
			md := &mDrawer{id: id, start: terminal.DefaultDrawerStartCents, open: true, staff: p.StaffID, terminalDeviceID: a.DeviceID}
			m.drawers[id], m.open[a.DeviceID] = md, md
		}
		mp := mPay{tender: p.Tender, amount: p.AmountCents, tip: p.TipCents, refund: p.Refund}
		if p.Tender == terminal.TenderCash {
			if d := m.open[a.DeviceID]; d != nil {
				mp.drawer = d.id
				if p.Refund {
					d.out += p.AmountCents + p.TipCents
				} else {
					d.in += p.AmountCents + p.TipCents
				}
			}
		}
		if mp.drawer != p.DrawerID {
			panic(fmt.Sprintf("payment %s: the Pi put it in drawer %q, the model in %q", p.ID, p.DrawerID, mp.drawer))
		}
		o.pays = append(o.pays, mp)
	}
}

var tenders = []string{terminal.TenderCash, terminal.TenderCash, terminal.TenderCard, terminal.TenderSquareApp, terminal.TenderManualCard, terminal.TenderComp}

func runDay(t *testing.T, r *rig, rng *rand.Rand, day int) *model {
	t.Helper()
	m := &model{drawers: map[string]*mDrawer{}, open: map[string]*mDrawer{}}
	m.openOnPi = func(dev string) string {
		if d, err := r.repo.DrawerByDevice(r.ctx, dev); err == nil && d != nil {
			return d.ID
		}
		return ""
	}
	// Till 1: Cara (cashier), Till 2: Khawar (manager). Ben (cashier) takes
	// over Till 1 now and then.
	pins := map[string]string{till.DeviceID: "1234", till2.DeviceID: "9876"}
	staffOn := map[string]string{till.DeviceID: "cash-1", till2.DeviceID: "mgr-1"}
	r.login(t, till, pins[till.DeviceID])
	r.login(t, till2, pins[till2.DeviceID])
	terms := []Actor{till, till2}
	key := 0
	nextKey := func() string { key++; return fmt.Sprintf("d%03d-k%05d", day, key) }
	quote := func(c Cart) int64 {
		q, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: c})
		if err != nil {
			t.Fatal(err)
		}
		return q.Quote.(pricing.Quote).TotalCents
	}
	ops := 15 + rng.Intn(25)
	for i := 0; i < ops; i++ {
		a := terms[rng.Intn(len(terms))]
		switch op := rng.Intn(100); {
		case op < 12: // open this terminal's drawer
			if m.open[a.DeviceID] != nil {
				continue
			}
			start := int64(rng.Intn(5)) * 2500
			d, err := r.svc.OpenDrawer(r.ctx, a, DrawerOpenIn{StartCents: &start})
			var has *terminal.CashierHasDrawer
			if errors.As(err, &has) {
				continue // Ben still holds Till 1's old one elsewhere: not in this model
			}
			if err != nil {
				t.Fatalf("open drawer: %v", err)
			}
			md := &mDrawer{id: d.ID, start: start, open: true, staff: staffOn[a.DeviceID], terminalDeviceID: a.DeviceID}
			m.drawers[d.ID], m.open[a.DeviceID] = md, md
		case op < 45: // place an order, maybe paying
			typ := []string{"counter", "pickup", "delivery"}[rng.Intn(3)]
			cart := sodaCart(1 + rng.Intn(3))
			if rng.Intn(2) == 0 {
				cart.Lines = append(cart.Lines, pizzaLine())
			}
			total := quote(cart)
			in := OrderIn{ClientKey: nextKey(), Type: typ, Cart: cart}
			if typ == "delivery" {
				in.Customer = &Customer{Name: "Ada", Phone: "4105550100", Address: &Address{Line1: "1 Test St", City: "Towson"}}
			}
			switch rng.Intn(4) {
			case 0: // unpaid (pay later)
			case 1: // all cash with a tip and change
				tip := int64(rng.Intn(3)) * 100
				in.Payments = []PaymentIn{{Tender: "cash", AmountCents: total, TipCents: tip, TenderedCents: total + tip + int64(rng.Intn(2000))}}
			case 2: // split
				part := 1 + rng.Int63n(total-1)
				in.Payments = []PaymentIn{{Tender: tenders[rng.Intn(5)], AmountCents: part, TipCents: int64(rng.Intn(2)) * 150},
					{Tender: "card", AmountCents: total - part}}
			case 3: // one tender (comp with the manager's PIN)
				in.Payments = []PaymentIn{{Tender: tenders[rng.Intn(len(tenders))], AmountCents: total}}
				in.ManagerPIN = "9876"
			}
			res, err := r.svc.PlaceOrder(r.ctx, a, in)
			if err != nil {
				t.Fatalf("place %+v: %v", in, err)
			}
			if res.Order.Order.Money.Total != total {
				t.Fatalf("total %d, quoted %d", res.Order.Order.Money.Total, total)
			}
			o := &mOrder{id: res.Order.Order.ID, typ: typ, total: total, terminal: a}
			pays, _ := r.repo.OrderPayments(r.ctx, o.id)
			m.record(o, a, pays...)
			m.orders = append(m.orders, o)
		case op < 60: // a payment on an order with a balance, from any terminal
			o := pick(rng, m.orders, func(o *mOrder) bool { amt, _ := o.net(); return !o.voided && amt < o.total })
			if o == nil {
				continue
			}
			amt, _ := o.net()
			due := o.total - amt
			pay := PaymentIn{Tender: tenders[rng.Intn(len(tenders))], AmountCents: 1 + rng.Int63n(due), TipCents: int64(rng.Intn(3)) * 50,
				ManagerPIN: "9876", ClientKey: nextKey()}
			res, err := r.svc.AddPayment(r.ctx, a, o.id, pay)
			if err != nil {
				t.Fatalf("payment: %v", err)
			}
			m.record(o, a, res.Payment)
		case op < 70: // a refund (a tender change, or money back)
			o := pick(rng, m.orders, func(o *mOrder) bool { amt, _ := o.net(); return !o.voided && amt > 0 })
			if o == nil {
				continue
			}
			amt, tip := o.net()
			ref := PaymentIn{Tender: tenders[rng.Intn(len(tenders))], AmountCents: 1 + rng.Int63n(amt), Refund: true, Reason: "property",
				ManagerPIN: "9876", ClientKey: nextKey()}
			if tip > 0 && rng.Intn(2) == 0 {
				ref.TipCents = 1 + rng.Int63n(tip)
			}
			res, err := r.svc.AddPayment(r.ctx, a, o.id, ref)
			if err != nil {
				t.Fatalf("refund %+v of %d/%d: %v", ref, amt, tip, err)
			}
			m.record(o, a, res.Payment)
		case op < 76: // void (refunds each tender's net)
			o := pick(rng, m.orders, func(o *mOrder) bool { return !o.voided })
			if o == nil {
				continue
			}
			// A paid order is refunded first (contract 2.10s §5): the
			// void's own split, as refunds; then the void takes no money.
			var back []terminal.Payment
			for _, pr := range refundAll(t, r, a, o.id) {
				back = append(back, pr.Payment)
			}
			res, err := r.svc.Void(r.ctx, a, o.id, VoidIn{Reason: "property"})
			if err != nil {
				t.Fatalf("void: %v", err)
			}
			back = append(back, res.Refunds...) // comps reversed by the void
			// The refunds give back exactly what is still held - the net
			// amount and the net tip - never more on a tender than it holds.
			byTender := o.netByTender()
			amt, tip := o.net()
			var backAmt, backTip int64
			for _, p := range back {
				w := byTender[p.Tender]
				if p.AmountCents > max(w[0], 0) || p.TipCents > max(w[1], 0) {
					t.Fatalf("void refund %s: %d+%d, the tender holds %v", p.Tender, p.AmountCents, p.TipCents, w)
				}
				backAmt, backTip = backAmt+p.AmountCents, backTip+p.TipCents
			}
			if backAmt != max(amt, 0) || backTip != max(tip, 0) {
				t.Fatalf("void gave back %d+%d, held %d+%d (%v)", backAmt, backTip, amt, tip, byTender)
			}
			m.record(o, a, back...)
			o.voided = true
		case op < 84: // pay-in / pay-out
			d := m.open[a.DeviceID]
			if d == nil {
				continue
			}
			if rng.Intn(2) == 0 {
				amt := int64(1 + rng.Intn(3000))
				if _, err := r.svc.MoveDrawer(r.ctx, a, terminal.MovePayIn, DrawerMoveIn{AmountCents: amt, Reason: "change"}); err != nil {
					t.Fatalf("pay-in: %v", err)
				}
				d.payIns += amt
			} else if exp := d.expected(); exp > 0 {
				amt := 1 + rng.Int63n(exp)
				if _, err := r.svc.MoveDrawer(r.ctx, a, terminal.MovePayOut, DrawerMoveIn{AmountCents: amt, Reason: "supplier", ManagerPIN: "9876"}); err != nil {
					t.Fatalf("pay-out %d of %d: %v", amt, exp, err)
				}
				d.payOuts += amt
			}
		case op < 90: // count out
			d := m.open[a.DeviceID]
			if d == nil {
				continue
			}
			counted := d.expected() + int64(rng.Intn(5)-2)*25
			if counted < 0 {
				counted = 0
			}
			got, err := r.svc.CloseDrawer(r.ctx, a, DrawerCloseIn{CountedCents: &counted, ManagerPIN: "9876"})
			if err != nil {
				t.Fatalf("close drawer: %v", err)
			}
			if got.ExpectedCents != d.expected() {
				t.Fatalf("drawer expected %d, model %d", got.ExpectedCents, d.expected())
			}
			d.counted, d.open, d.reason = &counted, false, terminal.DrawerClosedCounted
			m.open[a.DeviceID] = nil
		case op < 94: // handover on Till 1 between Cara and Ben
			if a.DeviceID != till.DeviceID || m.open[till.DeviceID] == nil {
				continue
			}
			d := m.open[till.DeviceID]
			nextPin, nextStaff := "55", "cash-2"
			if staffOn[till.DeviceID] == "cash-2" {
				nextPin, nextStaff = "1234", "cash-1"
			}
			r.svc.Logout(r.ctx, till)
			r.login(t, till, nextPin)
			staffOn[till.DeviceID] = nextStaff
			counted := d.expected() + int64(rng.Intn(3)-1)*100
			if counted < 0 {
				counted = 0
			}
			ho, err := r.svc.HandoverDrawer(r.ctx, till, DrawerCloseIn{CountedCents: &counted})
			if err != nil {
				t.Fatalf("handover: %v", err)
			}
			if ho.Closed.ExpectedCents != d.expected() {
				t.Fatalf("handover expected %d, model %d", ho.Closed.ExpectedCents, d.expected())
			}
			d.counted, d.open, d.reason = &counted, false, terminal.DrawerClosedHandover
			nd := &mDrawer{id: ho.Drawer.ID, start: counted, open: true, staff: nextStaff, terminalDeviceID: till.DeviceID}
			m.drawers[nd.id], m.open[till.DeviceID] = nd, nd
		default: // no sale
			if _, err := r.svc.PostJournal(r.ctx, a, JournalIn{Action: "no_sale"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, d := range m.open {
		if d != nil {
			d.reason = terminal.DrawerClosedBatchClose // Close batch closes it, not counted
			d.open = false
		}
	}
	return m
}

func pick(rng *rand.Rand, orders []*mOrder, ok func(*mOrder) bool) *mOrder {
	var c []*mOrder
	for _, o := range orders {
		if ok(o) {
			c = append(c, o)
		}
	}
	if len(c) == 0 {
		return nil
	}
	return c[rng.Intn(len(c))]
}

func (m *model) check(t *testing.T, where string, z *terminal.Z) {
	t.Helper()
	rec := z.Reconcile
	if rec.DifferenceCents != 0 || rec.DrawersDifferences != 0 {
		t.Fatalf("%s: unbalanced %+v", where, rec)
	}
	type tot struct{ count, collected, refunded, tips int64 }
	want := map[string]*tot{}
	var sales, unpaid, driverCash, tips, onCancelled, cashOutside int64
	var counts terminal.ZCounts
	for _, o := range m.orders {
		amt, tip := o.net()
		for _, p := range o.pays {
			w := want[p.tender]
			if w == nil {
				w = &tot{}
				want[p.tender] = w
			}
			if p.refund {
				w.refunded += p.amount + p.tip
				w.tips -= p.tip
			} else {
				w.count++
				w.collected += p.amount + p.tip
				w.tips += p.tip
			}
			if p.tender == terminal.TenderCash && p.drawer == "" {
				if p.refund {
					cashOutside -= p.amount + p.tip
				} else {
					cashOutside += p.amount + p.tip
				}
			}
		}
		tips += tip
		if o.voided {
			counts.Voided++
			onCancelled += amt
			continue
		}
		counts.Orders++
		switch o.typ {
		case "counter":
			counts.Counter++
		case "pickup":
			counts.Pickup++
		case "delivery":
			counts.Delivery++
		}
		sales += o.total
		if left := o.total - amt; left != 0 {
			if o.typ == "delivery" && left > 0 {
				driverCash += left // Close batch delivers it; the driver collects
			} else {
				unpaid += left
			}
		}
	}
	if z.Sales.TotalCents != sales || z.Unpaid.Cents != unpaid || rec.TipsCents != tips || rec.OnCancelledCents != onCancelled ||
		z.CashOutside != cashOutside {
		t.Fatalf("%s: sales %d/%d unpaid %d/%d tips %d/%d on cancelled %d/%d cash outside %d/%d", where, z.Sales.TotalCents, sales,
			z.Unpaid.Cents, unpaid, rec.TipsCents, tips, rec.OnCancelledCents, onCancelled, z.CashOutside, cashOutside)
	}
	if z.Counts.Orders != counts.Orders || z.Counts.Counter != counts.Counter || z.Counts.Pickup != counts.Pickup ||
		z.Counts.Delivery != counts.Delivery || z.Counts.Voided != counts.Voided || z.Voids.Count != counts.Voided {
		t.Fatalf("%s: counts %+v, model %+v", where, z.Counts, counts)
	}
	seen := map[string]bool{}
	for _, zt := range z.Tenders {
		seen[zt.Tender] = true
		if zt.Tender == terminal.TenderDriverCash {
			if zt.CollectedCents != driverCash {
				t.Fatalf("%s: driver cash %d, model %d", where, zt.CollectedCents, driverCash)
			}
			continue
		}
		w := want[zt.Tender]
		if w == nil {
			t.Fatalf("%s: tender %s not in the model", where, zt.Tender)
		}
		if zt.Count != w.count || zt.CollectedCents != w.collected || zt.RefundedCents != w.refunded || zt.TipsCents != w.tips ||
			zt.NetCents != w.collected-w.refunded {
			t.Fatalf("%s: tender %s %+v, model %+v", where, zt.Tender, zt, *w)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Fatalf("%s: tender %s missing from the Z", where, name)
		}
	}
	if driverCash != 0 && !seen[terminal.TenderDriverCash] {
		t.Fatalf("%s: driver cash missing", where)
	}
	if len(z.Drawers) != len(m.drawers) {
		t.Fatalf("%s: %d drawers, model %d", where, len(z.Drawers), len(m.drawers))
	}
	var overShort int64
	for _, zd := range z.Drawers {
		d := m.drawers[zd.ID]
		if d == nil {
			t.Fatalf("%s: drawer %s not in the model", where, zd.ID)
		}
		if zd.ExpectedCents != d.expected() || zd.StartCents != d.start || zd.PayInsCents != d.payIns || zd.PayOutsCents != d.payOuts ||
			zd.CloseReason != d.reason || (d.counted == nil) != (zd.CountedCents == nil) || zd.Flagged != (d.counted == nil) {
			t.Fatalf("%s: drawer %+v, model %+v", where, zd, *d)
		}
		if d.counted != nil {
			if *zd.OverShortCents != *d.counted-d.expected() {
				t.Fatalf("%s: over/short %d, model %d", where, *zd.OverShortCents, *d.counted-d.expected())
			}
			overShort += *d.counted - d.expected()
		}
	}
	if z.OverShort != overShort {
		t.Fatalf("%s: over/short %d, model %d", where, z.OverShort, overShort)
	}
}
