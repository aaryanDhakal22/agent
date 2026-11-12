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
	// ModeVoid (00019): lines voided at the POS terminal on a POS order, as
	// station tickets under a VOID banner (only where the POS ticket switch
	// for the order's type is on - the service checks it).
	ModeVoid Mode = "void"
	// ModeFutureEarly (2.4.0): a future order's first ACCEPT with the
	// store's future switches on - the station tickets as an early warning
	// (DO NOT MAKE, when it is due) and / or the customer receipt with a
	// FUTURE bar, per the switches. Every ticket has Future set.
	ModeFutureEarly Mode = "future_early"
	// ModeAcceptKitchen (2.4.0): the accept print without the customer
	// receipt - a future order whose receipt already printed early.
	ModeAcceptKitchen Mode = "accept_kitchen"
	// ModeFutureKitchen / ModeFutureReceipt / ModeFutureBoth (2.4.3): staff's
	// PRINT NOW on a waiting future order - the same early tickets as
	// ModeFutureEarly (Future set), whatever the store's future switches say.
	ModeFutureKitchen Mode = "future_kitchen"
	ModeFutureReceipt Mode = "future_receipt"
	ModeFutureBoth    Mode = "future_both"
	// ModeAddOn (terminal §2.10d): lines added to a counter order after
	// SEND, as station tickets under an ADD-ON banner (only the new lines,
	// no "other printers" list, no receipt).
	ModeAddOn Mode = "add_on"
	// ModeChange (terminal §2.10d): a counter order changed after SEND, one
	// change ticket per station: the snapshot's Items are the added lines
	// (under ADD / AGREGAR), its RemovedItems the removed ones (under
	// REMOVE / QUITAR, struck through). No "also on this order", no receipt.
	ModeChange Mode = "change"
	// ModeTypeChange (terminal §2.10d `type`): a counter order moved takeout
	// <-> delivery after SEND with no line changed: one ticket per station
	// with lines on it, its new type on top, a NOW <TYPE> / WAS <TYPE> band
	// and the station's lines (to find the bag). No receipt.
	ModeTypeChange Mode = "type_change"
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
	// Void: a VOID ticket (ModeVoid) - the items are what was voided.
	Void bool
	// AddOn: an ADD-ON ticket (ModeAddOn) - the items were added after SEND.
	AddOn bool
	// Change (ModeChange): a change ticket - Mine were added, Removed were
	// removed (a Void ticket prints its Mine as removed the same way).
	Change  bool
	Removed []order.LineItem
	// WasType (a change ticket, terminal §2.10d `type`): the order's type
	// before it moved takeout <-> delivery: a NOW <TYPE> / WAS <TYPE> band.
	WasType string
	// TypeOnly (ModeTypeChange): only the type changed; Mine are the
	// station's lines, printed plain under the band.
	TypeOnly bool
	// Future (ModeFutureEarly): printed early for a future order - a
	// kitchen ticket says DO NOT MAKE and when it is due, a receipt has a
	// FUTURE bar on top.
	Future bool
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
}
