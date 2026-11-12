package order

import "testing"

// 2.5.0 website orders on a driver report: a card order's tip = the
// checkout tip + the tip added on the tablet, pending until main charged it;
// a cash website order ignores any added tip (there can't be one).
func TestDriverReportLine_WebsiteOrders(t *testing.T) {
	card := []PaymentSummary{{Method: "card", AmountCents: 3000}}
	web := func(payments []PaymentSummary, added int64, locked bool) StoredOrder {
		return StoredOrder{
			Order:  Snapshot{Source: SourceWeb, Money: Money{Total: 3000, Tip: 300}, Payments: payments},
			Status: Status{State: StateDelivered, AddedTipCents: added, TipLocked: locked},
		}
	}
	l := NewDriverReportLine(web(card, 400, false))
	if l.Cash || l.TipCents != 700 || l.OrderTipCents != 700 || l.SlipTotalCents != 3400 || l.AddedTipCents != 400 || !l.TipPending {
		t.Fatalf("card web, pending: %+v", l)
	}
	locked := NewDriverReportLine(web(card, 200, true))
	if locked.TipCents != 500 || locked.TipPending || locked.AddedTipCents != 200 {
		t.Fatalf("card web, locked: %+v", locked)
	}
	cash := NewDriverReportLine(web(nil, 0, false))
	if !cash.Cash || cash.CashCents != 2700 || cash.TipCents != 0 || cash.TipPending {
		t.Fatalf("cash web: %+v", cash)
	}
	// The vendor's card order never shows pending (main doesn't hold it).
	online := web(card, 0, false)
	online.Order.Source = SourceOnline
	if o := NewDriverReportLine(online); o.TipPending || o.TipCents != 300 {
		t.Fatalf("online card: %+v", o)
	}
	r := DriverReport{Lines: []DriverReportLine{l, locked, cash, NewDriverReportLine(online)}}
	r.Finish(0)
	if r.TipCents != 1500 || r.PendingTipCents != 700 || r.CashCents != 2700 {
		t.Fatalf("report: tips %d pending %d cash %d", r.TipCents, r.PendingTipCents, r.CashCents)
	}
}

func TestStoredOrder_TippableAndSource(t *testing.T) {
	card := []PaymentSummary{{Method: "card", AmountCents: 3000}}
	so := StoredOrder{Order: Snapshot{Source: "web", Money: Money{Total: 3000}, Payments: card}, Status: Status{State: StateDelivered}}
	if !so.Tippable() || so.SourceLabel() != "web" {
		t.Fatalf("card web: tippable %v source %s", so.Tippable(), so.SourceLabel())
	}
	for name, f := range map[string]func(*StoredOrder){
		"cash":      func(o *StoredOrder) { o.Order.Payments = nil },
		"locked":    func(o *StoredOrder) { o.Status.TipLocked = true },
		"cancelled": func(o *StoredOrder) { o.Status.State = StateCancelled },
		"online":    func(o *StoredOrder) { o.Order.Source = "online" },
	} {
		x := so
		f(&x)
		if x.Tippable() {
			t.Errorf("%s: tippable", name)
		}
	}
	so.Order.Source = "online"
	if so.SourceLabel() != "online" {
		t.Errorf("online label %s", so.SourceLabel())
	}
}
