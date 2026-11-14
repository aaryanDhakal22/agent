package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The kitchen (station) ticket, the owner's design of 2026-11-11
// (TERMINAL-HALVES.md "Kitchen tickets"). The kitchen's steel rail hides the
// top four lines, so they carry only the order type (double size, two
// lines), "Printed h:mm PM" and a blank line; the ticket number sits right
// under the rail, extra big. Reverse print only for a future order's band,
// a change ticket's ADD / REMOVE bands and the reroute bands.
//
//	          COUNTER                 (double size: 2 lines)   ┐
//	       Printed 6:31 PM            (1 line)                 │ under the rail
//	                                  (1 line)                 ┘
//	            12                    (extra big, bold)
//	         SMITH, ANA               (bold)
//	[ REROUTED FROM PIZZA ]           (reverse: printed on a stand-in printer,
//	[ REDIRIGIDO DE PIZZA ]            English then Spanish, writeRerouted)
//	[ MAKE AT 6:30 · READY 6:45 ]     (reverse: a future order only)
//	------------------------------------------------
//	NOTE: ring the side bell          (bold: the order's note, when it has one)
//	------------------------------------------------
//	2   Pepperoni Pizza (Large)       (bold, the store's item size; 3 spaces
//	          - Extra cheese           after the quantity, 2026-11-14)
//	          Note: well done         (the food note)
//	1   6 pc Party Wings              (a counted item: the count first, #4)
//	          - Lite Ranch            (every choice its own line, never folded)
//	1   Cheese Pizza (XL)
//	          WHOLE:
//	            - Extra cheese
//	          1ST HALF:
//	            - Pepperoni ONLY
//	          2ND HALF:
//	            - Hawaiian
//	------------------------------------------------
//	ITEMS TO OTHER PRINTER            (font A 1x1: one size smaller, #2;
//	 - 1   Fries                       the heading alone when none)
//
// A change ticket (lines changed after SEND) has the same top, then the rule,
// then [ ADD / AGREGAR ] and the added lines, then [ REMOVE / QUITAR ] and
// each removed line as "(VOID) " + the line struck through (a small picture,
// strike.go; prefs.StrikeImages off = "(VOID)" in reverse + the text). A
// VOID ticket (a voided order or POS lines) is a change ticket with only
// REMOVE. Delivery and pickup tickets are the counter one with their type.
// No prices, no station names, no distances.

// clock is the "Printed" time (tests pin it).
var clock = time.Now

const (
	cmdSizeReset = "\x1d\x21\x00" // GS ! 0: 1x1
	ruleChar     = "-"
	itemIndent   = "   "
	voidMark     = "(VOID) "
)

// BuildStation renders a kitchen station ticket with the default receipt
// settings (Build takes the store's: item size, spacing, strike images).
func BuildStation(snap order.Snapshot, t printer.Ticket) []byte {
	snap, t = scrubSnapshot(snap), scrubTicket(t)
	return buildStation(snap, t, order.DefaultReceiptPrefs())
}

