// Package receipt builds ESC/POS byte streams from the canonical Snapshot.
// Store-level identity (name, address, phone, receipt footer) comes from the
// StoreContext main/ pushes on the SSE handshake — this package does not
// look anything up from disk or env.
package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

const (
	lineWidth    = 41 // 80mm thermal at base 2x font width
	marginSpaces = 0

	cmdInit      = "\x1b\x40"
	cmdCenter    = "\x1b\x61\x01"
	cmdLeft      = "\x1b\x61\x00"
	cmdBoldOn    = "\x1b\x45\x01"
	cmdBoldOff   = "\x1b\x45\x00"
	cmdDoubleSz  = "\x1d\x21\x11"
	cmdQuadSz    = "\x1d\x21\x22"
	cmdHugeSz    = "\x1d\x21\x33" // triple width + height
	cmdBaseSz    = "\x1b\x21\x10"
	cmdNormalSz  = "\x1b\x21\x00" // font A, single height
	cmdSmallSz   = "\x1b\x21\x01" // font B — the "rest of the order" list
	cmdFeed      = "\x0a"
	cmdCut       = "\x1d\x56\x42\x03"
	cmdInvertOn  = "\x1d\x42\x01"
	cmdInvertOff = "\x1d\x42\x00"
)

// Build renders one planned ticket with the store's receipt settings: a
// kitchen station ticket (BuildStation) or the customer receipt (repeated for
// prefs.Copies, each copy cut), with a "REROUTED from X" banner when it
// printed on a fallback printer. order.DefaultReceiptPrefs() prints exactly
// what the agent printed before the settings existed.
func Build(snap order.Snapshot, sc storeconfig.StoreContext, t printer.Ticket, prefs order.ReceiptPrefs) []byte {
	snap = localize(snap, sc)
	prefs = prefs.Normalize()
	if t.Kind == printer.TicketStation {
		return buildStation(snap, t, prefs)
	}
	out := buildFull(snap, sc, t.ReroutedFrom, prefs)
	if prefs.Copies > 1 {
		return bytes.Repeat(out, prefs.Copies)
	}
	return out
}

// BuildWith renders a canonical Snapshot into ESC/POS bytes using the
// provided StoreContext for the per-store header + footer.
//
// Money convention: every amount on the wire is integer cents; we render
// them as dollar strings here ("$X.YY") at format time and never anywhere
// else.
//
// Default receipt settings; Build takes the store's.
func BuildWith(snap order.Snapshot, sc storeconfig.StoreContext) []byte {
	return buildFull(localize(snap, sc), sc, "", order.DefaultReceiptPrefs())
}

// localize puts the order's times in the location's timezone so tickets show
// the restaurant's clock (main's timestamps arrive in UTC). An unknown or
// empty timezone leaves them as they are.
func localize(snap order.Snapshot, sc storeconfig.StoreContext) order.Snapshot {
	loc, err := time.LoadLocation(sc.Location.Timezone)
	if sc.Location.Timezone == "" || err != nil {
		return snap
	}
	snap.PlacedAt = snap.PlacedAt.In(loc)
	if p := snap.Fulfillment.PromisedAt; p != nil {
		t := p.In(loc)
		snap.Fulfillment.PromisedAt = &t
	}
	return snap
}

