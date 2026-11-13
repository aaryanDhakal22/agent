package receipt

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// An online delivery's tickets (§2.10aa) = the terminal's delivery layout:
// Merchant Copy then Customer Copy, each cut. Goldens
// testdata/delivery_copies_{card,cash}.golden (-update) + readable checks.

func deliverySnap(card bool) order.Snapshot {
	n := int64(1104)
	no := false
	s := order.Snapshot{ID: "o-d", Source: "online", SourceEventID: "V-5521", KitchenNumber: &n,
		PlacedAt: time.Date(2026, 11, 12, 22, 40, 0, 0, time.UTC),
		Customer: order.Customer{DisplayName: "Maria Lopez", Phone: "4105550100"},
		Fulfillment: order.Fulfillment{Type: "delivery", Address: &order.Address{Line1: "12 Test St", Line2: "4B", City: "Towson",
			State: "MD", PostalCode: "21204", Notes: "Side door"}},
		Items: []order.LineItem{
			{Name: "Cheese Pizza", Size: "Large", Quantity: 1, UnitPrice: 1200, LineTotal: 1300, Children: []order.LineItem{
				{Name: "Pepperoni", Quantity: 1, UnitPrice: 100}, {Name: "Well Done", Quantity: 1, ShowOnDriverTicket: &no}}},
			{Name: "Wings", Quantity: 2, UnitPrice: 300, LineTotal: 600, Notes: "extra hot"},
		},
		Money: order.Money{Subtotal: 1900, Tax: 190, Tip: 300, Total: 2790,
			Fees: []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 400}}},
		Notes: "Ring twice",
	}
	if card {
		s.Payments = []order.PaymentSummary{{Method: "card", AmountCents: 2790, Last4: "1881"}}
	}
	return s
}

func deliveryStoreCtx() storeconfig.StoreContext {
	return storeconfig.StoreContext{Location: storeconfig.LocationContext{Name: "Lombardi's", Timezone: "America/New_York",
		Phone: "4103219288", AddressLine1: "931 Taylor Ave", AddressLine2: "Towson, MD 21286",
		Config: map[string]any{"website": "lombardis.example", "receipt_footer": "Thank You\nPlease Come Again"}}}
}

var escSeq = regexp.MustCompile(`(?s)\x1b@|\x1b[a!E-].|\x1d[!B].|\x1dVB.`)

// textOf is the bytes as the paper reads (commands and the logo left out).
func textOf(b []byte) string {
	if i := bytes.Index(b, []byte("\x1dv0")); i >= 0 {
		// the logo raster: cut each one out up to its next line feed after the image
		var out []byte
		for {
			i = bytes.Index(b, []byte("\x1dv0"))
			if i < 0 {
				out = append(out, b...)
				break
			}
			out = append(out, b[:i]...)
			w := int(b[i+4]) | int(b[i+5])<<8
			h := int(b[i+6]) | int(b[i+7])<<8
			b = b[i+8+w*h:]
		}
		b = out
	}
	return escSeq.ReplaceAllString(string(b), "")
}

func TestDeliveryCopies(t *testing.T) {
	prevNow := deliveryNow
	deliveryNow = func() time.Time { return time.Date(2026, 11, 12, 22, 41, 0, 0, time.UTC) }
	defer func() { deliveryNow = prevNow }()
	prefs := order.DefaultReceiptPrefs()
	for _, card := range []bool{true, false} {
		name := "cash"
		if card {
			name = "card"
		}
		got := Build(deliverySnap(card), deliveryStoreCtx(), printer.Ticket{Kind: printer.TicketDeliveryCopies}, prefs)
		golden(t, "testdata/delivery_copies_"+name+".golden", got)
		txt := textOf(got)
		// SoftTouch's slip (the owner, 2026-11-12): Phone / Street / City (Company, Complex, Zone
		// Code, Gate Code went after the owner saw it printed), Directions, LAST , FIRST, the padded
		// time, a rule above and below the Order #, the Transaction ID's last 10 characters.
		for _, want := range []string{"Merchant Copy", "Customer Copy", "Order #: 1104", "DELIVERY", "LOPEZ , MARIA",
			"Phone      : (410) 555-0100", "Street     : 12 Test St, APT 4B", "City       : TOWSON, MD 21204",
			"Directions: Side door", "Notes: Ring twice", "Date: 11/12/26", "Time: 05:41 pm",
			"LARGE CHEESE PIZZA", "Pepperoni", "Note: extra hot", "Delivery:", "Tip:", "Order Online", "lombardis.example"} {
			if !strings.Contains(txt, want) {
				t.Errorf("%s: no %q in\n%s", name, want, txt)
			}
		}
		for _, gone := range []string{"Company", "Complex", "Zone Code", "Gate Code"} {
			if strings.Contains(txt, gone) {
				t.Errorf("%s: %q is gone from the slip", name, gone)
			}
		}
		if rule := strings.Repeat("-", 48); !strings.Contains(txt, rule+"\nOrder #: 1104\n"+rule+"\n") {
			t.Errorf("%s: the Order # between two rules\n%s", name, txt)
		}
		if strings.Contains(txt, "Well Done") {
			t.Errorf("%s: a $0 choice not for the driver printed", name)
		}
		if strings.Count(string(got), cmdCut) != 2 {
			t.Errorf("%s: %d cuts, want 2", name, strings.Count(string(got), cmdCut))
		}
		collect := strings.Contains(txt, "Collect Cash $27.90")
		if collect == card || (card && (!strings.Contains(txt, "Card $27.90") || !strings.Contains(txt, "XXXXXXXXXXXX1881 Card"))) {
			t.Errorf("%s: payment lines wrong:\n%s", name, txt)
		}
	}
}