func buildStation(snap order.Snapshot, t printer.Ticket, prefs order.ReceiptPrefs) []byte {
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(toPC437(s)) }
	nl := func() { buf.WriteString(cmdFeed) }
	l := layoutFor(t.Cols)                      // the printer it prints on (width.go)
	online := snap.Source == order.SourceOnline // an online order's size on its own line (2.5.2)

	added, removed := t.Mine, t.Removed
	if t.Void {
		added, removed = nil, t.Mine
	}
	change := t.Change || t.AddOn || t.Void

	// --- The top four lines, under the rail ---------------------------------
	w(cmdInit)
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(stationType(snap))
	nl()
	w(cmdBoldOff)
	w(cmdSizeReset)
	w(cmdNormalSz)
	w("Printed " + clock().In(snap.PlacedAt.Location()).Format("3:04 PM"))
	nl()
	nl()

	// --- The number, the name -----------------------------------------------
	num := stationNumber(snap)
	w(cmdBoldOn)
	w(numberSize(num, l))
	w(wrapJoin(num, numberCols(num, l), ""))
	nl()
	w(cmdSizeReset)
	w(cmdBaseSz)
	if name := strings.TrimSpace(snap.Customer.DisplayName); name != "" {
		for _, line := range wrapped(lastFirst(name), l.cols, "") {
			w(line)
			nl()
		}
	}
	w(cmdBoldOff)
	// Who took it (a call-center order, contract §2.10ab): the agent's name
	// alone (the owner, 2026-11-14, #3: "Jhoan", not "Call center - Jhoan").
	if by := kitchenTaker(snap.TakenBy); by != "" {
		for _, line := range wrapped(by, l.cols, "") {
			w(line)
			nl()
		}
	}
	if t.ReroutedFrom != "" {
		// Printed on a stand-in printer: the reroute bands, right under the
		// number + name (writeRerouted).
		writeRerouted(w, nl, t.ReroutedFrom, l)
		w(cmdBaseSz)
	}
	if p := snap.Fulfillment.PromisedAt; p != nil && !p.IsZero() {
		writeFutureBand(w, nl, snap, *p, l)
	}
	w(cmdLeft)
	w(l.rule(ruleChar))
	nl()
	// The order's note (the owner, 2026-11-12: on the station tickets too,
	// under the header; the receipt keeps it as well).
	if note := strings.TrimSpace(snap.Notes); note != "" {
		w(cmdBoldOn)
		for _, line := range wordWrap(printable("NOTE: "+note), l.cols, "      ") {
			w(line)
			nl()
		}
		w(cmdBoldOff)
		w(l.rule(ruleChar))
		nl()
	}

	if change && t.WasType != "" {
		// Moved takeout <-> delivery after SEND: the new type (also on top) and the old one.
		writeBand(w, nl, "NOW "+stationType(snap), l)
		w(cmdCenter)
		w(cmdBoldOn)
		w("(WAS " + typeWord(t.WasType) + ")")
		w(cmdBoldOff)
		nl()
		w(cmdLeft)
		if t.TypeOnly {
			nl()
			writeKitchenItems(w, nl, added, t, prefs, l, online)
			added = nil
		}
	}
	if change {
		if len(added) > 0 {
			writeBand(w, nl, "ADD / AGREGAR", l)
			writeKitchenItems(w, nl, added, t, prefs, l, online)
		}
		if len(removed) > 0 {
			if len(added) > 0 {
				nl()
			}
			writeBand(w, nl, "REMOVE / QUITAR", l)
			writeRemoved(w, nl, removed, prefs, l, online)
		}
	} else {
		writeKitchenItems(w, nl, added, t, prefs, l, online)
		w(l.rule(ruleChar))
		nl()
		writeOtherPrinters(w, nl, t.Others, l)
	}

	w(cmdBaseSz)
	for i := 0; i < 4; i++ {
		nl()
	}
	w(cmdCut)
	return buf.Bytes()
}

// stationType is the ticket's top line: COUNTER (the terminal's counter
// orders and POS counter checks), PICKUP, DELIVERY, else DINE IN / ORDER.
func stationType(snap order.Snapshot) string {
	switch strings.ToLower(strings.TrimSpace(snap.Fulfillment.Type)) {
	case "delivery":
		return "DELIVERY"
	case "pickup", "takeout":
		return "PICKUP"
	case "counter":
		return "COUNTER"
	case "dine_in":
		if snap.Source == order.SourceCounter {
			return "COUNTER"
		}
		return "DINE IN"
	}
	return "ORDER"
}

// typeWord is a terminal order type as the ticket's top line words it.
func typeWord(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "delivery":
		return "DELIVERY"
	case "pickup", "takeout":
		return "PICKUP"
	case "counter", "dine_in":
		return "COUNTER"
	}
	return strings.ToUpper(t)
}

// stationNumber is the ticket number staff call (the kitchen / counter
// number), else the vendor's order id.
func stationNumber(snap order.Snapshot) string {
	if snap.KitchenNumber != nil {
		return fmt.Sprintf("%d", *snap.KitchenNumber)
	}
	return snap.SourceEventID
}

