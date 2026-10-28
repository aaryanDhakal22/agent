package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"quiccpos/agent/internal/domain/order"
)

func TestUndoReady(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newPoolService(t)
	id := readyDelivery(t, svc, 10*time.Minute)

	so, err := svc.UndoReady(ctx, id)
	if err != nil || so.Status.State != order.StateAccepted || so.Status.ReadyAt != nil {
		t.Fatalf("undo: %v %+v", err, so.Status)
	}
	// Not ready any more: nothing to undo.
	if _, err := svc.UndoReady(ctx, id); !errors.Is(err, ErrUndoExpired) {
		t.Fatalf("second undo: %v", err)
	}
	// READY again works as before.
	if _, err := svc.Ready(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Too long ago: the window is checked on the Pi.
	if _, err := repo.UndoReady(ctx, id, time.Now().Add(time.Minute)); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("expired window: %v", err)
	}
	if _, err := svc.UndoReady(ctx, uuid.NewString()); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown order: %v", err)
	}
}

// A counter check the POS already closed is finished the moment it is READY
// (ticket 232, 2026-10-27): UNDO brings it back to Kitchen, not finished.
func TestUndoReadyOfAClosedPOSCheck(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	e := extOrder("dine_in", "a")
	e.Paid, e.CashDueCents = true, 0
	res := mustUpsert(t, svc, "232", e)
	e.Hash, e.Closed = "b", true
	mustUpsert(t, svc, "232", e)
	id := res.OrderID
	so, err := svc.Ready(ctx, id)
	if err != nil || so.Status.FinishedAt == nil {
		t.Fatalf("ready finishes a closed check: %v %+v", err, so.Status)
	}
	so, err = svc.UndoReady(ctx, id)
	if err != nil || so.Status.State != order.StateAccepted || so.Status.FinishedAt != nil || so.Status.FinishedBy != "" {
		t.Fatalf("undo: %v %+v", err, so.Status)
	}
}
