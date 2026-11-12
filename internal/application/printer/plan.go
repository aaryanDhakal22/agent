package printer

import (
	"strings"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// RosterEntry is one configured printer as the planner sees it.
type RosterEntry struct {
	ID        string
	Name      string
	IsReceipt bool
}

// Rules are the store's print switches (2.1.0): the six kitchen-ticket
// toggles, and whether the store hub inserts online cash pickups into the POS
// right now (features.order_insert && hub insert_mode live && hub seen within
// 60 s) — only then may an online cash pickup print kitchen tickets here.
type Rules struct {
	Tickets    order.TicketToggles
	InsertLive bool
}

// DefaultRules are the migration defaults (POS orders print nothing, online
// orders print as in 2.0.9, cash pickups receipt-only).
func DefaultRules() Rules { return Rules{Tickets: order.DefaultTicketToggles()} }

// Plan is the accept print: PlanFor(snap, roster, printer.ModeAccept).
func Plan(snap order.Snapshot, roster []RosterEntry) (tickets []printer.Ticket, unrouted []string) {
	return PlanFor(snap, roster, printer.ModeAccept)
}

// PlanFor is PlanWith under the default rules.
func PlanFor(snap order.Snapshot, roster []RosterEntry, mode printer.Mode) (tickets []printer.Ticket, unrouted []string) {
	return PlanWith(snap, roster, mode, DefaultRules())
}

// PlanWith turns an order into the tickets to print for one mode, given the
// printer roster (in roster order) and the store's print rules. Pure — no
// I/O — so it's exhaustively unit-testable.
//
// ModeAccept (what accept prints):
//
//   - No line item carries a routing value (order predates kitchen routing):
//     one full receipt on the receipt printer — the old behaviour.
//   - Otherwise, kitchen stations = every printer except the receipt printer
//     (or the receipt printer alone if it's the only one). Each top-level item
//     goes to its stamped printer_id; an item that's unmatched, has no printer,
//     or names a printer this agent doesn't have goes to EVERY station (and is
//     reported in unrouted). Each station with items gets a station ticket
//     (its items + the rest of the order listed small), and the receipt
//     printer gets the full customer receipt.
//   - The kitchen-ticket switches (rules.Tickets) pick, per kind of online
//     order, between that and the customer receipt ONLY: online card (no cash
//     due), online cash delivery, online cash pickup. A cash pickup prints
//     tickets only while the hub inserts it into the POS live
//     (rules.InsertLive) — otherwise staff type it into the store's old POS,
//     which prints the kitchen tickets (the 2.0.7 rule, Lombardi's).
//   - A channel order (2.5.0: the store's own website - source not online /
//     pos, see Snapshot.IsChannel) always prints the station tickets + the
//     customer receipt: the online switches and the cash-pickup rule don't
//     apply (it never goes into the store's POS).
//   - A POS order (source pos, pulled by the store hub) never prints a
//     receipt on accept; it prints its station tickets only when the switch
//     for its type is on (pos_delivery / pos_takeout / pos_counter). Its
//     lines without a Pi printer are receipt-only (the POS decides what the
//     kitchen gets).
//   - An item routed to the receipt printer is receipt-only: it gets no
//     station ticket of its own (drinks/desserts the counter or driver picks
//     up from the receipt) — unless the receipt printer is the only printer,
//     in which case it is also the kitchen station.
//
// ModeReceipt ("Reprint"): only the customer receipt on the receipt printer
// (the full receipt for an order that predates kitchen routing).
//
// ModeKitchen ("Resend to kitchen"): only the station tickets, built exactly
// as above but WITHOUT the pickup-cash shortcut (a deliberate tap sends the
// order to the kitchen) and without the receipt. An unrouted (legacy) order's
// items count as unrouted and go to every station. May be empty (e.g. only
// receipt-only items).
//
// With no receipt printer flagged, the first printer is used as the receipt
// printer. An empty roster yields no tickets.
func PlanWith(snap order.Snapshot, roster []RosterEntry, mode printer.Mode, rules Rules) (tickets []printer.Ticket, unrouted []string) {
	if len(roster) == 0 {
		return nil, nil
	}
	receipt := roster[0]
	for _, p := range roster {
		if p.IsReceipt {
			receipt = p
			break
		}
	}
	receiptTicket := printer.Ticket{Kind: printer.TicketReceipt, TargetID: receipt.ID, Station: receipt.Name}
	fullTicket := printer.Ticket{Kind: printer.TicketFull, TargetID: receipt.ID, Station: receipt.Name}

	switch mode {
	case printer.ModeReceipt:
		if !isRouted(snap) {
			return []printer.Ticket{fullTicket}, nil
		}
		return []printer.Ticket{receiptTicket}, nil
	case printer.ModeKitchen:
		return stationTickets(snap, roster, receipt)
	case printer.ModeFutureEarly, printer.ModeFutureKitchen, printer.ModeFutureReceipt, printer.ModeFutureBoth:
		kitchen := mode == printer.ModeFutureKitchen || mode == printer.ModeFutureBoth ||
			(mode == printer.ModeFutureEarly && rules.Tickets.FutureKitchen)
		receiptToo := mode == printer.ModeFutureReceipt || mode == printer.ModeFutureBoth ||
			(mode == printer.ModeFutureEarly && rules.Tickets.FutureReceipt)
		if kitchen {
			if !isRouted(snap) && !isPOS(snap) {
				tickets = []printer.Ticket{{Kind: printer.TicketStation, TargetID: receipt.ID, Station: receipt.Name, Mine: snap.Items}}
			} else {
				tickets, unrouted = stationTickets(snap, roster, receipt)
			}
		}
		if receiptToo {
			if isRouted(snap) {
				tickets = append(tickets, receiptTicket)
			} else {
				tickets = append(tickets, fullTicket)
			}
		}
		for i := range tickets {
			tickets[i].Future = true
		}
		return tickets, unrouted
	case printer.ModeAcceptKitchen:
		all, un := PlanWith(snap, roster, printer.ModeAccept, rules)
		for _, t := range all {
			if t.Kind == printer.TicketStation {
				tickets = append(tickets, t)
			}
		}
		return tickets, un
	case printer.ModeVoid:
		tickets, unrouted = stationTickets(snap, roster, receipt)
		for i := range tickets {
			tickets[i].Void, tickets[i].Others = true, nil
		}
		return tickets, unrouted
	case printer.ModeAddOn:
		tickets, unrouted = stationTickets(snap, roster, receipt)
		for i := range tickets {
			tickets[i].AddOn, tickets[i].Others = true, nil
		}
		return tickets, unrouted
	case printer.ModeChange:
		tickets, unrouted = changeTickets(snap, roster, receipt)
		for i := range tickets {
			tickets[i].WasType = snap.TypeChangedFrom
		}
		return tickets, unrouted
	case printer.ModeTypeChange:
		tickets, unrouted = stationTickets(snap, roster, receipt)
		for i := range tickets {
			tickets[i].Change, tickets[i].TypeOnly, tickets[i].WasType, tickets[i].Others = true, true, snap.TypeChangedFrom, nil
		}
		return tickets, unrouted
	}

	if isPOS(snap) {
		if !rules.Tickets.POSFor(snap.Fulfillment.Type) {
			return nil, nil
		}
		return stationTickets(snap, roster, receipt)
	}
	if !snap.IsChannel() && receiptOnly(snap, rules) {
		return []printer.Ticket{receiptTicket}, nil
	}
	if !isRouted(snap) {
		return []printer.Ticket{fullTicket}, nil
	}
	tickets, unrouted = stationTickets(snap, roster, receipt)
	return append(tickets, receiptTicket), unrouted
}

// changeTickets is ModeChange: the added lines (snap.Items) and the removed
// ones (snap.RemovedItems) routed like station tickets, merged into one
// change ticket per station (roster order). Empty when neither has a line.
func changeTickets(snap order.Snapshot, roster []RosterEntry, receipt RosterEntry) (tickets []printer.Ticket, unrouted []string) {
	added, un1 := stationTickets(snap, roster, receipt)
	gone := snap
	gone.Items, gone.RemovedItems = snap.RemovedItems, nil
	removed, un2 := stationTickets(gone, roster, receipt)
	byID := map[string]*printer.Ticket{}
	for _, t := range added {
		t.Change, t.Others = true, nil
		byID[t.TargetID] = &t
	}
	for _, t := range removed {
		if cur, ok := byID[t.TargetID]; ok {
			cur.Removed = t.Mine
			continue
		}
		t.Change, t.Others, t.Removed, t.Mine = true, nil, t.Mine, nil
		byID[t.TargetID] = &t
	}
	for _, p := range roster {
		if t, ok := byID[p.ID]; ok {
			tickets = append(tickets, *t)
		}
	}
	return tickets, append(un1, un2...)
}

// stationTickets builds the kitchen station tickets (see PlanFor).
func stationTickets(snap order.Snapshot, roster []RosterEntry, receipt RosterEntry) (tickets []printer.Ticket, unrouted []string) {
	stations := make([]RosterEntry, 0, len(roster))
	for _, p := range roster {
		if p.ID != receipt.ID {
			stations = append(stations, p)
		}
	}
	if len(stations) == 0 {
		stations = []RosterEntry{receipt}
	}
	known := make(map[string]bool, len(roster))
	for _, p := range roster {
		known[p.ID] = true
	}

	// Which station(s) each top-level item goes to (by index into snap.Items).
	assigned := make(map[string][]int, len(stations))
	pos := isPOS(snap)
	for i, it := range snap.Items {
		if pos && it.PrinterID == "" {
			continue // a POS line with no Pi printer: not for this kitchen
		}
		if it.Routing == order.RoutingNone {
			continue // "no kitchen ticket": no station, not in "Also on this order"; on the receipt only
		}
		if it.Routing == order.RoutingMatched && known[it.PrinterID] {
			assigned[it.PrinterID] = append(assigned[it.PrinterID], i)
			continue
		}
		unrouted = append(unrouted, it.Name)
		for _, st := range stations {
			assigned[st.ID] = append(assigned[st.ID], i)
		}
	}

	// Station tickets in roster order. The receipt printer gets one only when
	// it is also the sole station; otherwise its items ride on the receipt.
	receiptIsStation := len(stations) == 1 && stations[0].ID == receipt.ID
	// 2.4.0: "items to other printers" lists only food some kitchen station
	// makes - never what goes to the receipt printer alone (drinks,
	// desserts, chips) or a POS line no Pi printer gets.
	kitchen := map[int]bool{}
	for id, idx := range assigned {
		if id == receipt.ID && !receiptIsStation {
			continue
		}
		for _, i := range idx {
			kitchen[i] = true
		}
	}
	for _, p := range roster {
		idx := assigned[p.ID]
		if len(idx) == 0 || (p.ID == receipt.ID && !receiptIsStation) {
			continue
		}
		mine := make([]order.LineItem, 0, len(idx))
		in := make(map[int]bool, len(idx))
		for _, i := range idx {
			mine = append(mine, snap.Items[i])
			in[i] = true
		}
		others := make([]order.LineItem, 0, len(snap.Items)-len(idx))
		for i, it := range snap.Items {
			if !in[i] && kitchen[i] {
				others = append(others, it)
			}
		}
		tickets = append(tickets, printer.Ticket{
			Kind: printer.TicketStation, TargetID: p.ID, Station: p.Name, Mine: mine, Others: others,
		})
	}
	return tickets, unrouted
}

// isPickupCash: a pickup the customer pays (some) cash for — the one cash
// rule, Snapshot.CashDue (no payments at all = the whole total is cash).
func isPickupCash(snap order.Snapshot) bool {
	return strings.EqualFold(strings.TrimSpace(snap.Fulfillment.Type), "pickup") && snap.CashDue() > 0
}

// InsertsLive: an online cash pickup that prints its kitchen tickets on
// accept because the hub inserts it into the POS live (rules.InsertLive and
// the online_cash_pickup switch) — the hub is then expected to insert it.
// Never a channel order (2.5.0: the store's website never goes into the POS).
func InsertsLive(snap order.Snapshot, rules Rules) bool {
	return !isPOS(snap) && !snap.IsChannel() && isPickupCash(snap) && rules.InsertLive && rules.Tickets.OnlineCashPickup
}

// receiptOnly applies the online kitchen-ticket switches: an online order
// whose switch is off prints only the customer receipt. A cash pickup needs
// its switch AND the hub inserting live.
func receiptOnly(snap order.Snapshot, rules Rules) bool {
	switch {
	case snap.CashDue() == 0:
		return !rules.Tickets.OnlineCard
	case isPickupCash(snap):
		return !(rules.InsertLive && rules.Tickets.OnlineCashPickup)
	case strings.EqualFold(strings.TrimSpace(snap.Fulfillment.Type), "delivery"):
		return !rules.Tickets.OnlineCashDelivery
	}
	return false
}

// isPOS: an order the store hub pulled from the POS.
func isPOS(snap order.Snapshot) bool { return snap.Source == order.SourcePOS }

// isRouted reports whether main stamped kitchen routing on this order.
func isRouted(snap order.Snapshot) bool {
	for _, it := range snap.Items {
		if it.Routing != "" {
			return true
		}
	}
	return false
}
