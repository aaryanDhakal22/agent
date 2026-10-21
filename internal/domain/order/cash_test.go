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
// order (no payments) came out as $0 cash and "store owes driver".
func TestDriverReportLine_CashOrderWithoutPayments(t *testing.T) {
	l := NewDriverReportLine(StoredOrder{Order: Snapshot{Money: Money{Total: 3000, Tip: 500}}})
	if !l.Cash || l.CashCents != 3000 || l.TipCents != 500 {
		t.Fatalf("line = %+v, want cash 3000 tip 500", l)
	}
	r := DriverReport{Lines: []DriverReportLine{l}}
	r.Finish(600)
	if r.OwedCents != 3000-470 {
		t.Errorf("owed = %d, want %d", r.OwedCents, 3000-470)
	}
}
