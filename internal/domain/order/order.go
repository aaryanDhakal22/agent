// Package order is the agent's domain model for orders. Everything here
// mirrors main/'s canonical wire shape (internal/transport/sse/envelope.go
// + internal/domain/order/snapshot.go in main). The agent does NOT have an
// anti-corruption layer over main's events — main is the source of truth,
// and we lift the same canonical shape into this codebase so reads and
// writes use the same fields end-to-end.
//
// Naming convention: JSON keys here must match what main/'s Snapshot emits.
// If main adds a field, this file is one of the places that has to grow.
package order

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Snapshot is the wire-and-storage shape of an order inside the agent.
// Identical JSON shape to main/'s order.Snapshot. We persist the full JSON
// body in `orders.snapshot` and rebuild this struct on read.
type Snapshot struct {
	ID            string           `json:"id"`          // UUID
	TenantID      string           `json:"tenant_id"`   // UUID
	LocationID    string           `json:"location_id"` // UUID
	Source        string           `json:"source"`
	SourceEventID string           `json:"source_event_id"`
	Status        string           `json:"status"`
	Currency      string           `json:"currency"`
	Money         Money            `json:"money"`
	Customer      Customer         `json:"customer"`
	Fulfillment   Fulfillment      `json:"fulfillment"`
	Items         []LineItem       `json:"items"`
	Payments      []PaymentSummary `json:"payments,omitempty"`
	Notes         string           `json:"notes,omitempty"`
	PlacedAt      time.Time        `json:"placed_at"`
	ReceivedAt    time.Time        `json:"received_at"`
	SourceMeta    map[string]any   `json:"source_metadata,omitempty"`

	// Floor fields main mirrors back from the agent's floor events (absent on
	// a fresh order). The agent fills KitchenNumber from its own state before
	// printing so it lands on every ticket.
	KitchenNumber    *int64     `json:"kitchen_order_number,omitempty"`
	EstimatedReadyAt *time.Time `json:"estimated_ready_at,omitempty"`
	DriverName       string     `json:"driver_name,omitempty"`

	// TakenBy / DeviceKind (contract §2.10ab): an order the call center
	// placed - "Call center - <Name>" (lists, kitchen tickets, the receipt)
	// and "callcenter" (main's per-agent report). Empty on every other order.
	TakenBy    string `json:"taken_by,omitempty"`
	DeviceKind string `json:"device_kind,omitempty"`
	// OutOfArea (contract §2.10ab): a call-center delivery to an address
	// outside the store's area, placed after the agent acknowledged the
	// warning (the store sees it on the order, the checks list and the
	// delivery ticket). Absent on every other order.
	OutOfArea bool `json:"out_of_area,omitempty"`

	// RemovedItems (terminal orders): lines removed at a terminal after SEND
	// (§2.10d) - struck through, red, REMOVED on Kitchen and in History.
	// Never printed or counted (Items is what the order is now).
	RemovedItems []LineItem `json:"removed_items,omitempty"`

	// MakeAt (printing only, never on the wire): when the kitchen starts a
	// future order - its re-ring time, else promised_at less its lead or
	// prep minutes. The kitchen ticket's band says MAKE AT / READY.
	MakeAt *time.Time `json:"-"`
	// TypeChangedFrom (printing only, never on the wire): a terminal order
	// moved takeout <-> delivery after SEND (contract §2.10d `type`): its
	// change ticket says the new type and this old one.
	TypeChangedFrom string `json:"-"`
	// Voided (printing only, never on the wire): the order is voided /
	// cancelled ("VOID" or "CANCELLED", the receipt's banner; "" = not):
	// a reprint of it prints every line struck through (the owner,
	// 2026-11-12: removed / voided items are struck through).
	Voided string `json:"-"`
}

type Money struct {
	Subtotal  int64      `json:"subtotal_cents"`
	Tax       int64      `json:"tax_cents"`
	Tip       int64      `json:"tip_cents"`
	Fees      []Fee      `json:"fees,omitempty"`
	Discounts []Discount `json:"discounts,omitempty"`
	Total     int64      `json:"total_cents"`
}

type Fee struct {
	Type   string `json:"type"`
	Label  string `json:"label,omitempty"`
	Amount int64  `json:"amount_cents"`
	// DriverCents is the driver's share of a delivery fee (main's split of
	// the store's delivery fee; the store keeps the rest). 0 = no split
	// known (a vendor order, an older main).
	DriverCents int64 `json:"driver_cents,omitempty"`
}