// numberSize is the largest size the number fits on the printer's line at:
// 4 x 4 (up to 12 characters on 80 mm), 3 x 3 (16), else double.
func numberSize(num string, l layout) string {
	switch n := len([]rune(num)); {
	case n <= l.huge():
		return cmdHugeSz
	case n <= l.xl():
		return cmdQuadSz
	}
	return cmdDoubleSz
}

// numberCols is the line at the number's size (a vendor id too long even at
// double size wraps there).
func numberCols(num string, l layout) int {
	switch numberSize(num, l) {
	case cmdHugeSz:
		return l.huge()
	case cmdQuadSz:
		return l.xl()
	}
	return l.big()
}

// writeFutureBand is a future order's one reverse band: when the kitchen
// starts it and when it is due ("MAKE AT 6:30 · READY 6:45"; READY alone
// when there is nothing to count back from), with the day under it when it
// isn't today.
func writeFutureBand(w func(string), nl func(), snap order.Snapshot, ready time.Time, l layout) {
	text := "READY " + ready.Format("3:04")
	if m := snap.MakeAt; m != nil && !m.IsZero() {
		text = "MAKE AT " + m.In(ready.Location()).Format("3:04") + " \xfa " + text
	}
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdBaseSz)
	w(cmdInvertOn)
	w(pad(text, l.cols))
	if now := clock().In(ready.Location()); now.YearDay() != ready.YearDay() || now.Year() != ready.Year() {
		nl()
		w(pad(strings.ToUpper(ready.Format("Mon Jan 2")), l.cols))
	}
	w(cmdInvertOff)
	w(cmdBoldOff)
	nl()
}

// writeBand is a change ticket's reverse band, centred, one size up (double).
func writeBand(w func(string), nl func(), text string, l layout) {
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdInvertOn)
	w(pad(text, l.big()))
	w(cmdInvertOff)
	w(cmdSizeReset)
	w(cmdBaseSz)
	w(cmdBoldOff)
	nl()
	w(cmdLeft)
}

// pad centres text in a band width characters wide.
func pad(text string, width int) string {
	n := len([]rune(text))
	if n >= width {
		return text
	}
	left := (width - n) / 2
	return strings.Repeat(" ", left) + text + strings.Repeat(" ", width-n-left)
}

// otherPrintersHead heads the items that print on the order's other
// printers (the owner, 2026-11-14, #2; "Also on this order:" before).
const otherPrintersHead = "ITEMS TO OTHER PRINTER"

// writeOtherPrinters is the ticket's bottom block (the owner, 2026-11-14, #2):
// ITEMS TO OTHER PRINTER, then " - 1   Fries" per item another printer
// makes, one size smaller than the ticket's base font (font A 1x1, the base
// is 1x2); the heading alone when there are none. Store sizes stay in
// brackets on every order here (an online order's size has no "- " line).
func writeOtherPrinters(w func(string), nl func(), others []order.LineItem, l layout) {
	w(cmdSizeReset)
	w(cmdNormalSz)
	w(otherPrintersHead)
	nl()
	for _, it := range others {
		q := fmt.Sprintf("%d", it.Quantity)
		hang := strings.Repeat(" ", len(" - ")+len(q)+len(qtyGap))
		for _, line := range wordWrap(" - "+q+qtyGap+kitchenName(it, false), l.cols, hang) {
			w(printable(line))
			nl()
		}
	}
	w(cmdBaseSz)
}

// kitchenTaker is who took the order as the kitchen ticket prints it: the
// call-center agent's name alone ("Call center - Jhoan" -> "Jhoan"); the
// customer slips keep the whole "Call center - Jhoan" (Cashier).
func kitchenTaker(by string) string {
	by = strings.TrimSpace(by)
	const seat = "call center"
	if len(by) >= len(seat) && strings.EqualFold(by[:len(seat)], seat) {
		if name := strings.TrimSpace(strings.TrimLeft(by[len(seat):], " -:")); name != "" {
			return name
		}
	}
	return by
}

// qtyGap is the gap after an item's quantity (the owner, 2026-11-14, #4: "1   6
// pc Party Wings", a clear gap between the quantity and a piece count); \x1f
// is a space wordWrap keeps.
const qtyGap = "\x1f\x1f\x1f"

