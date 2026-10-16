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

	// Kitchen routing stamped by main at ingest (top-level items only):
	// the station printer this line prints on, and how it was matched.
	MenuItemID string `json:"menu_item_id,omitempty"`
	PrinterID  string `json:"printer_id,omitempty"`
	Routing    string `json:"routing,omitempty"` // matched | unmatched | unassigned | "" (pre-routing)
}

// Routing values (LineItem.Routing).
const (
	RoutingMatched    = "matched"
	RoutingUnmatched  = "unmatched"
	RoutingUnassigned = "unassigned"
)

// PaymentSummary mirrors main/'s PaymentSummary. method ∈ {card, cash, other}.
type PaymentSummary struct {
	Method      string `json:"method"`
	AmountCents int64  `json:"amount_cents"`
	Last4       string `json:"last4,omitempty"`
	AuthCode    string `json:"auth_code,omitempty"`
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
