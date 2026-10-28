package order

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
)

// The route message (2.3.10, owner): one block per stop in run order —
//
//	#323 4108 Old Court Rd Apt 2B · CASH $38.40
//	  Note: gate code 1234
//	  2× 2 Liter Coke · Diet
//	  1× Cannoli
//
// — the drinks and desserts the driver grabs at the counter listed under
// their stop, one item per line as the receipt prints it (no prices), the
// same line in a stop added up. Missing one means driving back, so an item
// counts when either rule says so: it goes to the receipt printer (online;
// a POS line on no kitchen station), or its name has a drink/dessert word.

// routeMsgMax is Pushover's message limit (characters).
const routeMsgMax = 1024

// routeNoteShort is how long a note may stay when the message is too long.
const routeNoteShort = 40

var grabWords = regexp.MustCompile(`(?i)\b(soda|pop|coke|cola|pepsi|sprite|fanta|7 ?up|dr\.? ?pepper|mountain dew|ginger ale|root beer|seltzer|water|juice|lemonade|iced tea|tea|snapple|gatorade|red bull|milk|shake|milkshake|drink|beverage|liter|litre|2l|20 ?oz|can|bottle|cannoli|cake|cheesecake|brownie|cookie|tiramisu|ice cream|gelato|dessert|pie|zeppole|cupcake|churro|pudding|baklava|cinnamon)s?\b`)

// feeWords are never something to grab; notGrab are kitchen food whose
// name happens to have a dessert word.
var (
	feeWords = regexp.MustCompile(`(?i)\b(fee|charge|processing|tip|delivery)\b`)
	notGrab  = regexp.MustCompile(`(?i)\b(crab|fish|potato|rice) cakes?\b|\bpot ?pie\b|\bpizza pie\b`)
)

// ReceiptPrinter says which printer is the receipt printer and how many
// printers the store has (an item routed to the receipt printer is a grab
// item only when there is a kitchen printer besides it).
type ReceiptPrinter func() (id string, printers int)

// isGrab reports whether a top-level item is a drink or dessert.
func isGrab(it order.LineItem, pos bool, rp ReceiptPrinter) bool {
	name := it.Name + " " + it.Size
	if feeWords.MatchString(it.Name) || notGrab.MatchString(name) {
		return false
	}
	if grabWords.MatchString(name) {
		return true
	}
	if pos {
		return !it.Kitchen // a POS line no kitchen station gets
	}
	if rp != nil && it.PrinterID != "" {
		if id, n := rp(); id != "" && n > 1 && it.PrinterID == id {
			return true
		}
	}
	return false
}

// grabLine is an item as the receipt prints it, on one line, no prices:
// size, name, its modifiers, its note.
func grabLine(it order.LineItem) string {
	parts := []string{strings.TrimSpace(strings.TrimSpace(it.Size) + " " + strings.TrimSpace(it.Name))}
	for _, c := range it.Children {
		n := strings.TrimSpace(c.Name)
		if n == "" {
			continue
		}
		if c.Quantity > 1 {
			n = fmt.Sprintf("%d× %s", c.Quantity, n)
		}
		parts = append(parts, n)
	}
	if note := strings.TrimSpace(it.Notes); note != "" {
		parts = append(parts, "Note: "+note)
	}
	return strings.Join(parts, " · ")
}

// stopGrab is a stop's drinks and desserts, the same line added up, in the
// order first seen.
func stopGrab(so order.StoredOrder, rp ReceiptPrinter) []string {
	pos := so.IsPOS()
	qty := map[string]int{}
	var order_ []string
	for _, it := range so.Order.Items {
		if !isGrab(it, pos, rp) {
			continue
		}
		l := grabLine(it)
		if _, ok := qty[l]; !ok {
			order_ = append(order_, l)
		}
		qty[l] += max(it.Quantity, 1)
	}
	out := make([]string, 0, len(order_))
	for _, l := range order_ {
		out = append(out, fmt.Sprintf("%d× %s", qty[l], l))
	}
	return out
}

// stopHead is "#323 4108 Old Court Rd Apt 2B · CASH $38.40" (or · PAID).
func stopHead(so order.StoredOrder) string {
	h := label(so)
	if a := so.Order.Fulfillment.Address; a != nil {
		street := strings.TrimSpace(a.Line1)
		if l2 := strings.TrimSpace(a.Line2); l2 != "" {
			street += " " + l2
		}
		if street != "" {
			h += " " + street
		}
	}
	if l := order.NewDriverReportLine(so); l.Cash && l.CashCents > 0 {
		h += fmt.Sprintf(" · CASH $%d.%02d", l.CashCents/100, l.CashCents%100)
	} else {
		h += " · PAID"
	}
	return h
}

// stopNote is the customer's delivery note and order note together.
func stopNote(so order.StoredOrder) string {
	var notes []string
	if a := so.Order.Fulfillment.Address; a != nil && strings.TrimSpace(a.Notes) != "" {
		notes = append(notes, strings.TrimSpace(a.Notes))
	}
	if n := strings.TrimSpace(so.Order.Notes); n != "" {
		notes = append(notes, n)
	}
	return strings.Join(notes, " / ")
}

// routeMessage lays the stops out within Pushover's limit: notes are cut
// to routeNoteShort first; still too long, the last lines go and the
// message says the rest is on the receipts. carried < len(stops): the map
// link holds only the first stops.
func routeMessage(stops []RouteStop, carried int) (title, text string) {
	n := len(stops)
	title = fmt.Sprintf("Route · %d stop", n)
	if n != 1 {
		title += "s"
	}
	build := func(short bool) []string {
		var lines []string
		for _, st := range stops {
			lines = append(lines, st.Head)
			if st.Note != "" {
				note := st.Note
				if short && utf8.RuneCountInString(note) > routeNoteShort {
					note = string([]rune(note)[:routeNoteShort-1]) + "…"
				}
				lines = append(lines, "  Note: "+note)
			}
			for _, g := range st.Grab {
				lines = append(lines, "  "+g)
			}
		}
		if carried < n {
			lines = append(lines, fmt.Sprintf("Map: the first %d stops", carried))
		}
		return lines
	}
	lines := build(false)
	if utf8.RuneCountInString(strings.Join(lines, "\n")) > routeMsgMax {
		lines = build(true)
	}
	if utf8.RuneCountInString(strings.Join(lines, "\n")) > routeMsgMax {
		const more = "+ more on the receipts"
		for len(lines) > 1 && utf8.RuneCountInString(strings.Join(append(lines[:len(lines):len(lines)], more), "\n")) > routeMsgMax {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, more)
	}
	text = strings.Join(lines, "\n")
	if text == "" {
		text = "Route"
	}
	return title, text
}
