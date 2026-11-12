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
	r.Finish(600)
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
		{"POS paid card: tip counts", StoredOrder{Order: Snapshot{Source: SourcePOS, Money: Money{Total: 3000, Tip: 400}},
			Status: Status{ExtSource: ExtSourceSoftTouch, Paid: true, TipCents: 400}}, false, 0, 400, -376},
		{"POS cash: the hub's cash due, tip ignored", StoredOrder{Order: Snapshot{Source: SourcePOS, Money: Money{Total: 3000, Tip: 400}},
			Status: Status{ExtSource: ExtSourceSoftTouch, CashDueCents: 2600, TipCents: 400}}, true, 2600, 0, 2600},
		{"POS unpaid, nothing due: no tip", StoredOrder{Order: Snapshot{Source: SourcePOS, Money: Money{Total: 3000}},
			Status: Status{ExtSource: ExtSourceSoftTouch, TipCents: 300}}, true, 0, 0, 0},
	} {
		l := NewDriverReportLine(tc.so)
		if l.Cash != tc.cash || l.CashCents != tc.cashC || l.TipCents != tc.tipC {
			t.Errorf("%s: line = %+v, want cash=%v %d tip %d", tc.name, l, tc.cash, tc.cashC, tc.tipC)
		}
		r := DriverReport{Lines: []DriverReportLine{l}}
		r.Finish(600)
		if r.OwedCents != tc.owedWith {
			t.Errorf("%s: owed = %d, want %d", tc.name, r.OwedCents, tc.owedWith)
		}
	}
}

// The delivery fee split: the driver's share of each order's delivery fee
// is on its line and summed on the report; the owed total doesn't count it.
func TestDriverReportLine_DriverShare(t *testing.T) {
	fee := []Fee{{Type: "delivery", Label: "Delivery fee", Amount: 400, DriverCents: 300}}
	a := NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 2400, Fees: fee}}})
	b := NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 2000}}}) // no split known
	if a.DriverShareCents != 300 || b.DriverShareCents != 0 {
		t.Fatalf("lines: %d %d", a.DriverShareCents, b.DriverShareCents)
	}
	r := DriverReport{Lines: []DriverReportLine{a, b}}
	r.Finish(0)
	if r.DriverShareCents != 300 || r.OwedCents != 4400 {
		t.Fatalf("report: share %d owed %d", r.DriverShareCents, r.OwedCents)
	}
	// A share above the fee counts only the fee.
	over := Money{Fees: []Fee{{Type: "delivery", Amount: 200, DriverCents: 300}, {Type: "service", Amount: 99, DriverCents: 50}}}
	if got := over.DeliveryDriverCents(); got != 200 {
		t.Fatalf("capped share = %d", got)
	}
}