func buildFull(snap order.Snapshot, sc storeconfig.StoreContext, reroutedFrom string, prefs order.ReceiptPrefs) []byte {
	var buf bytes.Buffer
	w := func(s string) {
		if len(s) > 0 && (s[0] == '\x1b' || s[0] == '\x1d' || s[0] == '\x0a') {
			buf.WriteString(s)
		} else {
			buf.WriteString(margin(s))
		}
	}
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz)
	if reroutedFrom != "" {
		writeRerouted(w, nl, reroutedFrom)
	}

	// --- Header: order id + service type + store identity --------------
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdQuadSz)
	writeOrderNumbers(w, nl, snap)
	w(cmdDoubleSz)
	w(formatServiceType(snap.Fulfillment.Type))
	nl()
	w(cmdBoldOff)
	w(cmdBaseSz)

	if prefs.ShowStoreAddress {
		if sc.Location.Name != "" {
			w(sc.Location.Name)
			nl()
		}
		if sc.Location.AddressLine1 != "" {
			w(sc.Location.AddressLine1)
			nl()
		}
		if sc.Location.AddressLine2 != "" {
			w(sc.Location.AddressLine2)
			nl()
		}
	}

	// --- Promised-for-later flag ---------------------------------------
	if snap.Fulfillment.PromisedAt != nil && !snap.Fulfillment.PromisedAt.IsZero() {
		w(cmdInvertOn)
		w(cmdDoubleSz)
		w(cmdCenter)
		w(cmdBoldOn)
		w("        FUTURE      ")
		w(cmdInvertOff)
		nl()
		nl()
		w(cmdBaseSz)
		w(" " + snap.Fulfillment.PromisedAt.Format("Jan 2, 2006 ----------  3:04 PM") + " ")
		w(cmdBoldOff)
		nl()
	}

	// --- Customer ------------------------------------------------------
	if name := strings.TrimSpace(snap.Customer.DisplayName); name != "" {
		nl()
		nl()
		w(cmdBoldOn)
		w(cmdDoubleSz)
		w(cmdCenter)
		w(" " + lastFirst(name) + " ")
		w(cmdBaseSz)
		w(cmdBoldOff)
		nl()
	}
	if phone := strings.TrimSpace(snap.Customer.Phone); phone != "" && prefs.ShowPhone {
		nl()
		w(cmdBoldOn)
		w(cmdDoubleSz)
		w("Phone: " + formatPhone(phone))
		w(cmdBoldOff)
		w(cmdBaseSz)
		nl()
	}

	// --- Placed at -----------------------------------------------------
	w(separator())
	nl()
	w(cmdBoldOn)
	w(snap.PlacedAt.Format("Jan 2, 2006 ----------  3:04 PM"))
	w(cmdBoldOff)
	nl()

	// --- Delivery address ----------------------------------------------
	if addr := snap.Fulfillment.Address; addr != nil && strings.ToLower(snap.Fulfillment.Type) == "delivery" {
		w(separator())
		nl()
		w(cmdLeft)
		w(cmdDoubleSz)
		w(cmdBoldOn)
		w("St: " + addr.Line1)
		w(cmdBoldOff)
		w(cmdBaseSz)
		nl()
		w("Apt: " + addr.Line2)
		nl()
		w("City: " + addr.City)
		nl()
		w("Zip: " + addr.PostalCode)
		nl()
		nl()
	}

	// --- Notes ---------------------------------------------------------
	if strings.TrimSpace(snap.Notes) != "" {
		w(cmdCenter)
		w(separator())
		nl()
		w("Notes: " + snap.Notes)
		nl()
	}

	// --- Payment -------------------------------------------------------
	w(separator())
	w(cmdCenter)
	nl()
	w(cmdBoldOn)
	w(cmdDoubleSz)
	if len(snap.Payments) == 0 || !hasCardPayment(snap.Payments) {
		w("CASH - " + dollars(snap.Money.Total))
	} else {
		// First card payment drives the displayed last4/auth code.
		card := firstCardPayment(snap.Payments)
		w("PAID - CARD")
		w(cmdBaseSz)
		w(cmdLeft)
		nl()
		if card.Last4 != "" {
			w("Card: ****" + card.Last4)
			nl()
		}
		if card.AuthCode != "" {
			w("AuthCode: " + card.AuthCode)
			nl()
		}
	}
	w(cmdBaseSz)
	w(cmdLeft)
	nl()

	// --- Items ---------------------------------------------------------
	w(separator())
	nl()
	w(cmdBoldOn)
	w(columnHeader())
	w(cmdBoldOff)
	nl()

	for _, item := range snap.Items {
		w(separator())
		nl()
		itemSum := int64(item.Quantity) * item.UnitPrice
		size := strings.TrimSpace(item.Size)
		w(cmdBoldOn)
		if size != "" {
			w(itemLine(item.Quantity, size, item.UnitPrice))
		} else {
			w(itemLine(item.Quantity, item.Name, item.UnitPrice))
		}
		w(cmdBoldOff)
		nl()
		if size != "" {
			w(cmdLeft)
			w("    " + item.Name)
			nl()
		}

		for _, mod := range item.Children {
			label := mod.Name
			if mod.Quantity > 1 {
				label = fmt.Sprintf("(x%d) %s", mod.Quantity, label)
			}
			// Sides (fries, salad) stand out, as on the v1 receipt.
			side := strings.Contains(strings.ToLower(mod.Name), "fries") || strings.Contains(strings.ToLower(mod.Name), "salad")
			if side {
				w(cmdBoldOn)
			}
			w(modifierLine(label, mod.UnitPrice))
			if side {
				w(cmdBoldOff)
			}
			nl()
			itemSum += mod.UnitPrice * int64(max(mod.Quantity, 1))
		}
		if strings.TrimSpace(item.Notes) != "" {
			w("    Note: " + item.Notes)
			nl()
		}
		w(rightAlign("Sum: "+dollars(itemSum), lineWidth))
		nl()
	}

	w(separator())
	nl()

	// --- Fees ----------------------------------------------------------
	for _, fee := range snap.Money.Fees {
		label := fee.Label
		if label == "" {
			label = fee.Type
		}
		if strings.ToLower(snap.Fulfillment.Type) != "delivery" && strings.EqualFold(fee.Type, "delivery") {
			continue
		}
		w(rightPair(label+":", dollars(fee.Amount)))
		nl()
	}

	// --- Totals --------------------------------------------------------
	w(rightPair("Subtotal:", dollars(snap.Money.Subtotal)))
	nl()
	if snap.Money.Tax > 0 {
		w(rightPair("Tax:", dollars(snap.Money.Tax)))
		nl()
	}
	if snap.Money.Tip > 0 {
		w(rightPair("Tips :", dollars(snap.Money.Tip)))
		nl()
	}
	for _, d := range snap.Money.Discounts {
		label := d.Label
		if label == "" {
			label = d.Type
		}
		w(rightPair(label+":", "-"+dollars(d.Amount)))
		nl()
	}
	w(rightPair("Total:", dollars(snap.Money.Total)))

	nl()
	w(separator())
	nl()
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdCenter)
	nl()
	if len(snap.Payments) == 0 || !hasCardPayment(snap.Payments) {
		w("Collect Cash: " + dollars(snap.Money.Total))
	} else {
		w("Order Total:" + dollars(snap.Money.Total))
	}
	w(cmdBoldOff)
	w(cmdBaseSz)
	nl()
	nl()

	// --- Footer --------------------------------------------------------
	// The setting, else the location's receipt_footer, else ONLINE ORDER;
	// an empty setting prints no footer line.
	if footer := prefs.Footer(sc.LocationConfigString("receipt_footer", "")); footer != "" {
		w(cmdBoldOn)
		w(textSize(prefs.FooterSize))
		w(cmdCenter)
		w(footer)
		w(cmdBoldOff)
		w(cmdBaseSz)
	}
	if prefs.ExtraPaper {
		// v1's ending: cut, feed well past it, cut again.
		w(cmdCut)
		nl()
		for i := 0; i < 4; i++ {
			nl()
		}
		w(cmdCut)
	} else {
		for i := 0; i < 3; i++ {
			nl()
		}
		w(cmdCut)
	}
	return buf.Bytes()
}

