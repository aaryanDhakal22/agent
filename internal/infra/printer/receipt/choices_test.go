package receipt

import (
	"fmt"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// choicesSnap: one pizza with a priced topping, a $0 choice stamped to show,
// one stamped hidden, one unstamped, a $0 half choice and a priced half
// topping, a priced "Side of Fries" and a $0 "House Salad" (stamped to show).
func choicesSnap(source, typ string) order.Snapshot {
	yes, no := true, false
	d := slipSnap("cash")
	d.Source = source
	d.Fulfillment.Type = typ
	d.Items = []order.LineItem{{Name: "Cheese Pizza", Size: "Large", Quantity: 1, UnitPrice: 1200, Notes: "cut in squares",
		Children: []order.LineItem{
			{Name: "Pepperoni", Quantity: 1, UnitPrice: 150},
			{Name: "Well Done", Quantity: 1, ShowOnDriverTicket: &yes},
			{Name: "Light Sauce", Quantity: 1, ShowOnDriverTicket: &no},
			{Name: "Regular Crust", Quantity: 1},
			{Name: "No Sauce", Quantity: 1, Portion: order.PortionFirst, Mode: "no"},
			{Name: "Mushrooms", Quantity: 1, UnitPrice: 100, Portion: order.PortionSecond},
			{Name: "Side of Fries", Quantity: 1, UnitPrice: 399},
			{Name: "House Salad", Quantity: 1, ShowOnDriverTicket: &yes},
			// 2.5.7: the vendor's way (never stamped): a can the driver hands over, a cooking choice, a NO.
			{Name: "Can of Ice Tea", Quantity: 1, Notes: "add"},
			{Name: "Add Mayo", Quantity: 1, Notes: "add"},
			{Name: "No Dip", Quantity: 1, Notes: "add"},
		}}}
	if source == order.SourceOnline { // the Pi never stamps an online vendor order
		for i := range d.Items[0].Children {
			d.Items[0].Children[i].ShowOnDriverTicket = nil
		}
	}
	return d
}

// The customer slips' choice rule (the owner, agent 2.5.2; 2.5.7): a priced choice always prints; a $0 one
// only when the driver hands it over: our menu stamped it show_on_driver_ticket, or (not a NO) its name
// holds a hand-over word ("Can of Ice Tea", "House Salad"), for an online vendor order too (never
// stamped); pickup and delivery alike; the item note stays. A choice holding a bold word prints bold. 48 / 42 / 32 columns; goldens
// testdata/slips/choices_*.golden.
func TestSlipChoicesAndBoldWords(t *testing.T) {
	sc, prefs := deliveryStoreCtx(), order.DefaultReceiptPrefs()
	for _, cols := range []int{48, 42, 32} {
		for _, c := range []struct {
			source, typ string
			shown       []string
		}{
			{order.SourceCounter, "pickup", []string{"Pepperoni", "Well Done", "2nd: Mushrooms", "Side of Fries", "House Salad", "Can of Ice Tea"}},
			{order.SourceCounter, "delivery", []string{"Pepperoni", "Well Done", "2nd: Mushrooms", "Side of Fries", "House Salad", "Can of Ice Tea"}},
			{order.SourceWeb, "pickup", []string{"Pepperoni", "Well Done", "2nd: Mushrooms", "Side of Fries", "House Salad", "Can of Ice Tea"}},
			{order.SourceOnline, "pickup", []string{"Pepperoni", "2nd: Mushrooms", "Side of Fries", "House Salad", "Can of Ice Tea"}},
			{order.SourceOnline, "delivery", []string{"Pepperoni", "2nd: Mushrooms", "Side of Fries", "House Salad", "Can of Ice Tea"}},
		} {
			name := fmt.Sprintf("%s_%s_%d", c.source, c.typ, cols)
			snap := choicesSnap(c.source, c.typ)
			var b []byte
			if c.typ == "delivery" {
				b = Build(snap, sc, printer.Ticket{Kind: printer.TicketDeliveryCopies, Cols: cols}, prefs)
			} else {
				b = Build(snap, sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: cols}, prefs)
			}
			txt := textOf(b)
			for _, want := range append(c.shown, "Note: cut in squares") {
				if !strings.Contains(txt, want) {
					t.Errorf("%s: %q missing\n%s", name, want, txt)
				}
			}
			all := []string{"Pepperoni", "Well Done", "Light Sauce", "Regular Crust", "No Sauce", "Mushrooms", "Side of Fries", "House Salad",
				"Can of Ice Tea", "Add Mayo", "No Dip"}
			for _, x := range all {
				if strings.Contains(txt, x) != containsAny(c.shown, x) {
					t.Errorf("%s: %q printed = %v", name, x, strings.Contains(txt, x))
				}
			}
			// bold: the item's own line (2.5.2), fries (priced) and salad (stamped $0) when they print, never
			// Pepperoni or the note
			for _, x := range []string{"Large Cheese Pizza", "Side of Fries", "House Salad", "Pepperoni", "Note: cut in squares"} {
				if !containsAny(c.shown, x) && x != "Large Cheese Pizza" && x != "Note: cut in squares" {
					continue
				}
				i := strings.Index(string(b), x)
				bold := strings.LastIndex(string(b[:i]), cmdBoldOn) > strings.LastIndex(string(b[:i]), cmdBoldOff)
				if want := x != "Pepperoni" && x != "Note: cut in squares"; bold != want {
					t.Errorf("%s: %q bold = %v, want %v", name, x, bold, want)
				}
			}
			if c.source == order.SourceCounter {
				golden(t, "testdata/slips/choices_"+name+".golden", b)
			}
		}
	}
	// No bold words: nothing bold among the choices.
	none := prefs
	none.BoldChoiceWords = order.NewWordList()
	b := string(Build(choicesSnap(order.SourceCounter, "pickup"), sc, printer.Ticket{Kind: printer.TicketReceipt, Cols: 48}, none))
	i := strings.Index(b, "Side of Fries")
	if strings.LastIndex(b[:i], cmdBoldOn) > strings.LastIndex(b[:i], cmdBoldOff) {
		t.Error("no bold words: Side of Fries is not bold")
	}
	// Kitchen tickets keep every choice.
	snap := choicesSnap(order.SourceOnline, "pickup")
	st := textOf(Build(snap, sc, printer.Ticket{Kind: printer.TicketStation, Station: "Pizza", Mine: snap.Items, Cols: 48}, prefs))
	for _, x := range []string{"Light Sauce", "Regular Crust", "Well Done"} {
		if !strings.Contains(st, x) {
			t.Errorf("station ticket lost %q\n%s", x, st)
		}
	}
}

func containsAny(list []string, s string) bool {
	for _, x := range list {
		if strings.Contains(x, s) || strings.Contains(s, x) {
			return true
		}
	}
	return false
}
