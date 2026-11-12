package terminal

import (
	"errors"
	"fmt"
	"time"
)

// Cash drawers (contract/agent.md §2.10k, TERMINAL-PLAN "Cash drawer",
// "Drawers"): a drawer session belongs to a terminal (its drawer) and the
// cashier counting it in. One open session per terminal and per cashier.
// Cash payments and refunds a terminal takes go in its open session;
// pay-ins / pay-outs move cash without a sale. Close = count out: expected
// vs counted, over / short. Close batch closes every open session
// ("closed by batch close", not counted, flagged).

// Close reasons.
const (
	DrawerClosedCounted    = "counted"
	DrawerClosedHandover   = "handover"
	DrawerClosedBatchClose = "batch_close"
)

// Movement kinds.
const (
	MovePayIn  = "pay_in"
	MovePayOut = "pay_out"
)

// DefaultCashierPayoutCents: what a cashier may pay out of a drawer without a
// manager when main's settings don't say (cashier_payout_cents).
const DefaultCashierPayoutCents = 2000

// Floor events (to main).
const (
	EventDrawerOpened   = "drawer.opened"
	EventDrawerMovement = "drawer.movement"
	EventDrawerClosed   = "drawer.closed"
	EventZReport        = "z.report"
)

// Errors.
var (
	ErrNoDrawer          = errors.New("this terminal has no open drawer")
	ErrDrawerNotAllowed  = errors.New("drivers don't use a cash drawer")
	ErrNotEnoughCash     = errors.New("more than the drawer holds")
	ErrDrawerNotFound    = errors.New("no such drawer")
	ErrDrawerClosed      = errors.New("that drawer is closed already")
	ErrSameCashier       = errors.New("this drawer is yours already: close it instead")
	ErrCountMismatch     = errors.New("counted_cents doesn't match the denominations")
	ErrBadDenominations  = errors.New("denominations: up to 20 {value_cents 1-10000, count 0-10000}, each value once")
	ErrMovementKeyReused = errors.New("that client_key was used for another movement")
)

// DrawerOpen is a terminal's open drawer (409 drawer_open).
type DrawerOpen struct{ Drawer Drawer }

func (e *DrawerOpen) Error() string { return "this terminal's drawer is open already" }

// CashierHasDrawer: the cashier has a drawer open on another terminal (409
// cashier_has_drawer: close it or hand it over first).
type CashierHasDrawer struct{ Drawer Drawer }

func (e *CashierHasDrawer) Error() string {
	return fmt.Sprintf("you have a drawer open on %s: close it or hand it over first", e.Drawer.DeviceName)
}

// Denomination is one line of a count.
type Denomination struct {
	ValueCents int64 `json:"value_cents"`
	Count      int64 `json:"count"`
}

// CountOf checks a count: counted from the denominations when only they are
// given, both must agree when both are. nil, nil = nothing given.
func CountOf(counted *int64, denoms []Denomination) (*int64, error) {
	if len(denoms) > 20 {
		return nil, ErrBadDenominations
	}
	seen := map[int64]bool{}
	var sum int64
	for _, d := range denoms {
		if d.ValueCents < 1 || d.ValueCents > 10000 || d.Count < 0 || d.Count > 10000 || seen[d.ValueCents] {
			return nil, ErrBadDenominations
		}
		seen[d.ValueCents] = true
		sum += d.ValueCents * d.Count
	}
	switch {
	case counted != nil && (*counted < 0 || *counted > 100_000_00):
		return nil, fmt.Errorf("%w: counted_cents is 0-10000000", ErrCountMismatch)
	case counted != nil && len(denoms) > 0 && *counted != sum:
		return nil, ErrCountMismatch
	case counted != nil:
		return counted, nil
	case len(denoms) > 0:
		return &sum, nil
	}
	return nil, nil
}

// DrawerTotals are the cash flows of a session.
type DrawerTotals struct {
	CashSalesCents   int64 `json:"cash_sales_cents"`   // cash payments' amounts
	CashTipsCents    int64 `json:"cash_tips_cents"`    // tips taken in cash with them
	CashRefundsCents int64 `json:"cash_refunds_cents"` // cash given back (amount + tip)
	PayInsCents      int64 `json:"pay_ins_cents"`
	PayOutsCents     int64 `json:"pay_outs_cents"`
	Payments         int64 `json:"payments"` // cash payments + refunds
	NoSales          int64 `json:"no_sales"`
}

// Expected is what the drawer should hold: start + cash sales + cash tips
// - cash refunds + pay-ins - pay-outs (the change given is already out of
// the net: a cash payment stores what it settled, not what was handed over).
func (t DrawerTotals) Expected(start int64) int64 {
	return start + t.CashSalesCents + t.CashTipsCents - t.CashRefundsCents + t.PayInsCents - t.PayOutsCents
}