// itemLabel is "2   Pepperoni Pizza (Large)", "1   6 pc Party Wings"
// (printable turns its \x1f into spaces).
func itemLabel(it order.LineItem, online bool) string {
	return fmt.Sprintf("%d%s%s", it.Quantity, qtyGap, kitchenName(it, online))
}

// labelHang is where a wrapped item name goes on: under the name.
func labelHang(it order.LineItem) string {
	return strings.Repeat(" ", len(fmt.Sprintf("%d", it.Quantity))+len(qtyGap))
}

// writeKitchenItems prints items: quantity + name bold at the store's item
// size, then every modifier and note on its own "- " line (halves under
// WHOLE: / 1ST HALF: / 2ND HALF:).
func writeKitchenItems(w func(string), nl func(), items []order.LineItem, t printer.Ticket, prefs order.ReceiptPrefs, lay layout, online bool) {
	sp := prefs.Spacing
	w(cmdLeft)
	for i, it := range items {
		if i > 0 {
			w(feedDots(sp.ItemItem))
		}
		w(cmdBoldOn)
		w(textSize(prefs.KitchenItemSize))
		for _, line := range wordWrap(itemLabel(it, online), lay.itemWidth(prefs.KitchenItemSize), labelHang(it)) {
			w(printable(line))
			nl()
		}
		w(cmdSizeReset)
		w(cmdBaseSz)
		if it.Routing != order.RoutingMatched || it.PrinterID != t.TargetID {
			w("** UNMATCHED **")
			nl()
		}
		w(cmdBoldOff)
		base := kitchenModIndent(prefs.KitchenItemSize, lay)
		for j, l := range modLines(it, online) {
			if j == 0 {
				w(feedDots(sp.ItemMod))
			} else {
				w(feedDots(sp.ModMod))
			}
			for _, line := range l.wrap(base, lay.cols) {
				w(printable(line))
				nl()
			}
		}
	}
}

// kitchenModIndent is where an item's choices start on a kitchen ticket
// (agent 2.5.7, the owner 2026-11-13: "every item's modifiers indented under
// the item"): two columns past the start of the item's name, which prints
// at the store's item size ("1   " at double width = 8 columns since the
// 2026-11-14 gap), so a choice never starts left of its item. The name's
// start is capped at 8 columns (xlarge items on 58 mm paper keep room for
// the choices).
func kitchenModIndent(size string, lay layout) int {
	wide := 1
	if c := lay.sizeCols(size); c > 0 {
		wide = max(lay.cols/c, 1) // 1 normal, 2 double, 3 quad, 4 huge
	}
	return min((1+len(qtyGap))*wide, 8) + 2
}

// ticketModLine is one line under an item on a kitchen ticket: a choice ("- X"),
// the food note ("Note: ..."), a half heading ("1ST HALF:"), or a topping
// of a half (level 1: nested under its heading).
type ticketModLine struct {
	text  string
	level int
}

// wrap lays the line out from column base (+ 2 per level); a wrapped line
// goes on under its text ("- " and "Note: " hang).
func (m ticketModLine) wrap(base, cols int) []string {
	lead := strings.Repeat(" ", base+2*m.level)
	hang := ""
	switch {
	case strings.HasPrefix(m.text, "- "):
		hang = "  "
	case strings.HasPrefix(m.text, noteMark):
		hang = strings.Repeat(" ", len(noteMark))
	}
	return wordWrap(lead+m.text, cols, lead+hang)
}

// noteMark starts an item's food note on the kitchen ticket ("Note: Please
// make well done"), so it never reads as a choice ("- Well done").
const noteMark = "Note: "

// modLines are an item's lines under it: "- Extra cheese", "- (x2) Ranch",
// the food note; a half-and-half pie's under its headings, each half's
// toppings nested under its heading.
func modLines(it order.LineItem, online bool) []ticketModLine {
	var out []ticketModLine
	if sz := kitchenSizeLine(it, online); sz != "" { // an online order's size, first (2.5.2)
		out = append(out, ticketModLine{text: "- " + sz})
	}
	if it.HasHalves() {
		out = append(out, halfLines(it)...)
	} else {
		for _, c := range it.Children {
			out = append(out, ticketModLine{text: "- " + modName(c, it.Quantity)})
		}
	}
	if note := strings.TrimSpace(it.Notes); note != "" { // the food note, told from a choice (the owner, 2.5.2)
		out = append(out, ticketModLine{text: noteMark + note})
	}
	return out
}

