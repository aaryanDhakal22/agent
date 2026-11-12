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
// prefs.Copies, each copy cut), with the reroute bands (writeRerouted) when
// it printed on a stand-in printer. order.DefaultReceiptPrefs() prints exactly
// what the agent printed before the settings existed.
func Build(snap order.Snapshot, sc storeconfig.StoreContext, t printer.Ticket, prefs order.ReceiptPrefs) []byte {
	if t.Kind == printer.TicketDeliveryCopies {
		return BuildDeliveryCopies(snap, sc, prefs, t.ReroutedFrom)
	}
	snap = localize(snap, sc)
	prefs = prefs.Normalize()
	if t.Kind == printer.TicketStation {
		return buildStation(snap, t, prefs)
	}
	out := buildFull(snap, sc, t.ReroutedFrom, prefs)
	if t.Future { // a future order's receipt printed early (2.4.0)
		out = futureReceiptTop(out)
	}
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
			buf.WriteString(toPC437(s))
		} else {
			buf.WriteString(toPC437(margin(s)))
		}
	}
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz)
	if reroutedFrom != "" {
		writeRerouted(w, nl, reroutedFrom)
	}

	// --- Header (2.4.0, owner): the store's name + address, then the
	// order number, then the order type (same sizes as before) ------------
	w(cmdCenter)
	if prefs.ShowStoreAddress {
		for _, t := range []string{sc.Location.Name, sc.Location.AddressLine1, sc.Location.AddressLine2} {
			if t == "" {
				continue
			}
			for _, l := range wrapped(t, paperWidth, "") {
				w(l)
				nl()
			}
		}
	}
	w(cmdBoldOn)
	w(cmdQuadSz)
	writeOrderNumbers(w, nl, snap)
	if t := orderTypeLine(snap); t != "" {
		w(cmdDoubleSz)
		w(t)
		nl()
	}
	w(cmdBoldOff)
	w(cmdBaseSz)
	if snap.Voided != "" {
		writeVoided(w, nl, snap.Voided)
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
		for i, l := range padBand(lastFirst(name), bigLineWidth) {
			if i > 0 {
				nl()
			}
			w(l)
		}
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
		for i, l := range wrapped("St: "+addr.Line1, bigLineWidth, "    ") {
			if i > 0 {
				nl()
			}
			w(l)
		}
		w(cmdBoldOff)
		w(cmdBaseSz)
		nl()
		for _, t := range []string{"Apt: " + addr.Line2, "City: " + addr.City, "Zip: " + addr.PostalCode} {
			for _, l := range wrapped(t, paperWidth, "      ") {
				w(l)
				nl()
			}
		}
		nl()
	}

	// --- Notes ---------------------------------------------------------
	if strings.TrimSpace(snap.Notes) != "" {
		w(cmdCenter)
		w(separator())
		nl()
		for _, l := range wrapped("Notes: "+snap.Notes, paperWidth, "") {
			w(l)
			nl()
		}
	}

	// --- Payment -------------------------------------------------------
	w(separator())
	w(cmdCenter)
	nl()
	w(cmdBoldOn)
	w(cmdDoubleSz)
	cashDue := snap.CashDue()
	if snap.Voided != "" {
		cashDue = 0 // nothing to collect on a voided / cancelled order
	}
	switch {
	case snap.Voided != "":
		w(snap.Voided + " - NOT CHARGED")
	case cashDue > 0:
		w("CASH - " + dollars(cashDue))
	case !hasCardPayment(snap.Payments):
		// Paid another way (a third party, an invoice): nothing to collect.
		w("PAID")
	default:
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

	sp := prefs.Spacing
	// A voided / cancelled order's lines print struck through (strike.go),
	// always in the expanded format; strike_images off: each item under a
	// reversed VOID tag.
	struck := snap.Voided != "" && prefs.StrikeImages
	if snap.Voided != "" {
		prefs.Format = order.FormatExpanded
	}
	line := func(s string) { // a struck line is a picture of its own: end() adds no line feed after it
		if struck {
			buf.WriteString(strikeRaster(margin(s), true))
			return
		}
		w(s)
	}
	end := func() {
		if !struck {
			nl()
		}
	}
	delivery := isDelivery(snap)
	if delivery { // the driver's copy: $0 choices main keeps off it
		snap.Items = driverItems(snap.Items)
	}
	snap.Items = couponOrder(snap.Items)
	if prefs.Format == order.FormatCondensed {
		writeCondensedItems(w, nl, snap.Items, sp, delivery)
	}
	for i, item := range snap.Items {
		if prefs.Format == order.FormatCondensed {
			break
		}
		if i > 0 {
			w(feedDots(sp.ItemItem)) // between items, with or without modifiers
		}
		w(separator())
		nl()
		if couponStarts(snap.Items, i) {
			writeCouponHeader(w, nl, item.Coupon)
		}
		itemSum := int64(item.Quantity) * item.UnitPrice
		size := strings.TrimSpace(item.Size)
		if snap.Voided != "" && !struck {
			w(cmdInvertOn + " " + snap.Voided + " " + cmdInvertOff)
			nl()
		}
		w(cmdBoldOn)
		head := item.Name
		if size != "" {
			head = size
		}
		for k, l := range receiptItemLines(item.Quantity, head, item.UnitPrice) {
			if k > 0 {
				w(cmdBoldOn)
			}
			line(l)
			w(cmdBoldOff)
			end()
		}
		if size != "" {
			w(cmdLeft)
			for _, l := range wrapped("    "+item.Name, lineWidth, "    ") {
				line(l)
				end()
			}
		}

		mods := item.Children
		if item.HasHalves() { // compact: whole, then "1st: …", then "2nd: …"
			mods = receiptHalfMods(item)
		}
		for j, mod := range mods {
			if j == 0 {
				w(feedDots(sp.ItemMod))
			} else {
				w(feedDots(sp.ModMod))
			}
			label := mod.Name
			if mod.Quantity > 1 {
				label = fmt.Sprintf("(x%d) %s", mod.Quantity, label)
			}
			// Sides (fries, salad) stand out, as on the v1 receipt.
			side := strings.Contains(strings.ToLower(mod.Name), "fries") || strings.Contains(strings.ToLower(mod.Name), "salad")
			if side {
				w(cmdBoldOn)
			}
			for _, l := range receiptModLines(label, mod.UnitPrice) {
				line(l)
				end()
			}
			if side {
				w(cmdBoldOff)
			}
			itemSum += mod.UnitPrice * int64(max(mod.Quantity, 1))
		}
		if strings.TrimSpace(item.Notes) != "" {
			for _, l := range wrapped("    Note: "+item.Notes, lineWidth, "          ") {
				line(l)
				end()
			}
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
		for _, l := range pairLines(label+":", dollars(fee.Amount)) {
			w(l)
			nl()
		}
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
		for _, l := range pairLines(label+":", "-"+dollars(d.Amount)) {
			w(l)
			nl()
		}
	}
	w(rightPair("Total:", dollars(snap.Money.Total)))

	nl()
	w(separator())
	nl()
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdCenter)
	nl()
	if cashDue > 0 {
		w("Collect Cash: " + dollars(cashDue))
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
	buf.WriteString(poweredByFooter())
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

// writeVoided is a voided / cancelled order's banner under its number:
// "VOID" (or "CANCELLED") reversed and double size, then a line saying
// nothing on it is charged.
func writeVoided(w func(string), nl func(), label string) {
	nl()
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdInvertOn + "  " + label + "  " + cmdInvertOff)
	w(cmdBaseSz)
	w(cmdBoldOff)
	nl()
	w("Nothing on this order is charged")
	nl()
}

// futureReceiptTop puts an inverted FUTURE bar at the very top of a
// customer receipt printed early for a future order (2.4.0), right after
// the printer reset that starts it.
func futureReceiptTop(out []byte) []byte {
	bar := cmdCenter + cmdBoldOn + cmdDoubleSz + cmdInvertOn + "   FUTURE   " + cmdInvertOff + cmdBaseSz + cmdBoldOff + cmdFeed + cmdFeed
	head := cmdInit + cmdBaseSz
	if !bytes.HasPrefix(out, []byte(head)) {
		return append([]byte(bar), out...)
	}
	return append(append([]byte(head), bar...), out[len(head):]...)
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
	rc := utf8.RuneCountInString
	var lines []string
	// Leading spaces (e.g. the "  - " of a note) stay on the first line;
	// later lines start with indent.
	lead := text[:len(text)-len(strings.TrimLeft(text, " "))]
	prefix := func() string {
		if len(lines) == 0 {
			return lead
		}
		return indent
	}
	cur := "" // the line being built, without its prefix
	for _, word := range strings.Fields(text) {
		for word != "" {
			p := prefix()
			avail := max(width-rc(p), 1)
			need := rc(word)
			if cur != "" {
				need += 1 + rc(cur)
			}
			switch {
			case need <= avail:
				if cur != "" {
					cur += " "
				}
				cur += word
				word = ""
			case cur != "":
				lines = append(lines, p+cur)
				cur = ""
			default: // a word longer than a whole line: split it
				r := []rune(word)
				lines = append(lines, p+string(r[:avail]))
				word = string(r[avail:])
			}
		}
	}
	if cur != "" || len(lines) == 0 {
		lines = append(lines, prefix()+cur)
	}
	return lines
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

// rerouteBandWidth is the reroute bands' width in double-size characters:
// 21 fits the narrowest printer a ticket can land on (the store's 72 mm
// Online printer: 512 dots = 21 characters at double width), so a band never
// wraps; the text wraps at rerouteTextWidth so the black band always has a
// space either side of it.
const (
	rerouteBandWidth = 21
	rerouteTextWidth = rerouteBandWidth - 2
)

// RerouteLines is the reroute banner's text, one band per line: the English
// line, then its Spanish counterpart (the owner, 2026-11-12: like ADD /
// AGREGAR and REMOVE / QUITAR), each wrapped at rerouteTextWidth:
// "REROUTED FROM PIZZA", "REDIRIGIDO DE PIZZA".
func RerouteLines(from string) []string {
	name := strings.ToUpper(strings.Join(strings.Fields(from), " "))
	var out []string
	for _, l := range wordWrap("REROUTED FROM "+name, rerouteTextWidth, "") {
		out = append(out, printable(l))
	}
	for _, l := range wordWrap("REDIRIGIDO DE "+name, rerouteTextWidth, "") {
		out = append(out, printable(l))
	}
	return out
}

// writeRerouted prints the reroute banner on any ticket or slip that printed
// on a stand-in printer (spec §4.42): each RerouteLines line as a band like
// the change ticket's ADD / REMOVE ones — centred, bold, double size,
// reverse (white on black), padded to the band width — then a blank line.
// It leaves the text left-aligned at the base size, bold off.
func writeRerouted(w func(string), nl func(), from string) {
	for _, l := range RerouteLines(from) {
		w(cmdCenter)
		w(cmdBoldOn)
		w(cmdDoubleSz)
		w(cmdInvertOn)
		w(pad(l, rerouteBandWidth))
		w(cmdInvertOff)
		w(cmdSizeReset)
		w(cmdBaseSz)
		w(cmdBoldOff)
		nl()
	}
	w(cmdLeft)
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

// feedDots is ESC J n — feed n dots of paper (the line before it has already
// ended, so it only adds space); "" for 0, so the default spacing prints the
// same bytes as before the setting.
func feedDots(n int) string {
	if n <= 0 {
		return ""
	}
	return string([]byte{0x1b, 0x4a, byte(min(n, 255))})
}

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

// writeCondensedItems is the condensed receipt's item list (2.4.0): per
// item one line "<qty> <size> <name>" priced with all its modifiers (so the
// lines add up to the subtotal), then only its first modifier (vendors and
// the POS list the main choice first - the soda flavour, the pizza's
// topping) without a price, then its note.
//
// On a delivery (driver) the hidden children are already gone
// (driverItems), so the first modifier is the first one the driver may see,
// and every child stamped show_on_driver_ticket follows it: the driver must
// see the drinks.
func writeCondensedItems(w func(string), nl func(), items []order.LineItem, sp order.LineSpacing, driver bool) {
	for i, item := range items {
		if i > 0 {
			w(feedDots(sp.ItemItem))
		}
		if couponStarts(items, i) {
			writeCouponHeader(w, nl, item.Coupon)
		}
		sum := int64(item.Quantity) * item.UnitPrice
		for _, mod := range item.Children {
			sum += mod.UnitPrice * int64(max(mod.Quantity, 1))
		}
		name := strings.TrimSpace(strings.TrimSpace(item.Size) + " " + strings.TrimSpace(item.Name))
		for _, l := range receiptItemLines(item.Quantity, name, sum) {
			w(cmdBoldOn)
			w(l)
			w(cmdBoldOff)
			nl()
		}
		if item.HasHalves() { // half and half: the kitchen summary instead
			w(feedDots(sp.ItemMod))
			w(cmdLeft)
			for _, line := range wordWrap("    - "+glue(halfText(item)), lineWidth, "      ") {
				w(printable(line))
				nl()
			}
		} else if len(item.Children) > 0 {
			w(feedDots(sp.ItemMod))
			for k, mod := range item.Children {
				if k > 0 && !(driver && mod.ShownOnDriverTicket()) {
					continue
				}
				label := mod.Name
				if mod.Quantity > 1 {
					label = fmt.Sprintf("(x%d) %s", mod.Quantity, label)
				}
				w(cmdLeft)
				for _, l := range wrapped("    - "+label, lineWidth, "      ") {
					w(l)
					nl()
				}
			}
		}
		if strings.TrimSpace(item.Notes) != "" {
			for _, l := range wrapped("    Note: "+item.Notes, lineWidth, "          ") {
				w(l)
				nl()
			}
		}
	}
}

// orderTypeLine is the line over the number on the receipt and the kitchen
// ticket (2.4.0): PICKUP, DELIVERY, COUNTER (a POS dine-in check), DINE IN
// (online), nothing when the type is unknown.
func orderTypeLine(snap order.Snapshot) string {
	switch strings.ToLower(strings.TrimSpace(snap.Fulfillment.Type)) {
	case "delivery":
		return "DELIVERY"
	case "pickup":
		return "PICKUP"
	case "dine_in":
		if snap.Source == order.SourceCounter {
			return "COUNTER"
		}
		return "DINE IN"
	}
	return ""
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

// isDelivery: the receipt is the delivery ticket (the driver's copy).
func isDelivery(snap order.Snapshot) bool {
	return strings.EqualFold(strings.TrimSpace(snap.Fulfillment.Type), "delivery")
}

// driverItems are a delivery's lines without the modifier children the
// driver doesn't need: a $0 choice stamped show_on_driver_ticket false
// (order.LineItem.HiddenOnDriverTicket). Unstamped children (vendor and
// older orders) stay, so those print exactly as before. A copy.
func driverItems(items []order.LineItem) []order.LineItem {
	out := make([]order.LineItem, len(items))
	for i, it := range items {
		hidden := false
		for _, c := range it.Children {
			hidden = hidden || c.HiddenOnDriverTicket()
		}
		if hidden {
			kids := make([]order.LineItem, 0, len(it.Children))
			for _, c := range it.Children {
				if !c.HiddenOnDriverTicket() {
					kids = append(kids, c)
				}
			}
			it.Children = kids
		}
		out[i] = it
	}
	return out
}

// Coupons (the owner, 2026-11-12): a coupon re-prices its lines (never a
// discount line); the receipt prints the coupon's name over its lines, which
// sit together.

// couponOrder moves each coupon's lines up next to its first one (the rest
// keep their order).
func couponOrder(items []order.LineItem) []order.LineItem {
	has := false
	for _, it := range items {
		has = has || it.Coupon != ""
	}
	if !has {
		return items
	}
	out := make([]order.LineItem, 0, len(items))
	done := map[int]bool{}
	for i, it := range items {
		if it.Coupon == "" {
			out = append(out, it)
			continue
		}
		if done[i] {
			continue
		}
		for j := i; j < len(items); j++ {
			if !done[j] && items[j].Coupon != "" && items[j].CouponNo == it.CouponNo && items[j].Coupon == it.Coupon {
				out = append(out, items[j])
				done[j] = true
			}
		}
	}
	return out
}

// couponStarts: line i is the first of its coupon's lines.
func couponStarts(items []order.LineItem, i int) bool {
	it := items[i]
	if it.Coupon == "" {
		return false
	}
	return i == 0 || items[i-1].Coupon != it.Coupon || items[i-1].CouponNo != it.CouponNo
}

// writeCouponHeader is the band over a coupon's lines: "** 8" Sub Combo **".
func writeCouponHeader(w func(string), nl func(), name string) {
	w(cmdBoldOn)
	w(cmdLeft)
	for _, l := range wrapped("** "+strings.TrimSpace(name)+" **", lineWidth, "   ") {
		w(printable(l))
		nl()
	}
	w(cmdBoldOff)
}
