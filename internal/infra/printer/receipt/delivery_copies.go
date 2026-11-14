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
// the Customer Copy, in one job, each cut, in SoftTouch's layout (softSlip,
// the owner 2026-11-12) like the terminal's own delivery copies: keep the two
// in step when either changes.

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
	// strike: lines print as struck pictures (a voided order's items,
	// strike.go); strikeImages = receipt_prefs.strike_images.
	strike, strikeImages bool
	// boldWords: a printed choice holding one of them prints bold
	// (receipt_prefs.bold_choice_words, 2.5.2).
	boldWords order.WordList
	// handoverWords: a $0 choice holding one of them prints on the slip
	// (receipt_prefs.handover_choice_words, 2.5.7, handedOver).
	handoverWords order.WordList
	// drinkMarks: receipt_prefs.drink_marks; a delivery's lines stamped a
	// drink end in order.DrinkMark (contract §2.1k, deliveryItems).
	drinkMarks bool
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
func (p *escw) line(s string) {
	if p.strike { // a picture of its own line: no line feed after it
		p.raw(strikeRaster(from437(dclean(s)), false))
		return
	}
	p.raw(dclean(s) + cmdFeed)
}
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
	p := &escw{width: lay.cols, lay: lay, strikeImages: prefs.StrikeImages, boldWords: prefs.BoldChoiceWords,
		handoverWords: prefs.HandoverChoiceWords, drinkMarks: prefs.DrinkMarks}
	p.raw(cmdInit + cmdNormalSz)
	st := storeOf(sc, prefs.Normalize())
	classic := prefs.Normalize().StyleFor(snap.Source) == order.StyleClassic
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
		if classic { // the classic receipt twice (2.5.2): its own reroute bands, no copy label
			p.raw(string(buildFull(snap, sc, reroutedFrom, prefs.Normalize(), lay)))
			continue
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
		softSlip(p, snap, st, at, copy, headerFor(sc, lay))
	}
	return p.buf.Bytes()
}

// BuildCustomerReceipt is the customer receipt of any order (pickup / takeout
// / delivery, a Reprint too): one SoftTouch slip, Customer Copy (contract
// §2.1j), the bands the caller asks for on top.
func BuildCustomerReceipt(snap order.Snapshot, sc storeconfig.StoreContext, prefs order.ReceiptPrefs, on printer.Paper) []byte {
	snap, sc, prefs, on = scrubSnapshot(snap), scrubStore(sc), scrubPrefs(prefs), scrubPaper(on)
	lay := layoutFor(on.Cols)
	snap = localize(snap, sc)
	at := deliveryNow()
	if loc, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
		at = at.In(loc)
	}
	p := &escw{width: lay.cols, lay: lay, strikeImages: prefs.StrikeImages, boldWords: prefs.BoldChoiceWords,
		handoverWords: prefs.HandoverChoiceWords, drinkMarks: prefs.DrinkMarks}
	p.raw(cmdInit + cmdNormalSz)
	if on.ReroutedFrom != "" { // the receipt's own bands (writeRerouted), as on every slip of a stand-in
		writeRerouted(p.raw, func() { p.raw(cmdFeed) }, on.ReroutedFrom, lay)
		p.raw(cmdNormalSz)
	}
	softSlip(p, snap, storeOf(sc, prefs.Normalize()), at, CustomerCopy, headerFor(sc, lay))
	return p.buf.Bytes()
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

