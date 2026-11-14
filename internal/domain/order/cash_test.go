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
	r.Finish(600, 0, false) // no delivery pay: the cash rule alone
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
		r.Finish(600, 0, false) // no delivery pay: the tip rules alone
		if r.OwedCents != tc.owedWith {
			t.Errorf("%s: owed = %d, want %d", tc.name, r.OwedCents, tc.owedWith)
		}
	}
}

// The owner's settlement rule (2.5.8, default): hand in = cash collected −
// card tips after the tip tax; the delivery pay is paid weekly, so only the
// count shows (no share, no pay). With the store's driver_pay_from_cash on
// (the 2.5.2 rule) the store's delivery pay × EVERY delivered delivery (a
// vendor order with no fee line and a waived fee too) comes off as well;
// negative = the store owes the driver.
func TestDriverReport_Settlement(t *testing.T) {
	fee := []Fee{{Type: "delivery", Label: "Delivery fee", Amount: 400, DriverCents: 300}}
	card := []PaymentSummary{{Method: "card", AmountCents: 3000}}
	lines := []DriverReportLine{
		NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 2400, Fees: fee}}}),                                  // cash 24.00, with a fee
		NewDriverReportLine(StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 2000}}}),                           // vendor cash, no fee line
		NewDriverReportLine(StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000, Tip: 500}, Payments: card}}), // card, $5 tip
		NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 1500, Fees: []Fee{{Type: "delivery", Amount: 0}}}}}), // fee waived
	}
	// Default (off): cash 24 + 20 + 15 = 59.00; tips 5.00 − 6% = 4.70 → 54.30; the pay isn't on it.
	r := DriverReport{Lines: append([]DriverReportLine(nil), lines...)}
	r.Finish(600, 300, false)
	if r.Deliveries != 4 || r.CashCents != 5900 || r.DriverShareCents != 0 || r.DriverPayCents != 0 || r.DriverPayFromCash ||
		r.TipAfterTaxCents != 470 || r.OwedCents != 5430 || r.OwedRoundedCents != 5400 {
		t.Fatalf("report (paid weekly): %+v", r)
	}
	for _, l := range r.Lines {
		if l.DriverShareCents != 0 {
			t.Fatalf("no line carries a pay when it is paid weekly: %+v", l)
		}
	}
	// All card: the store owes him his tips only.
	owes := DriverReport{Lines: []DriverReportLine{lines[2]}}
	owes.Finish(600, 300, false)
	if owes.OwedCents != -470 || owes.OwedRoundedCents != -500 {
		t.Fatalf("store owes (paid weekly): %d %d", owes.OwedCents, owes.OwedRoundedCents)
	}

	// driver_pay_from_cash on: pay 4 × 3 = 12.00 off too → 42.30.
	r = DriverReport{Lines: append([]DriverReportLine(nil), lines...)}
	r.Finish(600, 300, true)
	if r.Deliveries != 4 || r.CashCents != 5900 || r.DriverShareCents != 1200 || r.DriverPayCents != 300 || !r.DriverPayFromCash ||
		r.TipAfterTaxCents != 470 || r.OwedCents != 4230 {
		t.Fatalf("report (from the cash): %+v", r)
	}
	for _, l := range r.Lines {
		if l.DriverShareCents != 300 {
			t.Fatalf("every line pays $3: %+v", l)
		}
	}
	// All card: the store owes him his pay and his tips.
	owes = DriverReport{Lines: []DriverReportLine{lines[2]}}
	owes.Finish(600, 300, true)
	if owes.OwedCents != -770 || owes.OwedRoundedCents != -800 {
		t.Fatalf("store owes: %d %d", owes.OwedCents, owes.OwedRoundedCents)
	}
	// Another store's pay; negative pay reads as 0.
	r.Finish(600, 250, true)
	if r.DriverShareCents != 1000 || r.OwedCents != 4430 {
		t.Fatalf("pay 2.50: %+v", r)
	}
	r.Finish(600, -5, true)
	if r.DriverShareCents != 0 {
		t.Fatalf("negative pay: %d", r.DriverShareCents)
	}
	// Back off: the same report counted again drops the pay.
	r.Finish(600, 300, false)
	if r.DriverShareCents != 0 || r.OwedCents != 5430 {
		t.Fatalf("off again: %+v", r)
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

// Flow bug 3 (2.5.2): an online order edited at a terminal. The tip is inside the cash only when the
// order arrived unpaid; a card-paid order's tip stays on the card (a card tip on his report).
func TestDriverReportLine_EditedOnlineOrders(t *testing.T) {
	edited := func(arrivedTotal, arrivedTip, paid, total int64, pays []PaymentSummary) Snapshot {
		return Snapshot{Source: "online", Money: Money{Total: total, Tip: arrivedTip}, Payments: pays,
			SourceMeta: map[string]any{MetaPi: map[string]any{"online": OnlineBase{Money: Money{Total: arrivedTotal, Tip: arrivedTip}, PaidCents: paid}}}}
	}
	visa := []PaymentSummary{{Method: "card", AmountCents: 1499}}
	for _, tc := range []struct {
		name  string
		so    StoredOrder
		cash  bool
		cashC int64
		tipC  int64
	}{
		// 14.99 incl. a 2.00 tip paid by Visa, a Can Coke added (+1.35) left for the door
		{"card + tip + added item collected at the door", StoredOrder{Order: edited(1499, 200, 1499, 1634, visa)}, true, 135, 200},
		// the same, the 1.35 charged by card at the terminal
		{"card + tip + added item charged by card", StoredOrder{Order: edited(1499, 200, 1499, 1634, visa),
			Status: Status{TerminalMoney: true, CashDueCents: 0}}, false, 0, 200},
		{"online cash + tip", StoredOrder{Order: Snapshot{Source: "online", Money: Money{Total: 3000, Tip: 500}}}, true, 2500, 0},
		{"online cash + tip + added item", StoredOrder{Order: edited(3000, 500, 0, 3300, nil)}, true, 2800, 0},
	} {
		l := NewDriverReportLine(tc.so)
		if l.Cash != tc.cash || l.CashCents != tc.cashC || l.TipCents != tc.tipC {
			t.Errorf("%s: cash=%v %d tip %d, want cash=%v %d tip %d", tc.name, l.Cash, l.CashCents, l.TipCents, tc.cash, tc.cashC, tc.tipC)
		}
	}
}
