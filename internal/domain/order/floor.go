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
	// ClearedDrafts: the terminals' shared drafts Close batch deleted (their
	// removal is announced; never on the wire).
	ClearedDrafts []string `json:"-"`
	// ClosedDrawers: the terminal drawer sessions Close batch closed (not
	// counted, flagged; contract 2.10k). ZPending: a Z report was queued for
	// this batch (built right after, contract 2.10l).
	ClosedDrawers []string `json:"-"`
	ZPending      bool     `json:"-"`
	// Unsettled: the Unsettled Check List at the close (contract 2.10s §4;
	// nil when not a close).
	Unsettled *UnsettledList `json:"-"`
}

// Close kinds (the Z report's close_kind): Close batch by hand, or the Pi's
// auto-close.
const (
	CloseManual = "manual"
	CloseAuto   = "auto"
)

type closeKindKey struct{}

// WithCloseKind tells CloseBatch how it was started (the Z report).
func WithCloseKind(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, closeKindKey{}, kind)
}

// CloseKindOf is the close kind on ctx (CloseManual when none).
func CloseKindOf(ctx context.Context) string {
	if k, ok := ctx.Value(closeKindKey{}).(string); ok && k != "" {
		return k
	}
	return CloseManual
}

type closeTriggerKey struct{}

// WithCloseTrigger tells CloseBatch who started it (the batch.closes metric:
// terminal | tablet | web | auto).
func WithCloseTrigger(ctx context.Context, trigger string) context.Context {
	return context.WithValue(ctx, closeTriggerKey{}, trigger)
}

// CloseTriggerOf is the trigger on ctx ("auto" for an auto-close kind, else
// "tablet" when none).
func CloseTriggerOf(ctx context.Context) string {
	if t, ok := ctx.Value(closeTriggerKey{}).(string); ok && t != "" {
		return t
	}
	if CloseKindOf(ctx) == CloseAuto {
		return "auto"
	}
	return "tablet"
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
	// PushoverDevice (2.3.7) is the driver's phone in Pushover: Send Out
	// pushes them the route ("" = none).
	PushoverDevice string `json:"pushover_device,omitempty"`
	// RunOrderIDs (2.2.1): a driver who is out — the orders of the run, in
	// stop order (first stop first). Empty otherwise.
	RunOrderIDs []string `json:"run_order_ids,omitempty"`
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
	// FloorEventNotDelivered (2.2.1): a delivery came back with its driver.
	FloorEventNotDelivered = "order.not_delivered"
	// FloorEventOrderTip (2.5.0): ADD TIP on a card channel order - the
	// whole tip added after the order (absolute, so a replay is harmless).
	FloorEventOrderTip = "order.tip"
	// FloorEventBatchClosed (2.5.0): Close batch - main charges every card
	// hold left in the batch.
	FloorEventBatchClosed = "batch.closed"
)

// Cancel reasons (2.5.0 CANCEL ORDER on a channel order; order.status
// cancelled carries one so main applies it after accept too).
const (
	CancelCustomerAsked  = "customer_asked"
	CancelCouldntDeliver = "couldnt_deliver"
	CancelOther          = "other"
	MaxCancelNoteRunes   = 140
	MaxAddedTipCents     = 50000
)

// ValidCancelReason reports whether r is one of the cancel reasons.
func ValidCancelReason(r string) bool {
	return r == CancelCustomerAsked || r == CancelCouldntDeliver || r == CancelOther
}

