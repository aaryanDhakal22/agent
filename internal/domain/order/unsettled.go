package order

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Closing the day with money still due (contract/agent.md §2.10s §4, the
// owner 2026-11-12): an unsettled check is an order of the open batch (or a
// carried one) that isn't cancelled and still has money due, except a
// delivery already delivered (its driver collected it).

// UnsettledCheck is one line of the Unsettled Check List.
type UnsettledCheck struct {
	OrderID     string            `json:"order_id"`
	Number      int64             `json:"number"`
	Type        string            `json:"type"` // counter | pickup | delivery
	Source      string            `json:"source"`
	State       State             `json:"state"`
	PlacedAt    time.Time         `json:"placed_at"`
	BatchID     int64             `json:"batch_id"`
	Carried     bool              `json:"carried"`
	Customer    UnsettledCustomer `json:"customer"`
	StaffID     string            `json:"staff_id"`
	StaffName   string            `json:"staff_name"`
	TotalCents  int64             `json:"total_cents"`
	PaidCents   int64             `json:"paid_cents"`
	DueCents    int64             `json:"due_cents"`
	PickedUp    bool              `json:"picked_up"`
	BlocksClose bool              `json:"blocks_close"`
	// OnForce (a blocking check in a 409 unsettled_checks): what a forced
	// close does with it, waste | carry (ForceOutcome).
	OnForce string              `json:"on_force,omitempty"`
	Items   []UnsettledLineItem `json:"items"`
}

// UnsettledCustomer is the check's customer (empty strings when none).
type UnsettledCustomer struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

// UnsettledLineItem is one line of an unsettled check.
type UnsettledLineItem struct {
	Qty        int      `json:"qty"`
	Name       string   `json:"name"`
	Mods       []string `json:"mods"`
	PriceCents int64    `json:"price_cents"`
}

// UnsettledList is GET /api/terminal/unsettled and a close's `unsettled`.
type UnsettledList struct {
	BatchID     int64     `json:"batch_id"`
	GeneratedAt time.Time `json:"generated_at"`
	Count       int       `json:"count"`
	DueCents    int64     `json:"due_cents"`
	Blocking    int       `json:"blocking"`
	// ForceAction / Force (GET /api/terminal/unsettled while a check
	// blocks): what a forced close would do (the store's setting).
	ForceAction string           `json:"force_action,omitempty"`
	Force       *ForcePreview    `json:"force,omitempty"`
	Checks      []UnsettledCheck `json:"checks"`
}

// UnsettledError: a manual close without force refused while an unpaid
// counter check is open (409 unsettled_checks {checks, force_action,
// force}): the answer says what force will do with each check.
type UnsettledError struct {
	Checks []UnsettledCheck
	// ForceAction is the store's setting (waste | carry); Force sums what a
	// forced close would do (a partly paid check is always carried).
	ForceAction string
	Force       ForcePreview
}

// ForcePreview: what a forced close does with the blocking checks.
type ForcePreview struct {
	WasteCount    int   `json:"waste_count"`
	WasteCents    int64 `json:"waste_cents"`
	CarryCount    int   `json:"carry_count"`
	CarryDueCents int64 `json:"carry_due_cents"`
}

func (e *UnsettledError) Error() string {
	n := len(e.Checks)
	switch {
	case e.Force.WasteCount > 0 && e.Force.CarryCount > 0:
		return fmt.Sprintf("%d unpaid counter check(s) are open: force close writes off %d as waste (%s) and carries %d over",
			n, e.Force.WasteCount, centsText(e.Force.WasteCents), e.Force.CarryCount)
	case e.Force.WasteCount > 0:
		return fmt.Sprintf("%d unpaid counter check(s) are open: force close writes them off as waste (%s)", n, centsText(e.Force.WasteCents))
	case e.Force.CarryCount > 0:
		return fmt.Sprintf("%d unpaid counter check(s) are open: force close carries them over (still payable)", n)
	}
	return fmt.Sprintf("%d unpaid counter check(s) are open: take the payment or void them, then close", n)
}

func centsText(c int64) string { return fmt.Sprintf("$%d.%02d", c/100, c%100) }

// What a forced close (and the auto-close) does with an unpaid counter
// check (the owner, 2026-11-13; main's terminal setting
// close_unpaid_counter_action, default waste): write it off as Void waste
// "Unpaid at close" (nothing prints in the kitchen; the Z / till Waste
// section; order.void kind waste to main) or carry it like a pickup.
const (
	UnpaidWaste = "waste"
	UnpaidCarry = "carry"
	// UnpaidAtCloseReason is the waste reason of a check written off at a
	// close (not one of the store's waste_reasons: the Pi's own).
	UnpaidAtCloseReason = "Unpaid at close"
	// NotPickedUpReason is the waste reason of an unpaid pickup a close
	// wrote off (the owner, 2026-11-14: "when a pickup order isn't picked up
	// and the close batch command is sent, they're supposed to be voided as
	// waste and printed on a not picked up slip"; the Pi's own reason too).
	NotPickedUpReason = "Not picked up"
)

// UnpaidActionOf reads the setting: "carry", else waste (missing = waste).
func UnpaidActionOf(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), UnpaidCarry) {
		return UnpaidCarry
	}
	return UnpaidWaste
}

