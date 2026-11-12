package order

import (
	"time"

	"quiccpos/agent/internal/domain/order"
)

// makeAt is when the kitchen starts a future order (the kitchen ticket's
// MAKE AT): its re-ring time, else promised_at less its lead, else less its
// prep minutes. nil for an order with no promised time (or nothing to count
// back from).
func makeAt(so order.StoredOrder) *time.Time {
	p := so.Order.Fulfillment.PromisedAt
	if p == nil || p.IsZero() {
		return nil
	}
	var t time.Time
	switch st := so.Status; {
	case st.RingAt != nil:
		t = *st.RingAt
	case st.ScheduledLeadMinutes != nil:
		t = p.Add(-time.Duration(*st.ScheduledLeadMinutes) * time.Minute)
	case st.PrepMinutes != nil:
		t = p.Add(-time.Duration(*st.PrepMinutes) * time.Minute)
	default:
		return nil
	}
	return &t
}
