package receipt

import (
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The driver's $0 choices (agent 2.5.7, the owner 2026-11-13): the delivery
// ticket shows "only the ones the driver hands over" (drinks, sides, sauces,
// dressings), never a cooking choice. Lombardi's prod order #1109 (online
// vendor, 2026-11-13) printed "15 Piece Chicken Tender" alone: 2.5.2 hid
// every $0 choice of a vendor order. The vendor's children are never
// stamped, so the store's hand-over words decide for them; our own orders
// carry the menu's stamp (show_on_driver_ticket) and the words too.

// flat is the paper's words on one line (a long choice wraps at 42 / 32).
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func vendorKid(name string, cents int64) order.LineItem {
	return order.LineItem{Name: name, Quantity: 1, UnitPrice: cents, Notes: "add"}
}

// prodTenders is #1109's lines as main sent them (names, prices, notes).
func prodTenders() order.Snapshot {
	snap := slipSnap("cash")
	snap.Source = order.SourceOnline
	snap.Fulfillment.Type = "delivery"
	snap.Items = []order.LineItem{
		{Name: "Chicken Tender", Size: "15 Piece", Quantity: 1, UnitPrice: 1899, Routing: order.RoutingMatched, PrinterID: "p-wings",
			Children: []order.LineItem{vendorKid("Honey Mustard Dip", 0), vendorKid("Can of Ice Tea", 0), vendorKid("Side of Fries", 0)}},
		{Name: "Chicken Tender", Size: "10 Piece", Quantity: 1, UnitPrice: 1499, Routing: order.RoutingMatched, PrinterID: "p-wings",
			Children: []order.LineItem{vendorKid("Honey Mustard Dip", 0), vendorKid("Can of Ice Tea", 0),
				vendorKid("Add Honey Mustard Sauce", 75), vendorKid("Side of Fries", 0)}},
		// Cooking choices the vendor sends at $0: never on the driver's ticket.
		{Name: "Cheesesteak Sub", Size: "Serving", Quantity: 1, UnitPrice: 1099, Routing: order.RoutingMatched, PrinterID: "p-sub",
			Children: []order.LineItem{vendorKid("Add Mayo", 0), vendorKid("Add Fried Onions", 0), vendorKid("With American Cheese", 0),
				vendorKid("Breaded", 0), {Name: "No Tomatoes", Quantity: 1, Notes: "remove"}, vendorKid("No Dip", 0)}},
	}
	return snap
}

func TestDeliveryTicket_HandoverChoices_VendorOrder(t *testing.T) {
	prefs := order.DefaultReceiptPrefs()
	for _, cols := range []int{48, 42, 32} { // Lombardi's Online printer is 42
		txt := flat(textOf(Build(prodTenders(), deliveryStoreCtx(), printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: cols}, prefs)))
		for _, want := range []string{"Honey Mustard Dip", "Can of Ice Tea", "Side of Fries", "Mustard Sauce"} {
			if got := strings.Count(txt, want); got != 4 && !(want == "Mustard Sauce" && got == 2) { // 2 lines x 2 copies
				t.Errorf("%d: %q printed %d times\n%s", cols, want, got, txt)
			}
		}
		for _, not := range []string{"Mayo", "Fried Onions", "American Cheese", "Breaded", "No Tomatoes", "No Dip"} {
			if strings.Contains(txt, not) {
				t.Errorf("%d: the cooking choice %q is on the driver's ticket\n%s", cols, not, txt)
			}
		}
	}
	// The kitchen keeps every choice.
	snap := prodTenders()
	st := textOf(Build(snap, deliveryStoreCtx(), printer.Ticket{Kind: printer.TicketStation, TargetID: "p-sub", Mine: snap.Items[2:], Cols: 48}, prefs))
	for _, want := range []string{"Add Mayo", "Breaded", "No Tomatoes"} {
		if !strings.Contains(st, want) {
			t.Errorf("kitchen ticket lacks %q\n%s", want, st)
		}
	}
	// The words are the store's (receipt_prefs.handover_choice_words): none = only priced choices.
	none := prefs
	none.HandoverChoiceWords = order.NewWordList()
	txt := textOf(Build(prodTenders(), deliveryStoreCtx(), printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: 42}, none))
	if strings.Contains(txt, "Can of Ice Tea") || !strings.Contains(txt, "Add Honey Mustard") {
		t.Errorf("no hand-over words: only the priced choice\n%s", txt)
	}
}

