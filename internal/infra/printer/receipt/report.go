package receipt

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/storeconfig"
)

// BuildDriverReport renders one driver's slip for a batch: every delivery
// (kitchen #, customer, address, tip, CASH amount or CARD) and the footer the
// driver settles with — deliveries, cash owed, tips after tip tax and what is
// owed to the store (or, when negative, what the store owes the driver).
// The report must already be Finish()ed. printedAt is shown in the footer.
func BuildDriverReport(rep order.DriverReport, sc storeconfig.StoreContext, reroutedFrom string, printedAt time.Time) []byte {
	loc := time.Local
	if tz, err := time.LoadLocation(sc.Location.Timezone); sc.Location.Timezone != "" && err == nil {
		loc = tz
	}
	var buf bytes.Buffer
	w := func(s string) { buf.WriteString(s) }
	nl := func() { buf.WriteString(cmdFeed) }

	w(cmdInit)
	w(cmdBaseSz)
	if reroutedFrom != "" {
		writeRerouted(w, nl, reroutedFrom)
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

	// --- Deliveries -------------------------------------------------------
	if len(rep.Lines) == 0 {
		w(cmdCenter)
		w("No deliveries this batch")
		nl()
		w(cmdLeft)
	}
	for _, l := range rep.Lines {
		num := "#----"
		if l.KitchenNumber != nil {
			num = fmt.Sprintf("#%d", *l.KitchenNumber)
		} else if l.DisplayNumber != nil {
			num = fmt.Sprintf("#%d", *l.DisplayNumber) // a POS check
		}
		pay := "CARD"
		if l.Cash {
			pay = "CASH " + dollars(l.CashCents)
		}
		left := truncRunes(num+" "+l.Customer, lineWidth-len(pay)-1)
		w(cmdBoldOn)
		w(leftRight(left, pay, lineWidth))
		w(cmdBoldOff)
		nl()
		if l.Address != "" {
			w("  " + truncRunes(l.Address, lineWidth-2))
			nl()
		}
		if !l.Cash { // a cash order's tip is the driver's already (2.1.0)
			w("  Tip " + dollars(l.TipCents))
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

	label, amount := "OWED TO STORE", rep.OwedCents
	if amount < 0 {
		label, amount = "STORE OWES DRIVER", -amount
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
	nl()
	w("Printed " + printedAt.In(loc).Format("Jan 2 3:04 PM"))
	nl()

	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
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
