package terminal

import (
	"errors"

	"quiccpos/agent/internal/domain/order"
)

// Reopen a check from All checks (contract/agent.md §2.10ak, batch B, the
// owner 2026-11-14): any cashier, the open batch only; the check goes back to
// the Pickup list (pickup / counter) or Dispatch's New (delivery), as a paid
// takeout when it is paid. Nothing moves: no money, no state, nothing to main.

const (
	// ActionReopen journals a reopen {number, type, state, paid, due_cents}.
	ActionReopen = "check.reopen"
	// EventOrderReopened is the terminals' SSE event {order, device_id}.
	EventOrderReopened = "terminal.order.reopened"
)

// Errors of a reopen (409 each).
var (
	// ErrBatchClosed: the check is on a closed batch (not carried): closed
	// batches stay read-only (409 batch_closed).
	ErrBatchClosed = errors.New("this check is on a closed batch: closed batches stay read-only (reprint and view still work)")
	// ErrReopenVoided: voided at a terminal or cancelled (409 voided).
	ErrReopenVoided = errors.New("a voided or cancelled check can't be reopened")
	// ErrNotReopenable: not placed yet - still ringing (arrived) or a future
	// order (scheduled), which is listed anyway (409 not_reopenable).
	ErrNotReopenable = errors.New("this order isn't placed yet: nothing to reopen")
)

// CheckReopenable says why a check of batch b can't be reopened (nil = it
// can). open = the open batch's id.
func CheckReopenable(so order.StoredOrder, open int64) error {
	switch so.Status.State {
	case order.StateCancelled:
		return ErrReopenVoided
	case order.StateArrived, order.StateScheduled:
		return ErrNotReopenable
	}
	if so.Status.BatchID != open && !so.Status.Carried {
		return ErrBatchClosed
	}
	return nil
}