// FloorEvent is one change to sync to main. Payload is the complete JSON sent
// (it carries its own event_id + type + at, so main can dedupe on event_id).
type FloorEvent struct {
	ID       string          `json:"event_id"`
	OrderID  string          `json:"order_id,omitempty"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	At       time.Time       `json:"at"`
	Attempts int64           `json:"attempts"`
	// Subject (contract 2.4e): what the event is about ("" = nothing held).
	Subject string `json:"subject,omitempty"`
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
	// Reason (2.5.0): a cancel by hand (CANCEL ORDER) - main applies it in
	// any state before completed and releases the card hold.
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
	// The agent's whole status + batch, stored by main so a replacement Pi
	// (swapped SD card) can restore the open batch exactly.
	BatchID     int64   `json:"batch_id,omitempty"`
	AgentStatus *Status `json:"agent_status,omitempty"`
}

// NotDeliveredEvent is the payload of an order.not_delivered floor event: the
// order left its driver's run and is ready again with no driver (retry) or
// cancelled. main allows this one backwards step. Auto marks Close batch
// cancelling a came-back order nobody took out again.
type NotDeliveredEvent struct {
	EventID    string    `json:"event_id"`
	Type       string    `json:"type"` // order.not_delivered
	OrderID    string    `json:"order_id"`
	Action     string    `json:"action"` // retry | cancel
	Status     State     `json:"status"` // the order's state after it: ready | cancelled
	DriverID   string    `json:"driver_id,omitempty"`
	DriverName string    `json:"driver_name,omitempty"`
	Auto       bool      `json:"auto,omitempty"`
	At         time.Time `json:"at"`
	BatchID    int64     `json:"batch_id,omitempty"`
	// The agent's whole status, as on order.status (a replacement Pi's restore).
	AgentStatus *Status `json:"agent_status,omitempty"`
}

// OrderTipEvent is the payload of an order.tip floor event (2.5.0): the
// whole tip added after the order, in cents (absolute).
type OrderTipEvent struct {
	EventID  string    `json:"event_id"`
	Type     string    `json:"type"` // order.tip
	OrderID  string    `json:"order_id"`
	TipCents int64     `json:"tip_cents"`
	At       time.Time `json:"at"`
}

// BatchClosedEvent is the payload of a batch.closed floor event (2.5.0),
// written by Close batch after the batch's own order events.
type BatchClosedEvent struct {
	EventID string    `json:"event_id"`
	Type    string    `json:"type"` // batch.closed
	BatchID int64     `json:"batch_id"`
	At      time.Time `json:"at"`
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
	// ErrDriverNotOut: recall needs a driver out on a run (2.4.1).
	ErrDriverNotOut = fmt.Errorf("driver is not out on a run")
	ErrNoOrders     = fmt.Errorf("no orders given")
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
// what they owe. Cash = what the driver collected on cash orders, less the
// tip on them (the driver keeps a cash tip; it is not listed). Tips count only
// on card-paid deliveries (paid to the store, owed to the driver) minus the
// tip tax, so Owed = Cash - TipAfterTax; negative means the store owes the
// driver. POS deliveries count like online ones. Delivery fees are paid
// weekly — only the count shows. (2.1.0 rule.)
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
	// OwedRoundedCents is OwedCents rounded to whole dollars (RoundOwed) —
	// what the slip prints.
	OwedRoundedCents int64 `json:"owed_rounded_cents"`
	// PendingTipCents (2.5.0): the card channel tips in TipCents main hasn't
	// charged yet (the driver hasn't clocked out, the batch isn't closed) -
	// ADD TIP can still change them.
	PendingTipCents int64 `json:"pending_tip_cents"`
	// DoorTipCents (terminal driver view): the tips he got at the door on
	// cash / counter deliveries (kept by him; not in TipCents or OwedCents).
	DoorTipCents int64 `json:"door_tip_cents"`
	// DriverShareCents: his share of the delivery fees on his orders (main's
	// split of the store's delivery fee: store share + driver share). Shown
	// on the report; OwedCents doesn't count it (the store pays it out).
	DriverShareCents int64 `json:"driver_share_cents"`
}

// DriverReportLine is one delivery on a driver report.
type DriverReportLine struct {
	OrderID       string     `json:"order_id"`
	KitchenNumber *int64     `json:"kitchen_number,omitempty"`
	Source        string     `json:"source,omitempty"`
	Customer      string     `json:"customer"` // "Erik D."
	Address       string     `json:"address"`
	TipCents      int64      `json:"tip_cents"` // card-paid only (0 on a cash order)
	Cash          bool       `json:"cash"`
	CashCents     int64      `json:"cash_cents"` // collected in cash less its tip (0 for card)
	TotalCents    int64      `json:"total_cents"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	// OutAt (2.3.0): when it went out.
	OutAt *time.Time `json:"out_at,omitempty"`
	// What the slip prints (no customer name on it): Street = house number
	// + street only (no apt, city, notes); OrderTipCents = the tip on the
	// order, cash or card; SlipTotalCents = card: the total + the tip, cash:
	// just the total (both without the tip in the total).
	Street         string `json:"street"`
	OrderTipCents  int64  `json:"order_tip_cents"`
	SlipTotalCents int64  `json:"slip_total_cents"`
	// 2.5.0, a card channel order: AddedTipCents = the tip added on the
	// tablet after the order (included in TipCents / OrderTipCents);
	// TipPending = main hasn't charged the card yet (not tip_locked).
	AddedTipCents int64 `json:"added_tip_cents,omitempty"`
	TipPending    bool  `json:"tip_pending,omitempty"`
	// DoorTipCents (terminal driver view): a tip the driver got at the door
	// on a cash / counter delivery. In OrderTipCents (the slip's Tip), never
	// in TipCents: the driver already holds it, the store owes him nothing.
	DoorTipCents int64 `json:"door_tip_cents,omitempty"`
	// DriverShareCents: the driver's share of the order's delivery fee (0 =
	// none, or no split known: a vendor order).
	DriverShareCents int64 `json:"driver_share_cents,omitempty"`
}

