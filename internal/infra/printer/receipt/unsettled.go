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

// BuildUnsettledList renders the UNSETTLED CHECKS slip (contract/agent.md
// §2.10s §4, the owner 2026-11-12) the Pi prints on the driver reports'
// printer when it auto-closes a batch with checks still owing money: per
// check its number, type, customer, items, total / paid / due and whether it
// was carried (still open, payable next day); then the count and the total
// due. Same text size rule as the driver report (size = driver_report_size).
func BuildUnsettledList(l order.UnsettledList, sc storeconfig.StoreContext, size string, on printer.Paper, printedAt time.Time) []byte {
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

	w(cmdInit)
	w(cmdBaseSz)
	if on.ReroutedFrom != "" {
		writeRerouted(w, nl, on.ReroutedFrom, lay)
	}
	w(cmdCenter)
	w(cmdBoldOn)
	w(cmdDoubleSz)
	w("UNSETTLED")
	nl()
	w("CHECKS")
	nl()
	w(cmdBaseSz)
	w(cmdBoldOff)
	if sc.Location.Name != "" {
		w(wrapJoin(sc.Location.Name, lineWidth, ""))
		nl()
	}
	w(fmt.Sprintf("Batch #%d closed", l.BatchID))
	nl()
	w(cmdLeft)
	separator()

	for i, c := range l.Checks {
		if i > 0 {
			separator()
		}
		w(itemSize)
		head := fmt.Sprintf("#%d %s", c.Number, strings.ToUpper(c.Type))
		if c.Carried {
			head += " (KEPT OPEN)"
		}
		bold(wrapJoin(head, itemWidth, "  "))
		who := strings.TrimSpace(c.Customer.Name)
		if c.Customer.Phone != "" {
			who = strings.TrimSpace(who + " " + c.Customer.Phone)
		}
		if who != "" {
			w(wrapJoin(who, itemWidth, "  "))
			nl()
		}
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
		w(leftRight("Total", dollars(c.TotalCents), itemWidth))
		nl()
		w(leftRight("Paid", dollars(c.PaidCents), itemWidth))
		nl()
		w(cmdBoldOn)
		w(leftRight("DUE", dollars(c.DueCents), itemWidth))
		w(cmdBoldOff)
		nl()
		w(cmdBaseSz)
	}

	separator()
	w(leftRight("Checks", fmt.Sprintf("%d", l.Count), lineWidth))
	nl()
	w(cmdBoldOn)
	w(leftRight("Total due", dollars(l.DueCents), lineWidth))
	w(cmdBoldOff)
	nl()
	w(strings.Repeat("=", lineWidth))
	nl()
	w(cmdCenter)
	w(wrapJoin("Kept-open checks stay on the Pickup list", lineWidth, ""))
	nl()
	w("Printed " + printedAt.In(loc).Format("Jan 2 3:04 PM"))
	nl()
	w(cmdFeed + cmdFeed + cmdFeed + cmdFeed)
	w(cmdCut)
	return buf.Bytes()
}