// ForceOutcome is what a forced close does with one blocking check: waste
// only when the setting says so and the check has no payment row at all (a
// partial payment, a comp, a tip, even a payment given back = carried: a
// write-off never moves or reverses money).
func ForceOutcome(action string, paymentRows int) string {
	if UnpaidActionOf(action) == UnpaidWaste && paymentRows == 0 {
		return UnpaidWaste
	}
	return UnpaidCarry
}

type closeUnpaidKey struct{}

// WithCloseUnpaid gives CloseBatch the store's setting (waste | carry).
func WithCloseUnpaid(ctx context.Context, action string) context.Context {
	return context.WithValue(ctx, closeUnpaidKey{}, UnpaidActionOf(action))
}

// CloseUnpaidOf is the setting on ctx (waste when none).
func CloseUnpaidOf(ctx context.Context) string {
	if v, ok := ctx.Value(closeUnpaidKey{}).(string); ok {
		return UnpaidActionOf(v)
	}
	return UnpaidWaste
}

// CloseActor is who closed (the journal and order.void of a write-off).
type CloseActor struct {
	StaffID    string `json:"staff_id,omitempty"`
	StaffName  string `json:"staff_name,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	ApprovedBy string `json:"approved_by,omitempty"`
	Trigger    string `json:"trigger"` // terminal | tablet | web | auto
	// PrintsHere: the caller prints the Unpaid at close list itself (a
	// terminal that says so); else the Pi prints it.
	PrintsHere bool `json:"-"`
	// PrintsNotPickedUpHere: the caller prints the Not picked up slip
	// itself (a terminal that says print_not_picked_up_here); else the Pi
	// prints it (an older terminal never knew the slip).
	PrintsNotPickedUpHere bool `json:"-"`
}

type forceUnpaidKey struct{}

// WithForceUnpaid: the caller passes only the unpaid counter checks (body
// force_unpaid: the setting applies to them), not the leftovers (force).
func WithForceUnpaid(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, forceUnpaidKey{}, on)
}

// ForceUnpaidOf is WithForceUnpaid's flag (false when none).
func ForceUnpaidOf(ctx context.Context) bool {
	v, _ := ctx.Value(forceUnpaidKey{}).(bool)
	return v
}

type printNotPickedUpKey struct{}

// WithPrintNotPickedUpHere: the closing terminal prints the Not picked up
// slip itself (body print_not_picked_up_here, the owner 2026-11-14).
func WithPrintNotPickedUpHere(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, printNotPickedUpKey{}, on)
}

// PrintNotPickedUpHereOf is WithPrintNotPickedUpHere's flag (false when none).
func PrintNotPickedUpHereOf(ctx context.Context) bool {
	v, _ := ctx.Value(printNotPickedUpKey{}).(bool)
	return v
}

type closeActorKey struct{}

// WithCloseActor tells CloseBatch who closed.
func WithCloseActor(ctx context.Context, a CloseActor) context.Context {
	return context.WithValue(ctx, closeActorKey{}, a)
}

// CloseActorOf is who closed (the trigger filled in; device "pi" when none).
func CloseActorOf(ctx context.Context) CloseActor {
	a, _ := ctx.Value(closeActorKey{}).(CloseActor)
	if a.Trigger == "" {
		a.Trigger = CloseTriggerOf(ctx)
	}
	if a.DeviceID == "" {
		a.DeviceID = "pi"
	}
	return a
}

// UnpaidAtClose is the "Unpaid at close" report: the unpaid counter checks
// a close wrote off as waste (a close's `unpaid_at_close`; printed). The
// same shape is the "Not picked up" report (a close's `not_picked_up`, the
// owner 2026-11-14): the unpaid pickups it wrote off, Reason "Not picked
// up".
type UnpaidAtClose struct {
	BatchID    int64            `json:"batch_id"`
	Reason     string           `json:"reason"`
	At         time.Time        `json:"at"`
	Auto       bool             `json:"auto"`
	ClosedBy   CloseActor       `json:"closed_by"`
	Count      int              `json:"count"`
	TotalCents int64            `json:"total_cents"`
	PrintedBy  string           `json:"printed_by"` // pi | terminal
	Checks     []UnsettledCheck `json:"checks"`
}

// TerminalType is a terminal order's type (counter | pickup | delivery); an
// online order's is its fulfillment's (pickup | delivery).
func TerminalType(so StoredOrder) string {
	switch {
	case so.IsDelivery():
		return "delivery"
	case so.Order.Source == SourceCounter && so.Order.Fulfillment.Type == "dine_in":
		return "counter"
	}
	return "pickup"
}

// IsUnsettled: not cancelled, money due, and not a delivered delivery.
func IsUnsettled(so StoredOrder) bool {
	switch so.Status.State {
	case StateCancelled, StateArrived, StateScheduled:
		return false // not a check yet (never accepted / a future order)
	}
	if so.Status.CashDueCents <= 0 {
		return false
	}
	if so.Order.Source != SourceCounter && so.Status.Paid {
		return false
	}
	return !(so.IsDelivery() && so.Status.State == StateDelivered)
}

// BlocksClose: an unsettled counter (dine-in) check blocks a manual close.
func BlocksClose(so StoredOrder) bool {
	return IsUnsettled(so) && TerminalType(so) == "counter"
}

// NotPickedUp: an unsettled counter / phone pickup (source counter, never
// an online or website one) that isn't picked up - what a close writes off
// as waste "Not picked up" when it has no payment row and the store's
// close_unpaid_counter_action is waste (the owner, 2026-11-14). Open batch
// or carried from an earlier one; it never blocks a close.
func NotPickedUp(so StoredOrder) bool {
	return IsUnsettled(so) && so.Order.Source == SourceCounter && TerminalType(so) == "pickup" && so.Status.FinishedAt == nil
}

// Carries: an unsettled check a close carries to the next batch (still
// payable): a pickup at a manual close, every non-delivery one at an
// auto-close (contract 2.10s §4). A delivery is finished as before.
func Carries(so StoredOrder, auto bool) bool {
	if !IsUnsettled(so) || so.IsDelivery() {
		return false
	}
	return auto || TerminalType(so) == "pickup"
}

// Settled: a carried check is done with - cancelled, or nothing due and
// (a pickup) picked up.
func Settled(so StoredOrder) bool {
	if so.Status.State == StateCancelled {
		return true
	}
	if IsUnsettled(so) {
		return false
	}
	if TerminalType(so) == "pickup" {
		return so.Status.FinishedAt != nil
	}
	return true
}

// UnsettledCheckOf builds an order's line on the list (staffName resolves a
// staff id; nil = none).
func UnsettledCheckOf(so StoredOrder, staffName func(id string) string) UnsettledCheck {
	snap := so.Order
	c := UnsettledCheck{OrderID: snap.ID, Type: TerminalType(so), Source: snap.Source, State: so.Status.State,
		PlacedAt: snap.PlacedAt, BatchID: so.Status.BatchID, Carried: so.Status.Carried, TotalCents: snap.Money.Total,
		DueCents: max(so.Status.CashDueCents, 0), PickedUp: so.Status.FinishedAt != nil, Items: []UnsettledLineItem{}}
	if c.PlacedAt.IsZero() {
		c.PlacedAt = so.Status.ArrivalDate
	}
	if so.Status.KitchenNumber != nil {
		c.Number = *so.Status.KitchenNumber
	}
	base := snap.Money.Total
	if snap.Source != SourceCounter {
		base = snap.CashDue()
	}
	c.PaidCents = max(base-c.DueCents, 0)
	c.Customer = UnsettledCustomer{Name: snap.Customer.DisplayName, Phone: snap.Customer.Phone}
	if t, ok := snap.SourceMeta["terminal"].(map[string]any); ok {
		if id, ok := t["staff_id"].(string); ok {
			c.StaffID = id
		}
		if n, ok := t["staff_name"].(string); ok {
			c.StaffName = n
		}
	}
	if c.StaffName == "" && c.StaffID != "" && staffName != nil {
		c.StaffName = staffName(c.StaffID)
	}
	for _, it := range snap.Items {
		li := UnsettledLineItem{Qty: max(it.Quantity, 1), Name: it.Name, Mods: []string{}, PriceCents: it.LineTotal}
		for _, ch := range it.Children {
			li.Mods = append(li.Mods, ch.Name)
		}
		c.Items = append(c.Items, li)
	}
	c.BlocksClose = BlocksClose(so)
	return c
}