// addDoorTip shows a tip the driver got at the door on the line.
func (l *DriverReportLine) addDoorTip(cents int64) {
	if cents > 0 {
		l.DoorTipCents = cents
		l.OrderTipCents += cents
	}
}

// setSlipMoney fills the slip's Tip / Total from the order's total without
// its tip (base) and its tip.
func (l *DriverReportLine) setSlipMoney(base, tip int64) {
	l.OrderTipCents = max(tip, 0)
	l.SlipTotalCents = max(base, 0)
	if !l.Cash {
		l.SlipTotalCents += l.OrderTipCents
	}
}

// StreetOnly is the house number + street of an address line: what comes
// before the first comma, without an apartment / unit / suite part (a
// keyword right after the house number is the street: "7 Unit Rd").
func StreetOnly(line1 string) string {
	s := line1
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	words := strings.Fields(s)
	for i, w := range words {
		lw := strings.ToLower(strings.TrimRight(w, ".:"))
		if i > 1 && (strings.HasPrefix(w, "#") || lw == "apt" || lw == "apartment" || lw == "unit" || lw == "suite" ||
			lw == "ste" || lw == "bldg" || lw == "building" || lw == "fl" || lw == "floor" || lw == "rm" || lw == "room" || lw == "lot") {
			words = words[:i]
			break
		}
	}
	return strings.Join(words, " ")
}

// NewDriverReportLine builds a report line from a delivered order. Card-paid
// (no cash due) → the tip counts, no cash. Cash → cash due less the tip
// (CashDue - tip, floored at 0) and no tip.
func NewDriverReportLine(so StoredOrder) DriverReportLine {
	snap := so.Order
	l := DriverReportLine{
		OrderID:       snap.ID,
		KitchenNumber: so.Status.KitchenNumber,
		Source:        snap.Source,
		Customer:      ShortCustomerName(snap.Customer.DisplayName),
		TotalCents:    snap.Money.Total,
		DeliveredAt:   so.Status.DeliveredAt,
		OutAt:         so.Status.OutAt,
		// The driver's share of the delivery fee.
		DriverShareCents: snap.Money.DeliveryDriverCents(),
	}
	if a := snap.Fulfillment.Address; a != nil {
		l.Street = StreetOnly(a.Line1)
		l.Address = strings.TrimSpace(a.Line1)
		if l2 := strings.TrimSpace(a.Line2); l2 != "" {
			l.Address += ", " + l2
		}
	}
	if snap.Source == SourceCounter {
		// A terminal's delivery (the owner, 2026-11-12): the tips taken with
		// its payments at the counter are the driver's (the store owes them
		// to him); a comp lists at $0; split card + cash at the door: he
		// collects (and owes) only the cash part still due.
		var comp int64
		for _, p := range snap.Payments {
			if p.Method == "other" {
				comp += p.AmountCents
			}
		}
		base := max(snap.Money.Total-comp, 0)
		l.TotalCents = base
		l.TipCents = max(so.Status.TipCents, 0)
		if due := snap.CashDue(); due > 0 {
			l.Cash, l.CashCents = true, due
		}
		l.setSlipMoney(base, l.TipCents)
		l.addDoorTip(so.Status.DoorTipCents)
		return l
	}
	if due := snap.CashDue(); due > 0 {
		l.Cash, l.CashCents = true, max(due-snap.Money.Tip, 0)
		l.setSlipMoney(snap.Money.Total-snap.Money.Tip, snap.Money.Tip) // an online total includes the tip
		l.addDoorTip(so.Status.DoorTipCents)
		return l
	}
	tip := snap.Money.Tip
	if so.IsChannel() { // 2.5.0: the checkout tip + the tip added on the tablet
		tip += so.Status.AddedTipCents
		l.AddedTipCents, l.TipPending = so.Status.AddedTipCents, !so.Status.TipLocked
	}
	l.TipCents = tip
	l.setSlipMoney(snap.Money.Total-snap.Money.Tip, tip)
	l.addDoorTip(so.Status.DoorTipCents)
	return l
}

