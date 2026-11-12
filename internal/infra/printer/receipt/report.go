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

// BuildDriverReport renders one driver's slip for a batch, all at one text
// size (size = the receipt settings' driver_report_size: small | normal |
// large; nothing larger, so a long slip stays short): the header, the CASH
// orders first (each with its amount, then a subtotal), the CARD orders below
// (each with its tip, then the tips; a website card tip not charged yet is
// marked pending, 2.5.0), the POS notes, the totals, and what is
// owed — to the store, or (negative) by the store — rounded to whole dollars
// with the exact amount under it. The report must already be Finish()ed.
// printedAt is shown in the footer. It is laid out for on.Cols, the printer
// it prints on (width.go), with the reroute bands on a stand-in.
func BuildDriverReport(rep order.DriverReport, sc storeconfig.StoreContext, size string, on printer.Paper, printedAt time.Time) []byte {
	lay := layoutFor(on.Cols)
	lineWidth := lay.cols
	loc := time.Local
	if tz, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
		loc = tz
	}
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(toPC437(s)) }
	nl := func() { buf.WriteString(cmdFeed) }
	bold := func(s string) { w(cmdBoldOn); w(s); w(cmdBoldOff); nl() }
	// Headers, groups and totals print large (the base size of the other
	// slips, the printer's line); only the delivery lines use the report's
	// text size.
	itemWidth := lay.driverReportWidth(size)
	separator := func() string { return strings.Repeat("-", lineWidth) }
	itemSize := driverReportSize(size)

	w(cmdInit)
	w(cmdBaseSz)
	if on.ReroutedFrom != "" {
		writeRerouted(w, nl, on.ReroutedFrom, lay)
	}

	// --- Header -----------------------------------------------------------
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w("DRIVER REPORT")
	nl()
	nameSz, nameW := bigName(strings.ToUpper(rep.DriverName), lay)
	w(nameSz)
	w(wrapJoin(strings.ToUpper(rep.DriverName), nameW, ""))
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	if sc.Location.Name != "" {
		w(wrapJoin(sc.Location.Name, lineWidth, ""))
		nl()
	}
	w(fmt.Sprintf("Batch #%d", rep.BatchID))
	nl()
	w(batchWindow(rep, loc))
	nl()
	w(cmdLeft)
	w(separator())
	nl()

	// --- Deliveries: cash first, then card --------------------------------
	var cash, card []order.DriverReportLine
	var cashSum, tipSum int64
	pending := 0 // 2.5.0: card website tips main hasn't charged yet
	for _, l := range rep.Lines {
		if l.Cash {
			cash, cashSum = append(cash, l), cashSum+l.CashCents
		} else {
			card, tipSum = append(card, l), tipSum+l.TipCents
			if l.TipPending {
				pending++
			}
		}
	}
	if len(rep.Lines) == 0 {
		w(cmdCenter)
		w("No deliveries this batch")
		nl()
		w(cmdLeft)
	}
	// Each delivery, no customer name: "<ticket #> <street>", then the tip
	// at the far left and the total at the far right (card: total + tip;
	// cash: the total).
	line := func(l order.DriverReportLine) {
		num := "----"
		if l.KitchenNumber != nil {
			num = fmt.Sprintf("%d", *l.KitchenNumber)
		}
		w(itemSize)
		bold(wrapJoin(strings.TrimSpace(num+" "+l.Street), itemWidth, strings.Repeat(" ", len(num)+1)))
		tip := "Tip: " + dollars(l.OrderTipCents)
		if l.TipPending { // a website card tip ADD TIP can still change
			tip += " (pending)"
		}
		for _, s := range leftRightLines(tip, "Total: "+dollars(l.SlipTotalCents), itemWidth) {
			w(s)
			nl()
		}
		w(cmdBaseSz)
	}
	if len(cash) > 0 {
		bold(fmt.Sprintf("CASH ORDERS (%d)", len(cash)))
		for _, l := range cash {
			line(l)
		}
		w(leftRight("Cash subtotal", dollars(cashSum), lineWidth))
		nl()
	}
	if len(card) > 0 {
		if len(cash) > 0 {
			w(separator())
			nl()
		}
		bold(fmt.Sprintf("CARD ORDERS (%d)", len(card)))
		for _, l := range card {
			line(l)
		}
		tips := "Card tips"
		if pending > 0 {
			tips += fmt.Sprintf(" (%d pending)", pending)
		}
		w(leftRight(tips, dollars(tipSum), lineWidth))
		nl()
	}
	// --- Totals -----------------------------------------------------------
	w(separator())
	nl()
	w(leftRight("Deliveries", fmt.Sprintf("%d", rep.Deliveries), lineWidth))
	nl()
	w(leftRight("Total cash owed", dollars(rep.CashCents), lineWidth))
	nl()
	w(leftRight("Total tip (A/T)", dollars(rep.TipAfterTaxCents), lineWidth))
	nl()
	w(fmt.Sprintf("  (%s - %s)", dollars(rep.TipCents), percent(rep.TipTaxBps)))
	nl()
	if rep.DriverShareCents > 0 { // his share of the delivery fees (not in the owed total)
		w(leftRight("Delivery fee share", dollars(rep.DriverShareCents), lineWidth))
		nl()
	}
	w(strings.Repeat("=", lineWidth))
	nl()

	label, amount, exact := "OWED TO STORE", rep.OwedRoundedCents, rep.OwedCents
	if amount < 0 || (amount == 0 && exact < 0) {
		label, amount, exact = "STORE OWES DRIVER", -amount, -exact
	}
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(wrapJoin(label, lay.big(), ""))
	nl()
	w(cmdQuadSz)
	w(wrapJoin(dollars(amount), lay.xl(), ""))
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	if exact != amount {
		w("exact " + dollars(exact))
		nl()
	}
	nl()
	w("Printed " + printedAt.In(loc).Format("Jan 2 3:04 PM"))
	nl()

	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}

// driverReportWidth is the delivery lines' width at the slip's size on the
// printer: font A (also at double height) = its line, font B (small) 64 /
// 56 / 42.
func (l layout) driverReportWidth(size string) int {
	if size == order.SizeSmall {
		return l.fontB()
	}
	return l.cols
}

// driverReportSize is the slip's one text size: small = font B, normal =
// font A (the default), large = font A double height (the base font of the
// other slips, the driver report's size before it had a setting).
func driverReportSize(size string) string {
	switch size {
	case order.SizeSmall:
		return cmdSmallSz
	case order.SizeLarge:
		return cmdBaseSz
	}
	return cmdNormalSz
}

// batchWindow is "Oct 16 10:02 AM - 11:48 PM" (the end is "now" while open).
func batchWindow(rep order.DriverReport, loc *time.Location) string {
	start := rep.BatchOpenedAt.In(loc)
	if rep.BatchClosedAt == nil {
		return start.Format("Jan 2 3:04 PM") + " - open"
	}
	end := rep.BatchClosedAt.In(loc)
	if end.YearDay() == start.YearDay() && end.Year() == start.Year() {
		return start.Format("Jan 2 3:04 PM") + " - " + end.Format("3:04 PM")
	}
	return start.Format("Jan 2 3:04 PM") + " - " + end.Format("Jan 2 3:04 PM")
}

// percent renders basis points as "6%" / "6.5%".
func percent(bps int64) string {
	if bps%100 == 0 {
		return fmt.Sprintf("%d%%", bps/100)
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", float64(bps)/100), "0"), ".") + "%"
}
