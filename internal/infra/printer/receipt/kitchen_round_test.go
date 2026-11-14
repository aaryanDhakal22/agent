package receipt

import (
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The owner's ticket round (2026-11-14, batch D; contract §2.1i). Each test
// fails on the code before it.

// #4: a counted item prints its count first as "N pc", wherever the count is.
func TestCountParts(t *testing.T) {
	for _, c := range []struct{ size, name, count, size2, name2 string }{
		{"6 pc", "Party Wings", "6 pc", "", "Party Wings"},
		{"6 Piece", "Whole Wings Combo", "6 pc", "", "Whole Wings Combo"},
		{"6 / Piece", "WHOLE WINGS COMBO", "6 pc", "", "WHOLE WINGS COMBO"},
		{"Serving (2 Piece)", "Garlic Knots", "2 pc", "", "Garlic Knots"},
		{"", "Chicken Tenders Only (4 pc)", "4 pc", "", "Chicken Tenders Only"},
		{"", "Half Jalapeno Popper 40pc", "40 pc", "", "Half Jalapeno Popper"},
		{"", "Club Sandwich Plater 20-pc", "20 pc", "", "Club Sandwich Plater"},
		{"", "2 Pcs Crab Platter", "2 pc", "", "Crab Platter"},
		{"Large", "Pepperoni Pizza", "", "Large", "Pepperoni Pizza"},
		{"12 Medium", "Cheese Pizza", "", "12 Medium", "Cheese Pizza"},
		{"20 oz", "Coke", "", "20 oz", "Coke"},
		{"", "Crab Rangoon (6)", "", "", "Crab Rangoon (6)"}, // no unit: left alone
	} {
		n, s, m := countParts(c.size, c.name)
		if n != c.count || s != c.size2 || m != c.name2 {
			t.Errorf("%q + %q = %q / %q / %q, want %q / %q / %q", c.size, c.name, n, s, m, c.count, c.size2, c.name2)
		}
	}
}

// #4 where the owner sees it: "1   6 pc Party Wings", the mods indented below,
// on a store order and an online one alike, and the customer slip the same.
func TestKitchenTicket_CountFirst(t *testing.T) {
	for _, src := range []string{order.SourceCounter, order.SourceOnline} {
		wings := kitchenLine(1, "Party Wings", "6 pc", "p-pizza", kid("Lite Ranch"), kid("Lite Mayo"))
		snap := kitchenSnap("pickup", src, "Ana", 101, wings)
		got := string(BuildStation(snap, pizzaTicket(snap)))
		if !strings.Contains(got, cmdDoubleSz+"1   6 pc Party Wings\n") {
			t.Errorf("%s: no count-first line:\n%q", src, got)
		}
		if !strings.Contains(got, H8+"- Lite Ranch\n") || !strings.Contains(got, H8+"- Lite Mayo\n") {
			t.Errorf("%s: the mods indented below, one a line:\n%q", src, got)
		}
		if strings.Contains(got, "(6 pc)") || strings.Contains(got, "- 6 pc") {
			t.Errorf("%s: the count printed after the name too:\n%q", src, got)
		}
	}
	if got := slipItemName("6 pc", "Party Wings"); got != "6 pc Party Wings" {
		t.Errorf("slip: %q", got)
	}
	if got := slipItemName("", "Chicken Tenders Only (4 pc)"); got != "4 pc Chicken Tenders Only" {
		t.Errorf("slip from the name: %q", got)
	}
}

// #2: ITEMS TO OTHER PRINTER, " - <item>" per line, one size smaller than the
// ticket's base font; the heading alone when nothing goes elsewhere.
func TestKitchenTicket_ItemsToOtherPrinter(t *testing.T) {
	snap := counterOrder()
	got := string(BuildStation(snap, pizzaTicket(snap)))
	want := cmdSizeReset + cmdNormalSz + "ITEMS TO OTHER PRINTER\n - 1   Fries\n - 1   2L Coke\n" + cmdBaseSz
	if !strings.Contains(got, want) || strings.Contains(got, "Also on this order") {
		t.Errorf("the bottom block:\n%q", got)
	}
	tk := pizzaTicket(snap)
	tk.Others = nil
	alone := string(BuildStation(snap, tk))
	i := strings.Index(alone, "ITEMS TO OTHER PRINTER\n")
	if i < 0 || strings.Contains(alone[i:], " - ") || strings.Contains(alone, "nothing else") {
		t.Errorf("the heading alone:\n%q", alone)
	}
}

// #3: the kitchen prints the call-center agent's name alone.
func TestKitchenTicket_TakerNameOnly(t *testing.T) {
	for in, want := range map[string]string{"Call center - Jhoan": "Jhoan", "Call Center Jhoan": "Jhoan", "Call center": "Call center", "": ""} {
		if got := kitchenTaker(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
	snap := counterOrder()
	snap.TakenBy = "Call center - Jhoan"
	got := string(BuildStation(snap, pizzaTicket(snap)))
	if !strings.Contains(got, cmdBoldOff+"Jhoan\n") || strings.Contains(got, "Call center") {
		t.Errorf("the taker line:\n%q", got)
	}
}

// #20 (render side): every choice its own line, never folded; a counter
// line's children carry the line's quantity, so "2 Pizza" + Extra cheese
// prints no (x2); twice per pizza prints (x2).
func TestKitchenTicket_ChoicesNeverFold(t *testing.T) {
	pie := kitchenLine(2, "Cheese Pizza", "Large", "p-pizza",
		order.LineItem{Name: "Lite Fried Onions", Quantity: 2}, order.LineItem{Name: "Lite Mayo", Quantity: 2},
		order.LineItem{Name: "Ranch", Quantity: 2}, order.LineItem{Name: "Ranch", Quantity: 2},
		order.LineItem{Name: "Pepperoni", Quantity: 4})
	snap := kitchenSnap("pickup", order.SourceCounter, "Ana", 102, pie)
	got := string(BuildStation(snap, pizzaTicket(snap)))
	for _, want := range []string{H8 + "- Lite Fried Onions\n" + H8 + "- Lite Mayo\n", H8 + "- Ranch\n" + H8 + "- Ranch\n", H8 + "- (x2) Pepperoni\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%q", want, got)
		}
	}
	if strings.Contains(got, "(x2) Lite") || strings.Contains(got, "(x2) Ranch") || strings.Contains(got, "(x4)") {
		t.Errorf("a choice folded / counted per line:\n%q", got)
	}
	// The customer slip: the same lines, "2x" only per item, the price the child's total.
	snap.Items[0].UnitPrice = 1000
	snap.Items[0].Children[4].UnitPrice = 150
	slip := textOf(Build(snap, deliveryStoreCtx(), printer.Ticket{Kind: printer.TicketReceipt, Station: "Front"}, order.DefaultReceiptPrefs()))
	if !strings.Contains(slip, "2x Pepperoni") || strings.Contains(slip, "4x") || !strings.Contains(slip, "$6.00") {
		t.Errorf("slip choices:\n%s", slip)
	}
}
