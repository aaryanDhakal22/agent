package terminal

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/menuview"
	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
)

var tillT0 = time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)

func tillOrder(id, source, ful string, state order.State, items []order.LineItem, disc, fee, tax, tip int64, pays []order.PaymentSummary) order.StoredOrder {
	var sub int64
	for _, it := range items {
		sub += it.LineTotal
	}
	m := order.Money{Subtotal: sub, Tax: tax, Tip: tip}
	if disc > 0 {
		m.Discounts = []order.Discount{{Type: "manual", Label: "Discount 20%: regular", Amount: disc}}
	}
	if fee > 0 {
		m.Fees = []order.Fee{{Type: "delivery", Amount: fee}}
	}
	m.Total = sub - disc + fee + tax + tip
	so := order.StoredOrder{}
	so.Order = order.Snapshot{ID: id, Source: source, Money: m, Items: items, Payments: pays, Fulfillment: order.Fulfillment{Type: ful},
		ReceivedAt: tillT0}
	so.Status.State = state
	return so
}

func tline(menuID string, qty int, cents int64) order.LineItem {
	return order.LineItem{Name: "x", MenuItemID: menuID, Quantity: qty, LineTotal: cents}
}

func tillCats() *CategoryIndex {
	pizza, wings := uuid.New(), uuid.New()
	return NewCategoryIndex([]menuview.BuildCategory{
		{Name: "Wings", Position: 2, Items: []menuview.BuildItem{{ID: "v-wings", Anchor: menuview.Anchor{ItemIDs: []uuid.UUID{wings}}}}},
		{Name: "Pizza", Position: 1, Items: []menuview.BuildItem{{ID: "v-pizza", Anchor: menuview.Anchor{ItemIDs: []uuid.UUID{pizza}}}}},
	})
}

func checkTill(t *testing.T, name string, tl Till) {
	t.Helper()
	s := tl.Summary
	if s.TotalCollected+s.Unpaid != s.GrossReceipts {
		t.Fatalf("%s: collected %d + unpaid %d != gross receipts %d (%+v)", name, s.TotalCollected, s.Unpaid, s.GrossReceipts, s)
	}
	if s.NetSales != s.GrossSales+s.Adjustments+s.Refunds {
		t.Fatalf("%s: net sales %+v", name, s)
	}
	if tl.Detail.CategoriesTotal.Amount != s.GrossSales {
		t.Fatalf("%s: categories %d != gross %d", name, tl.Detail.CategoriesTotal.Amount, s.GrossSales)
	}
	var emp int64
	for _, e := range tl.Detail.Employees {
		emp += e.Amount
	}
	var online, driver int64
	for _, o := range tl.Detail.Other {
		if o.Tender == TenderOnline {
			online = o.Amount
		}
		if o.Tender == TenderDriverCash {
			driver = o.Amount
		}
	}
	if emp != s.TotalCollected-online-driver {
		t.Fatalf("%s: employees %d != terminal money %d", name, emp, s.TotalCollected-online-driver)
	}
	if len(tl.Detail.Cards) != 5 {
		t.Fatalf("%s: cards %v", name, tl.Detail.Cards)
	}
}

