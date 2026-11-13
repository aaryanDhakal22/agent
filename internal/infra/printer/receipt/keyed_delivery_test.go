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
			"Tip:":                        2, // both copies
			"$3.00":                       2,
			"$38.25":                      2, // the Total: $35.25 + the $3.00 tip
			"Card $19.44":                 2, // the first card: charged amount + tip
			"Card $18.81":                 2,
			"XXXXXXXXXXXX1111 Card":       4, // a payment block per card, both copies
			"MANUALLY ENTERED":            4,
			"Cashier: Call center - Cece": 4,
			"ccd-0f3a9c-key":              0, // never the order's own key when a card has Square's id
			"Collect Cash":                0,
			"I agree to pay":              0, // a tip was taken: no signature block
		} {
			if got := strings.Count(strings.ReplaceAll(text, "\n", " "), want); got != n {
				t.Errorf("cols %d: %q printed %d times, want %d\n%s", cols, want, got, n, text)
			}
		}
		// each card's Square id: its last 10 characters, one line (the owner, 2026-11-12)
		for _, id := range []string{"ZOXVpwpZZY", "UFg60hoXZY"} {
			if n := strings.Count(text, "Transaction ID: "+id+"\n"); n != 2 {
				t.Errorf("cols %d: %s as a Transaction ID %d times, want 2\n%s", cols, id, n, text)
			}
		}
	}
	// The customer receipt (the Reprint layout, staging 2026-11-12): one card line per payment with
	// what it was charged + its entry + Square's id, ONE total (Total = Order Total, the tip in it),
	// and a reprint says REPRINT / REIMPRESION on top.
	for _, typ := range []string{"pickup", "delivery"} {
		p := d
		p.Fulfillment.Type = typ
		text := textOf(Build(p, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front", Reprint: true}, prefs))
		for want, n := range map[string]int{
			"MANUALLY ENTERED": 2, "XXXXXXXXXXXX1111 Card": 2, "Card $19.44": 1, "Card $18.81": 1, "Tip:": 1,
			"Transaction ID: ZOXVpwpZZY\n": 1, "Transaction ID: UFg60hoXZY\n": 1,
			"$38.25":  1, // the Total, the tip in it
			"$35.25":  0, // never the total without the tip
			"REPRINT": 1, "REIMPRESION": 1, "Customer Copy": 1,
		} {
			if got := strings.Count(text, want); got != n {
				t.Errorf("%s receipt: %q printed %d times, want %d\n%s", typ, want, got, n, text)
			}
		}
		if strings.Index(text, "REPRINT") > strings.Index(text, "Lombardi") {
			t.Errorf("%s receipt: the REPRINT band isn't on top\n%s", typ, text)
		}
		first := textOf(Build(p, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs))
		if strings.Contains(first, "REPRINT") {
			t.Errorf("%s: the first receipt says REPRINT", typ)
		}
	}
	// Printed again after a change: both copies say UPDATED / ACTUALIZADO on top; the first print doesn't.
	up := textOf(Build(d, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front", Updated: true}, prefs))
	if strings.Count(up, "UPDATED") != 2 || strings.Count(up, "ACTUALIZADO") != 2 {
		t.Errorf("updated copies: the band on each copy\n%s", up)
	}
	if first := textOf(Build(d, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front"}, prefs)); strings.Contains(first, "UPDATED") {
		t.Error("the copies at placement say UPDATED")
	}
	// A website order's money already holds its tip: nothing is added twice.
	w := deliverySnap(true)
	w.Payments[0].TipCents = 300
	wt := textOf(Build(w, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Station: "Front"}, prefs))
	if !strings.Contains(wt, "$27.90") || strings.Contains(wt, "$30.90") {
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
