package order

import "testing"

func TestCashDue(t *testing.T) {
	total := Money{Total: 3000, Tip: 500}
	for _, tc := range []struct {
		name     string
		payments []PaymentSummary
		want     int64
	}{
		{"no payments = a cash order", nil, 3000},
		{"explicit cash", []PaymentSummary{{Method: "cash", AmountCents: 3000}}, 3000},
		{"card", []PaymentSummary{{Method: "card", AmountCents: 3000}}, 0},
		{"part-paid: card + cash owing", []PaymentSummary{{Method: "card", AmountCents: 1000}, {Method: "CASH", AmountCents: 2000}}, 2000},
		{"third party / invoice", []PaymentSummary{{Method: "other", AmountCents: 3000}}, 0},
	} {
		s := Snapshot{Money: total, Payments: tc.payments}
		if got := s.CashDue(); got != tc.want {
			t.Errorf("%s: CashDue = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The driver report counted only explicit cash payments, so a vendor cash
// order (no payments) came out as $0 cash and "store owes driver". Since
// 2.1.0 the tip on a cash order is the driver's already (not listed).
func TestDriverReportLine_CashOrderWithoutPayments(t *testing.T) {
	l := NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 3000, Tip: 500}}})
	if !l.Cash || l.CashCents != 2500 || l.TipCents != 0 {
		t.Fatalf("line = %+v, want cash 2500 (total - tip) tip 0", l)
	}
	r := DriverReport{Lines: []DriverReportLine{l}}
	r.Finish(600, 0) // no delivery pay: the cash rule alone
	if r.OwedCents != 2500 {
		t.Errorf("owed = %d, want 2500", r.OwedCents)
	}
}

// 2.1.0 tip rules: tips only on card-paid deliveries (online no cash due,
// POS paid with no cash due); cash = cash due less the tip.
func TestDriverReportLine_TipRules(t *testing.T) {
	card := []PaymentSummary{{Method: "card", AmountCents: 3000}}
	for _, tc := range []struct {
		name     string
		so       StoredOrder
		cash     bool
		cashC    int64
		tipC     int64
		owedWith int64 // Finish(600) on this line alone
	}{
		{"online card: tip counts", StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000, Tip: 500}, Payments: card}},
			false, 0, 500, -470},
		{"online cash: cash - tip, tip dropped", StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000, Tip: 500}}},
			true, 2500, 0, 2500},
		{"online cash tip above cash due: floored at 0", StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000, Tip: 2500},
			Payments: []PaymentSummary{{Method: "card", AmountCents: 1000}, {Method: "cash", AmountCents: 2000}}}}, true, 0, 0, 0},
	} {
		l := NewDriverReportLine(tc.so)
		if l.Cash != tc.cash || l.CashCents != tc.cashC || l.TipCents != tc.tipC {
			t.Errorf("%s: line = %+v, want cash=%v %d tip %d", tc.name, l, tc.cash, tc.cashC, tc.tipC)
		}
		r := DriverReport{Lines: []DriverReportLine{l}}
		r.Finish(600, 0) // no delivery pay: the tip rules alone
		if r.OwedCents != tc.owedWith {
			t.Errorf("%s: owed = %d, want %d", tc.name, r.OwedCents, tc.owedWith)
		}
	}
}

// The owner's settlement rule (2.5.2): hand in = cash collected − the store's
// delivery pay × EVERY delivered delivery (a vendor order with no fee line and
// a waived fee too) − card tips after the tip tax; negative = the store owes
// the driver.
func TestDriverReport_Settlement(t *testing.T) {
	fee := []Fee{{Type: "delivery", Label: "Delivery fee", Amount: 400, DriverCents: 300}}
	card := []PaymentSummary{{Method: "card", AmountCents: 3000}}
	lines := []DriverReportLine{
		NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 2400, Fees: fee}}}),                                  // cash 24.00, with a fee
		NewDriverReportLine(StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 2000}}}),                           // vendor cash, no fee line
		NewDriverReportLine(StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000, Tip: 500}, Payments: card}}), // card, $5 tip
		NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 1500, Fees: []Fee{{Type: "delivery", Amount: 0}}}}}), // fee waived
	}
	r := DriverReport{Lines: lines}
	r.Finish(600, 300)
	// cash 24 + 20 + 15 = 59.00; pay 4 × 3 = 12.00; tips 5.00 − 6% = 4.70 → 42.30
	if r.Deliveries != 4 || r.CashCents != 5900 || r.DriverShareCents != 1200 || r.DriverPayCents != 300 ||
		r.TipAfterTaxCents != 470 || r.OwedCents != 4230 {
		t.Fatalf("report: %+v", r)
	}
	for _, l := range r.Lines {
		if l.DriverShareCents != 300 {
			t.Fatalf("every line pays $3: %+v", l)
		}
	}
	// All card: the store owes him his pay and his tips.
	owes := DriverReport{Lines: []DriverReportLine{lines[2]}}
	owes.Finish(600, 300)
	if owes.OwedCents != -770 || owes.OwedRoundedCents != -800 {
		t.Fatalf("store owes: %d %d", owes.OwedCents, owes.OwedRoundedCents)
	}
	// Another store's pay; negative pay reads as 0.
	r.Finish(600, 250)
	if r.DriverShareCents != 1000 || r.OwedCents != 4430 {
		t.Fatalf("pay 2.50: %+v", r)
	}
	r.Finish(600, -5)
	if r.DriverShareCents != 0 {
		t.Fatalf("negative pay: %d", r.DriverShareCents)
	}
	// A share above the fee counts only the fee.
	over := Money{Fees: []Fee{{Type: "delivery", Amount: 200, DriverCents: 300}, {Type: "service", Amount: 99, DriverCents: 50}}}
	if got := over.DeliveryDriverCents(); got != 200 {
		t.Fatalf("capped share = %d", got)
	}
}

// A tip a terminal took on an online / website card order (a tip added at the counter or afterwards:
// Status.TipCents) is the driver's too (2.5.2); a cash order's stays out.
func TestDriverReportLine_TerminalTipOnOnlineOrder(t *testing.T) {
	card := []PaymentSummary{{Method: "card", AmountCents: 3000}}
	l := NewDriverReportLine(StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000, Tip: 500}, Payments: card},
		Status: Status{TipCents: 300}})
	if l.TipCents != 800 || l.Cash {
		t.Fatalf("online card + terminal tip: %+v", l)
	}
	c := NewDriverReportLine(StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000}}, Status: Status{TipCents: 300}})
	if !c.Cash || c.TipCents != 0 {
		t.Fatalf("online cash: %+v", c)
	}
}
