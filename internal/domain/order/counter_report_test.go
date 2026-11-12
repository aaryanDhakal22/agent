package order

import "testing"

// A terminal's delivery on the driver report (the owner, 2026-11-12): card
// tips taken with its payments are his; a comp lists at $0; a split
// delivery (card + cash at the door) owes only the cash part.
func TestCounterDeliveryReportLine(t *testing.T) {
	so := StoredOrder{}
	so.Order = Snapshot{Source: SourceCounter, Money: Money{Total: 3000}, Fulfillment: Fulfillment{Type: "delivery"},
		Payments: []PaymentSummary{{Method: "card", AmountCents: 2000}}}
	so.Status.TipCents = 400
	l := NewDriverReportLine(so)
	if !l.Cash || l.CashCents != 1000 || l.TipCents != 400 || l.TotalCents != 3000 || l.SlipTotalCents != 3000 {
		t.Fatalf("split: %+v", l)
	}
	so.Order.Payments = []PaymentSummary{{Method: "other", AmountCents: 3000}} // comped
	so.Status.TipCents = 0
	l = NewDriverReportLine(so)
	if l.Cash || l.CashCents != 0 || l.TotalCents != 0 || l.SlipTotalCents != 0 {
		t.Fatalf("comp: %+v", l)
	}
	so.Order.Payments = []PaymentSummary{{Method: "card", AmountCents: 3000}} // manually entered card + tip
	so.Status.TipCents = 500
	l = NewDriverReportLine(so)
	if l.Cash || l.TipCents != 500 || l.SlipTotalCents != 3500 {
		t.Fatalf("card: %+v", l)
	}
}
