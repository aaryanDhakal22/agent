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
// under the rail, extra big. Reverse print only for a future order's band
// and a change ticket's ADD / REMOVE bands.
//
//	          COUNTER                 (double size: 2 lines)   ┐
//	       Printed 6:31 PM            (1 line)                 │ under the rail
//	                                  (1 line)                 ┘
//	            12                    (extra big, bold)
//	         SMITH, ANA               (bold)
//	[ MAKE AT 6:30 · READY 6:45 ]     (reverse: a future order only)
//	------------------------------------------------
//	NOTE: ring the side bell          (bold: the order's note, when it has one)
//	------------------------------------------------
//	2  PEPPERONI PIZZA (LARGE)        (bold, the store's item size)
//	   - Extra cheese
//	   - Well done                    (a food note is a line too)
//	1  CHEESE PIZZA (XL)
//	   WHOLE:
//	   - Extra cheese
//	   1ST HALF:
//	   - Pepperoni ONLY
//	   2ND HALF:
//	   - Hawaiian
//	------------------------------------------------
//	Also on this order:
//	   1 Fries
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
	return buildStation(snap, t, order.DefaultReceiptPrefs())
}

func buildStation(snap order.Snapshot, t printer.Ticket, prefs order.ReceiptPrefs) []byte {
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }

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
	w(numberSize(num))
	w(num)
	nl()
	w(cmdSizeReset)
	w(cmdBaseSz)
	if name := strings.TrimSpace(snap.Customer.DisplayName); name != "" {
		w(lastFirst(name))
		nl()
	}
	if t.ReroutedFrom != "" {
		w("REROUTED FROM " + strings.ToUpper(t.ReroutedFrom))
		nl()
	}
	w(cmdBoldOff)
	if p := snap.Fulfillment.PromisedAt; p != nil && !p.IsZero() {
		writeFutureBand(w, nl, snap, *p)
	}
	w(cmdLeft)
	w(strings.Repeat(ruleChar, stationWidth))
	nl()
	// The order's note (the owner, 2026-11-12: on the station tickets too,
	// under the header; the receipt keeps it as well).
	if note := strings.TrimSpace(snap.Notes); note != "" {
		w(cmdBoldOn)
		for _, l := range wordWrap(printable("NOTE: "+note), stationWidth, "      ") {
			w(l)
			nl()
		}
		w(cmdBoldOff)
		w(strings.Repeat(ruleChar, stationWidth))
		nl()
	}

	if change && t.WasType != "" {
		// Moved takeout <-> delivery after SEND: the new type (also on top) and the old one.
		writeBand(w, nl, "NOW "+stationType(snap))
		w(cmdCenter)
		w(cmdBoldOn)
		w("(WAS " + typeWord(t.WasType) + ")")
		w(cmdBoldOff)
		nl()
		w(cmdLeft)
		if t.TypeOnly {
			nl()
			writeKitchenItems(w, nl, added, t, prefs)
			added = nil
		}
	}
	if change {
		if len(added) > 0 {
			writeBand(w, nl, "ADD / AGREGAR")
			writeKitchenItems(w, nl, added, t, prefs)
		}
		if len(removed) > 0 {
			if len(added) > 0 {
				nl()
			}
			writeBand(w, nl, "REMOVE / QUITAR")
			writeRemoved(w, nl, removed, prefs)
		}
	} else {
		writeKitchenItems(w, nl, added, t, prefs)
		w(strings.Repeat(ruleChar, stationWidth))
		nl()
		w("Also on this order:")
		nl()
		if len(t.Others) == 0 {
			w(itemIndent + "nothing else")
			nl()
		}
		for _, it := range t.Others {
			for _, line := range wordWrap(fmt.Sprintf("%s%d %s", itemIndent, it.Quantity, itemName(it)), stationWidth, itemIndent+"  ") {
				w(printable(line))
				nl()
			}
		}
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

// numberSize is the largest size the number fits on the line at: 4 x 4 (up
// to 12 characters), 3 x 3 (16), else double.
func numberSize(num string) string {
	switch n := len([]rune(num)); {
	case n <= stationWidth/4:
		return cmdHugeSz
	case n <= xlLineWidth:
		return cmdQuadSz
	}
	return cmdDoubleSz
}

// writeFutureBand is a future order's one reverse band: when the kitchen
// starts it and when it is due ("MAKE AT 6:30 · READY 6:45"; READY alone
// when there is nothing to count back from), with the day under it when it
// isn't today.
func writeFutureBand(w func(string), nl func(), snap order.Snapshot, ready time.Time) {
	text := "READY " + ready.Format("3:04")
	if m := snap.MakeAt; m != nil && !m.IsZero() {
		text = "MAKE AT " + m.In(ready.Location()).Format("3:04") + " \xfa " + text
	}
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdBaseSz)
	w(cmdInvertOn)
	w(pad(text, stationWidth))
	if now := clock().In(ready.Location()); now.YearDay() != ready.YearDay() || now.Year() != ready.Year() {
		nl()
		w(pad(strings.ToUpper(ready.Format("Mon Jan 2")), stationWidth))
	}
	w(cmdInvertOff)
	w(cmdBoldOff)
	nl()
}

