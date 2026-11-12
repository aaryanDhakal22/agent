package receipt

import (
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// A delivery of the store's own (a call center's, or a terminal's) paid with a
// typed card + the driver's tip, then more added and charged on a second typed
// card (staging, 2026-11-12: $16.44 + $3.00 tip, then $18.81): the copies show
// the tip (it stays on the payment, never in Money), the Total that was
// charged, each card with what it was charged and MANUALLY ENTERED, and
// Square's payment id as the Transaction ID — never the order's own key.
func TestKeyedDeliveryOfTheStoresOwnOrderPrintsTipEntryAndSquareID(t *testing.T) {
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	d := deliverySnap(false)
	d.Source, d.SourceEventID, d.TakenBy = order.SourceCounter, "ccd-0f3a9c-key", "Call center - Cece"
	d.Money.Tip, d.Money.Total = 0, 3525 // a counter order's money carries no tip
	d.Payments = []order.PaymentSummary{
		{Method: "card", AmountCents: 1644, TipCents: 300, Last4: "1111", Tender: "manual_card", Entry: "keyed", Ref: "s9EkUvdx48VTZ3C3MZVZOXVpwpZZY"},
		{Method: "card", AmountCents: 1881, Last4: "1111", Tender: "manual_card", Entry: "keyed", Ref: "4RDCNrVavBjANQ7boscUFg60hoXZY"},
	}
	for _, cols := range []int{0, 42, 32} {
		text := textOf(Build(d, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front", Cols: cols}, prefs))
		for want, n := range map[string]int{
			"Tip:":                 2, // both copies
			"$3.00":                2,
			"$38.25":               2, // the Total: $35.25 + the $3.00 tip
			"Card XXXX1111 $19.44": 2, // the first card: charged amount + tip
			"Card XXXX1111 $18.81": 2,
			"MANUALLY ENTERED":     4, // under each card line, both copies
			"ccd-0f3a9c-key":       0, // never the order's own key when a card has Square's id
			"Collect Cash":         0,
		} {
			if got := strings.Count(strings.ReplaceAll(text, "\n", " "), want); got != n {
				t.Errorf("cols %d: %q printed %d times, want %d\n%s", cols, want, got, n, text)
			}
		}
		// the last card's Square id (it wraps under the label on narrow paper)
		flat := strings.NewReplacer(" ", "", "\n", "").Replace(text)
		if n := strings.Count(flat, "TransactionID:4RDCNrVavBjANQ7boscUFg60hoXZY"); n != 2 {
			t.Errorf("cols %d: Square's id as the Transaction ID %d times, want 2\n%s", cols, n, text)
		}
	}
	// The pickup's customer receipt says how the card was taken and adds the tip too.
	p := d
	p.Fulfillment = order.Fulfillment{Type: "pickup"}
	text := textOf(Build(p, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs))
	for _, want := range []string{"MANUALLY ENTERED", "$3.00", "$38.25"} {
		if !strings.Contains(text, want) {
			t.Errorf("customer receipt: no %q\n%s", want, text)
		}
	}
	// A website order's money already holds its tip: nothing is added twice.
	w := deliverySnap(true)
	w.Payments[0].TipCents = 300
	wt := textOf(Build(w, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front"}, prefs))
	if !strings.Contains(wt, "$27.90") || strings.Contains(wt, "$30.90") || !strings.Contains(wt, "V-5521") {
		t.Errorf("a website order's copy changed\n%s", wt)
	}
}

// A store terminal's card (a reader tap) and the Square app print their own words.
func TestEntryLabel(t *testing.T) {
	for _, c := range []struct {
		p    order.PaymentSummary
		want string
	}{
		{order.PaymentSummary{Method: "card", Tender: "manual_card"}, "MANUALLY ENTERED"},
		{order.PaymentSummary{Method: "card", Tender: "manual_card", Entry: "keyed"}, "MANUALLY ENTERED"},
		{order.PaymentSummary{Method: "card", Tender: "card", Entry: "contactless"}, "CONTACTLESS"},
		{order.PaymentSummary{Method: "card", Tender: "square_app", Entry: "contactless"}, "SQUARE APP - TAP TO PAY"},
		{order.PaymentSummary{Method: "card", Tender: "square_app"}, "SQUARE APP"},
		{order.PaymentSummary{Method: "card"}, ""},
		{order.PaymentSummary{Method: "cash", Tender: "cash"}, ""},
	} {
		if got := c.p.EntryLabel(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.p, got, c.want)
		}
	}
}
