package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// A terminal moves an order's ready time (contract 2.10v): the estimate,
// the prep minutes from accepted_at and an order.status event with the same
// state, in one tx; only while cooking or ready.
func TestSetReadyAt(t *testing.T) {
	r := newFloorRepo(t)
	ctx := context.Background()
	so := accept(t, r, arrive(t, r, "pickup"))
	before := len(eventsOf(t, r))
	at := so.Status.AcceptedAt.Add(42 * time.Minute)
	got, prev, err := r.SetReadyAt(ctx, so.Order.ID, at, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || !prev.Equal(*so.Status.EstimatedReadyAt) {
		t.Fatalf("prev %v, was %v", prev, so.Status.EstimatedReadyAt)
	}
	if got.Status.EstimatedReadyAt == nil || !got.Status.EstimatedReadyAt.Equal(at.UTC().Truncate(time.Millisecond)) ||
		got.Status.PrepMinutes == nil || *got.Status.PrepMinutes != 42 || got.Status.State != order.StateAccepted {
		t.Fatalf("after %+v", got.Status)
	}
	evs := eventsOf(t, r)
	if len(evs) != before+1 || evs[len(evs)-1]["type"] != order.FloorEventOrderStatus || evs[len(evs)-1]["status"] != "accepted" {
		t.Fatalf("events %v", evs[before:])
	}
	// A waiting order (not accepted) can't be moved.
	waiting := arrive(t, r, "pickup")
	if _, _, err := r.SetReadyAt(ctx, waiting.Order.ID, at, time.Now()); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("arrived: %v", err)
	}
	if _, _, err := r.SetReadyAt(ctx, "00000000-0000-0000-0000-000000000000", at, time.Now()); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