// Drawer is a drawer session as the terminals, the web page and main see it.
type Drawer struct {
	ID           string     `json:"id"`
	DeviceID     string     `json:"device_id"`
	DeviceName   string     `json:"device_name"`
	StaffID      string     `json:"staff_id"`
	StaffName    string     `json:"staff_name"`
	BatchID      int64      `json:"batch_id"`
	OpenedAt     time.Time  `json:"opened_at"`
	StartCents   int64      `json:"start_cents"`
	HandoverFrom string     `json:"handover_from,omitempty"`
	State        string     `json:"state"` // open | closed
	ClosedAt     *time.Time `json:"closed_at"`
	ClosedBy     string     `json:"closed_by"`
	CloseReason  string     `json:"close_reason"`
	// CountedCents nil = not counted (closed by Close batch, or still open).
	CountedCents  *int64         `json:"counted_cents"`
	Denominations []Denomination `json:"denominations"`
	// ExpectedCents: now (open) or at the close.
	ExpectedCents int64 `json:"expected_cents"`
	// OverShortCents = counted - expected (nil when not counted).
	OverShortCents *int64 `json:"over_short_cents"`
	Flagged        bool   `json:"flagged"`
	DrawerTotals
	Movements []Movement `json:"movements,omitempty"`
}

// Finish fills the derived fields from the totals (expected, over / short).
func (d *Drawer) Finish() {
	if d.ClosedAt == nil {
		d.State = "open"
		d.ExpectedCents = d.Expected(d.StartCents)
	} else {
		d.State = "closed"
	}
	if d.CountedCents != nil {
		os := *d.CountedCents - d.ExpectedCents
		d.OverShortCents = &os
	}
	if d.Denominations == nil {
		d.Denominations = []Denomination{}
	}
}

// Wire is the drawer as main's drawer.opened / drawer.closed carry it
// (contract/main.md §2.5zb).
func (d Drawer) Wire() map[string]any {
	m := map[string]any{"id": d.ID, "device_id": d.DeviceID, "device_name": d.DeviceName, "staff_id": nilIfBlank(d.StaffID),
		"staff_name": d.StaffName, "batch_id": d.BatchID, "opened_at": d.OpenedAt.UTC().Truncate(time.Millisecond),
		"start_cents": d.StartCents}
	if d.HandoverFrom != "" {
		m["handover_from"] = d.HandoverFrom
	}
	if d.ClosedAt == nil {
		return m
	}
	var denoms any
	if len(d.Denominations) > 0 {
		denoms = d.Denominations
	}
	m["closed_at"] = d.ClosedAt.UTC().Truncate(time.Millisecond)
	m["closed_by"] = d.ClosedBy
	m["close_reason"] = d.CloseReason
	m["counted_cents"] = d.CountedCents
	m["denominations"] = denoms
	m["expected_cents"] = d.ExpectedCents
	m["over_short_cents"] = d.OverShortCents
	m["flagged"] = d.Flagged
	m["cash_sales_cents"] = d.CashSalesCents
	m["cash_refunds_cents"] = d.CashRefundsCents
	m["cash_tips_cents"] = d.CashTipsCents
	m["pay_ins_cents"] = d.PayInsCents
	m["pay_outs_cents"] = d.PayOutsCents
	m["no_sales"] = d.NoSales
	m["payments"] = d.Payments
	return m
}

func nilIfBlank(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Movement is a pay-in or pay-out.
type Movement struct {
	ID          string    `json:"id"`
	DrawerID    string    `json:"drawer_id"`
	Kind        string    `json:"kind"` // pay_in | pay_out
	AmountCents int64     `json:"amount_cents"`
	Reason      string    `json:"reason"`
	StaffID     string    `json:"staff_id"`
	ApprovedBy  string    `json:"approved_by,omitempty"`
	ClientKey   string    `json:"client_key,omitempty"`
	At          time.Time `json:"at"`
}

// Wire is the movement as main's drawer.movement carries it.
func (m Movement) Wire() map[string]any {
	out := map[string]any{"id": m.ID, "drawer_id": m.DrawerID, "kind": m.Kind, "amount_cents": m.AmountCents, "reason": m.Reason,
		"staff_id": nilIfBlank(m.StaffID), "at": m.At.UTC().Truncate(time.Millisecond)}
	if m.ApprovedBy != "" {
		out["approved_by"] = m.ApprovedBy
	}
	return out
}

// DrawerClose is how a session is closed (repo input).
type DrawerClose struct {
	DrawerID      string
	By            string // staff id ("" = the Pi: Close batch)
	Reason        string // counted | handover | batch_close
	CountedCents  *int64
	Denominations []Denomination
	At            time.Time
	Journal       []Entry
}

// PayoutLimit is what a staff member may pay out of a drawer alone:
// unlimited for a manager (or anyone main made unlimited), main's
// cashier_payout_cents for a cashier (default DefaultCashierPayoutCents).
func (c *Config) PayoutLimit(s Staff) (cents int64, unlimited bool) {
	if c.LimitsOf(s).Unlimited {
		return 0, true
	}
	if c != nil && c.parsed.CashierPayoutCents != nil {
		return max(*c.parsed.CashierPayoutCents, 0), false
	}
	return DefaultCashierPayoutCents, false
}

// UsesDrawer: cashiers and managers keep a drawer; drivers don't.
func UsesDrawer(s Staff) bool { return s.Role != RoleDriver }
