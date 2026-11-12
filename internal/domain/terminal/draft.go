package terminal

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// Shared drafts (contract/agent.md §2.10i, TERMINAL-PLAN "Suspend / hold"):
// an order put aside (held) or open on one terminal, kept on the Pi so both
// terminals see it. Soft lock: a draft open on one terminal answers the
// other 409 open_elsewhere until it takes it over (TAKE OVER).

// SSE events for the shared drafts and the Pickup list.
const (
	EventDraftChanged  = "terminal.draft.changed"
	EventDraftRemoved  = "terminal.draft.removed"
	EventDraftTaken    = "terminal.draft.taken"
	EventOrderPickedUp = "terminal.order.picked_up"
)

// Limits on a draft.
const (
	MaxDraftBytes = 64 << 10 // the terminal's own JSON
	MaxDrafts     = 100      // per store
	MaxDraftName  = 80
)

// Holder is the terminal that has a draft open (and who is on it).
type Holder struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	StaffID    string `json:"staff_id"`
	StaffName  string `json:"staff_name"`
}

// Draft is one shared draft. Draft (the order itself) is the terminal's own
// JSON object, opaque to the Pi. Holder is nil while it is held (put aside:
// nobody has it open). StaffID / StaffName: who last put it (held under
// them, or open by them).
type Draft struct {
	ID         string          `json:"id"`
	LocationID string          `json:"-"`
	Type       string          `json:"type"`
	Name       string          `json:"name"`
	Held       bool            `json:"held"`
	Holder     *Holder         `json:"holder"`
	StaffID    string          `json:"staff_id"`
	StaffName  string          `json:"staff_name"`
	OrderID    string          `json:"order_id,omitempty"`
	Draft      json.RawMessage `json:"draft"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// OpenElsewhere: another terminal has the draft open (409 open_elsewhere
// {holder}); TAKE OVER sends take_over: true.
type OpenElsewhere struct{ Holder Holder }

func (e *OpenElsewhere) Error() string {
	name := e.Holder.DeviceName
	if name == "" {
		name = "another terminal"
	}
	return fmt.Sprintf("this order is open on %s", name)
}

// ErrTooManyDrafts: the store has MaxDrafts drafts already.
var ErrTooManyDrafts = errors.New("too many orders put aside: send or clear some first")

// DraftPut is one PUT: who sends it and what it says.
type DraftPut struct {
	By       Holder
	Held     bool
	TakeOver bool
}

// Decide applies a PUT to the stored draft (nil = new) under the soft lock:
// a draft nobody has open (held, or new) or open on the same terminal is
// free; one open on another terminal needs TakeOver. It answers the new
// holder (nil = held) and the holder it was taken from (nil = none).
func (p DraftPut) Decide(cur *Draft) (holder *Holder, taken *Holder, err error) {
	if cur != nil && cur.Holder != nil && cur.Holder.DeviceID != p.By.DeviceID {
		if !p.TakeOver {
			return nil, nil, &OpenElsewhere{Holder: *cur.Holder}
		}
		h := *cur.Holder
		taken = &h
	}
	if !p.Held {
		h := p.By
		holder = &h
	}
	return holder, taken, nil
}

// CheckDelete: a draft open on another terminal can't be deleted from here.
func CheckDelete(cur *Draft, deviceID string) error {
	if cur != nil && cur.Holder != nil && cur.Holder.DeviceID != deviceID {
		return &OpenElsewhere{Holder: *cur.Holder}
	}
	return nil
}

// --- the Pickup list --------------------------------------------------------------

// FinishedByTerminal is Status.FinishedBy of an order a terminal marked
// picked up (POST /api/terminal/orders/{id}/picked-up).
const FinishedByTerminal = "terminal"

// Errors of the Pickup list.
var (
	ErrNotPickable = errors.New("this order can't be marked picked up (a pickup or counter order of the open batch, accepted or ready)")
)

// CashDue: the order still has money to collect (409 cash_due).
type CashDue struct{ BalanceCents int64 }

func (e *CashDue) Error() string {
	return fmt.Sprintf("take the payment first: %d.%02d is due", e.BalanceCents/100, e.BalanceCents%100)
}

// IsPickup: a pickup / counter order (any source), not a delivery.
func IsPickup(so order.StoredOrder) bool {
	return !so.IsDelivery()
}

// DueOf is what is still to collect on an order at the counter: a counter
// order's balance, an online order's cash due (0 once the terminal took it).
func DueOf(so order.StoredOrder) int64 {
	if so.Order.Source != order.SourceCounter && so.Status.Paid {
		return 0
	}
	return max(so.Status.CashDueCents, 0)
}

// OnPickupList: a pickup of the open batch (or carried) that isn't done with -
// not voided / cancelled, accepted, ready or scheduled (an online order still
// ringing isn't on it). The owner, 2026-11-12:
//   - a counter / phone pickup (source counter) is on it while it owes money:
//     fully paid = off the list (no Picked Up key on the terminal);
//   - an online / website pickup is on it while it owes money or isn't
//     finished: a prepaid one shows too (the terminal tones and sorts them), and
//     leaves when it is finished - picked up (POST .../picked-up), the store's
//     finish time after ready (settings.finishing.online_pickup_minutes) or the
//     day's close.
func OnPickupList(so order.StoredOrder) bool {
	if !IsPickup(so) || !OnPickupStates(so) {
		return false
	}
	if so.Order.Source == order.SourceCounter {
		return DueOf(so) > 0
	}
	return DueOf(so) > 0 || so.Status.FinishedAt == nil
}

// OnlinePayableStates: an online / website order takes money at a terminal
// (its balance after an edit, a cash pickup's total, §2.10aa) while it is
// accepted, ready or scheduled; a delivery also once out or delivered (the
// balance collected after the run).
func OnlinePayableStates(so order.StoredOrder) bool {
	if OnPickupStates(so) {
		return true
	}
	return so.IsDelivery() && (so.Status.State == order.StateOnDelivery || so.Status.State == order.StateDelivered)
}

// OnPickupStates: accepted, ready or scheduled (the states a pickup is on
// the list in; an online order still ringing is not).
func OnPickupStates(so order.StoredOrder) bool {
	switch so.Status.State {
	case order.StateAccepted, order.StateReady, order.StateScheduled:
		return true
	}
	return false
}