// modName is a choice's line: its name, "(x2) Ranch" only when it counts
// twice or more on each item (perItem). Never folded with another choice
// (the owner, 2026-11-14, #20): every child is its own line.
func modName(c order.LineItem, lineQty int) string {
	if n := perItem(c.Quantity, lineQty); n > 1 {
		return fmt.Sprintf("(x%d) %s", n, c.Name)
	}
	return c.Name
}

// perItem is a choice's count on each item: a counter line's children carry
// the line's quantity ("2 Pepperoni Pizza" + Extra cheese stores the child
// at 2: once per pizza), so a child's quantity that the line's divides is
// per item; any other is printed as it is.
func perItem(childQty, lineQty int) int {
	if lineQty > 1 && childQty >= lineQty && childQty%lineQty == 0 {
		return childQty / lineQty
	}
	return childQty
}

// halfLines is a half-and-half pie: WHOLE: (only when something is on the
// whole pie), then 1ST HALF: and 2ND HALF: always ("- nothing added" when a
// half has nothing). Each topping on its own line: a specialty or topping by
// name, "NO X", and "X ONLY" (ONLY after the topping it applies to).
func halfLines(it order.LineItem) []ticketModLine {
	var parts [3][]ticketModLine
	for _, c := range it.Children {
		k, name := halfIndex(c.Portion), bareName(c)
		switch c.Mode {
		case order.ModeNo:
			name = "NO " + name
		case order.ModeOnly:
			name = strings.TrimPrefix(name, "ONLY ") + " ONLY"
		}
		if n := perItem(c.Quantity, it.Quantity); n > 1 {
			name = fmt.Sprintf("(x%d) %s", n, name)
		}
		parts[k] = append(parts[k], ticketModLine{text: "- " + name, level: 1})
	}
	var out []ticketModLine
	if len(parts[0]) > 0 {
		out = append(append(out, ticketModLine{text: "WHOLE:"}), parts[0]...)
	}
	for k, head := range []string{"", "1ST HALF:", "2ND HALF:"} {
		if k == 0 {
			continue
		}
		out = append(out, ticketModLine{text: head})
		if len(parts[k]) == 0 {
			out = append(out, ticketModLine{text: "- nothing added", level: 1})
		}
		out = append(out, parts[k]...)
	}
	return out
}

// writeRemoved prints removed lines: "(VOID) " + the line struck through
// (its modifiers too, under it), at font A so the 24-dot picture sits in the
// text line. Without pictures: "(VOID)" in reverse + the text.
func writeRemoved(w func(string), nl func(), items []order.LineItem, prefs order.ReceiptPrefs, lay layout, online bool) {
	w(cmdLeft)
	w(cmdSizeReset)
	w(cmdNormalSz)
	w(cmdBoldOn)
	room := lay.cols - len(voidMark)
	base := kitchenModIndent(order.SizeNormal, lay) // the removed lines print at the normal size
	for _, it := range items {
		parts := [][]string{wordWrap(itemLabel(it, online), room, labelHang(it))}
		for _, l := range modLines(it, online) {
			parts = append(parts, l.wrap(base, room))
		}
		for i, wrapped := range parts {
			for j, part := range wrapped {
				switch {
				case i == 0 && j == 0 && prefs.StrikeImages:
					w(voidMark)
				case i == 0 && j == 0:
					w(cmdInvertOn + strings.TrimSpace(voidMark) + cmdInvertOff + " ")
				default:
					w(strings.Repeat(" ", len(voidMark)))
				}
				if prefs.StrikeImages {
					text := strings.TrimLeft(part, " ")
					w(part[:len(part)-len(text)]) // the indent stays blank
					w(strikeImage(strings.ReplaceAll(text, "\x1f", " ")))
				} else {
					w(printable(part))
				}
				nl()
			}
		}
	}
	w(cmdBoldOff)
	w(cmdBaseSz)
}