// deliveryItems is itemLines: quantity, the name in capitals (size first),
// the line's price at the right; modifiers indented under it with their own
// price (half-pie modifiers by half); a line's note under them. A $0 choice
// prints only when the driver hands it over (slipShowsChoice, §2.1j). On a
// delivery (marks) a line stamped a drink ends in "#####" (withDrinkMark).
func deliveryItems(p *escw, snap order.Snapshot, voided, marks bool) {
	w := p.width
	const priceW = 10
	nameW := w - 4 - priceW
	for _, it := range snap.Items {
		var mods []order.LineItem
		for _, c := range it.Children {
			if slipShowsChoice(c, p.handoverWords) {
				mods = append(mods, c)
			}
		}
		if voided && !p.strikeImages { // strike_images off: a reversed VOID tag over the item
			p.raw(cmdInvertOn + " VOID " + cmdInvertOff + cmdFeed)
		}
		p.strike = voided && p.strikeImages // the item, its choices and note struck through
		name := slipItemName(it.Size, it.Name)
		qty := max(it.Quantity, 1)
		free := it.UnitPrice == 0
		for _, m := range mods {
			free = free && m.UnitPrice == 0
		}
		price := ""
		if !free {
			price = moneyC(int64(qty) * it.UnitPrice)
		}
		p.bold(true) // the item's own lines bold (the owner, 2.5.2); its choices follow the bold words
		for i, l := range withDrinkMark(dwrap(dclean(name), nameW, ""), nameW, marks && it.IsDrink()) {
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
		p.bold(false)
		for _, m := range deliveryModLines(mods) {
			amt := ""
			if m.unit != 0 {
				amt = moneyC(m.unit * int64(max(m.qty, 1)))
			}
			label := m.name
			if n := perItem(m.qty, qty); n > 1 { // per item, never folded (#20)
				label = fmt.Sprintf("%dx %s", n, m.name)
			}
			bold := p.boldWords.Matches(m.name)
			if bold {
				p.bold(true)
			}
			for i, l := range withDrinkMark(dwrap(dclean(label), nameW-2, ""), nameW-2, marks && m.drink) {
				head := "      " + l
				if i == 0 && amt != "" {
					p.line(dleftRight(head, amt, w))
				} else {
					p.line(head)
				}
			}
			if bold {
				p.bold(false)
			}
		}
		if n := strings.TrimSpace(it.Notes); n != "" {
			p.wrapped("      Note: "+n, strings.Repeat(" ", 12))
		}
		p.strike = false
	}
}

// slipShowsChoice is the customer slips' rule for a choice (kitchen tickets
// keep every choice; pickup and delivery alike, reprints too): a priced one
// always prints; a $0 one prints only when the driver hands it over
// (handedOver). Agent 2.5.2 hid every $0 choice of an online vendor order,
// which left the cans and the fries off its delivery ticket (Lombardi's,
// 2026-11-13: "15 pc Chicken Tender" printed alone); 2.5.7 reads the
// vendor's names with the store's hand-over words.
func slipShowsChoice(c order.LineItem, words order.WordList) bool {
	return c.UnitPrice != 0 || handedOver(c, words)
}

// handedOver (agent 2.5.7, the owner 2026-11-13: "only the ones the driver
// hands over": drinks, sides, sauces, dressings, never a cooking choice such
// as "Well Done"): our menu stamped the choice show_on_driver_ticket true,
// or it isn't a NO and its name holds one of the store's hand-over words
// (receipt_prefs.handover_choice_words) as a whole word. The vendor's
// choices are never stamped: the words decide for them.
func handedOver(c order.LineItem, words order.WordList) bool {
	if c.ShowOnDriverTicket != nil && *c.ShowOnDriverTicket {
		return true
	}
	if isNoChoice(c) {
		return false
	}
	if c.IsDrink() { // a drink the Pi stamped (§2.1k): the driver hands it over
		return true
	}
	// A half of a half-and-half pie (its specialty or topping) is what the
	// pizza is: it always shows (the owner 2026-11-13: "its halves must show").
	return c.Portion == order.PortionFirst || c.Portion == order.PortionSecond ||
		words.MatchesWord(halfSuffix.ReplaceAllString(c.Name, ""))
}

// isNoChoice: a NO (our mode no, the vendor's "remove" note, a name that
// starts with "No ").
func isNoChoice(c order.LineItem) bool {
	n := strings.ToLower(strings.TrimSpace(c.Name))
	return c.Mode == order.ModeNo || strings.EqualFold(strings.TrimSpace(c.Notes), "remove") ||
		n == "no" || strings.HasPrefix(n, "no ")
}

type modLine struct {
	name  string
	qty   int
	unit  int64
	drink bool // stamped a drink (LineItem.Drink)
}

// withDrinkMark ends a drink's wrapped name in " #####" (SoftTouch's marks,
// the owner 2026-11-14, contract §2.1k): on its last line when it fits in
// width, else on a line of its own (a wrapped line; the caller indents it).
// The marks are whole, never split; added after the scrub (they are ours).
func withDrinkMark(lines []string, width int, drink bool) []string {
	if !drink {
		return lines
	}
	if n := len(lines); n > 0 && len(lines[n-1])+1+len(order.DrinkMark) <= width {
		out := append([]string(nil), lines...)
		out[n-1] += " " + order.DrinkMark
		return out
	}
	return append(append([]string(nil), lines...), order.DrinkMark)
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
			out = append(out, modLine{name: name, qty: m.Quantity, unit: m.UnitPrice, drink: m.IsDrink()})
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
