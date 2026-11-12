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