// BuildStation renders a kitchen station ticket:
//
//	#1123                      (biggest)
//	GONZALEZ, MARIA
//	-----------------------------------------
//	2 Pepperoni Pizza (Large)       (big, bold)
//	  - Extra cheese             $2.00
//	  - Note: well done
//	-----------------------------------------
//	ITEMS TO OTHER PRINTERS
//	2 Buffalo Wings (10 pc)         (one size smaller)
//
// A REROUTED banner tops a ticket printed on a fallback printer, a scheduled
// order says when it's for, items main couldn't route here are flagged
// ** UNMATCHED **, and a food note sits under its item (the order's notes are
// on the customer receipt only). Modifiers show a price only when they aren't
// free. Default receipt settings; Build takes the store's (item and number
// sizes).
func BuildStation(snap order.Snapshot, t printer.Ticket) []byte {
	return buildStation(snap, t, order.DefaultReceiptPrefs())
}

func buildStation(snap order.Snapshot, t printer.Ticket, prefs order.ReceiptPrefs) []byte {
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz)
	if t.ReroutedFrom != "" {
		writeRerouted(w, nl, t.ReroutedFrom)
	}

	// --- Ticket number, customer ---------------------------------------------
	w(cmdCenter)
	w(cmdBoldOn)
	w(textSize(prefs.KitchenNumberSize))
	writeOrderNumbers(w, nl, snap)
	if name := strings.TrimSpace(snap.Customer.DisplayName); name != "" {
		w(cmdDoubleSz)
		w(lastFirst(name))
		nl()
	}
	w(cmdBaseSz)
	if snap.Fulfillment.PromisedAt != nil && !snap.Fulfillment.PromisedAt.IsZero() {
		w(cmdInvertOn)
		w(" FUTURE " + snap.Fulfillment.PromisedAt.Format("Jan 2  3:04 PM") + " ")
		w(cmdInvertOff)
		nl()
	}
	w(cmdBoldOff)
	w(cmdLeft)
	w(strings.Repeat("-", stationWidth))
	nl()

	// --- This station's items ----------------------------------------------
	for _, it := range t.Mine {
		w(cmdBoldOn)
		w(textSize(prefs.KitchenItemSize))
		for _, line := range wordWrap(fmt.Sprintf("%d %s", it.Quantity, itemName(it)), itemWidth(prefs.KitchenItemSize), "  ") {
			w(line)
			nl()
		}
		w(cmdBaseSz)
		w(cmdBoldOff)
		if it.Routing != order.RoutingMatched || it.PrinterID != t.TargetID {
			w(cmdInvertOn)
			w(" ** UNMATCHED ** ")
			w(cmdInvertOff)
			nl()
		}
		for _, mod := range it.Children {
			label := mod.Name
			if mod.Quantity > 1 {
				label = fmt.Sprintf("(x%d) %s", mod.Quantity, label)
			}
			w(stationModifierLine(label, mod.UnitPrice))
			nl()
		}
		if note := strings.TrimSpace(it.Notes); note != "" {
			w(cmdBoldOn)
			for _, line := range wordWrap("  - Note: "+note, stationWidth, "          ") {
				w(line)
				nl()
			}
			w(cmdBoldOff)
		}
	}
	w(strings.Repeat("-", stationWidth))
	nl()

	// --- The rest of the order, one size smaller ---------------------------------
	if len(t.Others) > 0 {
		w(cmdNormalSz)
		w(cmdBoldOn)
		w("ITEMS TO OTHER PRINTERS")
		w(cmdBoldOff)
		nl()
		for _, it := range t.Others {
			for _, line := range wordWrap(fmt.Sprintf("%d %s", it.Quantity, itemName(it)), stationWidth, "  ") {
				w(line)
				nl()
			}
		}
		w(cmdBaseSz)
	}

	for i := 0; i < 4; i++ {
		nl()
	}
	w(cmdCut)
	return buf.Bytes()
}

