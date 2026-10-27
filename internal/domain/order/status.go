package order

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// State is the lifecycle of an order inside the agent.
//
//   - StateArrived    — persisted; awaiting acceptance (tablet / web UI / auto-accept).
//   - StateScheduled  — a future order confirmed once (lead time picked): no
//     print, out of Deliveries; rings again (back to arrived) at ring_at.
//   - StateAccepted   — accepted, kitchen number assigned, tickets printed.
//   - StateReady      — food is ready. Terminal for pickup / dine-in.
//   - StateOnDelivery — a driver took it out.
//   - StateDelivered  — the driver came back. Terminal for delivery.
//   - StateCancelled  — never accepted; cancelled by Close batch. Terminal.
type State string

const (
	StateArrived    State = "arrived"
	StateScheduled  State = "scheduled"
	StateAccepted   State = "accepted"
	StateReady      State = "ready"
	StateOnDelivery State = "on_delivery"
	StateDelivered  State = "delivered"
	StateCancelled  State = "cancelled"
)

func (s State) Valid() bool {
	switch s {
	case StateArrived, StateScheduled, StateAccepted, StateReady, StateOnDelivery, StateDelivered, StateCancelled:
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
	// PoolDriverID: the available driver whose pool holds this delivery
	// (accepted or ready) until Send Out; empty = unassigned.
	PoolDriverID string     `json:"pool_driver_id,omitempty"`
	PooledAt     *time.Time `json:"pooled_at,omitempty"`
	// PoolSeq is the stop number (1..n) inside the pool: the AUTO plan's
	// driving order on confirm, else the fastest order from the drive times.
	PoolSeq *int64 `json:"pool_seq,omitempty"`
	// PoolPlanID: the AUTO plan whose confirm pooled this order (empty when
	// pooled by hand); a plain Send Out logs the run against it.
	PoolPlanID string `json:"pool_plan_id,omitempty"`

	// POS orders (2.1.0): pulled from the store's old POS by the store hub.
	// ExtSource "softtouch", ExtRef its check number, DisplayNumber the check
	// number staff call (shown in place of the kitchen number).
	ExtSource     string `json:"ext_source,omitempty"`
	ExtRef        string `json:"ext_ref,omitempty"`
	DisplayNumber *int64 `json:"display_number,omitempty"`
	// Paid: settled at the POS terminal (a POS order, or an online cash pickup
	// the hub inserted). CashDueCents: what is collected in cash (POS: the
	// hub's balance less a cash tip; online: Snapshot.CashDue). TipCents: the
	// tip on a POS check.
	Paid         bool  `json:"paid,omitempty"`
	CashDueCents int64 `json:"cash_due_cents,omitempty"`
	TipCents     int64 `json:"tip_cents,omitempty"`
	// BoardHidden: a POS counter check with no kitchen item (not on Kitchen).
	BoardHidden bool `json:"board_hidden,omitempty"`
	// SentAt: when the cashier pressed Send (prep + the AUTO clock count from it).
	SentAt *time.Time `json:"sent_at,omitempty"`
	// LocalOnly: never synced to main (every POS order).
	LocalOnly bool  `json:"local_only,omitempty"`
	Flag      *Flag `json:"flag,omitempty"`
	// PosCheckNumber: the POS check number of an online order the hub inserted.
	PosCheckNumber *int64 `json:"pos_check_number,omitempty"`
	// PosDriverNote: a driver the POS set that couldn't be mirrored.
	PosDriverNote string `json:"pos_driver_note,omitempty"`

	// Future orders (2.1.0). Future is computed on read (features.future_orders
	// on and promised_at after now + the default time); a scheduled order
	// rings again at RingAt (promised_at - lead) with Rerung set.
	Future               bool       `json:"future,omitempty"`
	ScheduledLeadMinutes *int64     `json:"scheduled_lead_minutes,omitempty"`
	RingAt               *time.Time `json:"ring_at,omitempty"`
	Rerung               bool       `json:"rerung,omitempty"`

	// AutoAccepted (2.2.0): accepted by auto-accept, not by staff (a future
	// order auto-accepted at its re-ring too). The tablet shows a banner.
	AutoAccepted bool `json:"auto_accepted,omitempty"`

	// Not delivered (2.2.1): the driver came back with the food. CameBack
	// holds a retried order aside (ready, no driver, no pool) until staff
	// release it; NotDelivered is the last such action (kept after release
	// and on a cancelled order).
	CameBack     bool          `json:"came_back,omitempty"`
	NotDelivered *NotDelivered `json:"not_delivered,omitempty"`
	// RunSeq is a sent order's stop number on its driver's run (1..n, the
	// run's stop order; a stop joining the run goes last). Set while
	// on_delivery and kept once delivered.
	RunSeq *int64 `json:"run_seq,omitempty"`

	// Finishing (00020): a ready pickup / counter order that is done with
	// (picked up). FinishedBy "pos" = the POS closed the check, "auto" = an
	// online pickup sat ready for the store's finish time. A finished order
	// no longer counts as open (a forced Pi update can run). PosClosed: the
	// POS closed the check (one still cooking finishes when marked ready).
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	FinishedBy string     `json:"finished_by,omitempty"`
	PosClosed  bool       `json:"pos_closed,omitempty"`
	// PosChange: the terminal changed a check already on the tablet and
	// nobody pressed OK yet (the tablet rings until they do).
	PosChange *PosChange `json:"pos_change,omitempty"`
}

// Finished-by values (Status.FinishedBy).
const (
	FinishedByPOS  = "pos"
	FinishedByAuto = "auto"
)

// Not-delivered actions (POST /api/orders/{id}/not-delivered).
const (
	NotDeliveredRetry  = "retry"
	NotDeliveredCancel = "cancel"
)

// NotDelivered records a delivery that came back: the action staff chose,
// when, and the driver it came back from.
type NotDelivered struct {
	Action   string    `json:"action"` // retry | cancel
	At       time.Time `json:"at"`
	DriverID string    `json:"driver_id"`
}

// Flag is a note the hub puts on an order ("Settle manually: unmapped item X").
type Flag struct {
	Text     string `json:"text"`
	Severity string `json:"severity"` // info | warn | serious
}

// StoredOrder is the repository's view of an order: the canonical Snapshot
// (everything the wire delivered) plus the agent-local lifecycle metadata.
type StoredOrder struct {
	Order  Snapshot `json:"order"`
	Status Status   `json:"status"`
	// Geo is where main located a delivery address (nil until it did).
	Geo *OrderGeo `json:"geo,omitempty"`
}

// OrderID is a convenience for log/metric labels — handlers prefer the
// canonical UUID over the vendor's source_event_id.
func (so StoredOrder) OrderID() uuid.UUID { return so.Order.MustParseID() }

var ErrNotFound = fmt.Errorf("order not found")
var ErrAlreadyAccepted = fmt.Errorf("order already accepted")

// Not delivered (2.2.1): the order is not out with a driver or delivered in
// the open batch (or not a delivery); release on an order that isn't held as
// came back; a came-back order can't be pooled or sent until released.
var (
	ErrNotOutOrDelivered = fmt.Errorf("order is not out with a driver or delivered in the open batch")
	ErrNotCameBack       = fmt.Errorf("order is not in the came-back list")
	ErrCameBack          = fmt.Errorf("order came back with its driver: release it first")
)

// ErrInvalidState is returned when a floor transition doesn't apply to the
// order's current state (e.g. Ready on an order that isn't accepted, or a
// driver assigned to a non-delivery order).
var ErrInvalidState = fmt.Errorf("order is not in a state that allows this")

// IsPOS reports whether the order was pulled from the store's POS.
func (so StoredOrder) IsPOS() bool { return so.Status.ExtSource != "" }

// Number is the number staff call: the POS check number, else the kitchen
// number (nil before accept).
func (so StoredOrder) Number() *int64 {
	if so.Status.DisplayNumber != nil {
		return so.Status.DisplayNumber
	}
	return so.Status.KitchenNumber
}

// IsDelivery reports whether the order is a delivery (driver flow applies).
func (so StoredOrder) IsDelivery() bool { return so.Order.Fulfillment.Type == "delivery" }

// IsFinished reports whether the order is done on the floor: delivered,
// cancelled, or ready for a non-delivery order.
func (so StoredOrder) IsFinished() bool {
	return so.Status.State == StateDelivered || so.Status.State == StateCancelled ||
		(so.Status.State == StateReady && !so.IsDelivery())
}
