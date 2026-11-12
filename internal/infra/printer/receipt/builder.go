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
//
// Every ticket is laid out for t.Cols, the width of the printer it prints on
// (width.go).
func Build(snap order.Snapshot, sc storeconfig.StoreContext, t printer.Ticket, prefs order.ReceiptPrefs) []byte {
	if t.Kind == printer.TicketDeliveryCopies {
		return BuildDeliveryCopies(snap, sc, prefs, printer.Paper{Cols: t.Cols, ReroutedFrom: t.ReroutedFrom})
	}
	snap = localize(snap, sc)
	prefs = prefs.Normalize()
	if t.Kind == printer.TicketStation {
		return buildStation(snap, t, prefs)
	}
	out := buildFull(snap, sc, t.ReroutedFrom, prefs, layoutFor(t.Cols))
	if t.Future { // a future order's receipt printed early (2.4.0)
		out = futureReceiptTop(out, layoutFor(t.Cols))
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
	return buildFull(localize(snap, sc), sc, "", order.DefaultReceiptPrefs(), defaultLayout)
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

func buildFull(snap order.Snapshot, sc storeconfig.StoreContext, reroutedFrom string, prefs order.ReceiptPrefs, l layout) []byte {
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
		writeRerouted(w, nl, reroutedFrom, l)
	}

	// --- Header (2.4.0, owner): the store's name + address, then the
	// order number, then the order type (same sizes as before) ------------
	w(cmdCenter)
	if prefs.ShowStoreAddress {
		for _, t := range []string{sc.Location.Name, sc.Location.AddressLine1, sc.Location.AddressLine2} {
			if t == "" {
				continue
			}
			for _, line := range wrapped(t, l.cols, "") {
				w(line)
				nl()
			}
		}
	}
	w(cmdBoldOn)
	w(cmdQuadSz)
	writeOrderNumbers(w, nl, snap, l)
	if t := orderTypeLine(snap); t != "" {
		w(cmdDoubleSz)
		w(t)
		nl()
	}
	w(cmdBoldOff)
	w(cmdBaseSz)
	if snap.Voided != "" {
		writeVoided(w, nl, snap.Voided, l)
	}

	// --- Promised-for-later flag ---------------------------------------
	if snap.Fulfillment.PromisedAt != nil && !snap.Fulfillment.PromisedAt.IsZero() {
		w(cmdInvertOn)
		w(cmdDoubleSz)
		w(cmdCenter)
		w(cmdBoldOn)
		w(futureBand(l))
		w(cmdInvertOff)
		nl()
		nl()
		w(cmdBaseSz)
		w(dateLine(*snap.Fulfillment.PromisedAt, l.cols-2, true))
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
		for i, line := range padBand(lastFirst(name), l.big()) {
			if i > 0 {
				nl()
			}
			w(line)
		}
		w(cmdBaseSz)
		w(cmdBoldOff)
		nl()
	}
	if phone := strings.TrimSpace(snap.Customer.Phone); phone != "" && prefs.ShowPhone {
		nl()
		w(cmdBoldOn)
		w(cmdDoubleSz)
		w(wrapJoin("Phone: "+formatPhone(phone), l.big(), ""))
		w(cmdBoldOff)
		w(cmdBaseSz)
		nl()
	}

	// --- Placed at -----------------------------------------------------
	w(l.rule("-"))
	nl()
	w(cmdBoldOn)
	w(dateLine(snap.PlacedAt, l.cols, false))
	w(cmdBoldOff)
	nl()

	// --- Delivery address ----------------------------------------------
	if addr := snap.Fulfillment.Address; addr != nil && strings.ToLower(snap.Fulfillment.Type) == "delivery" {
		w(l.rule("-"))
		nl()
		w(cmdLeft)
		w(cmdDoubleSz)
		w(cmdBoldOn)
		for i, line := range wrapped("St: "+addr.Line1, l.big(), "    ") {
			if i > 0 {
				nl()
			}
			w(line)
		}
		w(cmdBoldOff)
		w(cmdBaseSz)
		nl()
		for _, t := range []string{"Apt: " + addr.Line2, "City: " + addr.City, "Zip: " + addr.PostalCode} {
			for _, line := range wrapped(t, l.cols, "      ") {
				w(line)
				nl()
			}
		}
		nl()
	}

	// --- Notes ---------------------------------------------------------
	if strings.TrimSpace(snap.Notes) != "" {
		w(cmdCenter)
		w(l.rule("-"))
		nl()
		for _, line := range wrapped("Notes: "+snap.Notes, l.cols, "") {
			w(line)
			nl()
		}
	}

	// --- Payment -------------------------------------------------------
	w(l.rule("-"))
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
		w(wrapJoin(snap.Voided+" - NOT CHARGED", l.big(), ""))
	case cashDue > 0:
		w(wrapJoin("CASH - "+dollars(cashDue), l.big(), ""))
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
	w(l.rule("-"))
	nl()
	w(cmdBoldOn)
	w(l.columnHeader())
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
		writeCondensedItems(w, nl, snap.Items, sp, delivery, l)
	}
	for i, item := range snap.Items {
		if prefs.Format == order.FormatCondensed {
			break
		}
		if i > 0 {
			w(feedDots(sp.ItemItem)) // between items, with or without modifiers
		}
		w(l.rule("-"))
		nl()
		if couponStarts(snap.Items, i) {
			writeCouponHeader(w, nl, item.Coupon, l)
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
		for k, s := range l.receiptItemLines(item.Quantity, head, item.UnitPrice) {
			if k > 0 {
				w(cmdBoldOn)
			}
			line(s)
			w(cmdBoldOff)
			end()
		}
		if size != "" {
			w(cmdLeft)
			for _, s := range wrapped("    "+item.Name, l.cols, "    ") {
				line(s)
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
			for _, s := range l.receiptModLines(label, mod.UnitPrice) {
				line(s)
				end()
			}
			if side {
				w(cmdBoldOff)
			}
			itemSum += mod.UnitPrice * int64(max(mod.Quantity, 1))
		}
		if strings.TrimSpace(item.Notes) != "" {
			for _, s := range wrapped("    Note: "+item.Notes, l.cols, "          ") {
				line(s)
				end()
			}
		}
		w(rightAlign("Sum: "+dollars(itemSum), l.cols))
		nl()
	}

	w(l.rule("-"))
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
		for _, s := range l.pairLines(label+":", dollars(fee.Amount)) {
			w(s)
			nl()
		}
	}

	// --- Totals --------------------------------------------------------
	w(l.rightPair("Subtotal:", dollars(snap.Money.Subtotal)))
	nl()
	if snap.Money.Tax > 0 {
		w(l.rightPair("Tax:", dollars(snap.Money.Tax)))
		nl()
	}
	if snap.Money.Tip > 0 {
		w(l.rightPair("Tips :", dollars(snap.Money.Tip)))
		nl()
	}
	for _, d := range snap.Money.Discounts {
		label := d.Label
		if label == "" {
			label = d.Type
		}
		for _, s := range l.pairLines(label+":", "-"+dollars(d.Amount)) {
			w(s)
			nl()
		}
	}
	w(l.rightPair("Total:", dollars(snap.Money.Total)))

	nl()
	w(l.rule("-"))
	nl()
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdCenter)
	nl()
	if cashDue > 0 {
		w(wrapJoin("Collect Cash: "+dollars(cashDue), l.big(), ""))
	} else {
		w(wrapJoin("Order Total:"+dollars(snap.Money.Total), l.big(), ""))
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
		w(wrapJoin(footer, l.sizeCols(prefs.FooterSize), ""))
		w(cmdBoldOff)
		w(cmdBaseSz)
	}
	buf.WriteString(poweredByFooter(l))
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
func writeVoided(w func(string), nl func(), label string, l layout) {
	nl()
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(cmdInvertOn + "  " + label + "  " + cmdInvertOff)
	w(cmdBaseSz)
	w(cmdBoldOff)
	nl()
	w(wrapJoin("Nothing on this order is charged", l.cols, ""))
	nl()
}

// futureReceiptTop puts an inverted FUTURE bar at the very top of a
// customer receipt printed early for a future order (2.4.0), right after
// the printer reset that starts it.
func futureReceiptTop(out []byte, l layout) []byte {
	bar := cmdCenter + cmdBoldOn + cmdDoubleSz + cmdInvertOn + pad("FUTURE", min(12, l.big())) + cmdInvertOff + cmdBaseSz + cmdBoldOff + cmdFeed + cmdFeed
	head := cmdInit + cmdBaseSz
	if !bytes.HasPrefix(out, []byte(head)) {
		return append([]byte(bar), out...)
	}
	return append(append([]byte(head), bar...), out[len(head):]...)
}

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
func (l layout) itemWidth(size string) int { return l.sizeCols(size) }

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
func writeOrderNumbers(w func(string), nl func(), snap order.Snapshot, l layout) {
	if snap.KitchenNumber == nil {
		w(wrapJoin("#"+snap.SourceEventID, l.xl(), ""))
	} else {
		w(fmt.Sprintf("#%d", *snap.KitchenNumber))
	}
	nl()
}

// The reroute bands are as wide as the printer the ticket landed on prints a
// double-size line (24 / 21 / 16 characters: layout.big), so a band never
// wraps on it; the text wraps two characters narrower so the black band
// always has a space either side of it.

// RerouteLines is the reroute banner's text for a printer of cols columns,
// one band per line: the English line, then its Spanish counterpart (the
// owner, 2026-11-12: like ADD / AGREGAR and REMOVE / QUITAR), each wrapped
// to the band: "REROUTED FROM PIZZA", "REDIRIGIDO DE PIZZA".
func RerouteLines(from string, cols int) []string {
	text := layoutFor(cols).big() - 2
	name := strings.ToUpper(strings.Join(strings.Fields(from), " "))
	var out []string
	for _, l := range wordWrap("REROUTED FROM "+name, text, "") {
		out = append(out, printable(l))
	}
	for _, l := range wordWrap("REDIRIGIDO DE "+name, text, "") {
		out = append(out, printable(l))
	}
	return out
}

// writeRerouted prints the reroute banner on any ticket or slip that printed
// on a stand-in printer (spec §4.42): each RerouteLines line as a band like
// the change ticket's ADD / REMOVE ones — centred, bold, double size,
// reverse (white on black), padded to the band width — then a blank line.
// It leaves the text left-aligned at the base size, bold off.
func writeRerouted(w func(string), nl func(), from string, lay layout) {
	for _, l := range RerouteLines(from, lay.cols) {
		w(cmdCenter)
		w(cmdBoldOn)
		w(cmdDoubleSz)
		w(cmdInvertOn)
		w(pad(l, lay.big()))
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

// leftRightLines is leftRight when both fit on the line, else the left on
// its own line(s) and the right under it, right-aligned (a narrow printer).
func leftRightLines(left, right string, width int) []string {
	if utf8.RuneCountInString(left)+1+utf8.RuneCountInString(right) <= width {
		return []string{leftRight(left, right, width)}
	}
	return append(wrapped(left, width, "  "), rightAlign(right, width))
}

// bigName is a name printed large (a driver's, a printer's): triple size
// when every word fits the printer's triple-size line, else double (a word
// never splits); the size command and its line width.
func bigName(text string, l layout) (string, int) {
	for _, word := range strings.Fields(text) {
		if utf8.RuneCountInString(word) > l.xl() {
			return cmdDoubleSz, l.big()
		}
	}
	return cmdQuadSz, l.xl()
}

func rightAlign(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", width-len(s)) + s
}

// rightPair is a totals row: the label right-aligned so the amount ends the line.
func (l layout) rightPair(label, value string) string {
	return leftRight(strings.Repeat(" ", l.pairIndent())+label, value, l.cols)
}

func (l layout) columnHeader() string {
	return fmt.Sprintf("%-3s %-*s %7s", "Qty", l.nameW(), "Item", "Price")
}

func (l layout) itemLine(qty int, name string, unitCents int64) string {
	return fmt.Sprintf("%-3d %-*s %7s", qty, l.nameW(), truncRunes(name, l.nameW()), dollars(unitCents))
}

// modifierLine is a modifier under its item; free modifiers carry no price.
func (l layout) modifierLine(name string, priceCents int64) string {
	indent := "     "
	if priceCents == 0 {
		return indent + truncRunes(name, l.freeModW())
	}
	return fmt.Sprintf("%s%-*s %7s", indent, l.modW(), truncRunes(name, l.modW()), dollars(priceCents))
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
func writeCondensedItems(w func(string), nl func(), items []order.LineItem, sp order.LineSpacing, driver bool, lay layout) {
	for i, item := range items {
		if i > 0 {
			w(feedDots(sp.ItemItem))
		}
		if couponStarts(items, i) {
			writeCouponHeader(w, nl, item.Coupon, lay)
		}
		sum := int64(item.Quantity) * item.UnitPrice
		for _, mod := range item.Children {
			sum += mod.UnitPrice * int64(max(mod.Quantity, 1))
		}
		name := strings.TrimSpace(strings.TrimSpace(item.Size) + " " + strings.TrimSpace(item.Name))
		for _, l := range lay.receiptItemLines(item.Quantity, name, sum) {
			w(cmdBoldOn)
			w(l)
			w(cmdBoldOff)
			nl()
		}
		if item.HasHalves() { // half and half: the kitchen summary instead
			w(feedDots(sp.ItemMod))
			w(cmdLeft)
			for _, line := range wordWrap("    - "+glue(halfText(item)), lay.cols, "      ") {
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
				for _, l := range wrapped("    - "+label, lay.cols, "      ") {
					w(l)
					nl()
				}
			}
		}
		if strings.TrimSpace(item.Notes) != "" {
			for _, l := range wrapped("    Note: "+item.Notes, lay.cols, "          ") {
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
func writeCouponHeader(w func(string), nl func(), name string, lay layout) {
	w(cmdBoldOn)
	w(cmdLeft)
	for _, l := range wrapped("** "+strings.TrimSpace(name)+" **", lay.cols, "   ") {
		w(printable(l))
		nl()
	}
	w(cmdBoldOff)
}