// The kitchen ticket uses the full 80 mm line: 48 characters in the base font,
// 24 at double size, 16 at quad. (The customer receipt keeps v1's 41-column
// layout.)
const (
	stationWidth = 48
	bigLineWidth = 24
	xlLineWidth  = 16
)

// textSize maps a receipt-settings size onto its ESC/POS size command:
// normal = the base font (double height), large = double, xlarge = quad,
// huge = triple. Validation keeps each setting to its own subset.
func textSize(size string) string {
	switch size {
	case order.SizeNormal:
		return cmdBaseSz
	case order.SizeLarge:
		return cmdDoubleSz
	case order.SizeHuge:
		return cmdHugeSz
	default:
		return cmdQuadSz
	}
}

// itemWidth is how many characters of a kitchen item fit on a line at size.
func itemWidth(size string) int {
	switch size {
	case order.SizeNormal:
		return stationWidth
	case order.SizeXLarge:
		return xlLineWidth
	default:
		return bigLineWidth
	}
}

// wordWrap breaks text into lines of at most width characters at spaces (a
// word longer than a line is split), continuation lines starting with indent —
// the printer itself would break mid-word.
func wordWrap(text string, width int, indent string) []string {
	var lines []string
	// Leading spaces (e.g. the "  - " of a note) stay on the first line.
	lead := text[:len(text)-len(strings.TrimLeft(text, " "))]
	cur := ""
	for _, word := range strings.Fields(text) {
		for utf8.RuneCountInString(word) > width-utf8.RuneCountInString(indent) && cur == "" && len(lines) > 0 {
			r := []rune(word)
			n := width - utf8.RuneCountInString(indent)
			lines = append(lines, indent+string(r[:n]))
			word = string(r[n:])
		}
		switch {
		case cur == "":
			cur = lead + word
			if len(lines) > 0 {
				cur = indent + word
			}
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(word) <= width:
			cur += " " + word
		default:
			lines = append(lines, cur)
			cur = indent + word
		}
	}
	if cur != "" || len(lines) == 0 {
		lines = append(lines, cur)
	}
	return lines
}

// stationModifierLine is "  - Extra cheese" with the price right-aligned when
// the modifier isn't free.
func stationModifierLine(name string, priceCents int64) string {
	label := "  - " + name
	if priceCents == 0 {
		return label
	}
	return leftRight(truncRunes(label, stationWidth-9), dollars(priceCents), stationWidth)
}

