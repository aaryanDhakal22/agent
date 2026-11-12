package order

import (
	"context"
	"errors"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/printroute"
)

// AgentState is the agent's whole floor state, reported to main on every
// change (RPC `agent.state`) so a replacement Pi can continue the shift:
// the open batch, the kitchen counter, the settings, and the drivers with
// their queue and open shifts.
type AgentState struct {
	Version         int        `json:"version"`
	Batch           StateBatch `json:"batch"`
	KitchenNext     int64      `json:"kitchen_next"`
	LastPrepMinutes int64      `json:"last_prep_minutes"`
	// CounterNext (terminal stage 0d): the counter sequence's next number,
	// 0 = start again at the terminal config's counter_start.
	CounterNext int64      `json:"counter_next"`
	Prefs       FloorPrefs `json:"prefs"`
	// ReceiptPrefs: nil in reports from agents older than receipt settings.
	ReceiptPrefs *ReceiptPrefs `json:"receipt_prefs,omitempty"`
	// PrintRouting: the routing + emergency reroute setting (printroute);
	// nil in reports from agents older than it (a restore keeps mode none).
	PrintRouting      *printroute.Routing `json:"print_routing,omitempty"`
	DriverResetTime   string              `json:"driver_reset_time"`
	LastDriverResetAt *time.Time          `json:"last_driver_reset_at,omitempty"`
	AutoAccept        bool                `json:"auto_accept"`
	Drivers           []StateDriver       `json:"drivers"`
	OpenShifts        []StateShift        `json:"open_shifts"`
}

type StateBatch struct {
	ID       int64     `json:"id"`
	OpenedAt time.Time `json:"opened_at"`
}

type StateDriver struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Position       int64      `json:"position"`
	Status         string     `json:"status"`
	LastReturnedAt *time.Time `json:"last_returned_at,omitempty"`
}

type StateShift struct {
	ID         string    `json:"id"`
	DriverID   string    `json:"driver_id"`
	DriverName string    `json:"driver_name"`
	BatchID    int64     `json:"batch_id"`
	ClockIn    time.Time `json:"clock_in"`
}

// RestoreData is main's GET /store/v1/agent/restore answer.
type RestoreData struct {
	State   *AgentState    `json:"state"`
	Orders  []RestoreOrder `json:"orders"`
	Drivers []MirrorDriver `json:"drivers"`
	// Drawers (HARDENING F-9, contract 2.10s): the open batch's drawer
	// sessions main stored from drawer.* events (an older main sends none).
	Drawers []RestoreDrawer `json:"drawers,omitempty"`
}

// RestoreOrder is one order of the open batch: main's snapshot plus the old
// agent's own status (nil → derived from the snapshot).
type RestoreOrder struct {
	Order        Snapshot `json:"order"`
	AgentStatus  *Status  `json:"agent_status"`
	AgentBatchID *int64   `json:"agent_batch_id,omitempty"`
	// Payments (F-9): a counter / terminal-paid order's payments as main
	// stored them from order.payment (a refund negative). Without them the
	// Pi rebuilds them from the snapshot's payments (no tips, no drawer).
	Payments []RestorePayment `json:"payments,omitempty"`
}

// RestorePayment is main's Payment of an order.payment, back to a new Pi.
type RestorePayment struct {
	ID          string    `json:"id"`
	Method      string    `json:"method"`
	AmountCents int64     `json:"amount_cents"` // negative = a refund
	TipCents    int64     `json:"tip_cents"`
	Last4       string    `json:"last4"`
	Ref         string    `json:"ref"`
	At          time.Time `json:"at"`
	StaffID     string    `json:"staff_id"`
	DeviceID    string    `json:"device_id"`
	DrawerID    string    `json:"drawer_id"`
	Brand       string    `json:"brand"`
	RefundKind  string    `json:"refund_kind"`
	Reason      string    `json:"reason"`
	ApprovedBy  string    `json:"approved_by"`
}

// RestoreDrawer is an open drawer session back to a new Pi (F-9).
type RestoreDrawer struct {
	ID           string    `json:"id"`
	DeviceID     string    `json:"device_id"`
	DeviceName   string    `json:"device_name"`
	StaffID      string    `json:"staff_id"`
	StaffName    string    `json:"staff_name"`
	BatchID      int64     `json:"batch_id"`
	OpenedAt     time.Time `json:"opened_at"`
	StartCents   int64     `json:"start_cents"`
	HandoverFrom string    `json:"handover_from"`
	Auto         bool      `json:"auto"`
	Closed       bool      `json:"closed"` // a closed one isn't restored
}

// MirrorDriver is main's mirrored roster entry (fallback when there's no state).
type MirrorDriver struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int64  `json:"position"`
	Status   string `json:"status"`
}

// RestoreResult summarizes a restore.
type RestoreResult struct {
	BatchID     int64 `json:"batch_id"`
	Orders      int   `json:"orders"`
	Waiting     int   `json:"waiting"` // still to be accepted (they ring)
	Drivers     int   `json:"drivers"`
	Shifts      int   `json:"shifts"`
	KitchenNext int64 `json:"kitchen_next"`
	CounterNext int64 `json:"counter_next"`
	// Payments / Drawers restored (F-9); RebuiltPayments: payments rebuilt
	// from a snapshot because main sent none for the order.
	Payments        int `json:"payments"`
	RebuiltPayments int `json:"rebuilt_payments"`
	Drawers         int `json:"drawers"`
}

// ErrNotEmpty: restore only runs on a blank agent.
var ErrNotEmpty = errors.New("this agent already has orders or drivers — restore only runs on a blank agent")

// RecoveryRepository builds the state report and restores a blank agent.
type RecoveryRepository interface {
	AgentState(ctx context.Context) (AgentState, error)
	Restore(ctx context.Context, data RestoreData) (RestoreResult, error)
}

// StatusFromSnapshot derives an agent status for an order the old agent never
// reported one for (or from before agents did), from main's mirror.
func StatusFromSnapshot(s Snapshot) Status {
	st := Status{ArrivalDate: s.ReceivedAt, KitchenNumber: s.KitchenNumber,
		EstimatedReadyAt: s.EstimatedReadyAt, DriverName: s.DriverName}
	switch strings.ToLower(s.Status) {
	case "accepted", "preparing":
		st.State = StateAccepted
	case "ready":
		st.State = StateReady
	case "on_delivery":
		st.State = StateOnDelivery
	case "completed":
		st.State = StateDelivered
	case "cancelled", "rejected":
		st.State = StateCancelled
	default:
		st.State = StateArrived
	}
	return st
}
