package receipt

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/storeconfig"
)

// The delivery tickets of an online delivery (agent contract §2.10aa, the
// owner 2026-11-12: "the online order will use the same receipt printing ...
// as the POS one. No difference"): the Merchant Copy (stapled on the bag) and
// the Customer Copy, in one job, each cut. A port of the terminal's
// printouts.dart deliveryTicket (and the helpers it uses), line for line, so
// an online delivery's tickets read like a phone delivery's: keep the two in
// step when either changes.

// The copy labels.
const (
	MerchantCopy = "Merchant Copy"
	CustomerCopy = "Customer Copy"
)

// deliveryNow is the print time (tests pin it).
var deliveryNow = time.Now

// GS ! n sizes as the terminal's Scale (normal, tall = 1x2, big = 2x2).
const (
	gsNormal = "\x1d\x21\x00"
	gsTall   = "\x1d\x21\x01"
	gsBig    = "\x1d\x21\x11"
)

// escw is the terminal's EscPos builder, the parts the tickets use.
type escw struct {
	buf   bytes.Buffer
	width int
	lay   layout
}

func (p *escw) raw(s string) { p.buf.WriteString(s) }
func (p *escw) center()      { p.raw(cmdCenter) }
func (p *escw) left()        { p.raw(cmdLeft) }
func (p *escw) bold(on bool) {
	if on {
		p.raw(cmdBoldOn)
	} else {
		p.raw(cmdBoldOff)
	}
}
func (p *escw) line(s string) { p.raw(dclean(s) + cmdFeed) }
func (p *escw) feed(n int) {
	for i := 0; i < n; i++ {
		p.raw(cmdFeed)
	}
}
func (p *escw) wrapped(s, indent string) {
	for _, l := range dwrap(dclean(s), p.width, indent) {
		p.line(l)
	}
}

// dclean is what reaches the printer, one byte a character (so dwrap's
// byte counts are columns): the Pi's PC437 replacements (an accented letter
// is its PC437 byte, like the terminal's printable(); an emoji '?'), no
// control bytes. A PC437 byte passes again unchanged (lines are cleaned once
// to wrap and once to print).
func dclean(s string) string {
	return cleanText(toPC437(printable(s)))
}

// dwrap is the terminal's wrap(): words onto lines of width, a word longer
// than a line split; each paragraph keeps its leading spaces on its first
// line, the following lines start with indent.
func dwrap(text string, width int, indent string) []string {
	var lines []string
	for _, para := range strings.Split(text, "\n") {
		lead := para[:len(para)-len(strings.TrimLeft(para, " "))]
		start := len(lines)
		prefix := func() string {
			if len(lines) == start {
				return lead
			}
			return indent
		}
		cur := ""
		for _, word := range strings.Split(para, " ") {
			for word != "" {
				p := prefix()
				avail := min(max(width-len(p), 1), width)
				need := len(word)
				if cur != "" {
					need = len(cur) + 1 + len(word)
				}
				switch {
				case need <= avail:
					if cur == "" {
						cur = word
					} else {
						cur += " " + word
					}
					word = ""
				case cur != "":
					lines = append(lines, p+cur)
					cur = ""
				default:
					lines = append(lines, p+word[:avail])
					word = word[avail:]
				}
			}
		}
		if cur != "" || len(lines) == start {
			lines = append(lines, prefix()+cur)
		}
	}
	return lines
}

