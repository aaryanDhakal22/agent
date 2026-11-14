package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// Drink marks (the owner 2026-11-14, contract §2.1k): on a delivery's
// customer slips every line the Pi stamped a drink ends in "#####", whole on
// its last line or on a line of its own; never on a pickup or counter
// receipt, never on a kitchen ticket. Goldens testdata/widths/drinks_{48,42,32}.golden.

func drinksSnap(typ string) order.Snapshot {
	yes := true
	n := int64(100)
	combo := func(kind, sauce, dressing, can string) order.LineItem {
		return order.LineItem{Name: "8 Wings & Fries", Quantity: 1, UnitPrice: 1299, LineTotal: 1299, PrinterID: "p-fry", Routing: order.RoutingMatched,
			Children: []order.LineItem{{Name: kind, Quantity: 1}, {Name: sauce, Quantity: 1}, {Name: dressing, Quantity: 1},
				{Name: can, Quantity: 1, ShowOnDriverTicket: &yes, Drink: &yes}}}
	}
	return order.Snapshot{ID: "o-drinks", Source: "counter", SourceEventID: "K-100", KitchenNumber: &n,
		PlacedAt: time.Date(2026, 11, 14, 22, 40, 0, 0, time.UTC), TakenBy: "Khawar",
		Customer:    order.Customer{DisplayName: "Maria Lopez", Phone: "4105550100"},
		Fulfillment: order.Fulfillment{Type: typ, Address: &order.Address{Line1: "12 Test St", City: "Towson", State: "MD", PostalCode: "21204"}},
		Items: []order.LineItem{
			combo("Regular", "Hot Sauce", "Ranch", "W Can Coke"),
			combo("Boneless", "BBQ", "Blue Cheese", "W Can Sprite"),
			{Name: "2Lt Pepsi", Quantity: 1, UnitPrice: 365, LineTotal: 365, PrinterID: "p-front", Routing: order.RoutingMatched, Drink: &yes},
		},
		Money: order.Money{Subtotal: 2963, Tax: 178, Total: 3441, Fees: []order.Fee{{Type: "delivery", Label: "Delivery fee", Amount: 300}}},
	}
}

func TestDrinkMarks_DeliverySlips(t *testing.T) {
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	for _, cols := range []int{Cols80, Cols72, Cols58} {
		got := BuildDeliveryCopies(drinksSnap("delivery"), sc, prefs, printer.Paper{Cols: cols})
		golden(t, fmt.Sprintf("testdata/widths/drinks_%d.golden", cols), got)
		txt := textOf(got)
		if n := strings.Count(txt, order.DrinkMark); n != 6 { // 2 cans + the 2 liter, on both copies
			t.Errorf("%d cols: %d marks, want 6\n%s", cols, n, txt)
		}
		w := layoutFor(cols).cols
		if !strings.Contains(txt, "\n"+dleftRight("1   2Lt Pepsi #####", "$3.65", w)+"\n") {
			t.Errorf("%d cols: the 2 liter's line:\n%s", cols, txt)
		}
		sprite := "\n      W Can Sprite #####\n" // 18 characters: on 32 columns (16 for a choice) the marks go below
		if cols == Cols58 {
			sprite = "\n      W Can Sprite\n      #####\n"
		}
		if !strings.Contains(txt, "\n      W Can Coke #####\n") || !strings.Contains(txt, sprite) {
			t.Errorf("%d cols: the cans' lines:\n%s", cols, txt)
		}
		for _, l := range strings.Split(txt, "\n") {
			if len(l) > w {
				t.Errorf("%d cols: a line wider than the paper: %q", cols, l)
			}
			if strings.Contains(l, "##") && !strings.HasSuffix(strings.TrimRight(strings.TrimSuffix(strings.TrimRight(l, " "), dollarsTail(l)), " "), order.DrinkMark) {
				t.Errorf("%d cols: marks not at the end of the name: %q", cols, l)
			}
		}
		// A reprint of the delivery (the customer receipt) carries them too.
		if r := textOf(BuildCustomerReceipt(drinksSnap("delivery"), sc, prefs, printer.Paper{Cols: cols})); strings.Count(r, order.DrinkMark) != 3 {
			t.Errorf("%d cols: the delivery's customer receipt:\n%s", cols, r)
		}
	}
}

// dollarsTail is a line's price at its right end ("$3.65"), "" when none.
func dollarsTail(l string) string {
	l = strings.TrimRight(l, " ")
	if i := strings.LastIndex(l, " "); i >= 0 && strings.HasPrefix(l[i+1:], "$") {
		return l[i+1:]
	}
	return ""
}

