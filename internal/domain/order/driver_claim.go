package order

import "time"

// The terminal driver view's Dispatch section (contract/agent.md §2.10b):
// a driver assigns himself deliveries nobody took. Not a send-out: the
// orders become his, delivered, and he never goes out.

// ByTerminal: a driver did it himself on a terminal (MarkedBy.Kind).
const ByTerminal = "terminal"

// ClaimSince is when the order went to the kitchen (accepted_at: a counter
// order at SEND, an online order at ACCEPT, a future order when its kitchen
// work starts), else when it reached the Pi: the Dispatch list's waiting
// time, and the claim rule's base when an order has no promised time.
func ClaimSince(so StoredOrder) time.Time {
	if so.Status.AcceptedAt != nil {
		return *so.Status.AcceptedAt
	}
	return so.Status.ArrivalDate
}

// Unclaimed: a delivery of the batch with no driver and in no pool -
// cooking, ready, or delivered with no driver (MARK DELIVERED). Not one held
// after coming back (staff decide that one), never an unaccepted, scheduled,
// out or cancelled one.
func Unclaimed(so StoredOrder, batchID int64) bool {
	st := so.Status
	if !so.IsDelivery() || st.BatchID != batchID || st.DriverID != "" || st.PoolDriverID != "" || st.CameBack {
		return false
	}
	switch st.State {
	case StateAccepted, StateReady, StateDelivered:
		return true
	}
	return false
}

// ClaimRule is the claim rule's clock (the owner, 2026-11-12: a driver may
// claim a delivery nobody took once it is past the order's promised time,
// not after a fixed wait). Fallback: how long after ClaimSince an order with
// no promised time becomes claimable (the Pi's default delivery minutes).
type ClaimRule struct {
	Now      time.Time
	Fallback time.Duration
	// OldAfter (#27, batch F, contract 2.10am): a delivery still not
	// delivered this long after it went to the kitchen is old - every driver
	// may claim it, whoever has it (OldDelivery); 0 = off.
	OldAfter time.Duration
}

// Claimable: ClaimableLate, or an old undelivered delivery (OldDelivery).
func Claimable(so StoredOrder, batchID int64, rule ClaimRule) bool {
	return ClaimableLate(so, batchID, rule) || OldDelivery(so, batchID, rule.Now, rule.OldAfter)
}

// PromisedAt is the order's promised time on the Pi: the later of its
// estimated_ready_at (accept / SEND + the quoted minutes; a future order's
// due time) and the customer's promised_at; neither = ClaimSince + fallback.
func PromisedAt(so StoredOrder, fallback time.Duration) time.Time {
	var at time.Time
	if t := so.Status.EstimatedReadyAt; t != nil {
		at = *t
	}
	if t := so.Order.Fulfillment.PromisedAt; t != nil && t.After(at) {
		at = *t
	}
	if at.IsZero() {
		at = ClaimSince(so).Add(fallback)
	}
	return at
}

// ClaimableLate: Unclaimed and past its promised time.
func ClaimableLate(so StoredOrder, batchID int64, rule ClaimRule) bool {
	return Unclaimed(so, batchID) && !rule.Now.Before(PromisedAt(so, rule.Fallback))
}