// dleftRight is left and right on one line of width, at least one space apart.
func dleftRight(left, right string, width int) string {
	gap := max(width-len(left)-len(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

// moneyC is "$2,449.41" / "-$3.40".
func moneyC(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	d := fmt.Sprintf("%d", cents/100)
	for i := len(d) - 3; i > 0; i -= 3 {
		d = d[:i] + "," + d[i:]
	}
	return fmt.Sprintf("%s$%s.%02d", sign, d, cents%100)
}

// paren is "($15.50)": an amount taken off.
func paren(cents int64) string {
	if cents < 0 {
		cents = -cents
	}
	return "(" + moneyC(cents) + ")"
}

func shortDate(t time.Time) string { return t.Format("01/02/06") }

func shortTime(t time.Time) string {
	h := t.Hour() % 12
	if h == 0 {
		h = 12
	}
	ap := "pm"
	if t.Hour() < 12 {
		ap = "am"
	}
	return fmt.Sprintf("%d:%02d %s", h, t.Minute(), ap)
}

var nonDigits = regexp.MustCompile(`\D`)

// phoneParens is "(410) 321-9288" for a US number, else as given.
func phoneParens(p string) string {
	d := nonDigits.ReplaceAllString(p, "")
	if len(d) == 11 && strings.HasPrefix(d, "1") {
		d = d[1:]
	}
	if len(d) != 10 {
		return p
	}
	return "(" + d[:3] + ") " + d[3:6] + "-" + d[6:]
}

var aptOnly = regexp.MustCompile(`^[0-9][0-9A-Za-z-]*$`)

// apt is the second address line as SoftTouch prints it: "Apt 1" → "APT 1"; a bare "1" or "4B" → "APT 1".
func apt(l string) string {
	if aptOnly.MatchString(l) {
		l = "Apt " + l
	}
	return strings.ToUpper(l)
}

// deliveryStore is the header / footer (the terminals' StoreInfo, §2.10q).
type deliveryStore struct {
	name, phone, website, footer string
	addressLines                 []string
}

func storeOf(sc storeconfig.StoreContext, prefs order.ReceiptPrefs) deliveryStore {
	return deliveryStore{name: sc.StoreName(), phone: strings.TrimSpace(sc.Location.Phone),
		website:      strings.TrimSpace(sc.LocationConfigString("website", "")),
		footer:       prefs.Footer(sc.LocationConfigString("receipt_footer", "")),
		addressLines: []string{strings.TrimSpace(sc.Location.AddressLine1), strings.TrimSpace(sc.Location.AddressLine2)}}
}

// BuildDeliveryCopies renders an online delivery's two tickets (Merchant
// Copy, Customer Copy) with the store's identity, printed now in the store's
// time zone, laid out for on.Cols (the printer's line, like the terminal's
// printer profile: 48 / 42 / 32).
func BuildDeliveryCopies(snap order.Snapshot, sc storeconfig.StoreContext, prefs order.ReceiptPrefs, on printer.Paper) []byte {
	snap, sc, prefs, on = scrubSnapshot(snap), scrubStore(sc), scrubPrefs(prefs), scrubPaper(on)
	lay := layoutFor(on.Cols)
	reroutedFrom := on.ReroutedFrom
	snap = localize(snap, sc)
	at := deliveryNow()
	if loc, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
		at = at.In(loc)
	}
	p := &escw{width: lay.cols, lay: lay}
	p.raw(cmdInit + cmdNormalSz)
	st := storeOf(sc, prefs.Normalize())
	for _, copy := range []string{MerchantCopy, CustomerCopy} {
		if on.Updated { // printed again after the order changed: the driver drops the old pair
			for _, l := range []string{"UPDATED", "ACTUALIZADO"} {
				p.center()
				p.raw(cmdInvertOn + cmdBoldOn + gsTall)
				p.line(" " + l + " ")
				p.raw(gsNormal + cmdInvertOff + cmdBoldOff)
				p.left()
			}
			p.feed(1)
		}
		if reroutedFrom != "" {
			for _, l := range RerouteLines(reroutedFrom, lay.cols) {
				p.center()
				p.raw(cmdInvertOn + cmdBoldOn)
				p.line(l)
				p.raw(cmdInvertOff + cmdBoldOff)
				p.left()
			}
		}
		deliveryTicket(p, snap, st, at, copy)
	}
	return p.buf.Bytes()
}

// deliveryTicket is printouts.dart deliveryTicket (no DUPLICATE: the Pi
// prints them once, at arrival).
func deliveryTicket(p *escw, snap order.Snapshot, st deliveryStore, at time.Time, copy string) {
	w := p.width
	voided := snap.Voided != ""
	if voided {
		p.center()
		p.bold(true)
		p.raw(gsBig + cmdInvertOn + " VOID " + cmdInvertOff)
		p.feed(2)
		p.raw(gsNormal)
		p.bold(false)
		p.left()
	}
	// storeHeader
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
	p.feed(1)
	// dateTimeLine
	d := "Date: " + shortDate(at)
	pad := w / 2
	if pad < len(d)+2 {
		pad = len(d) + 2
	}
	p.line(d + strings.Repeat(" ", pad-len(d)) + "Time: " + shortTime(at))
	// orderNumber
	num := snap.SourceEventID
	if snap.KitchenNumber != nil {
		num = fmt.Sprintf("%d", *snap.KitchenNumber)
	}
	t := "Order #: " + num
	p.bold(true)
	if len(dclean(t))*2 <= w {
		p.raw(gsBig)
	} else {
		p.raw(gsTall)
	}
	p.line(t)
	p.raw(gsNormal)
	p.bold(false)
	p.line("DELIVERY")
	if by := strings.TrimSpace(snap.TakenBy); by != "" { // a call-center order (contract §2.10ab)
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
		p.wrapped(strings.ToUpper(n), "")
	}
	phone := strings.TrimSpace(snap.Customer.Phone)
	if phone != "" {
		phone = phoneParens(phone)
	}
	labelled(p, "Phone", phone)
	if a := snap.Fulfillment.Address; a != nil {
		labelled(p, "Street", a.Line1)
		if l2 := strings.TrimSpace(a.Line2); l2 != "" {
			p.wrapped(apt(l2), "")
		}
		labelled(p, "City", strings.ToUpper(cityLine(a)))
		if notes := strings.TrimSpace(a.Notes); notes != "" {
			p.feed(1)
			labelled(p, "Directions", notes)
		}
	}
	if n := strings.TrimSpace(snap.Notes); n != "" {
		labelled(p, "Notes", n)
	}
	p.feed(1)
	pays := snap.Payments
	var last *order.PaymentSummary
	for i := range pays {
		if pays[i].AmountCents > 0 {
			last = &pays[i]
		}
	}
	if last != nil {
		p.wrapped("Payment Date Time: "+shortDate(at)+" "+shortTime(at), "  ")
	}
	if ref := snap.TransactionRef(); ref != "" { // the card's processor id (Square's), else the order's key
		p.wrapped("Transaction ID: "+ref, strings.Repeat(" ", 16))
	}
	p.line(strings.Repeat("-", w))
	deliveryItems(p, snap, voided)
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
	paid := int64(0)
	for _, x := range pays {
		paid += x.AmountCents
	}
	due := min(max(snap.Money.Total-paid, 0), max(snap.Money.Total, 0))
	if len(pays) == 0 {
		due = snap.CashDue()
	}
	if !voided && due > 0 {
		p.bold(true)
		p.wrapped("Collect Cash "+moneyC(due), "")
		p.bold(false)
	} else {
		for _, x := range pays {
			name := tenderName(x)
			amt := snap.TenderCents(x) // a card charge of the store's own order prints with its tip
			switch {
			case x.AmountCents < 0:
				p.wrapped("Refund "+name+" -"+moneyC(-amt), "  ")
				continue
			case x.AmountCents == 0 && x.TipCents != 0: // a tip added to a card afterwards
				p.wrapped(name+" tip "+moneyC(x.TipCents), "  ")
				continue
			}
			p.wrapped(name+" "+moneyC(amt), "  ")
			if e := x.EntryLabel(); e != "" && strings.ToLower(x.Method) == "card" {
				p.wrapped(e, "  ") // MANUALLY ENTERED, SQUARE APP, CONTACTLESS …
			}
		}
	}
	p.feed(1)
	p.center()
	p.line(copy)
	p.left()
	p.feed(1)
	// storeFooter
	p.center()
	if st.website != "" {
		p.line("Order Online")
		p.wrapped(st.website, "")
		p.feed(1)
	}
	i := 0
	for _, l := range strings.Split(st.footer, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		if i == 0 {
			p.raw(gsTall)
		}
		p.wrapped(l, "")
		if i == 0 {
			p.raw(gsNormal)
		}
		i++
	}
	p.left()
	p.raw(poweredByFooter(p.lay))
	p.raw(cmdNormalSz)
	p.feed(3)
	p.raw(cmdCut)
}

// tenderName is a payment as the terminal names it on the delivery tickets:
// a card with its last four ("Card XXXX1881"), else the method.
func tenderName(x order.PaymentSummary) string {
	m := strings.ToLower(strings.TrimSpace(x.Method))
	switch m {
	case "card", "credit_card", "credit", "square_app", "manual_card":
		if x.Last4 != "" {
			return "Card XXXX" + x.Last4
		}
		return "Card"
	case "cash":
		return "Cash"
	case "":
		return "Paid"
	}
	return strings.ToUpper(m[:1]) + strings.ReplaceAll(m[1:], "_", " ")
}

func cityLine(a *order.Address) string {
	tail := strings.TrimSpace(strings.Join(nonEmpty(strings.TrimSpace(a.State), strings.TrimSpace(a.PostalCode)), " "))
	return strings.Join(nonEmpty(strings.TrimSpace(a.City), tail), ", ")
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// labelled is "Phone      : (667) 403-0865", only when value says something;
// a long value wraps under itself.
func labelled(p *escw, label, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	head := fmt.Sprintf("%-10s : ", label)
	for i, l := range dwrap(dclean(value), p.width-len(head), "") {
		if i == 0 {
			p.line(head + l)
		} else {
			p.line(strings.Repeat(" ", len(head)) + l)
		}
	}
}

// deliveryItems is itemLines: quantity, the name in capitals (size first),
// the line's price at the right; modifiers indented under it with their own
// price (half-pie modifiers by half); a line's note under them. $0 choices
// stamped not-for-the-driver are left off (a delivery's tickets go with the
// driver, §2.1g).
func deliveryItems(p *escw, snap order.Snapshot, voided bool) {
	w := p.width
	const priceW = 10
	nameW := w - 4 - priceW
	for _, it := range snap.Items {
		var mods []order.LineItem
		for _, c := range it.Children {
			if c.UnitPrice == 0 && c.ShowOnDriverTicket != nil && !*c.ShowOnDriverTicket {
				continue
			}
			mods = append(mods, c)
		}
		if voided {
			p.raw(cmdInvertOn + " VOID " + cmdInvertOff + cmdFeed)
		}
		name := strings.ToUpper(strings.Join(nonEmpty(strings.TrimSpace(displaySize(it.Size)), strings.TrimSpace(it.Name)), " "))
		qty := max(it.Quantity, 1)
		free := it.UnitPrice == 0
		for _, m := range mods {
			free = free && m.UnitPrice == 0
		}
		price := ""
		if !free {
			price = moneyC(int64(qty) * it.UnitPrice)
		}
		for i, l := range dwrap(dclean(name), nameW, "") {
			head := "    " + l
			if i == 0 {
				head = fmt.Sprintf("%-3d %s", qty, l)
			}
			if i == 0 && price != "" {
				p.line(dleftRight(head, price, w))
			} else {
				p.line(head)
			}
		}
		for _, m := range deliveryModLines(mods) {
			amt := ""
			if m.unit != 0 {
				amt = moneyC(m.unit * int64(max(m.qty, 1)))
			}
			label := m.name
			if m.qty > 1 {
				label = fmt.Sprintf("%dx %s", m.qty, m.name)
			}
			for i, l := range dwrap(dclean(label), nameW-2, "") {
				head := "      " + l
				if i == 0 && amt != "" {
					p.line(dleftRight(head, amt, w))
				} else {
					p.line(head)
				}
			}
		}
		if n := strings.TrimSpace(it.Notes); n != "" {
			p.wrapped("      Note: "+n, strings.Repeat(" ", 12))
		}
	}
}

type modLine struct {
	name string
	qty  int
	unit int64
}

var halfSuffix = regexp.MustCompile(` \((1st|2nd) half\)$`)

// deliveryModLines is receipt_render.dart modLines: the whole pie's
// modifiers, then the first half's ("1st: X"), then the second's.
func deliveryModLines(mods []order.LineItem) []modLine {
	half := func(p string) int {
		switch p {
		case order.PortionFirst:
			return 1
		case order.PortionSecond:
			return 2
		}
		return 0
	}
	var out []modLine
	for k := 0; k < 3; k++ {
		for _, m := range mods {
			if half(m.Portion) != k {
				continue
			}
			name := m.Name
			if k > 0 {
				name = halfSuffix.ReplaceAllString(name, "")
				if m.Mode == "no" && !strings.HasPrefix(name, "NO ") {
					name = "NO " + name
				}
				if m.Mode == "only" && !strings.HasPrefix(name, "ONLY ") {
					name = "ONLY " + name
				}
				if k == 1 {
					name = "1st: " + name
				} else {
					name = "2nd: " + name
				}
			}
			out = append(out, modLine{name: name, qty: m.Quantity, unit: m.UnitPrice})
		}
	}
	return out
}

// deliveryTotals is totalLines: Sub-total, Discounts, the other fees, Tax,
// Delivery, Tip, then Total in large letters.
func deliveryTotals(p *escw, snap order.Snapshot) {
	w := p.width
	m := snap.Money
	pair := func(l, v string) { p.line(dleftRight(dclean(l), dclean(v), w)) }
	pair("Sub-total:", moneyC(m.Subtotal))
	var disc int64
	for _, d := range m.Discounts {
		disc += d.Amount
	}
	if disc > 0 {
		pair("Discounts:", paren(disc))
	}
	var deliveryFee int64
	for _, f := range m.Fees {
		label := f.Label
		if label == "" {
			label = f.Type
		}
		if strings.EqualFold(f.Type, "delivery") || strings.Contains(strings.ToLower(f.Label), "delivery") {
			deliveryFee += f.Amount
			continue
		}
		pair(label+":", moneyC(f.Amount))
	}
	pair("Tax:", moneyC(m.Tax))
	if deliveryFee > 0 {
		pair("Delivery:", moneyC(deliveryFee))
	}
	// The store's own orders keep their tips on the payments (main's rule):
	// the copy shows them as the Tip and inside the Total that was charged.
	tips := snap.PaymentTips()
	if tip := m.Tip + tips; tip > 0 {
		pair("Tip:", moneyC(tip))
	}
	total := dclean(moneyC(m.Total + tips))
	p.bold(true)
	if len("Total:")+1+len(total) <= w/2 {
		p.raw(gsBig)
		p.line(dleftRight("Total:", total, w/2))
	} else {
		p.raw(gsTall)
		p.line(dleftRight("Total:", total, w))
	}
	p.raw(gsNormal)
	p.bold(false)
}
