package order

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Driver pools (2.0.9): delivery orders (accepted or ready) parked with an
// available driver until "Send Out". A pool lives in orders.pool_driver_id;
// every change is an order.status floor event with the unchanged state (main
// keeps the agent_status, so a replacement Pi restores pools too).

// FloorEventDeliveryRun is the floor event written when a driver leaves with
// a run (send-out or the old assign): the suggestion vs what was sent, for
// tuning AUTO later.
const FloorEventDeliveryRun = "delivery.run"

// ErrNotReady: Send Out without mark_ready while pooled orders still cook.
type ErrNotReady struct{ IDs []string }

func (e *ErrNotReady) Error() string {
	return fmt.Sprintf("%d pooled order(s) not ready: %s", len(e.IDs), strings.Join(e.IDs, ", "))
}

// ErrPoolEmpty: Send Out with nothing in the pool (a bad request).
var ErrPoolEmpty = fmt.Errorf("pool is empty: %w", ErrNoOrders)

// ErrPlanNotFound: an AUTO plan id this Pi never made (or already trimmed).
var ErrPlanNotFound = fmt.Errorf("plan not found: %w", ErrNotFound)

// RunMeta is what Send Out knows about the run beyond the orders: the AUTO
// plan it came from (if any), the ids AUTO suggested for this driver, and the
// driving order to estimate stop times in (ids not listed go last).
type RunMeta struct {
	PlanID       string
	SuggestedIDs []string
	StopOrder    []string
}

// DeliveryRunEvent is the payload of a delivery.run floor event.
type DeliveryRunEvent struct {
	EventID      string    `json:"event_id"`
	Type         string    `json:"type"` // delivery.run
	At           time.Time `json:"at"`
	BatchID      int64     `json:"batch_id"`
	DriverID     string    `json:"driver_id"`
	DriverName   string    `json:"driver_name"`
	PlanID       string    `json:"plan_id"`
	SuggestedIDs []string  `json:"suggested_ids"`
	SentIDs      []string  `json:"sent_ids"`
	Stops        []RunStop `json:"stops"`
}

// RunStop is one stop of a sent run with its estimated arrival (nil when the
// drive times aren't known).
type RunStop struct {
	OrderID string     `json:"order_id"`
	ETA     *time.Time `json:"eta,omitempty"`
}

// PoolSkip is an AUTO suggestion that could not be pooled at confirm time.
type PoolSkip struct {
	OrderID string `json:"order_id"`
	Reason  string `json:"reason"`
}

// Confirm skip reasons.
const (
	SkipNotFound           = "not_found"
	SkipInvalidState       = "invalid_state"
	SkipAlreadyPooled      = "already_pooled"
	SkipDriverNotAvailable = "driver_not_available"
)

// PlanRun is one driver's suggested additions from a stored AUTO plan.
type PlanRun struct {
	DriverID     string
	SuggestedIDs []string
}

// DeliveryPrefs are the AUTO settings (Settings > Orders).
type DeliveryPrefs struct {
	// SoonReadyMinutes: a cooking delivery estimated ready within this many
	// minutes is planned by AUTO as if ready.
	SoonReadyMinutes int64 `json:"soon_ready_minutes"`
	// DeliveryTargetMinutes: placed-to-door target; AUTO treats the last 10
	// minutes before it as urgent and anything past it as late.
	DeliveryTargetMinutes int64 `json:"delivery_target_minutes"`
}

// DefaultDeliveryPrefs are the migration defaults.
var DefaultDeliveryPrefs = DeliveryPrefs{SoonReadyMinutes: 5, DeliveryTargetMinutes: 45}

// Validate checks the ranges the contract allows.
func (p DeliveryPrefs) Validate() error {
	if p.SoonReadyMinutes < 0 || p.SoonReadyMinutes > 30 {
		return fmt.Errorf("soon_ready_minutes must be 0-30")
	}
	if p.DeliveryTargetMinutes < 15 || p.DeliveryTargetMinutes > 120 {
		return fmt.Errorf("delivery_target_minutes must be 15-120")
	}
	return nil
}

// PoolRepository persists pools, geo and AUTO plans (part of the order
// repository; every pool change writes its floor events in the same tx).
type PoolRepository interface {
	// PoolOrders puts delivery orders (accepted|ready) into an available
	// driver's pool, or back to unassigned (driverID ""). All or nothing:
	// ErrNotFound, ErrDriverNotFound, ErrDriverNotAvailable, ErrInvalidState.
	PoolOrders(ctx context.Context, ids []string, driverID string) ([]StoredOrder, error)
	// SendOut pools include to the driver, then sends the whole pool out
	// (marking cooking orders ready first when markReady, else *ErrNotReady),
	// like AssignDriver, and writes the delivery.run event. Returns the sent
	// orders and the ones it marked ready.
	SendOut(ctx context.Context, driverID string, include []string, markReady bool, meta RunMeta) (sent, readied []StoredOrder, err error)
	// ListOpenDeliveries: the open batch's accepted/ready/on_delivery deliveries.
	ListOpenDeliveries(ctx context.Context) ([]StoredOrder, error)

	// UpsertGeo stores main's order.geo (ErrNotFound for an unknown order)
	// and the drive times in both directions; returns the order with geo.
	UpsertGeo(ctx context.Context, g GeoUpdate) (*StoredOrder, error)
	// DriveTimes returns the known drive times between the given orders.
	DriveTimes(ctx context.Context, ids []string) (map[string]map[string]int64, error)

	SaveAutoPlan(ctx context.Context, id string, at time.Time, planJSON []byte) error
	GetAutoPlan(ctx context.Context, id string) ([]byte, error)
	// ConfirmAutoPlan pools each still-valid suggestion (unassigned,
	// accepted|ready, driver available) and records confirmed_at.
	ConfirmAutoPlan(ctx context.Context, id string, runs []PlanRun, at time.Time) (applied []StoredOrder, skipped []PoolSkip, err error)

	DeliveryPrefs(ctx context.Context) (DeliveryPrefs, error)
	SetDeliveryPrefs(ctx context.Context, p DeliveryPrefs) error
}

// StopServiceS is the time a driver spends at each door (estimates only;
// dispatch.ServiceS is the optimizer's copy).
const StopServiceS = 120

// EstimateStops orders a sent run's ids (stopOrder first, in that order; the
// rest after, as given) and estimates each arrival leaving at depart: store ->
// first stop, then stop -> stop, StopServiceS at each door. storeTo[id] and
// pairs[a][b] are drive seconds; after an unknown leg the ETAs are nil.
func EstimateStops(ids, stopOrder []string, depart time.Time, storeTo map[string]int64, pairs map[string]map[string]int64) []RunStop {
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	seq := make([]string, 0, len(ids))
	used := map[string]bool{}
	for _, id := range stopOrder {
		if in[id] && !used[id] {
			seq, used[id] = append(seq, id), true
		}
	}
	for _, id := range ids {
		if !used[id] {
			seq, used[id] = append(seq, id), true
		}
	}
	out := make([]RunStop, 0, len(seq))
	at, known := depart, true
	for i, id := range seq {
		var leg int64
		var ok bool
		if i == 0 {
			leg, ok = storeTo[id]
		} else {
			leg, ok = pairs[seq[i-1]][id]
			at = at.Add(StopServiceS * time.Second)
		}
		known = known && ok
		stop := RunStop{OrderID: id}
		if known {
			at = at.Add(time.Duration(leg) * time.Second)
			eta := at.UTC().Truncate(time.Second)
			stop.ETA = &eta
		}
		out = append(out, stop)
	}
	return out
}