// A counter (or call-center / website) delivery: the menu's stamp first
// (the W Can drinks are flagged at Lombardi's), and the words for what the
// menu doesn't flag: a dressing or a side prints, the sub's fixings don't.
func TestDeliveryTicket_HandoverChoices_CounterOrder(t *testing.T) {
	yes, no := true, false
	snap := slipSnap("cash")
	snap.Source = order.SourceCounter
	snap.Fulfillment.Type = "delivery"
	snap.Items = []order.LineItem{
		{Name: "Chicken Tender", Size: "15 Piece", Quantity: 1, UnitPrice: 1899, Children: []order.LineItem{
			{Name: "W Can Ice Tea", Quantity: 1, ShowOnDriverTicket: &yes},
			{Name: "Ranch", Quantity: 1, ShowOnDriverTicket: &no}, // not flagged in main yet: the owner's list (report)
		}},
		{Name: "Garden Salad", Size: "Small", Quantity: 1, UnitPrice: 499, Children: []order.LineItem{
			{Name: "Balsamic Vinaigrette Dressing", Quantity: 1, ShowOnDriverTicket: &no},
			{Name: "No Tomato", Quantity: 1, Mode: order.ModeNo, ShowOnDriverTicket: &no},
		}},
		{Name: "Cheesesteak Sub", Size: `12"`, Quantity: 1, UnitPrice: 1299, Children: []order.LineItem{
			{Name: "Add Everything no Hot", Quantity: 1, ShowOnDriverTicket: &no},
			{Name: "Add Mayo", Quantity: 1, ShowOnDriverTicket: &no},
			{Name: "Add American Chz", Quantity: 1, ShowOnDriverTicket: &no}, // "can" is no whole word in "American"
			{Name: "Toast Well", Quantity: 1, ShowOnDriverTicket: &no},
		}},
	}
	txt := flat(textOf(Build(snap, deliveryStoreCtx(), printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: 42}, order.DefaultReceiptPrefs())))
	for _, want := range []string{"W Can Ice Tea", "Balsamic Vinaigrette Dressing"} {
		if strings.Count(txt, want) != 2 {
			t.Errorf("%q not on both copies\n%s", want, txt)
		}
	}
	for _, not := range []string{"Ranch", "No Tomato", "Add Everything", "Mayo", "American", "Toast Well"} {
		if strings.Contains(txt, not) {
			t.Errorf("%q on the driver's ticket\n%s", not, txt)
		}
	}
}