// Deliveries only (the owner 2026-11-14): a pickup's and a counter order's
// receipts never carry the marks; the switch off prints none; a kitchen
// ticket never does.
func TestDrinkMarks_OnlyDeliveries(t *testing.T) {
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	for _, typ := range []string{"pickup", "dine_in", "counter"} {
		s := drinksSnap(typ)
		for _, b := range [][]byte{
			BuildCustomerReceipt(s, sc, prefs, printer.Paper{}),
			Build(s, sc, printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, prefs),
		} {
			if bytes.Contains(b, []byte(order.DrinkMark)) {
				t.Errorf("%s: a receipt with drink marks:\n%s", typ, textOf(b))
			}
		}
	}
	off := prefs
	off.DrinkMarks = false
	if b := BuildDeliveryCopies(drinksSnap("delivery"), sc, off, printer.Paper{}); bytes.Contains(b, []byte(order.DrinkMark)) {
		t.Errorf("drink_marks off: marks printed:\n%s", textOf(b))
	}
	// The kitchen tickets of the same delivery: unchanged by the stamps.
	s := drinksSnap("delivery")
	plain := drinksSnap("delivery")
	for i := range plain.Items {
		plain.Items[i].Drink = nil
		for j := range plain.Items[i].Children {
			plain.Items[i].Children[j].Drink = nil
		}
	}
	for _, tk := range []printer.Ticket{
		{Kind: printer.TicketStation, TargetID: "p-fry", Station: "Fry", Mine: s.Items[:2], Others: s.Items[2:]},
		{Kind: printer.TicketStation, TargetID: "p-front", Station: "Front", Mine: s.Items[2:], Others: s.Items[:2]},
	} {
		tp := tk
		tp.Mine, tp.Others = plain.Items[:len(tk.Mine)], plain.Items[len(tk.Mine):]
		if tk.TargetID == "p-front" {
			tp.Mine, tp.Others = plain.Items[2:], plain.Items[:2]
		}
		got, want := Build(s, sc, tk, prefs), Build(plain, sc, tp, prefs)
		if !bytes.Equal(got, want) || bytes.Contains(got, []byte(order.DrinkMark)) {
			t.Errorf("%s: the kitchen ticket moved with the drink stamps", tk.Station)
		}
	}
	// The classic style (receipt_prefs.style_*: classic) keeps its layout: no marks.
	classic := prefs
	classic.StyleStore = order.StyleClassic
	if b := BuildDeliveryCopies(drinksSnap("delivery"), sc, classic, printer.Paper{}); bytes.Contains(b, []byte(order.DrinkMark)) {
		t.Error("the classic slip carries marks")
	}
}

// A long drink name: the marks stay whole, on the last line when they fit
// with the name, else on a line of their own indented like a wrap.
func TestDrinkMarks_LongNames(t *testing.T) {
	yes := true
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	names := []string{
		"Jumbo 44oz Diet Pepsi Wild Cherry Zero Sugar Caffeine Free Extra Large Cup",
		"Jumbo 44oz Pink Lemonade",
		"Lg 32oz Fruit Punch Lemonade Mix",
	}
	for _, cols := range []int{Cols80, Cols72, Cols58} {
		w := layoutFor(cols).cols
		nameW := w - 14
		for _, n := range names {
			s := drinksSnap("delivery")
			s.Items = []order.LineItem{{Name: n, Quantity: 1, UnitPrice: 399, Drink: &yes,
				Children: []order.LineItem{{Name: n, Quantity: 1, UnitPrice: 50, Drink: &yes}}}}
			txt := textOf(BuildDeliveryCopies(s, sc, prefs, printer.Paper{Cols: cols}))
			lines := strings.Split(txt, "\n")
			marks := 0
			for i, l := range lines {
				if !strings.Contains(l, "##") {
					continue
				}
				marks++
				if len(l) > w || strings.Count(l, "#") != 5 || !strings.Contains(l, order.DrinkMark) {
					t.Errorf("%d cols %q: marks split or too wide: %q", cols, n, l)
				}
				body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(strings.TrimRight(l, " "), dollarsTail(l))), order.DrinkMark))
				if body == "" { // a line of their own: they didn't fit after the previous line
					head, width := 4, nameW
					if strings.HasPrefix(l, "      #") {
						head, width = 6, nameW-2
					}
					if !strings.HasPrefix(l, strings.Repeat(" ", head)+order.DrinkMark) {
						t.Errorf("%d cols %q: marks not indented like a wrap: %q", cols, n, l)
					}
					prev := strings.TrimRight(lines[i-1], " ")
					if p := dollarsTail(prev); p != "" {
						prev = strings.TrimRight(strings.TrimSuffix(prev, p), " ")
					}
					if len(prev) >= head && len(prev)-head+1+len(order.DrinkMark) <= width {
						t.Errorf("%d cols %q: marks alone though they fit after %q", cols, n, prev)
					}
				}
			}
			if marks != 4 { // the item and its choice, on both copies
				t.Errorf("%d cols %q: %d lines with marks, want 4\n%s", cols, n, marks, txt)
			}
		}
	}
	// withDrinkMark itself: fits = appended; doesn't = a line of its own.
	if got := withDrinkMark([]string{"2Lt Pepsi"}, 18, true); len(got) != 1 || got[0] != "2Lt Pepsi #####" {
		t.Errorf("fits: %q", got)
	}
	if got := withDrinkMark([]string{"Jumbo 44oz Pink", "Lemonade Extra"}, 18, true); len(got) != 3 || got[2] != order.DrinkMark {
		t.Errorf("doesn't fit: %q", got)
	}
	if got := withDrinkMark([]string{"Pepsi"}, 18, false); len(got) != 1 || got[0] != "Pepsi" {
		t.Errorf("not a drink: %q", got)
	}
}