// Finish fills the report's totals with the store's tip tax.
func (r *DriverReport) Finish(tipTaxBps int64) {
	r.Deliveries = len(r.Lines)
	r.CashCents, r.TipCents, r.PendingTipCents, r.DoorTipCents, r.DriverShareCents = 0, 0, 0, 0, 0
	for _, l := range r.Lines {
		r.DriverShareCents += l.DriverShareCents
		r.CashCents += l.CashCents
		r.TipCents += l.TipCents
		r.DoorTipCents += l.DoorTipCents
		if l.TipPending {
			r.PendingTipCents += l.TipCents
		}
	}
	r.TipTaxBps = tipTaxBps
	r.TipAfterTaxCents = (r.TipCents*(10000-tipTaxBps) + 5000) / 10000
	r.OwedCents = r.CashCents - r.TipAfterTaxCents
	r.OwedRoundedCents = RoundOwed(r.OwedCents)
}

// RoundOwed rounds a driver's owed total to whole dollars on its absolute
// value: 50 cents or more up, 49 or less down ($32.50 → $33, $32.49 → $32,
// -$7.50 → -$8).
func RoundOwed(cents int64) int64 {
	a := cents
	if a < 0 {
		a = -a
	}
	r := (a + 50) / 100 * 100
	if cents < 0 {
		return -r
	}
	return r
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
	// MarkReady: accepted -> ready; callout false (2.4.0, READY held on the
	// tablet) tells the hub not to call the order out.
	MarkReady(ctx context.Context, id string, callout bool) (*StoredOrder, error)
	// UndoReady puts an order marked ready at or after since back to
	// cooking (2.3.8); ErrInvalidState when it isn't (any more).
	UndoReady(ctx context.Context, id string, since time.Time) (*StoredOrder, error)
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
	// CreateDriver adds a driver.
	CreateDriver(ctx context.Context, name string) (*Driver, error)
	RenameDriver(ctx context.Context, id, name string) error
	// SetDriverPushoverDevice sets ("" clears) a driver's Pushover device.
	SetDriverPushoverDevice(ctx context.Context, id, device string) error
	// RemoveDriver also returns the orders its pool held (back to unassigned).
	RemoveDriver(ctx context.Context, id string) ([]StoredOrder, error)
	ReorderDrivers(ctx context.Context, ids []string) error
	// ClockIn puts an off driver into the queue (back) and opens a shift;
	// ClockOut takes an available driver out of the queue and closes it
	// (ErrInvalidState while the driver is out on a run).
	ClockIn(ctx context.Context, id string) (*Driver, error)
	// ClockOut also returns the orders its pool held (back to unassigned)
	// and those whose tip locked with the shift (2.5.0).
	ClockOut(ctx context.Context, id string) (*Driver, []StoredOrder, error)
	// AutoClockOut is the daily reset: clocks out every available driver
	// (drivers out on a run are clocked out when they return) and records the
	// run time. Returns how many were clocked out and the orders their pools
	// held (back to unassigned).
	AutoClockOut(ctx context.Context, at time.Time) (int, []StoredOrder, error)
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

	// NotDelivered takes a delivery that came back with its driver off the
	// run (on_delivery or delivered, open batch): retry → ready, no driver,
	// came_back; cancel → cancelled. The driver stays out while other stops
	// remain, else is back in the queue; returnedDriver reports that.
	// ErrNotOutOrDelivered otherwise.
	NotDelivered(ctx context.Context, id, action string) (so *StoredOrder, returnedDriver bool, err error)
	// ReleaseCameBack ends a came-back hold (ErrNotCameBack when not held).
	ReleaseCameBack(ctx context.Context, id string) (*StoredOrder, error)

	// 2.5.0 website / channel orders. SetAddedTip saves ADD TIP (the whole
	// tip added after the order, absolute) and writes order.tip - a card
	// channel order of the open batch, not cancelled (ErrNotTippable), not
	// charged yet (ErrTipLocked); an unchanged tip writes nothing.
	// CancelChannelOrder is CANCEL ORDER: a channel order before delivered
	// (arrived|scheduled|accepted|ready|on_delivery, not mid-accept) is
	// cancelled with the reason, off any pool or run (the last stop of a run
	// brings the driver back: returnedDriver), order.status cancelled +
	// reason; prev is the order before. ErrNotCancellable otherwise.
	SetAddedTip(ctx context.Context, id string, cents int64) (*StoredOrder, error)
	CancelChannelOrder(ctx context.Context, id, reason string) (so *StoredOrder, prev StoredOrder, returnedDriver bool, err error)

	// 2.4.0 Unassigned. MarkDeliveredNoDriver delivers an open delivery of
	// the open batch (cooking or ready, out of its pool) with no driver
	// (ErrInvalidState otherwise). Undeliver puts an Unassigned delivery
	// back the way it was (ErrNotUnassigned). ReassignDeliveries moves
	// delivered / out deliveries of the open batch to a clocked-in driver,
	// or delivered ones to Unassigned (driverID ""), all or nothing; a
	// driver whose last out stop moved is back in the queue, one who got an
	// out stop while available is out (driversChanged). ListUnassigned is a
	// batch's (0 = open) Unassigned deliveries.
	MarkDeliveredNoDriver(ctx context.Context, id string, by MarkedBy) (*StoredOrder, error)
	Undeliver(ctx context.Context, id string) (*StoredOrder, error)
	ReassignDeliveries(ctx context.Context, ids []string, driverID string, by MarkedBy) (moved []StoredOrder, driversChanged bool, err error)
	ListUnassigned(ctx context.Context, batchID int64) ([]StoredOrder, error)

	// RecordAccepted writes the order.status=accepted floor event (after the
	// tickets printed — a reverted claim never reaches main) and, for an
	// online order, the hub's order.accepted with insertExpected.
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

// UnassignedReport (2.4.0) is the slip Close batch prints after the driver
// reports when deliveries were left delivered with no driver: everything
// known about each, so staff can work out who took it.
type UnassignedReport struct {
	BatchID       int64            `json:"batch_id"`
	BatchOpenedAt time.Time        `json:"batch_opened_at"`
	BatchClosedAt *time.Time       `json:"batch_closed_at,omitempty"`
	Lines         []UnassignedLine `json:"lines"`
	CashCents     int64            `json:"cash_cents"`
	TipCents      int64            `json:"tip_cents"`
}

// UnassignedLine is one Unassigned delivery on the report: its number,
// street + apt, money (as a driver report line counts it), the times it
// was placed / accepted / ready, who marked it delivered and when, the
// driver it was taken from (if any) and the drivers available then.
type UnassignedLine struct {
	DriverReportLine
	PlacedAt   *time.Time `json:"placed_at,omitempty"`
	AcceptedAt *time.Time `json:"accepted_at,omitempty"`
	ReadyAt    *time.Time `json:"ready_at,omitempty"`
	MarkedAt   time.Time  `json:"marked_at"`
	MarkedBy   MarkedBy   `json:"marked_by"`
	FromDriver string     `json:"from_driver,omitempty"`
	Available  []string   `json:"available"`
}

// NewUnassignedReport builds the report of a batch's Unassigned orders.
func NewUnassignedReport(b Batch, orders []StoredOrder) UnassignedReport {
	rep := UnassignedReport{BatchID: b.ID, BatchOpenedAt: b.OpenedAt, BatchClosedAt: b.ClosedAt, Lines: []UnassignedLine{}}
	for _, so := range orders {
		l := UnassignedLine{DriverReportLine: NewDriverReportLine(so), AcceptedAt: so.Status.AcceptedAt, ReadyAt: so.Status.ReadyAt}
		if a := so.Order.Fulfillment.Address; a != nil {
			l.Street = strings.TrimSpace(StreetOnly(a.Line1) + " " + strings.TrimSpace(a.Line2))
		}
		if p := so.Order.PlacedAt; !p.IsZero() {
			l.PlacedAt = &p
		}
		if md := so.Status.MarkedDelivered; md != nil {
			l.MarkedAt, l.MarkedBy, l.FromDriver, l.Available = md.At, md.By, md.FromDriver, md.Available
		}
		if l.Cash {
			rep.CashCents += l.CashCents
		}
		rep.TipCents += l.OrderTipCents
		rep.Lines = append(rep.Lines, l)
	}
	return rep
}