func TestHandoverWords_WholeWords(t *testing.T) {
	w := order.DefaultReceiptPrefs().HandoverChoiceWords
	for name, want := range map[string]bool{
		"Can of Ice Tea": true, "W Can Pepsi": true, "Side of Fries": true, "Ranch Dip": true, "Caesar Dressing": true,
		"Sprite": true, "2 Liter Coke": true, "Naan Bread": true,
		"Add American Chz": false, "Pecan Pie": false, "Breaded": false, "Add Mayo": false, "Well Done": false, "Dipped": false,
	} {
		if got := w.MatchesWord(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
	for _, c := range []order.LineItem{{Name: "No Dip", Notes: "add"}, {Name: "Side of Fries", Notes: "remove"}, {Name: "Fries", Mode: order.ModeNo}} {
		if handedOver(c, w) {
			t.Errorf("a NO is never handed over: %+v", c)
		}
	}
}

// Drinks (the owner, 2026-11-13: "drink choices are very important, so the
// driver knows which drinks the customer got"): a 2-liter's flavour, the
// fountain half & half and its ice, a combo's drink, from the vendor (by its
// names) and from our menu (stamped, or by name when main's toggle is off).
func TestDeliveryTicket_DrinkChoices(t *testing.T) {
	yes, no := true, false
	vendor := slipSnap("cash")
	vendor.Source = order.SourceOnline
	vendor.Fulfillment.Type = "delivery"
	vendor.Items = []order.LineItem{
		{Name: "2 Liter Soda", Quantity: 1, UnitPrice: 399, Children: []order.LineItem{vendorKid("Diet Pepsi", 0)}},
		{Name: "Fountain Drink", Size: "Large", Quantity: 1, UnitPrice: 249, Children: []order.LineItem{vendorKid("Half & Half", 0), vendorKid("No Ice", 0)}},
		{Name: "Whole Wings Combo", Quantity: 1, UnitPrice: 1299, Children: []order.LineItem{vendorKid("Can of Ginger Ale", 0), vendorKid("Mild Flavor", 0)}},
	}
	counter := slipSnap("cash")
	counter.Source = order.SourceCounter
	counter.Fulfillment.Type = "delivery"
	counter.Items = []order.LineItem{
		{Name: "Pepsi (2 liter)", Quantity: 1, UnitPrice: 399},
		{Name: "Fountain Half & Half", Size: "Large", Quantity: 1, UnitPrice: 249,
			Children: []order.LineItem{{Name: "Lite Ice", Quantity: 1, ShowOnDriverTicket: &yes}}}, // a Beverages item: stamped on
		{Name: "Whole Wings with Fries", Quantity: 1, UnitPrice: 1299, Children: []order.LineItem{
			{Name: "Med Half/Half", Quantity: 1, UnitPrice: 100, ShowOnDriverTicket: &yes},
			{Name: "Jumbo Lemonade", Quantity: 1, ShowOnDriverTicket: &no}, // toggle off (a $0 coupon drink): its name prints it
			{Name: "W Can Root Beer", Quantity: 1, ShowOnDriverTicket: &yes},
			{Name: "Mild", Quantity: 1, ShowOnDriverTicket: &no}}},
	}
	for _, c := range []struct {
		name      string
		snap      order.Snapshot
		want, not []string
	}{
		{"vendor", vendor, []string{"2 Liter Soda", "Diet Pepsi", "Half & Half", "Can of Ginger Ale"}, []string{"No Ice", "Mild Flavor"}},
		{"counter", counter, []string{"Pepsi (2 Liter)", "Fountain", "Half & Half", "Lite Ice", "Med Half/Half", "Jumbo Lemonade", "W Can Root Beer"},
			[]string{"Mild"}},
	} {
		for _, cols := range []int{48, 42, 32} {
			txt := flat(textOf(Build(c.snap, deliveryStoreCtx(), printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: cols}, order.DefaultReceiptPrefs())))
			for _, w := range c.want {
				if strings.Count(strings.ToUpper(txt), strings.ToUpper(w)) < 2 {
					t.Errorf("%s %d: %q not on both copies\n%s", c.name, cols, w, txt)
				}
			}
			for _, n := range c.not {
				if strings.Contains(txt, n+" ") || strings.HasSuffix(txt, n) {
					t.Errorf("%s %d: %q on the driver's ticket\n%s", c.name, cols, n, txt)
				}
			}
		}
	}
}

// A half-and-half pie's halves always show on the customer slips (2.5.7): its
// $0 specialty half is what the pizza is; a NO on a half stays off.
func TestDeliveryTicket_PizzaHalvesShow(t *testing.T) {
	no := false
	snap := slipSnap("cash")
	snap.Source = order.SourceCounter
	snap.Fulfillment.Type = "delivery"
	snap.Items = []order.LineItem{{Name: "Cheese Pizza", Size: `16" X-Large`, Quantity: 1, UnitPrice: 1999, Children: []order.LineItem{
		{Name: "Pepperoni (1st half)", Portion: order.PortionFirst, Mode: order.ModeAdd, Quantity: 1, UnitPrice: 150},
		{Name: "Hawaiian (2nd half)", Portion: order.PortionSecond, Mode: order.ModeSpecialty, Quantity: 1},
		{Name: "NO Pineapple (2nd half)", Portion: order.PortionSecond, Mode: order.ModeNo, Quantity: 1, ShowOnDriverTicket: &no},
	}}}
	for _, typ := range []string{"delivery", "pickup"} {
		snap.Fulfillment.Type = typ
		kind := printer.TicketDeliveryCopies
		if typ == "pickup" {
			kind = printer.TicketReceipt
		}
		txt := flat(textOf(Build(snap, deliveryStoreCtx(), printer.Ticket{Kind: kind, Cols: 42}, order.DefaultReceiptPrefs())))
		if !strings.Contains(txt, "1st: Pepperoni") || !strings.Contains(txt, "2nd: Hawaiian") || strings.Contains(txt, "Pineapple") {
			t.Errorf("%s: the halves\n%s", typ, txt)
		}
	}
}
