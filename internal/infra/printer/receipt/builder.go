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
	cmdBaseSz    = "\x1b\x21\x10"
	cmdNormalSz  = "\x1b\x21\x00" // font A, single height
	cmdSmallSz   = "\x1b\x21\x01" // font B — the "rest of the order" list
	cmdFeed      = "\x0a"
	cmdCut       = "\x1d\x56\x42\x03"
	cmdInvertOn  = "\x1d\x42\x01"
	cmdInvertOff = "\x1d\x42\x00"
)

// Build renders one planned ticket: a kitchen station ticket (BuildStation) or
// a full receipt, with a "REROUTED from X" banner when it printed on a
// fallback printer.
func Build(snap order.Snapshot, sc storeconfig.StoreContext, t printer.Ticket) []byte {
	snap = localize(snap, sc)
	if t.Kind == printer.TicketStation {
		return BuildStation(snap, t)
	}
	return buildFull(snap, sc, t.ReroutedFrom)
}

// BuildWith renders a canonical Snapshot into ESC/POS bytes using the
// provided StoreContext for the per-store header + footer.
//
// Money convention: every amount on the wire is integer cents; we render
// them as dollar strings here ("$X.YY") at format time and never anywhere
// else.
func BuildWith(snap order.Snapshot, sc storeconfig.StoreContext) []byte {
	return buildFull(localize(snap, sc), sc, "")
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

func buildFull(snap order.Snapshot, sc storeconfig.StoreContext, reroutedFrom string) []byte {
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
		w(" " + strings.ToUpper(name) + " ")
		w(cmdBaseSz)
		w(cmdBoldOff)
		nl()
	}
	if phone := strings.TrimSpace(snap.Customer.Phone); phone != "" {
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
		if addr.Line2 != "" {
			w("Apt: " + addr.Line2)
			nl()
		}
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
		w(cmdBoldOn)
		w(itemLine(item.Quantity, itemName(item), item.UnitPrice))
		w(cmdBoldOff)
		nl()

		for _, mod := range item.Children {
			label := mod.Name
			if mod.Quantity > 1 {
				label = fmt.Sprintf("(x%d) %s", mod.Quantity, label)
			}
			w(modifierLine(label, mod.UnitPrice))
			nl()
		}
		if strings.TrimSpace(item.Notes) != "" {
			w("    Note: " + item.Notes)
			nl()
		}
		w(rightAlign("Sum: "+dollars(item.LineTotal), lineWidth))
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
	footer := sc.LocationConfigString("receipt_footer", "ONLINE ORDER")
	w(cmdBoldOn)
	w(cmdQuadSz)
	w(cmdCenter)
	w(footer)
	w(cmdBoldOff)
	w(cmdBaseSz)
	w(cmdCut)
	nl()

	for i := 0; i < 4; i++ {
		nl()
	}
	w(cmdCut)
	return buf.Bytes()
}

// BuildStation renders a kitchen station ticket: the order header, the
// station's name, its own items in full (size, modifiers, notes — items main
// couldn't route to this station are flagged ** UNMATCHED **), then a rule and
// the rest of the order in the small font so the cook knows what else is on it.
// No prices, payment or address — that's the customer receipt's job.
func BuildStation(snap order.Snapshot, t printer.Ticket) []byte {
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz)
	if t.ReroutedFrom != "" {
		writeRerouted(w, nl, t.ReroutedFrom)
	}

	// --- Header ------------------------------------------------------------
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdQuadSz)
	writeOrderNumbers(w, nl, snap)
	w(cmdDoubleSz)
	w(formatServiceType(snap.Fulfillment.Type))
	nl()
	w(cmdBaseSz)
	w(cmdInvertOn)
	w(" " + strings.ToUpper(t.Station) + " ")
	w(cmdInvertOff)
	w(cmdBoldOff)
	nl()
	if name := strings.TrimSpace(snap.Customer.DisplayName); name != "" {
		w(strings.ToUpper(name))
		nl()
	}
	if snap.Fulfillment.PromisedAt != nil && !snap.Fulfillment.PromisedAt.IsZero() {
		w(cmdBoldOn)
		w("FUTURE: " + snap.Fulfillment.PromisedAt.Format("Jan 2  3:04 PM"))
		w(cmdBoldOff)
	} else {
		w(snap.PlacedAt.Format("Jan 2  3:04 PM"))
	}
	nl()
	w(cmdLeft)

	// --- This station's items ----------------------------------------------
	for _, it := range t.Mine {
		w(separator())
		nl()
		w(cmdBoldOn)
		w(cmdDoubleSz)
		w(fmt.Sprintf("%d x %s", it.Quantity, itemName(it)))
		w(cmdBaseSz)
		w(cmdBoldOff)
		nl()
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
			w("   + " + label)
			nl()
		}
		if note := strings.TrimSpace(it.Notes); note != "" {
			w(cmdBoldOn)
			w("   Note: " + note)
			w(cmdBoldOff)
			nl()
		}
	}
	if note := strings.TrimSpace(snap.Notes); note != "" {
		w(separator())
		nl()
		w(cmdBoldOn)
		w("Order notes: " + note)
		w(cmdBoldOff)
		nl()
	}
	w(strings.Repeat("=", lineWidth))
	nl()

	// --- The rest of the order, small ----------------------------------------
	if len(t.Others) > 0 {
		w(cmdSmallSz)
		w("Also on this order:")
		nl()
		for _, it := range t.Others {
			w(fmt.Sprintf("  %d %s", it.Quantity, itemName(it)))
			nl()
		}
		w(cmdNormalSz)
	}

	for i := 0; i < 4; i++ {
		nl()
	}
	w(cmdCut)
	return buf.Bytes()
}

// writeOrderNumbers prints the big header number: the kitchen number when the
// order has one (staff call orders by it), with the vendor's order number
// underneath; otherwise just the vendor's "#<order id>". Expects quad size on
// entry and leaves it on.
func writeOrderNumbers(w func(string), nl func(), snap order.Snapshot) {
	if snap.KitchenNumber == nil {
		w("#" + snap.SourceEventID)
		nl()
		return
	}
	w(fmt.Sprintf("%d", *snap.KitchenNumber))
	nl()
	w(cmdBaseSz)
	w("Order #" + snap.SourceEventID)
	nl()
	w(cmdQuadSz)
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

// itemName is the printed item label: "Name (Size)" when the vendor sent a size.
func itemName(it order.LineItem) string {
	if sz := strings.TrimSpace(it.Size); sz != "" {
		return it.Name + " (" + sz + ")"
	}
	return it.Name
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

func modifierLine(name string, priceCents int64) string {
	indent := "     "
	return fmt.Sprintf("%s%-28s %7s", indent, truncRunes(name, 28), dollars(priceCents))
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