func TestTillBatchAndDrawer(t *testing.T) {
	cats := tillCats()
	var pizzaID, wingsID string
	for id, n := range cats.of {
		if id[:2] == "v-" {
			continue
		}
		if cats.names[n] == "Pizza" {
			pizzaID = id
		} else {
			wingsID = id
		}
	}
	// Counter order on Till 1: pizza + wings, 20 % off, paid Visa + cash.
	c1 := tillOrder("c1", order.SourceCounter, "dine_in", order.StateReady,
		[]order.LineItem{tline(pizzaID, 1, 2000), tline(wingsID, 2, 1000)}, 600, 0, 144, 0, nil)
	// Counter pickup on Till 1, unpaid.
	c2 := tillOrder("c2", order.SourceCounter, "pickup", order.StateAccepted, []order.LineItem{tline(pizzaID, 1, 1500)}, 0, 0, 90, 0, nil)
	// Online card delivery (prepaid, its tip in the total), and an online cash delivery delivered (driver cash).
	o1 := tillOrder("o1", "online", "delivery", order.StateDelivered, []order.LineItem{tline("unknown", 1, 1800)}, 0, 400, 108, 300,
		[]order.PaymentSummary{{Method: "card", AmountCents: 2608}})
	o2 := tillOrder("o2", "online", "delivery", order.StateDelivered, []order.LineItem{tline(pizzaID, 1, 1000)}, 0, 400, 60, 0, nil)
	// An online pickup paid in cash at Till 2.
	o3 := tillOrder("o3", "online", "pickup", order.StateReady, []order.LineItem{{SKU: order.SKUCustom, Name: "Open", Quantity: 1, LineTotal: 500}}, 0, 0, 30, 0, nil)
	// A voided counter order (paid cash, refunded).
	v1 := tillOrder("v1", order.SourceCounter, "dine_in", order.StateCancelled, []order.LineItem{tline(pizzaID, 1, 900)}, 0, 0, 54, 0, nil)
	v1.Status.CancelReason = VoidReason

	at := func(m int) time.Time { return tillT0.Add(time.Duration(m) * time.Minute) }
	closed := at(300)
	d1 := Drawer{ID: "d1", DeviceID: "t1", DeviceName: "Till 1", StaffID: "s1", StaffName: "Cara S.", BatchID: 7, OpenedAt: at(1), State: "closed",
		ClosedAt: &closed, StartCents: 10000}
	d1.PayInsCents = 500
	pays := []Payment{
		{ID: "p1", OrderID: "c1", At: at(10), Tender: TenderCard, AmountCents: 2000, TipCents: 200, Brand: BrandVisa, StaffID: "s1", DeviceID: "t1"},
		{ID: "p2", OrderID: "c1", At: at(11), Tender: TenderCash, AmountCents: 544, StaffID: "s1", DeviceID: "t1", DrawerID: "d1"},
		{ID: "p3", OrderID: "c1", At: at(20), Tender: TenderCash, AmountCents: 300, Refund: true, Reason: "cold", StaffID: "s1", DeviceID: "t1", DrawerID: "d1"},
		{ID: "p4", OrderID: "o3", At: at(30), Tender: TenderCash, AmountCents: 530, StaffID: "s2", DeviceID: "t2"},
		{ID: "p5", OrderID: "v1", At: at(40), Tender: TenderCash, AmountCents: 954, StaffID: "s1", DeviceID: "t1", DrawerID: "d1"},
		{ID: "p6", OrderID: "v1", At: at(41), Tender: TenderCash, AmountCents: 954, Refund: true, StaffID: "s1", DeviceID: "t1", DrawerID: "d1"},
		{ID: "p7", OrderID: "c2", At: at(50), Tender: TenderSquareApp, AmountCents: 500, StaffID: "s1", DeviceID: "t1"},
	}
	journal := []Entry{
		{Action: ActionOrderPlaced, DeviceID: "t1", OrderID: "c1", At: at(9)},
		{Action: ActionOrderPlaced, DeviceID: "t1", OrderID: "c2", At: at(12)},
		{Action: ActionOrderPlaced, DeviceID: "t1", OrderID: "v1", At: at(39)},
	}
	z := ZInput{BatchID: 7, OpenedAt: tillT0, ClosedAt: at(400), Now: at(400), Loc: time.UTC, Open: true,
		Orders: []order.StoredOrder{c1, c2, o1, o2, o3, v1}, Payments: pays, Drawers: []Drawer{d1}, Journal: journal,
		Drivers: []order.DriverReport{{DriverID: "dr", TipCents: 300}}}
	names := map[string]string{"s1": "Cara S.", "s2": "Ben"}

	b := BuildTill(TillInput{Z: z, Store: "Lombardi's", Categories: cats, StaffNames: names})
	checkTill(t, "batch", b)
	if b.Scope != TillScopeBatch || b.State != "open" || b.Batch.ClosedAt != nil || b.Drawer != nil || b.Store != "Lombardi's" {
		t.Fatalf("header %+v", b)
	}
	if got := b.Summary; got.GrossSales != 2000+1000+1500+1800+1000+500 || got.Adjustments != -600 || got.Refunds != -300 ||
		got.DeliveryFees != 800 || got.TipsCollected != 200+300 || got.TipsPaid != 300 || got.PaidInOut != 500 {
		t.Fatalf("summary %+v", got)
	}
	// Pizza first (position), wings, then Open items + Other.
	cs := b.Detail.Categories
	if len(cs) != 4 || cs[0].Name != "Pizza" || cs[0].Count != 3 || cs[0].Amount != 4500 || cs[1].Name != "Wings" || cs[1].Count != 2 ||
		cs[2].Name != CategoryOpenItems || cs[3].Name != CategoryOther || cs[3].Amount != 1800 {
		t.Fatalf("categories %+v", cs)
	}
	if b.Detail.Cards[3].Brand != BrandVisa || b.Detail.Cards[3].Amount != 2200 || b.Detail.Cards[4].Amount != 500 || b.Detail.Cards[4].Label != "Credit Other" {
		t.Fatalf("cards %+v", b.Detail.Cards)
	}
	// Cash: 544 - 300 + 530 + 954 - 954.
	if b.Detail.Cash.Amount != 774 || b.Detail.Cash.Count != 3 {
		t.Fatalf("cash %+v", b.Detail.Cash)
	}
	if len(b.Detail.Adjustments) != 1 || b.Detail.Adjustments[0].Amount != -600 || b.Detail.Adjustments[0].Kind != "typed" {
		t.Fatalf("adjustments %+v", b.Detail.Adjustments)
	}
	// c2: 1590 due, 500 taken = 1090 unpaid; o2 delivered cash = driver cash.
	if b.Summary.Unpaid != 1090 {
		t.Fatalf("unpaid %d", b.Summary.Unpaid)
	}

	// Till 1's drawer: c1, c2, v1 placed there; o3's cash was Till 2's (no drawer there: not here).
	d := BuildTill(TillInput{Z: z, Store: "Lombardi's", Drawer: &d1, Categories: cats, StaffNames: names})
	checkTill(t, "drawer", d)
	if d.Scope != TillScopeDrawer || d.Drawer == nil || d.Drawer.DeviceName != "Till 1" || d.State != "closed" || !d.To.Equal(closed) {
		t.Fatalf("drawer header %+v", d)
	}
	if d.Summary.GrossSales != 3000+1500 || d.Summary.TotalCash != 544-300 || d.Summary.TotalCreditCard != 2700 ||
		len(d.Detail.Employees) != 1 || d.Detail.Employees[0].Name != "Cara S." || d.Summary.PaidInOut != 500 {
		t.Fatalf("drawer %+v / %+v", d.Summary, d.Detail.Employees)
	}

	// Terminal scope (the owner, 2026-11-12: drawer A = counter A): every payment on t1 in the batch.
	tt := BuildTill(TillInput{Z: z, Terminal: &TillDevice{DeviceID: "t1", DeviceName: "Till 1"}, Categories: cats, StaffNames: names})
	checkTill(t, "terminal", tt)
	if tt.Scope != TillScopeTerminal || tt.Terminal.DeviceName != "Till 1" || tt.Drawer == nil || tt.Drawer.ID != "d1" ||
		tt.Summary.GrossSales != 3000+1500 || tt.Summary.TotalCash != 544-300 || tt.Summary.TotalCreditCard != 2700 ||
		tt.Summary.StartingCash != 10000 || d.Summary.StartingCash != 10000 {
		t.Fatalf("terminal %+v %+v", tt.Summary, tt.Drawer)
	}
}

