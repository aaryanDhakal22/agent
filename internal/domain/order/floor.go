package order

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Batch is a shift's worth of orders. Exactly one batch is open; "Close
// batch" closes it and opens the next (resetting kitchen numbers to 1100).
type Batch struct {
	ID         int64      `json:"id"`
	OpenedAt   time.Time  `json:"opened_at"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
	OrderCount int64      `json:"order_count"`
}

// Driver statuses: off = clocked out (not in the queue), available = clocked
// in and in the queue, out = on a delivery run.
const (
	DriverOff       = "off"
	DriverAvailable = "available"
	DriverOut       = "out"
)

// Driver is a delivery driver in the agent's roster. Position orders the
// queue (lowest = next up). Deliveries counts this batch's runs.
type Driver struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Position       int64      `json:"position"`
	Status         string     `json:"status"`
	LastReturnedAt *time.Time `json:"last_returned_at,omitempty"`
	Deliveries     int64      `json:"deliveries"`
	// ClockedInAt is the start of the driver's open shift (nil when off).
	ClockedInAt *time.Time `json:"clocked_in_at,omitempty"`
}

// Shift is one clock-in → clock-out period of a driver — a record for reports
// (not wages). AutoOut marks a shift closed by the daily reset.
type Shift struct {
	ID         string     `json:"id"`
	DriverID   string     `json:"driver_id"`
	DriverName string     `json:"driver_name"`
	BatchID    int64      `json:"batch_id"`
	ClockIn    time.Time  `json:"clock_in"`
	ClockOut   *time.Time `json:"clock_out,omitempty"`
	AutoOut    bool       `json:"auto_out"`
}

// Floor event types (floor_events.type → the `type` field main receives).
const (
	FloorEventOrderStatus   = "order.status"
	FloorEventDriversRoster = "drivers.roster"
	FloorEventDriverShift   = "driver.shift"
)

// FloorEvent is one change to sync to main. Payload is the complete JSON sent
// (it carries its own event_id + type + at, so main can dedupe on event_id).
type FloorEvent struct {
	ID       string          `json:"event_id"`
	OrderID  string          `json:"order_id,omitempty"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	At       time.Time       `json:"at"`
	Attempts int64           `json:"attempts"`
}

// OrderStatusEvent is the payload of an order.status floor event.
type OrderStatusEvent struct {
	EventID          string     `json:"event_id"`
	Type             string     `json:"type"` // order.status
	OrderID          string     `json:"order_id"`
	Status           State      `json:"status"`
	KitchenNumber    *int64     `json:"kitchen_number,omitempty"`
	PrepMinutes      *int64     `json:"prep_minutes,omitempty"`
	EstimatedReadyAt *time.Time `json:"estimated_ready_at,omitempty"`
	DriverName       string     `json:"driver_name,omitempty"`
	Auto             bool       `json:"auto,omitempty"` // set by Close batch's auto-finish / auto-cancel
	At               time.Time  `json:"at"`
}

// DriversRosterEvent is the payload of a drivers.roster floor event (the whole
// active roster, so main can replace its copy).
type DriversRosterEvent struct {
	EventID string         `json:"event_id"`
	Type    string         `json:"type"` // drivers.roster
	Drivers []RosterDriver `json:"drivers"`
	At      time.Time      `json:"at"`
}

// RosterDriver is one driver as synced to main.
type RosterDriver struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Position int64  `json:"position"`
	Status   string `json:"status"`
}

// DriverShiftEvent is the payload of a driver.shift floor event (sent on clock
// in and again on clock out, keyed by shift_id).
type DriverShiftEvent struct {
	EventID    string     `json:"event_id"`
	Type       string     `json:"type"` // driver.shift
	ShiftID    string     `json:"shift_id"`
	DriverID   string     `json:"driver_id"`
	DriverName string     `json:"driver_name"`
	ClockIn    time.Time  `json:"clock_in"`
	ClockOut   *time.Time `json:"clock_out,omitempty"`
	Auto       bool       `json:"auto,omitempty"`
	At         time.Time  `json:"at"`
}

// DriverReset is the daily auto clock-out setting.
type DriverReset struct {
	Time    string     `json:"time"` // location-local "HH:MM"
	LastRun *time.Time `json:"last_run,omitempty"`
}

