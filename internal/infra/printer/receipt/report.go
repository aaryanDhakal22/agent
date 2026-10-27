package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/storeconfig"
)

// BuildDriverReport renders one driver's slip for a batch, all at one text
// size (size = the receipt settings' driver_report_size: small | normal |
// large; nothing larger, so a long slip stays short): the header, the CASH
// orders first (each with its amount, then a subtotal), the CARD orders below
// (each with its tip, then the tips), the POS notes, the totals, and what is
// owed — to the store, or (negative) by the store — rounded to whole dollars
// with the exact amount under it. The report must already be Finish()ed.
// printedAt is shown in the footer.
func BuildDriverReport(rep order.DriverReport, sc storeconfig.StoreContext, size, reroutedFrom string, printedAt time.Time) []byte {
	loc := time.Local
	if tz, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
		loc = tz
	}
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }
	bold := func(s string) { w(cmdBoldOn); w(s); w(cmdBoldOff); nl() }
	// Headers, groups and totals print large (the base size of the other
	// slips, 41 characters — the store's 72 mm-wide Online printer wraps a
	// 48-character line); only the delivery lines use the report's text size.
	itemWidth := driverReportWidth(size)
	separator := func() string { return strings.Repeat("-", lineWidth) }
	itemSize := driverReportSize(size)

	w(cmdInit)
	w(cmdBaseSz)
	if reroutedFrom != "" {
		w(cmdCenter)
		w(cmdBoldOn)
		w(cmdInvertOn)
		w(" REROUTED from " + strings.ToUpper(reroutedFrom) + " ")
		w(cmdInvertOff)
		w(cmdBoldOff)
		nl()
	}

	// --- Header -----------------------------------------------------------
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w("DRIVER REPORT")
	nl()
	w(cmdQuadSz)
	w(truncRunes(strings.ToUpper(rep.DriverName), 12))
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	if sc.Location.Name != "" {
		w(truncRunes(sc.Location.Name, lineWidth))
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
	for _, l := range rep.Lines {
		if l.Cash {
			cash, cashSum = append(cash, l), cashSum+l.CashCents
		} else {
			card, tipSum = append(card, l), tipSum+l.TipCents
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
	// cash: the total), then a POS note if any.
	line := func(l order.DriverReportLine) {
		num := "----"
		if l.KitchenNumber != nil {
			num = fmt.Sprintf("%d", *l.KitchenNumber)
		} else if l.DisplayNumber != nil {
			num = fmt.Sprintf("%d", *l.DisplayNumber) // a POS check
		}
		w(itemSize)
		bold(truncRunes(strings.TrimSpace(num+" "+l.Street), itemWidth))
		w(leftRight("Tip: "+dollars(l.OrderTipCents), "Total: "+dollars(l.SlipTotalCents), itemWidth))
		nl()
		if l.Note != "" { // how it matched the POS
			w("  * " + truncRunes(l.Note, itemWidth-4))
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
		w(leftRight("Card tips", dollars(tipSum), lineWidth))
		nl()
	}
	if len(rep.Notes) > 0 {
		w(separator())
		nl()
	}
	for _, n := range rep.Notes {
		for _, l := range wordWrap("* "+n, lineWidth, "  ") {
			w(l)
			nl()
		}
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
	w(strings.Repeat("=", lineWidth))
	nl()

	label, amount, exact := "OWED TO STORE", rep.OwedRoundedCents, rep.OwedCents
	if amount < 0 || (amount == 0 && exact < 0) {
		label, amount, exact = "STORE OWES DRIVER", -amount, -exact
	}
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w(label)
	nl()
	w(cmdQuadSz)
	w(dollars(amount))
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	if exact != amount {
		w("exact " + dollars(exact))
		nl()
	}
	nl()
	if p := rep.POS; p != nil { // was it checked against the POS?
		if p.Checked {
			pos := "POS checked"
			if p.AsOf != nil {
				pos += " " + p.AsOf.In(loc).Format("3:04 PM")
			}
			w(pos)
			nl()
		} else {
			bold("POS NOT CHECKED")
			if p.Note != "" {
				w(truncRunes(p.Note, lineWidth))
				nl()
			}
		}
	}
	w("Printed " + printedAt.In(loc).Format("Jan 2 3:04 PM"))
	nl()

	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}

// driverReportWidth is the delivery lines' width at the slip's size, sized
// for the store's 72 mm printer (font A 41 characters like the receipt,
// also at double height; font B 54) — 48 / 64 wrapped there.
func driverReportWidth(size string) int {
	if size == order.SizeSmall {
		return 54 // font B on the store's 72 mm printer
	}
	return lineWidth
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