// writeBand is a change ticket's reverse band, centred, one size up (double).
func writeBand(w func(string), nl func(), text string) {
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdInvertOn)
	w(pad(text, bigLineWidth))
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

// itemLabel is "2  PEPPERONI PIZZA (LARGE)" (printable turns its \x1f into
// spaces).
func itemLabel(it order.LineItem) string {
	return fmt.Sprintf("%d\x1f\x1f%s", it.Quantity, strings.ToUpper(itemName(it))) // \x1f: a space wordWrap keeps
}

// writeKitchenItems prints items: quantity + name bold at the store's item
// size, then every modifier and note on its own "- " line (halves under
// WHOLE: / 1ST HALF: / 2ND HALF:).
func writeKitchenItems(w func(string), nl func(), items []order.LineItem, t printer.Ticket, prefs order.ReceiptPrefs) {
	sp := prefs.Spacing
	w(cmdLeft)
	for i, it := range items {
		if i > 0 {
			w(feedDots(sp.ItemItem))
		}
		w(cmdBoldOn)
		w(textSize(prefs.KitchenItemSize))
		for _, line := range wordWrap(itemLabel(it), itemWidth(prefs.KitchenItemSize), itemIndent) {
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
		for j, l := range modLines(it) {
			if j == 0 {
				w(feedDots(sp.ItemMod))
			} else {
				w(feedDots(sp.ModMod))
			}
			for _, line := range wordWrap(itemIndent+l, stationWidth, itemIndent+"  ") {
				w(printable(line))
				nl()
			}
		}
	}
}

// modLines are an item's lines under it: "- Extra cheese", "- (x2) Ranch",
// the food note "- Well done"; a half-and-half pie's under its headings.
func modLines(it order.LineItem) []string {
	var out []string
	if it.HasHalves() {
		out = halfLines(it)
	} else {
		for _, c := range it.Children {
			out = append(out, "- "+modName(c))
		}
	}
	if note := strings.TrimSpace(it.Notes); note != "" {
		out = append(out, "- "+note)
	}
	return out
}

func modName(c order.LineItem) string {
	if c.Quantity > 1 {
		return fmt.Sprintf("(x%d) %s", c.Quantity, c.Name)
	}
	return c.Name
}

// halfLines is a half-and-half pie: WHOLE: (only when something is on the
// whole pie), then 1ST HALF: and 2ND HALF: always ("- nothing added" when a
// half has nothing). Each topping on its own line: a specialty or topping by
// name, "NO X", and "X ONLY" (ONLY after the topping it applies to).
func halfLines(it order.LineItem) []string {
	var parts [3][]string
	for _, c := range it.Children {
		k, name := halfIndex(c.Portion), bareName(c)
		switch c.Mode {
		case order.ModeNo:
			name = "NO " + name
		case order.ModeOnly:
			name = strings.TrimPrefix(name, "ONLY ") + " ONLY"
		}
		if c.Quantity > 1 {
			name = fmt.Sprintf("(x%d) %s", c.Quantity, name)
		}
		parts[k] = append(parts[k], "- "+name)
	}
	var out []string
	if len(parts[0]) > 0 {
		out = append(append(out, "WHOLE:"), parts[0]...)
	}
	for k, head := range []string{"", "1ST HALF:", "2ND HALF:"} {
		if k == 0 {
			continue
		}
		out = append(out, head)
		if len(parts[k]) == 0 {
			out = append(out, "- nothing added")
		}
		out = append(out, parts[k]...)
	}
	return out
}

// writeRemoved prints removed lines: "(VOID) " + the line struck through
// (its modifiers too, under it), at font A so the 24-dot picture sits in the
// text line. Without pictures: "(VOID)" in reverse + the text.
func writeRemoved(w func(string), nl func(), items []order.LineItem, prefs order.ReceiptPrefs) {
	w(cmdLeft)
	w(cmdSizeReset)
	w(cmdNormalSz)
	w(cmdBoldOn)
	room := stationWidth - len(voidMark)
	for _, it := range items {
		lines := []string{itemLabel(it)}
		for _, l := range modLines(it) {
			lines = append(lines, itemIndent+l)
		}
		for i, l := range lines {
			for j, part := range wordWrap(l, room, itemIndent+"  ") {
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
