package order

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	// The agent's whole status + batch, stored by main so a replacement Pi
	// (swapped SD card) can restore the open batch exactly.
	BatchID     int64   `json:"batch_id,omitempty"`
	AgentStatus *Status `json:"agent_status,omitempty"`
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

// FloorPrefs are store settings edited in the agent web UI.
type FloorPrefs struct {
	// GreenTillMinutes: a kitchen ticket stays green this long after the
	// order arrived, then shades to fully red at twice that.
	GreenTillMinutes int64 `json:"green_till_minutes"`
	// TipTaxBps is taken off drivers' tips on the driver report (600 = 6%).
	TipTaxBps int64 `json:"tip_tax_bps"`
}

// DefaultFloorPrefs are the migration defaults.
var DefaultFloorPrefs = FloorPrefs{GreenTillMinutes: 20, TipTaxBps: 600}

// UndoReturnWindow is how long a driver's return can be undone.
const UndoReturnWindow = 10 * time.Minute

// DriverReport is one driver's slip for a batch: every delivery they took and
// what they owe. Cash = what the driver collected on cash orders (card orders
// are paid to the store and never count). The driver keeps all tips (cash and
// card) minus the tip tax, so Owed = Cash - TipAfterTax; negative means the
// store owes the driver. Delivery fees are paid weekly — only the count shows.
type DriverReport struct {
	BatchID          int64              `json:"batch_id"`
	BatchOpenedAt    time.Time          `json:"batch_opened_at"`
	BatchClosedAt    *time.Time         `json:"batch_closed_at,omitempty"`
	DriverID         string             `json:"driver_id"`
	DriverName       string             `json:"driver_name"`
	Lines            []DriverReportLine `json:"lines"`
	Deliveries       int                `json:"deliveries"`
	CashCents        int64              `json:"cash_cents"`
	TipCents         int64              `json:"tip_cents"`
	TipTaxBps        int64              `json:"tip_tax_bps"`
	TipAfterTaxCents int64              `json:"tip_after_tax_cents"`
	OwedCents        int64              `json:"owed_cents"`
}

// DriverReportLine is one delivery on a driver report.
type DriverReportLine struct {
	OrderID       string     `json:"order_id"`
	KitchenNumber *int64     `json:"kitchen_number,omitempty"`
	Customer      string     `json:"customer"` // "Erik D."
	Address       string     `json:"address"`
	TipCents      int64      `json:"tip_cents"`
	Cash          bool       `json:"cash"`
	CashCents     int64      `json:"cash_cents"` // collected in cash (0 for card)
	TotalCents    int64      `json:"total_cents"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
}

// NewDriverReportLine builds a report line from a delivered order.
func NewDriverReportLine(so StoredOrder) DriverReportLine {
	snap := so.Order
	l := DriverReportLine{
		OrderID:       snap.ID,
		KitchenNumber: so.Status.KitchenNumber,
		Customer:      ShortCustomerName(snap.Customer.DisplayName),
		TipCents:      snap.Money.Tip,
		TotalCents:    snap.Money.Total,
		DeliveredAt:   so.Status.DeliveredAt,
	}
	if a := snap.Fulfillment.Address; a != nil {
		l.Address = strings.TrimSpace(a.Line1)
		if l2 := strings.TrimSpace(a.Line2); l2 != "" {
			l.Address += ", " + l2
		}
	}
	for _, p := range snap.Payments {
		if strings.EqualFold(p.Method, "cash") {
			l.Cash = true
			l.CashCents += p.AmountCents
		}
	}
	return l
}

// Finish fills the report's totals with the store's tip tax.
func (r *DriverReport) Finish(tipTaxBps int64) {
	r.Deliveries = len(r.Lines)
	r.CashCents, r.TipCents = 0, 0
	for _, l := range r.Lines {
		r.CashCents += l.CashCents
		r.TipCents += l.TipCents
	}
	r.TipTaxBps = tipTaxBps
	r.TipAfterTaxCents = (r.TipCents*(10000-tipTaxBps) + 5000) / 10000
	r.OwedCents = r.CashCents - r.TipAfterTaxCents
}

// ShortCustomerName turns "Erik Davis" into "Erik D." (a single word stays
// as is; empty becomes "Customer").
func ShortCustomerName(display string) string {
	parts := strings.Fields(display)
	switch len(parts) {
	case 0:
		return "Customer"
	case 1:
		return parts[0]
	}
	last := []rune(parts[len(parts)-1])
	return parts[0] + " " + strings.ToUpper(string(last[0])) + "."
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
	// UndoReturn puts the driver whose return was the last one recorded back
	// out on the run at their old queue position (within UndoReturnWindow,
	// while still available). Their orders stay delivered.
	UndoReturn(ctx context.Context, driverID string, now time.Time) (*Driver, error)

	FloorPrefs(ctx context.Context) (FloorPrefs, error)
	SetFloorPrefs(ctx context.Context, p FloorPrefs) error
	// ReceiptPrefs returns the receipt settings (defaults for missing keys).
	ReceiptPrefs(ctx context.Context) (ReceiptPrefs, error)
	SetReceiptPrefs(ctx context.Context, p ReceiptPrefs) error
	// DriverReports returns one report per driver who delivered in, or was
	// clocked in during, the batch (0 = the open batch), sorted by name.
	// Totals are not filled (see DriverReport.Finish).
	DriverReports(ctx context.Context, batchID int64) ([]DriverReport, error)

	// Escalation: arrived orders waiting since before cutoff and not alerted
	// since cutoff; MarkAlerted stamps last_alert_at.
	ListOverdueArrived(ctx context.Context, cutoff time.Time) ([]StoredOrder, error)
	MarkAlerted(ctx context.Context, id string, at time.Time) error

	// RecordAccepted writes the order.status=accepted floor event (after the
	// tickets printed — a reverted claim never reaches main).
	RecordAccepted(ctx context.Context, so StoredOrder) error
	// ListInterruptedAccepts: orders claimed for accept whose accept was never
	// recorded (the agent stopped mid-accept).
	ListInterruptedAccepts(ctx context.Context) ([]string, error)

	// Sync outbox.
	ListUnsyncedEvents(ctx context.Context, limit int) ([]FloorEvent, error)
	MarkEventSynced(ctx context.Context, id string, at time.Time) error
	MarkEventFailed(ctx context.Context, id, errMsg string) error
	CountUnsyncedEvents(ctx context.Context) (int64, error)

	// Retention: keep the open batch + the newest keepClosed closed batches;
	// drop synced floor events older than syncedBefore.
	TrimBatches(ctx context.Context, keepClosed int, syncedBefore time.Time) (int64, error)
}
