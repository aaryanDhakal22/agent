package order

// Which orders the counter tablet (Quibase) gets (2.5.4, the owner
// 2026-11-13; contract §2.4 "What Quibase gets"). Quibase shows the orders of
// every source and every delivery, but a counter or takeout order rung at one
// of the store's counter terminals (Centurion) only when BOTH:
//
//   - its total is over the store's X (main's terminal setting
//     quibase_counter_min_cents, default $5), and
//   - it has a kitchen line: a top-level line that isn't "No kitchen ticket"
//     (Routing "none": a drink, Advance Payment).
//
// A can of soda rung at the counter stays off the tablet. Deliveries always
// show (the owner, 2026-11-13): Quibase is where they are sent out. The rule
// is the Pi's, applied to what it hands a paired tablet (GET /api/orders,
// /api/orders/arrived, the /api/events order frames), so every Quibase build
// obeys it; the order still counts everywhere else (the terminals, the Pi
// page, the Z, driver reports, main).

// deviceKindCallcenter is Snapshot.DeviceKind of a call-center order
// (terminal.DeviceKindCallcenter; that package imports this one).
const deviceKindCallcenter = "callcenter"

// IsTerminalRung reports an order rung at one of the store's counter
// terminals (source counter, not the call center's).
func (s Snapshot) IsTerminalRung() bool {
	return s.Source == SourceCounter && s.DeviceKind != deviceKindCallcenter
}

// HasKitchenLine reports whether a top-level line goes to a kitchen station:
// any line not marked "No kitchen ticket" (an unrouted or custom line prints
// on every station, so it counts too). An order with no lines has none.
func (s Snapshot) HasKitchenLine() bool {
	for _, it := range s.Items {
		if it.Routing != RoutingNone {
			return true
		}
	}
	return false
}

// ShownOnTablet reports whether Quibase gets the order: every order that
// isn't rung at a counter terminal, and every delivery (the owner,
// 2026-11-13: a delivery always shows, whatever its total or lines, so it is
// sent out there); a terminal's counter or takeout order only when its total
// is over minCents and it has a kitchen line.
func (so StoredOrder) ShownOnTablet(minCents int64) bool {
	if !so.Order.IsTerminalRung() || so.IsDelivery() {
		return true
	}
	return so.Order.Money.Total > minCents && so.Order.HasKitchenLine()
}
