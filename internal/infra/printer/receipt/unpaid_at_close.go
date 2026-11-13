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

// BuildUnpaidAtClose renders the UNPAID AT CLOSE slip (the owner,
// 2026-11-13; contract/agent.md §2.10s §4): the unpaid counter checks a
// forced close (or the auto-close) wrote off as Void waste "Unpaid at
// close". Per check its number, time, cashier, items and total; then the
// count, the total written off and who closed. Printed on the driver
// reports' printer like the Unsettled Check List (same text size rule).
func BuildUnpaidAtClose(l order.UnpaidAtClose, sc storeconfig.StoreContext, size string, on printer.Paper, printedAt time.Time) []byte {
	l, sc, on = scrubUnpaidAtClose(l), scrubStore(sc), scrubPaper(on)
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
	itemWidth := lay.driverReportWidth(size)
	itemSize := driverReportSize(size)
	separator := func() { w(strings.Repeat("-", lineWidth)); nl() }

	w(cmdInit)
	w(cmdBaseSz)
	if on.ReroutedFrom != "" {
		writeRerouted(w, nl, on.ReroutedFrom, lay)
	}
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w("UNPAID")
	nl()
	w("AT CLOSE")
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	if sc.Location.Name != "" {
		w(wrapJoin(sc.Location.Name, lineWidth, ""))
		nl()
	}
	w(fmt.Sprintf("Batch #%d closed", l.BatchID))
	nl()
	w(wrapJoin("Written off as waste: "+l.Reason, lineWidth, ""))
	nl()
	w(cmdLeft)
	separator()

	for i, c := range l.Checks {
		if i > 0 {
			separator()
		}
		w(itemSize)
		bold(wrapJoin(fmt.Sprintf("#%d %s", c.Number, strings.ToUpper(c.Type)), itemWidth, "  "))
		placed := c.PlacedAt.In(loc).Format("Jan 2 3:04 PM")
		if c.StaffName != "" {
			placed += " by " + c.StaffName
		}
		w(wrapJoin(placed, itemWidth, "  "))
		nl()
		for _, it := range c.Items {
			for _, line := range wordWrap(fmt.Sprintf("%d %s", it.Qty, it.Name), itemWidth, "  ") {
				w(line)
				nl()
			}
			for _, m := range it.Mods {
				for _, line := range wordWrap("  - "+m, itemWidth, "    ") {
					w(line)
					nl()
				}
			}
		}
		w(cmdBoldOn)
		w(leftRight("Total", dollars(c.TotalCents), itemWidth))
		w(cmdBoldOff)
		nl()
		w(cmdBaseSz)
	}

	separator()
	w(leftRight("Checks", fmt.Sprintf("%d", l.Count), lineWidth))
	nl()
	w(cmdBoldOn)
	w(leftRight("Written off", dollars(l.TotalCents), lineWidth))
	w(cmdBoldOff)
	nl()
	w(strings.Repeat("=", lineWidth))
	nl()
	w(cmdCenter)
	who := l.ClosedBy.StaffName
	switch {
	case l.Auto:
		who = "the auto-close"
	case who == "" && l.ClosedBy.DeviceName != "":
		who = l.ClosedBy.DeviceName
	case who == "":
		who = "the Pi"
	}
	w(wrapJoin("Closed by "+who+" "+l.At.In(loc).Format("Jan 2 3:04 PM"), lineWidth, ""))
	nl()
	w(wrapJoin("On the Z report's Waste section", lineWidth, ""))
	nl()
	w("Printed " + printedAt.In(loc).Format("Jan 2 3:04 PM"))
	nl()
	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}

func scrubUnpaidAtClose(l order.UnpaidAtClose) order.UnpaidAtClose {
	l = scrubbed(l)
	l.Reason = cleanName(l.Reason)
	l.ClosedBy.StaffName, l.ClosedBy.DeviceName = cleanName(l.ClosedBy.StaffName), cleanName(l.ClosedBy.DeviceName)
	for i := range l.Checks {
		c := &l.Checks[i]
		c.Customer.Name, c.Customer.Phone = cleanName(c.Customer.Name), cleanName(c.Customer.Phone)
		c.StaffName = cleanName(c.StaffName)
	}
	return l
}