// Random days: the identity holds for the batch and for every drawer.
func TestTillProperty(t *testing.T) {
	cats := tillCats()
	var ids []string
	for id := range cats.of {
		ids = append(ids, id)
	}
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		at := func() time.Time { return tillT0.Add(time.Duration(rng.Intn(600)) * time.Minute) }
		var orders []order.StoredOrder
		var pays []Payment
		var journal []Entry
		devices := []string{"t1", "t2", "web"}
		var drawers []Drawer
		for i, dev := range devices[:2] {
			c := tillT0.Add(600 * time.Minute)
			drawers = append(drawers, Drawer{ID: fmt.Sprintf("d%d", i), DeviceID: dev, OpenedAt: tillT0, ClosedAt: &c, State: "closed"})
		}
		for i := 0; i < 1+rng.Intn(25); i++ {
			id := fmt.Sprintf("o%d", i)
			var items []order.LineItem
			for j := 0; j < 1+rng.Intn(4); j++ {
				items = append(items, tline(ids[rng.Intn(len(ids))], 1+rng.Intn(3), int64(100+rng.Intn(3000))))
			}
			src := order.SourceCounter
			if rng.Intn(3) == 0 {
				src = "online"
			}
			ful := []string{"dine_in", "pickup", "delivery"}[rng.Intn(3)]
			state := []order.State{order.StateAccepted, order.StateReady, order.StateDelivered, order.StateCancelled}[rng.Intn(4)]
			var sum []order.PaymentSummary
			tip := int64(0)
			if src != order.SourceCounter && rng.Intn(2) == 0 {
				tip = int64(rng.Intn(500))
			}
			so := tillOrder(id, src, ful, state, items, int64(rng.Intn(300)), int64(rng.Intn(2))*400, int64(rng.Intn(200)), tip, sum)
			if src != order.SourceCounter && rng.Intn(2) == 0 {
				so.Order.Payments = []order.PaymentSummary{{Method: "card", AmountCents: so.Order.Money.Total}}
			}
			if state == order.StateCancelled && src == order.SourceCounter {
				so.Status.CancelReason = VoidReason
			}
			orders = append(orders, so)
			dev := devices[rng.Intn(3)]
			if src == order.SourceCounter {
				journal = append(journal, Entry{Action: ActionOrderPlaced, DeviceID: dev, OrderID: id, At: at()})
			}
			// Payments up to the balance, refunds up to what was paid.
			due := so.Order.CashDue()
			if src == order.SourceCounter {
				due = so.Order.Money.Total
			}
			var paid int64
			for k := 0; k < rng.Intn(4); k++ {
				tender := []string{TenderCash, TenderCard, TenderSquareApp, TenderManualCard, TenderComp}[rng.Intn(5)]
				pdev := devices[rng.Intn(3)]
				p := Payment{ID: fmt.Sprintf("%s-%d", id, k), OrderID: id, At: at(), Tender: tender, StaffID: []string{"s1", "s2", ""}[rng.Intn(3)],
					DeviceID: pdev, Brand: []string{"", BrandVisa, BrandAmex, BrandOther}[rng.Intn(4)]}
				if tender == TenderCash && pdev != "web" {
					p.DrawerID = "d" + pdev[1:2]
					if pdev == "t1" {
						p.DrawerID = "d0"
					} else {
						p.DrawerID = "d1"
					}
				}
				if rng.Intn(3) == 0 && paid > 0 {
					p.Refund, p.AmountCents = true, 1+rng.Int63n(paid)
					paid -= p.AmountCents
				} else if due-paid > 0 {
					p.AmountCents = 1 + rng.Int63n(due-paid)
					p.TipCents = int64(rng.Intn(300))
					paid += p.AmountCents
				} else {
					continue
				}
				pays = append(pays, p)
			}
		}
		// General refunds (contract 2.10x): cash back with no order.
		var general int64
		for k := 0; k < rng.Intn(3); k++ {
			pdev := devices[rng.Intn(3)]
			p := Payment{ID: fmt.Sprintf("g-%d", k), At: at(), Tender: TenderCash, Refund: true, RefundKind: RefundGeneral,
				AmountCents: 1 + rng.Int63n(5000), StaffID: "s1", DeviceID: pdev}
			if pdev == "t1" {
				p.DrawerID = "d0"
			} else if pdev == "t2" {
				p.DrawerID = "d1"
			}
			general += p.AmountCents
			pays = append(pays, p)
		}
		z := ZInput{BatchID: 1, OpenedAt: tillT0, ClosedAt: tillT0.Add(700 * time.Minute), Now: tillT0.Add(700 * time.Minute), Loc: time.UTC,
			Orders: orders, Payments: pays, Drawers: drawers, Journal: journal}
		batch := BuildTill(TillInput{Z: z, Categories: cats})
		if batch.Detail.GeneralRefunds.Amount != -general {
			t.Fatalf("seed %d: general refunds %+v, want -%d", seed, batch.Detail.GeneralRefunds, general)
		}
		checkTill(t, fmt.Sprintf("seed %d batch", seed), batch)
		for i := range drawers {
			checkTill(t, fmt.Sprintf("seed %d drawer %d", seed, i), BuildTill(TillInput{Z: z, Drawer: &drawers[i], Categories: cats}))
		}
		for _, dev := range devices {
			checkTill(t, fmt.Sprintf("seed %d terminal %s", seed, dev), BuildTill(TillInput{Z: z, Terminal: &TillDevice{DeviceID: dev}, Categories: cats}))
		}
	}
}
