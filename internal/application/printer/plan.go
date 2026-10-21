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

// Plan turns an order into the tickets to print, given the printer roster
// (in roster order). Pure — no I/O — so it's exhaustively unit-testable.
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
//   - A pickup order with cash to collect prints ONLY the customer receipt:
//     staff type it into the store's old POS, which prints the kitchen
//     tickets (Lombardi's, 2026-10). Delivery and card-paid pickups are normal.
//   - An item routed to the receipt printer is receipt-only: it gets no
//     station ticket of its own (drinks/desserts the counter or driver picks
//     up from the receipt) — unless the receipt printer is the only printer,
//     in which case it is also the kitchen station.
//
// With no receipt printer flagged, the first printer is used as the receipt
// printer. An empty roster yields no tickets.
func Plan(snap order.Snapshot, roster []RosterEntry) (tickets []printer.Ticket, unrouted []string) {
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

	if isPickupCash(snap) {
		return []printer.Ticket{{Kind: printer.TicketReceipt, TargetID: receipt.ID, Station: receipt.Name}}, nil
	}

	if !isRouted(snap) {
		return []printer.Ticket{{Kind: printer.TicketFull, TargetID: receipt.ID, Station: receipt.Name}}, nil
	}

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
	for i, it := range snap.Items {
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
			if !in[i] {
				others = append(others, it)
			}
		}
		tickets = append(tickets, printer.Ticket{
			Kind: printer.TicketStation, TargetID: p.ID, Station: p.Name, Mine: mine, Others: others,
		})
	}
	tickets = append(tickets, printer.Ticket{Kind: printer.TicketReceipt, TargetID: receipt.ID, Station: receipt.Name})
	return tickets, unrouted
}

// isPickupCash: a pickup the customer pays (some) cash for — the one cash
// rule, Snapshot.CashDue (no payments at all = the whole total is cash).
func isPickupCash(snap order.Snapshot) bool {
	return strings.EqualFold(strings.TrimSpace(snap.Fulfillment.Type), "pickup") && snap.CashDue() > 0
}

// isRouted reports whether main stamped kitchen routing on this order.
func isRouted(snap order.Snapshot) bool {
	for _, it := range snap.Items {
		if it.Routing != "" {
			return true
		}
	}
	return false
}
