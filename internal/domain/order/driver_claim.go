package order

import "time"

// The terminal driver view's Dispatch section (contract/agent.md §2.10b):
// a driver assigns himself deliveries nobody took. Not a send-out: the
// orders become his, delivered, and he never goes out.

// ByTerminal: a driver did it himself on a terminal (MarkedBy.Kind).
const ByTerminal = "terminal"

// DefaultClaimAfterMinutes is how long a delivery waits unassigned before a
// driver may claim it (main's terminal setting driver_claim_after_minutes).
const DefaultClaimAfterMinutes = 45

// ClaimSince is the clock the claim rule counts from: when the order went to
// the kitchen (accepted_at: a counter order at SEND, an online order at
// ACCEPT, a future order when its kitchen work starts), else when it reached
// the Pi. "An order that has been around 45 minutes" is 45 minutes past this.
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

// ClaimableLate: Unclaimed and waiting since at or before cutoff (now - the
// store's driver_claim_after_minutes).
func ClaimableLate(so StoredOrder, batchID int64, cutoff time.Time) bool {
	return Unclaimed(so, batchID) && !ClaimSince(so).After(cutoff)
}
