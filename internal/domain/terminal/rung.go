package terminal

// Prices don't move under an order being taken (the owner, 2026-11-12;
// contract/agent.md §2.10u). A cart line may carry the price the terminal
// first got for it from a quote (rung_cents / rung_name / rung_mods); the Pi
// keeps that price when the menu's moved since, and prices a line whose item
// left the menu as an open-price custom line at it. Neither is a manager
// price change.

// Journal actions (Pi only).
const (
	// ActionMenuGoneLine: a line whose item (size, option, half or deal) is
	// no longer on the menu, sold as a custom line at its rung price
	// {line, item_id?, deal_id?, name, price_cents, qty, reason, from}.
	ActionMenuGoneLine = "menu_gone_line"
	// ActionPriceKept: a line the terminal rang at a price the menu has
	// moved from since, kept at it {line, item_id, name, menu_cents,
	// rung_cents, qty}. Not a price change: no manager, no limit.
	ActionPriceKept = "price_kept"
	// ActionSessionEnded: a staff session the Pi ended because main's
	// terminal config took the person off, switched them off or changed
	// their PIN {reason, device_name?}.
	ActionSessionEnded = "session.ended"
)

// Where a kept price came from.
const (
	RungFromTerminal = "terminal" // the cart line's rung_cents
	RungFromOrder    = "order"    // the sent order's own line (M-15)
)

// GoneLine is a cart line priced as a custom line because its menu item is
// gone (quote menu_gone[]).
type GoneLine struct {
	Line       int    `json:"line"`
	ItemID     string `json:"item_id,omitempty"`
	DealID     string `json:"deal_id,omitempty"`
	Name       string `json:"name"`
	PriceCents int64  `json:"price_cents"` // per unit
	Qty        int    `json:"qty"`
	// Reason: the engine's issue code (unknown_item | unknown_size |
	// unknown_option | deal_ended).
	Reason string `json:"reason"`
	From   string `json:"from"` // terminal | order
}

// KeptPrice is a cart line priced at its rung price instead of today's
// (quote kept_prices[]).
type KeptPrice struct {
	Line      int    `json:"line"`
	ItemID    string `json:"item_id"`
	Name      string `json:"name"`
	MenuCents int64  `json:"menu_cents"` // today's unit price
	RungCents int64  `json:"rung_cents"` // the unit price kept
	From      string `json:"from"`       // terminal | order
}

// Why a staff session ended (SSE terminal.session.ended).
const (
	SessionRemoved     = "removed"     // no longer in main's staff list
	SessionDeactivated = "deactivated" // switched off in main
	SessionPinChanged  = "pin_changed" // a new PIN: log in with it
)