// DeliveryDriverCents is the driver's share of the order's delivery fee(s).
func (m Money) DeliveryDriverCents() int64 {
	var n int64
	for _, f := range m.Fees {
		if f.Type == "delivery" && f.DriverCents > 0 {
			n += min(f.DriverCents, max(f.Amount, 0))
		}
	}
	return n
}

type Discount struct {
	Type   string `json:"type"`
	Code   string `json:"code,omitempty"`
	Label  string `json:"label,omitempty"`
	Amount int64  `json:"amount_cents"`
}

type Customer struct {
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
}

type Fulfillment struct {
	Type       string     `json:"type"` // dine_in | pickup | delivery
	Address    *Address   `json:"address,omitempty"`
	PromisedAt *time.Time `json:"promised_at,omitempty"`
	TableID    string     `json:"table_id,omitempty"`
}

type Address struct {
	Line1      string `json:"line1,omitempty"`
	Line2      string `json:"line2,omitempty"`
	City       string `json:"city,omitempty"`
	State      string `json:"state,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
	Notes      string `json:"notes,omitempty"`
}

type LineItem struct {
	ID        string     `json:"id"`
	Position  int        `json:"position"`
	SKU       string     `json:"sku,omitempty"`
	Name      string     `json:"name"`
	Size      string     `json:"size,omitempty"`
	Quantity  int        `json:"quantity"`
	UnitPrice int64      `json:"unit_price_cents"`
	LineTotal int64      `json:"line_total_cents"`
	Notes     string     `json:"notes,omitempty"`
	Children  []LineItem `json:"children,omitempty"`

	// Half and half (modifier children, main Stage 0b): Portion is "" (the
	// whole pie) | first | second; Mode is add | only | no | specialty ("" on
	// older and vendor orders = add). Name still reads on its own
	// ("Pepperoni (1st half)").
	Portion string `json:"portion,omitempty"`
	Mode    string `json:"mode,omitempty"`

	// ShowOnDriverTicket (modifier children; the Pi stamps it when it stores
	// a counter or channel order, from the counter menu's option flag
	// show_on_driver_ticket): true = a drink / dessert / side the driver must
	// see, false = a choice the delivery receipt leaves off when it is $0
	// ("Well Done", a NO). nil = not known (vendor and older orders, no
	// counter menu): printed as before. HiddenOnDriverTicket reads it.
	ShowOnDriverTicket *bool `json:"show_on_driver_ticket,omitempty"`

	// Kitchen routing stamped by main at ingest (top-level items only):
	// the station printer this line prints on, and how it was matched.
	MenuItemID string `json:"menu_item_id,omitempty"`
	PrinterID  string `json:"printer_id,omitempty"`
	Routing    string `json:"routing,omitempty"` // matched | unmatched | unassigned | none | "" (pre-routing)

	// Coupons (terminal orders, the owner 2026-11-12: a coupon is not a
	// discount, it re-prices its lines): Coupon is the coupon's name on a
	// line sold in one, CouponNo which coupon of the order it is (1 = the
	// first applied; two of the same coupon are told apart), RungCents the
	// cart line's own unit price (its unit_price is the coupon's), which an
	// edit keeps. Top-level lines only; "" / 0 otherwise.
	Coupon    string `json:"coupon,omitempty"`
	CouponNo  int    `json:"coupon_no,omitempty"`
	RungCents int64  `json:"rung_cents,omitempty"`
}

// Portions and modes (LineItem.Portion / Mode).
const (
	PortionFirst  = "first"
	PortionSecond = "second"

	ModeAdd       = "add"
	ModeOnly      = "only"
	ModeNo        = "no"
	ModeSpecialty = "specialty"
)

// HasHalves says the line is a pizza built in halves: a child sits on the
// 1st or 2nd half.
func (it LineItem) HasHalves() bool {
	for _, c := range it.Children {
		if c.Portion == PortionFirst || c.Portion == PortionSecond {
			return true
		}
	}
	return false
}

// HiddenOnDriverTicket says a modifier child stays off a delivery order's
// receipt (the driver's copy): a $0 choice stamped show_on_driver_ticket
// false. Unset or a priced child always prints.
func (it LineItem) HiddenOnDriverTicket() bool {
	return it.UnitPrice == 0 && it.ShowOnDriverTicket != nil && !*it.ShowOnDriverTicket
}

// ShownOnDriverTicket says a child is stamped show_on_driver_ticket true.
func (it LineItem) ShownOnDriverTicket() bool {
	return it.ShowOnDriverTicket != nil && *it.ShowOnDriverTicket
}

// SKUCustom marks a custom line (the counter terminal's Quicc Add: a free
// name and price, no menu item; it prints on every station). main keeps it
// like any SKU.
const SKUCustom = "custom"

// Routing values (LineItem.Routing).
const (
	RoutingMatched    = "matched"
	RoutingUnmatched  = "unmatched"
	RoutingUnassigned = "unassigned"
	// RoutingNone: the menu item is marked "no kitchen ticket" in main
	// (the owner, 2026-11-12): no station ticket of its own and never in
	// "Also on this order"; it still prints on the customer receipt /
	// delivery ticket.
	RoutingNone = "none"
)

// PaymentSummary mirrors main/'s PaymentSummary. method ∈ {card, cash, other}.
type PaymentSummary struct {
	Method      string `json:"method"`
	AmountCents int64  `json:"amount_cents"`
	Last4       string `json:"last4,omitempty"`
	AuthCode    string `json:"auth_code,omitempty"`
}

// SourceCounter is Snapshot.Source of an order placed at one of the store's
// own POS terminals (terminal stage 0d): the Pi makes it, accepted at once.
const SourceCounter = "counter"

// JournalRetention: how long the Pi keeps the terminals' action journal
// (main keeps it all).
const JournalRetention = 30 * 24 * time.Hour

// CashDue is what is collected in cash for the order (at the counter or by
// the driver): the cash payments, or the whole total when the order carries
// no payments at all — that is how the vendor sends a cash order (v1 printed
// "Collect Cash" for exactly that case). Card and other payments (a third
// party, an invoice) are already paid and never count.
func (s Snapshot) CashDue() int64 {
	if s.Source == SourceCounter {
		// A counter order (terminal stage 0d) lists the money already taken
		// at the counter (refunds negative): what is left is still due.
		due := s.Money.Total
		for _, p := range s.Payments {
			due -= p.AmountCents
		}
		return max(due, 0)
	}
	if ob := s.OnlineBase(); ob != nil {
		// An online order edited at a terminal (§2.10aa): what it arrived
		// paid with covers the new total, the rest is due.
		return max(s.Money.Total-ob.PaidCents, 0)
	}
	if len(s.Payments) == 0 {
		return s.Money.Total
	}
	var cash int64
	for _, p := range s.Payments {
		if strings.EqualFold(p.Method, "cash") {
			cash += p.AmountCents
		}
	}
	return cash
}

// IsChannel (2.5.0) reports an order from one of the store's own channels:
// any source other than the vendor's "online" (main sends "web" for the
// website, "counter" for the terminals, later "operator"). An empty source
// is an old online order; "pos" was the old POS's (SoftTouch, gone in H3).
// Channel orders always print station tickets + the receipt.
func (s Snapshot) IsChannel() bool {
	src := strings.ToLower(strings.TrimSpace(s.Source))
	return src != "" && src != SourceOnline && src != "pos"
}

// MustParseID returns the snapshot id as a uuid.UUID. The wire shape uses
// strings; callers that need to put the id in a pgtype.UUID convert through
// this helper. Empty / invalid ids surface as uuid.Nil — callers should
// validate before persisting.
func (s Snapshot) MustParseID() uuid.UUID {
	u, _ := uuid.Parse(s.ID)
	return u
}

func (s Snapshot) MustParseTenantID() uuid.UUID {
	u, _ := uuid.Parse(s.TenantID)
	return u
}

func (s Snapshot) MustParseLocationID() uuid.UUID {
	u, _ := uuid.Parse(s.LocationID)
	return u
}

// String returns a debugging-friendly summary. Avoid in hot paths — it's only
// for log statements that want some shape on the snapshot.
func (s Snapshot) String() string {
	return fmt.Sprintf("Snapshot{id=%s vendor=%s items=%d total_cents=%d}",
		s.ID, s.SourceEventID, len(s.Items), s.Money.Total)
}
