package order

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// State is the lifecycle of an order inside the agent.
//
//   - StateArrived    — persisted; awaiting acceptance (tablet / web UI / auto-accept).
//   - StateAccepted   — accepted, kitchen number assigned, tickets printed.
//   - StateReady      — food is ready. Terminal for pickup / dine-in.
//   - StateOnDelivery — a driver took it out.
//   - StateDelivered  — the driver came back. Terminal for delivery.
//   - StateCancelled  — never accepted; cancelled by Close batch. Terminal.
type State string

const (
	StateArrived    State = "arrived"
	StateAccepted   State = "accepted"
	StateReady      State = "ready"
	StateOnDelivery State = "on_delivery"
	StateDelivered  State = "delivered"
	StateCancelled  State = "cancelled"
)

func (s State) Valid() bool {
	switch s {
	case StateArrived, StateAccepted, StateReady, StateOnDelivery, StateDelivered, StateCancelled:
		return true
	}
	return false
}

// Status captures the order's lifecycle metadata.
type Status struct {
	State       State      `json:"state"`
	ArrivalDate time.Time  `json:"arrival_date"`
	PrintedDate *time.Time `json:"printed_date,omitempty"`

	// Floor flow (agent-owned; synced to main as floor events).
	BatchID          int64      `json:"batch_id"`
	KitchenNumber    *int64     `json:"kitchen_number,omitempty"`
	PrepMinutes      *int64     `json:"prep_minutes,omitempty"`
	EstimatedReadyAt *time.Time `json:"estimated_ready_at,omitempty"`
	AcceptedAt       *time.Time `json:"accepted_at,omitempty"`
	ReadyAt          *time.Time `json:"ready_at,omitempty"`
	OutAt            *time.Time `json:"out_at,omitempty"`
	DeliveredAt      *time.Time `json:"delivered_at,omitempty"`
	CancelledAt      *time.Time `json:"cancelled_at,omitempty"`
	DriverID         string     `json:"driver_id,omitempty"`
	DriverName       string     `json:"driver_name,omitempty"`
}

// StoredOrder is the repository's view of an order: the canonical Snapshot
// (everything the wire delivered) plus the agent-local lifecycle metadata.
type StoredOrder struct {
	Order  Snapshot `json:"order"`
	Status Status   `json:"status"`
}

// OrderID is a convenience for log/metric labels — handlers prefer the
// canonical UUID over the vendor's source_event_id.
func (so StoredOrder) OrderID() uuid.UUID { return so.Order.MustParseID() }

var ErrNotFound = fmt.Errorf("order not found")
var ErrAlreadyAccepted = fmt.Errorf("order already accepted")

// ErrInvalidState is returned when a floor transition doesn't apply to the
// order's current state (e.g. Ready on an order that isn't accepted, or a
// driver assigned to a non-delivery order).
var ErrInvalidState = fmt.Errorf("order is not in a state that allows this")

// IsDelivery reports whether the order is a delivery (driver flow applies).
func (so StoredOrder) IsDelivery() bool { return so.Order.Fulfillment.Type == "delivery" }

// IsFinished reports whether the order is done on the floor: delivered,
// cancelled, or ready for a non-delivery order.
func (so StoredOrder) IsFinished() bool {
	return so.Status.State == StateDelivered || so.Status.State == StateCancelled ||
		(so.Status.State == StateReady && !so.IsDelivery())
}
