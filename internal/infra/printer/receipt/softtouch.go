package receipt

import (
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// The customer's paper, laid out like SoftTouch's (the owner, 2026-11-12, the
// photo in .dev/notes/softtouch-receipt-2026-11-09.jpg; contract §2.1j): the
// delivery's Merchant + Customer Copy and the customer receipt of any order
// (pickup / takeout / delivery, a Reprint too) are one slip:
//
//	header (the store's logo left + its details right as one raster, else the
//	details centred) · Date / Time · Order # (large) · type · LAST , FIRST ·
//	the labelled block (printed even when empty) · one payment
//	block per card · items · Coupons · rule · totals (Total large) · a line per
//	payment · the signature block (a card order with no tip taken yet) · the
//	copy label · Order Online · the footer (large) · powered by.
//
// Our additions: Square's id as the Transaction ID (wrapped under itself),
// Cashier = who took it, the entry line, the bands on top (REPRINT, UPDATED,
// REROUTED: the callers), powered by at the bottom.

// cardAgreement is the signature block's agreement, as SoftTouch prints it.
var cardAgreement = []string{"I agree to pay the above total amount", "according to card issuer agreement",
	"(merchant agreement if credit voucher)"}

// softSlip writes one copy (copy = MerchantCopy / CustomerCopy) of the slip.
// header = the store's raster header for this width (nil: the details as
// centred text).
func softSlip(p *escw, snap order.Snapshot, st deliveryStore, at time.Time, copy string, header []byte) {
	w := p.width
	voided := snap.Voided != ""
	delivery := strings.EqualFold(snap.Fulfillment.Type, "delivery")
	if voided {
		p.center()
		p.bold(true)
		p.raw(gsBig + cmdInvertOn + "  " + strings.ToUpper(snap.Voided) + "  " + cmdInvertOff) // VOID / CANCELLED
		p.raw(gsNormal + cmdFeed)
		p.bold(false)
		p.wrapped("Nothing on this order is charged", "")
		p.feed(1)
		p.left()
	}
	// The header: the logo raster, or the store's details centred.
	if len(header) > 0 {
		p.center()
		p.raw(string(header))
		p.left()
	} else {
		p.center()
		if st.name != "" {
			p.raw(gsTall)
			p.wrapped(st.name, "")
			p.raw(gsNormal)
		}
		for _, l := range st.addressLines {
			if strings.TrimSpace(l) != "" {
				p.wrapped(l, "")
			}
		}
		if st.phone != "" {
			p.line(phoneParens(st.phone))
		}
		p.left()
	}
	p.feed(1)
	// Date / Time
	d := "Date: " + shortDate(at)
	pad := max(w/2, len(d)+2)
	p.line(d + strings.Repeat(" ", pad-len(d)) + "Time: " + shortTimePadded(at))
	// Order # (large)
	num := snap.SourceEventID
	if snap.KitchenNumber != nil {
		num = fmt.Sprintf("%d", *snap.KitchenNumber)
	}
	t := "Order #: " + num
	p.feed(1) // an empty line above and below (the owner, 2026-11-12, 2.5.2)
	p.bold(true)
	if len(dclean(t))*2 <= w {
		p.raw(gsBig)
	} else {
		p.raw(gsTall)
	}
	p.line(t)
	p.raw(gsNormal)
	p.bold(false)
	p.feed(1)
	if typ := slipType(snap); typ != "" {
		// the word + its icon, a bag or a car, as one picture (icons.go, the owner 2026-11-14 #16)
		if typeIcon(typ) != nil {
			p.raw(typeLine(typ, p.strikeImages))
		} else {
			p.line(typ)
		}
	}
	if by := strings.TrimSpace(snap.TakenBy); by != "" && !hasCardCharge(snap) {
		// a call-center order (contract §2.10ab); with a card the payment block's Cashier line says it
		p.wrapped(by, "")
	}
	if snap.OutOfArea { // outside the store's area, taken after the warning (contract §2.10ab)
		p.bold(true)
		p.line("OUT OF AREA")
		p.bold(false)
	}
	if due := snap.Fulfillment.PromisedAt; due != nil && !due.IsZero() {
		p.bold(true)
		p.wrapped("FUTURE: for "+shortDate(*due)+" "+shortTime(*due), "")
		p.bold(false)
	}
	if n := strings.TrimSpace(snap.Customer.DisplayName); n != "" {
		p.wrapped(softName(n), "")
	}
	// The labelled block, printed even when empty (SoftTouch does).
	phone := strings.TrimSpace(snap.Customer.Phone)
	if phone != "" {
		phone = phoneParens(phone)
	}
	always(p, "Phone", phone)
	if delivery {
		street, city := "", ""
		if a := snap.Fulfillment.Address; a != nil {
			street = strings.TrimSpace(a.Line1)
			if l2 := strings.TrimSpace(a.Line2); l2 != "" {
				street += ", " + apt(l2)
			}
			city = strings.ToUpper(cityLine(a))
		}
		always(p, "Street", street)
		always(p, "City", city) // no Directions line: drivers go by the street (the owner, 2.5.2)
	}
	if n := strings.TrimSpace(snap.Notes); n != "" {
		p.wrapped("Notes: "+n, strings.Repeat(" ", 7))
	}
	p.feed(1)
	// One payment block per card payment.
	cashier := cashierOf(snap)
	for _, x := range snap.Payments {
		if strings.ToLower(x.Method) != "card" || x.AmountCents <= 0 {
			continue
		}
		if id := strings.TrimSpace(x.ID); id != "" {
			p.line("Payment #: " + shortPaymentID(id))
		}
		when := at
		if x.At != nil && !x.At.IsZero() {
			when = x.At.In(at.Location())
		}
		p.wrapped("Payment Date Time: "+shortDate(when)+" "+shortTimePadded(when), "  ")
		ref := strings.TrimSpace(x.Ref)
		if ref == "" && snap.Source == order.SourceCounter {
			ref = strings.TrimSpace(snap.SourceEventID)
		}
		if ref != "" { // its last 10 characters, one line (the owner, 2026-11-12)
			p.line("Transaction ID: " + lastRunes(dclean(ref), min(10, max(p.width-16, 1))))
		}
		if cashier != "" {
			p.wrapped("Cashier: "+cashier, strings.Repeat(" ", 9))
		}
		card := x.BrandName()
		if x.Last4 != "" {
			card = strings.Repeat("X", 12) + x.Last4 + " " + x.BrandName()
		}
		p.line(card)
		if e := x.EntryLabel(); e != "" {
			p.line(e)
		}
		if a := strings.TrimSpace(x.AuthCode); a != "" {
			p.line("Auth Code: " + a)
		}
	}
	softItems(p, snap, voided, delivery)
	// Coupons
	var coupons []string
	for i, it := range snap.Items {
		if couponStarts(snap.Items, i) {
			coupons = append(coupons, it.Coupon)
		}
	}
	if len(coupons) > 0 {
		p.feed(1)
		p.bold(true)
		p.line("Coupons")
		p.bold(false)
		for _, c := range coupons {
			p.wrapped(strings.ToUpper(c)+" (Coupon)", "")
		}
	}
	if len(snap.Money.Discounts) > 0 {
		p.bold(true)
		p.line("Adjustments")
		p.bold(false)
		for _, dc := range snap.Money.Discounts {
			amt := paren(dc.Amount)
			label := dc.Label
			if label == "" {
				label = dc.Type
			}
			for i, l := range dwrap(dclean(label), w-len(amt)-1, "") {
				if i == 0 {
					p.line(dleftRight(l, amt, w))
				} else {
					p.line(l)
				}
			}
		}
	}
	p.line(strings.Repeat("-", w))
	p.feed(1)
	deliveryTotals(p, snap)
	p.feed(1)
	// A line per payment (or the cash to collect).
	paid := int64(0)
	for _, x := range snap.Payments {
		paid += x.AmountCents
	}
	due := min(max(snap.Money.Total-paid, 0), max(snap.Money.Total, 0))
	if len(snap.Payments) == 0 {
		due = snap.CashDue()
	}
	if voided {
		p.bold(true)
		p.wrapped(strings.ToUpper(snap.Voided)+" - NOT CHARGED", "")
		p.bold(false)
	}
	if !voided && due > 0 {
		p.bold(true)
		p.wrapped("Collect Cash "+moneyC(due), "")
		p.bold(false)
	}
	for _, x := range snap.Payments {
		name := tenderName(x)
		if strings.ToLower(x.Method) == "card" {
			name = x.BrandName()
		}
		amt := snap.TenderCents(x) // a card of the store's own order prints with its tip
		switch {
		case x.AmountCents < 0:
			p.wrapped("Refund "+name+" -"+moneyC(-amt), "  ")
		case x.AmountCents == 0 && x.TipCents != 0: // a tip added to a card afterwards
			p.wrapped(name+" tip "+moneyC(x.TipCents), "  ")
		case x.AmountCents > 0:
			p.wrapped(name+" "+moneyC(amt), "  ")
		}
	}
	// The signature block: a card order with no tip taken yet (the owner).
	if !voided && signatureWanted(snap) {
		blank := strings.Repeat("_", min(max(w-20, 10), 24))
		p.feed(1)
		p.line("Tip     : " + blank)
		p.feed(1)
		p.line("Total   : " + blank)
		p.feed(3)
		p.line("X " + strings.Repeat("_", max(w-4, 10)))
		p.feed(1)
		p.center()
		for _, l := range cardAgreement {
			p.wrapped(l, "")
		}
		p.left()
	}
	p.feed(1)
	p.center()
	p.line(copy)
	p.left()
	p.feed(1)
	// storeFooter: Order Online + the website, then the footer large.
	p.center()
	if st.website != "" {
		p.line("Order Online")
		p.wrapped(st.website, "")
		p.feed(1)
	}
	for _, l := range strings.Split(st.footer, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		p.raw(gsTall)
		p.wrapped(l, "")
		p.raw(gsNormal)
	}
	p.left()
	p.feed(2) // powered by and what follows sit 2 lines lower (the owner, 2026-11-12)
	p.raw(slipPoweredBy(p.lay))
	p.raw(cmdNormalSz)
	p.feed(3)
	p.raw(cmdCut)
}

// slipType is the order's type line: DELIVERY / PICKUP / COUNTER / DINE IN.
func slipType(snap order.Snapshot) string {
	switch strings.ToLower(snap.Fulfillment.Type) {
	case "delivery":
		return "DELIVERY"
	case "pickup":
		return "PICKUP"
	case "counter":
		return "COUNTER"
	case "dine_in":
		if snap.Source == order.SourceCounter {
			return "COUNTER"
		}
		return "DINE IN"
	}
	return strings.ToUpper(snap.Fulfillment.Type)
}

// softName is the customer's name as SoftTouch prints it: "LAST , FIRST".
func softName(name string) string {
	f := strings.Fields(name)
	if len(f) < 2 {
		return strings.ToUpper(name)
	}
	return strings.ToUpper(f[len(f)-1] + " , " + strings.Join(f[:len(f)-1], " "))
}

// always prints "Label      : value", the label even with no value (SoftTouch's
// block): "Company    :".
func always(p *escw, label, value string) {
	value = strings.TrimSpace(value)
	head := fmt.Sprintf("%-10s : ", label)
	if value == "" {
		p.line(strings.TrimRight(head, " "))
		return
	}
	for i, l := range noOrphan(dwrap(dclean(value), p.width-len(head), ""), p.width-len(head)) {
		if i == 0 {
			p.line(head + l)
		} else {
			p.line(strings.Repeat(" ", len(head)) + l)
		}
	}
}

// cmdFontBTall: font B, double height.
const cmdFontBTall = "\x1b\x21\x11"

// lastRunes is the last n characters of s.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// slipPoweredBy is poweredByFooter with "powered by" one size up: font B at
// double height (the owner, 2026-11-12).
func slipPoweredBy(l layout) string {
	b := poweredByFooter(l)
	return strings.Replace(b, cmdFontBSz+"powered by", cmdFontBTall+"Powered By", 1)
}

// noOrphan keeps a lone character off a row of its own ("APT" / "4"): the
// previous row's last word goes down with it when it fits.
func noOrphan(lines []string, width int) []string {
	n := len(lines)
	if n < 2 || len([]rune(strings.TrimSpace(lines[n-1]))) != 1 {
		return lines
	}
	prev := strings.TrimRight(lines[n-2], " ")
	i := strings.LastIndex(prev, " ")
	if i <= 0 {
		return lines
	}
	moved := prev[i+1:] + " " + strings.TrimSpace(lines[n-1])
	if len([]rune(moved)) > width {
		return lines
	}
	out := append([]string(nil), lines[:n-2]...)
	return append(out, strings.TrimRight(prev[:i], " "), moved)
}

// cashierOf is who took the order: the call center's "Call center - Name", a
// terminal's cashier, else nobody.
func cashierOf(snap order.Snapshot) string {
	if by := strings.TrimSpace(snap.TakenBy); by != "" {
		return by
	}
	if t, ok := snap.SourceMeta["terminal"].(map[string]any); ok {
		if n, ok := t["staff_name"].(string); ok {
			return strings.TrimSpace(n)
		}
	}
	return ""
}

// shortPaymentID is the payment's short number: the last 8 of its id, upper case.
func shortPaymentID(id string) string {
	id = strings.ReplaceAll(id, "-", "")
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return strings.ToUpper(id)
}

// shortTimePadded is "04:48 pm" (SoftTouch pads the hour).
func shortTimePadded(t time.Time) string {
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	ap := "pm"
	if t.Hour() < 12 {
		ap = "am"
	}
	return fmt.Sprintf("%02d:%02d %s", h, t.Minute(), ap)
}

// signatureWanted: a card was charged and no tip was taken on the order (not
// with a card, not in its money): the customer signs and may write one.
func signatureWanted(snap order.Snapshot) bool {
	card := false
	for _, x := range snap.Payments {
		if strings.ToLower(x.Method) == "card" && x.AmountCents > 0 {
			card = true
		}
		if x.TipCents != 0 {
			return false
		}
	}
	return card && snap.Money.Tip == 0
}

// softItems is the items block: SoftTouch's lines (quantity, the name in
// capitals with its size, the price at the right; modifiers and notes
// indented under it). A delivery drops the $0 choices stamped not for the
// driver (§2.1g); a pickup prints them all.
func softItems(p *escw, snap order.Snapshot, voided, delivery bool) {
	_ = delivery // one rule for pickup and delivery (slipShowsChoice, 2.5.2)
	deliveryItems(p, snap, voided)
}

// hasCardCharge: a card payment with an amount (it gets a payment block).
func hasCardCharge(snap order.Snapshot) bool {
	for _, x := range snap.Payments {
		if strings.ToLower(x.Method) == "card" && x.AmountCents > 0 {
			return true
		}
	}
	return false
}
