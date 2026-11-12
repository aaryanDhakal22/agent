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

// BuildUnassignedReport renders the UNASSIGNED slip (2.4.0) Close batch
// prints after the driver reports: per delivery left with no driver, the
// number and street + apt, CASH / PAID with the total and tip, its times
// (placed, accepted, ready, delivered), who marked it delivered, the driver
// it was taken from and the drivers in the store then; then the counts.
// Same text size rule as the driver report (size = driver_report_size).
func BuildUnassignedReport(rep order.UnassignedReport, sc storeconfig.StoreContext, size string, on printer.Paper, printedAt time.Time) []byte {
	rep, sc, on = scrubUnassignedReport(rep), scrubStore(sc), scrubPaper(on)
	lay := layoutFor(on.Cols) // the printer it prints on (width.go)
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
	clock := func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "--"
		}
		return t.In(loc).Format("3:04 PM")
	}

	w(cmdInit)
	w(cmdBaseSz)
	if on.ReroutedFrom != "" {
		writeRerouted(w, nl, on.ReroutedFrom, lay)
	}

	// --- Header -----------------------------------------------------------
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w("UNASSIGNED")
	nl()
	w(cmdBaseSz)
	w("DELIVERED - NO DRIVER")
	nl()
	w(cmdBoldOff)
	if sc.Location.Name != "" {
		w(wrapJoin(sc.Location.Name, lineWidth, ""))
		nl()
	}
	w(fmt.Sprintf("Batch #%d", rep.BatchID))
	nl()
	w(batchWindow(order.DriverReport{BatchOpenedAt: rep.BatchOpenedAt, BatchClosedAt: rep.BatchClosedAt}, loc))
	nl()
	w(cmdLeft)
	separator()

	// --- Each delivery ------------------------------------------------------
	for i, l := range rep.Lines {
		if i > 0 {
			separator()
		}
		num := "----"
		if l.KitchenNumber != nil {
			num = fmt.Sprintf("%d", *l.KitchenNumber)
		}
		w(itemSize)
		for _, line := range wordWrap("#"+num+" "+l.Street, itemWidth, "  ") {
			bold(line)
		}
		pay := "PAID " + dollars(l.SlipTotalCents)
		if l.Cash {
			pay = "CASH " + dollars(l.SlipTotalCents)
		}
		for _, s := range leftRightLines(pay, "Tip: "+dollars(l.OrderTipCents), itemWidth) {
			w(s)
			nl()
		}
		for _, s := range leftRightLines("Placed "+clock(l.PlacedAt), "Accepted "+clock(l.AcceptedAt), itemWidth) {
			w(s)
			nl()
		}
		for _, s := range leftRightLines("Ready "+clock(l.ReadyAt), "Delivered "+clock(l.DeliveredAt), itemWidth) {
			w(s)
			nl()
		}
		by := l.MarkedBy.Name
		if l.MarkedBy.Kind == order.ByTablet {
			by = "tablet " + by
		}
		if by == "" {
			by = "Pi web page"
		}
		for _, line := range wordWrap(fmt.Sprintf("Marked by %s %s", by, clock(&l.MarkedAt)), itemWidth, "  ") {
			w(line)
			nl()
		}
		if l.FromDriver != "" {
			w(wrapJoin("Taken off "+l.FromDriver, itemWidth, "  "))
			nl()
		}
		inStore := "nobody"
		if len(l.Available) > 0 {
			inStore = strings.Join(l.Available, ", ")
		}
		for _, line := range wordWrap("In store then: "+inStore, itemWidth, "  ") {
			w(line)
			nl()
		}
		w(cmdBaseSz)
	}

	// --- Totals -----------------------------------------------------------
	separator()
	w(leftRight("Deliveries", fmt.Sprintf("%d", len(rep.Lines)), lineWidth))
	nl()
	w(leftRight("Cash to collect", dollars(rep.CashCents), lineWidth))
	nl()
	w(leftRight("Tips", dollars(rep.TipCents), lineWidth))
	nl()
	w(strings.Repeat("=", lineWidth))
	nl()
	w(cmdCenter)
	bold("WHO TOOK THESE?")
	w("Printed " + printedAt.In(loc).Format("Jan 2 3:04 PM"))
	nl()

	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}
