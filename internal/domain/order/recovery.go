package order

import (
	"context"
	"errors"
	"strings"
	"time"
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
	Prefs           FloorPrefs `json:"prefs"`
	// ReceiptPrefs: nil in reports from agents older than receipt settings.
	ReceiptPrefs      *ReceiptPrefs `json:"receipt_prefs,omitempty"`
	DriverResetTime   string        `json:"driver_reset_time"`
	LastDriverResetAt *time.Time    `json:"last_driver_reset_at,omitempty"`
	AutoAccept        bool          `json:"auto_accept"`
	Drivers           []StateDriver `json:"drivers"`
	OpenShifts        []StateShift  `json:"open_shifts"`
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
	PosNumber      string     `json:"pos_number,omitempty"` // 2.1.0
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
}

// RestoreOrder is one order of the open batch: main's snapshot plus the old
// agent's own status (nil → derived from the snapshot).
type RestoreOrder struct {
	Order        Snapshot `json:"order"`
	AgentStatus  *Status  `json:"agent_status"`
	AgentBatchID *int64   `json:"agent_batch_id,omitempty"`
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