// writeOrderNumbers prints the big header number: "#<kitchen number>" (it took
// the place of the vendor's order number on the v1 receipt — staff call orders
// by it, and the tablet shows the same number); "#<vendor order id>" only for
// an order that has none yet. Expects quad size on entry and leaves it on.
func writeOrderNumbers(w func(string), nl func(), snap order.Snapshot) {
	if snap.KitchenNumber == nil {
		w("#" + snap.SourceEventID)
	} else {
		w(fmt.Sprintf("#%d", *snap.KitchenNumber))
	}
	nl()
}

// writeRerouted prints the inverted "REROUTED from X" banner that tops a
// ticket printed on a fallback printer.
func writeRerouted(w func(string), nl func(), from string) {
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdInvertOn)
	w(" REROUTED from " + strings.ToUpper(from) + " ")
	w(cmdInvertOff)
	w(cmdBaseSz)
	w(cmdBoldOff)
	nl()
	nl()
}

// itemName is the kitchen-ticket item label: "Name (Size)" when the size says
// something (see displaySize).
func itemName(it order.LineItem) string {
	if sz := displaySize(it.Size); sz != "" {
		return it.Name + " (" + sz + ")"
	}
	return it.Name
}

// displaySize drops the vendor's placeholder size: "Serving" (a single-size
// item) prints nothing and "Serving (2 Piece)" prints "2 Piece". The customer
// receipt keeps the vendor's size as v1 printed it.
func displaySize(size string) string {
	s := strings.TrimSpace(size)
	if strings.EqualFold(s, "serving") {
		return ""
	}
	if len(s) > len("serving") && strings.EqualFold(s[:len("serving")], "serving") {
		rest := strings.TrimSpace(s[len("serving"):])
		if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
			return strings.TrimSpace(rest[1 : len(rest)-1])
		}
	}
	return s
}

// --- formatters -----------------------------------------------------------

func dollars(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

func hasCardPayment(payments []order.PaymentSummary) bool {
	for _, p := range payments {
		if p.Method == "card" {
			return true
		}
	}
	return false
}

func firstCardPayment(payments []order.PaymentSummary) order.PaymentSummary {
	for _, p := range payments {
		if p.Method == "card" {
			return p
		}
	}
	return order.PaymentSummary{}
}

func separator() string { return strings.Repeat("-", lineWidth) }

func margin(s string) string {
	return strings.Repeat(" ", marginSpaces) + s
}

func leftRight(left, right string, width int) string {
	space := width - len(left) - len(right)
	if space < 1 {
		space = 1
	}
	return left + strings.Repeat(" ", space) + right
}

func rightAlign(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

func rightPair(label, value string) string {
	return leftRight(strings.Repeat(" ", 19)+label, value, lineWidth)
}

func columnHeader() string {
	return fmt.Sprintf("%-3s %-29s %7s", "Qty", "Item", "Price")
}

func itemLine(qty int, name string, unitCents int64) string {
	return fmt.Sprintf("%-3d %-29s %7s", qty, truncRunes(name, 29), dollars(unitCents))
}

// modifierLine is a modifier under its item; free modifiers carry no price.
func modifierLine(name string, priceCents int64) string {
	indent := "     "
	if priceCents == 0 {
		return indent + truncRunes(name, 36)
	}
	return fmt.Sprintf("%s%-28s %7s", indent, truncRunes(name, 28), dollars(priceCents))
}

// lastFirst turns "Maria Gonzalez" into "GONZALEZ, MARIA" (the v1 receipt's
// customer line); a single word is just upper-cased.
func lastFirst(name string) string {
	f := strings.Fields(name)
	if len(f) < 2 {
		return strings.ToUpper(name)
	}
	return strings.ToUpper(f[len(f)-1] + ", " + strings.Join(f[:len(f)-1], " "))
}

// truncRunes cuts s to at most n characters without splitting a multi-byte
// character (a byte cut would print garbage on the ticket).
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func formatServiceType(t string) string {
	switch strings.ToLower(t) {
	case "delivery":
		return "Delivery"
	case "pickup":
		return "Pickup"
	case "dine_in":
		return "Dine In"
	default:
		if t != "" {
			return strings.ToUpper(t)
		}
		return "Online Order"
	}
}

func formatPhone(p string) string {
	digits := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c >= '0' && c <= '9' {
			digits = append(digits, c)
		}
	}
	if len(digits) == 10 {
		return fmt.Sprintf("%s-%s-%s", digits[:3], digits[3:6], digits[6:])
	}
	return p
}
