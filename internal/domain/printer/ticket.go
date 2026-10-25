package printer

import "quiccpos/agent/internal/domain/order"

// TicketKind is what a printed ticket is for.
type TicketKind string

const (
	// TicketFull is the classic single full receipt — used for orders that
	// predate kitchen routing (no line item carries a routing value).
	TicketFull TicketKind = "full"
	// TicketReceipt is the full customer receipt (totals, payment, address),
	// printed on the location's receipt printer.
	TicketReceipt TicketKind = "receipt"
	// TicketStation is a kitchen ticket: only the station's own items in full,
	// then the rest of the order listed small below a rule.
	TicketStation TicketKind = "station"
)

// Mode is which of an order's tickets a print job produces
// (application/printer.PlanFor).
type Mode string

const (
	// ModeAccept is the accept print: the kitchen station tickets + the
	// customer receipt, with the pickup-cash rule (receipt only) applied.
	ModeAccept Mode = "accept"
	// ModeReceipt is "Reprint": only the customer receipt (the full receipt
	// for an order that predates kitchen routing).
	ModeReceipt Mode = "receipt"
	// ModeKitchen is "Resend to kitchen": only the station tickets accept
	// would print, ignoring the pickup-cash rule.
	ModeKitchen Mode = "kitchen"
)

// Ticket is one thing to print for an order. A planned order is a set of
// Tickets (see application/printer.Plan); the receipt builder renders each.
type Ticket struct {
	Kind TicketKind
	// TargetID is the printer (store_config id) this ticket is meant for.
	TargetID string
	// Station is the target printer's display name (printed on station tickets).
	Station string
	// Mine are the station's own top-level items, printed in full (size,
	// modifiers, notes). Others are the order's remaining top-level items,
	// listed small after a rule so the cook knows what else is on the order.
	Mine   []order.LineItem
	Others []order.LineItem
	// ReroutedFrom is set when the ticket printed on a fallback printer
	// because TargetID was unreachable — rendered as a "REROUTED from X" banner.
	ReroutedFrom string
}

// TicketResult records where one ticket printed (or why it didn't).
type TicketResult struct {
	Kind         TicketKind
	Station      string // intended printer's name
	PrintedOn    string // actual printer's name ("" if it didn't print)
	ReroutedFrom string // non-empty when PrintedOn != the intended printer
	Err          string // non-empty when it printed nowhere
}

// Outcome is the result of printing an order's tickets.
type Outcome struct {
	Printed []TicketResult
	Failed  []TicketResult
	// Unrouted names the items that had no matched printer (unmatched name,
	// no printer assigned, or a printer this agent doesn't know) — they were
	// sent to every station; worth an alert so the menu can be fixed.
	Unrouted []string
	// InsertExpected (accept only): an online cash pickup whose kitchen
	// tickets this accept printed because the hub inserts it into the POS
	// live (the insert_live rule, from the same rules snapshot as the
	// print). The feed's order.accepted carries it: the hub inserts only
	// when it is true.
	InsertExpected bool
}