// LeftoversError is returned by CloseBatch (without force) when the open batch
// still has unfinished orders; staff confirm, then close with force.
type LeftoversError struct{ Orders []StoredOrder }

func (e *LeftoversError) Error() string {
	return fmt.Sprintf("batch has %d unfinished order(s)", len(e.Orders))
}

var (
	ErrDriverNotFound = fmt.Errorf("driver not found")
	// ErrDriverNotAvailable: the driver isn't clocked in and in the queue
	// (off, or already out on a run).
	ErrDriverNotAvailable = fmt.Errorf("driver is not clocked in and available")
	ErrNoOrders           = fmt.Errorf("no orders given")
)

// FloorSettings are the floor counters shown to staff.
type FloorSettings struct {
	KitchenNext     int64 `json:"kitchen_next"`
	LastPrepMinutes int64 `json:"last_prep_minutes"`
}

// FloorRepository persists floor transitions. Every mutating method writes the
// matching floor event(s) in the same transaction.
type FloorRepository interface {
	MarkReady(ctx context.Context, id string) (*StoredOrder, error)
	AssignDriver(ctx context.Context, driverID string, orderIDs []string) ([]StoredOrder, error)
	DriverReturned(ctx context.Context, driverID string) ([]StoredOrder, error)
	// CloseBatch closes the open batch and opens the next. Without force it
	// returns *LeftoversError if unfinished orders remain; with force they are
	// auto-finished (events flagged auto): accepted-or-later orders → ready /
	// delivered, never-accepted ones → cancelled. Returns the closed batch.
	CloseBatch(ctx context.Context, force bool) (*Batch, []StoredOrder, error)

	OpenBatch(ctx context.Context) (*Batch, error)
	ListBatches(ctx context.Context, limit int) ([]Batch, error)
	ListByBatch(ctx context.Context, batchID int64, offset, limit int) ([]StoredOrder, error)
	FloorSettings(ctx context.Context) (FloorSettings, error)

	ListDrivers(ctx context.Context) ([]Driver, error)
	CreateDriver(ctx context.Context, name string) (*Driver, error)
	RenameDriver(ctx context.Context, id, name string) error
	RemoveDriver(ctx context.Context, id string) error
	ReorderDrivers(ctx context.Context, ids []string) error
	// ClockIn puts an off driver into the queue (back) and opens a shift;
	// ClockOut takes an available driver out of the queue and closes it
	// (ErrInvalidState while the driver is out on a run).
	ClockIn(ctx context.Context, id string) (*Driver, error)
	ClockOut(ctx context.Context, id string) (*Driver, error)
	// AutoClockOut is the daily reset: clocks out every available driver
	// (drivers out on a run are clocked out when they return) and records the
	// run time. Returns how many were clocked out.
	AutoClockOut(ctx context.Context, at time.Time) (int, error)
	GetDriverReset(ctx context.Context) (DriverReset, error)
	SetDriverResetTime(ctx context.Context, hhmm string) error
	// MarkDriverReset records `at` as the last reset run without clocking
	// anyone out (first start, or after the time setting changes).
	MarkDriverReset(ctx context.Context, at time.Time) error
	ListShifts(ctx context.Context, since time.Time) ([]Shift, error)

	// Escalation: arrived orders waiting since before cutoff and not alerted
	// since cutoff; MarkAlerted stamps last_alert_at.
	ListOverdueArrived(ctx context.Context, cutoff time.Time) ([]StoredOrder, error)
	MarkAlerted(ctx context.Context, id string, at time.Time) error

	// RecordAccepted writes the order.status=accepted floor event (after the
	// tickets printed — a reverted claim never reaches main).
	RecordAccepted(ctx context.Context, so StoredOrder) error

	// Sync outbox.
	ListUnsyncedEvents(ctx context.Context, limit int) ([]FloorEvent, error)
	MarkEventSynced(ctx context.Context, id string, at time.Time) error
	MarkEventFailed(ctx context.Context, id, errMsg string) error
	CountUnsyncedEvents(ctx context.Context) (int64, error)

	// Retention: keep the open batch + the newest keepClosed closed batches;
	// drop synced floor events older than syncedBefore.
	TrimBatches(ctx context.Context, keepClosed int, syncedBefore time.Time) (int64, error)
}
