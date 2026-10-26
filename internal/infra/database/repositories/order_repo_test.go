package repositories

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database"
	"quiccpos/agent/internal/migrate"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// TestOrderRepositoryRoundTrip exercises the pure-Go SQLite path end-to-end:
// migrations apply to a fresh file, an order upserts + reads back, and the
// floor columns read back.
func TestOrderRepositoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")

	if err := migrate.Run(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	repo := NewOrderRepository(db, zerolog.Nop())

	newSnap := func() order.Snapshot {
		return order.Snapshot{
			ID:            uuid.NewString(),
			TenantID:      uuid.NewString(),
			LocationID:    uuid.NewString(),
			Source:        "test",
			SourceEventID: uuid.NewString(),
			PlacedAt:      time.Now().UTC(),
		}
	}

	first := newSnap()
	if _, err := repo.UpsertArrived(ctx, first); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := repo.GetByID(ctx, uuid.MustParse(first.ID))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Order.ID != first.ID {
		t.Fatalf("round-trip id mismatch: got %s want %s", got.Order.ID, first.ID)
	}
	if got.Status.State != order.StateArrived {
		t.Fatalf("expected arrived state, got %s", got.Status.State)
	}
	if got.Status.ArrivalDate.IsZero() {
		t.Fatal("arrival_date did not parse")
	}

}

// TestClaimAndRevertAccept covers the claim-then-print accept path: a claim
// wins once, a second claim loses, a revert makes it claimable again.
func TestClaimAndRevertAccept(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "agent.db")
	if err := migrate.Run(ctx, dbPath); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	repo := NewOrderRepository(db, zerolog.Nop())

	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
		Source: "test", SourceEventID: "1", PlacedAt: time.Now().UTC()}
	if _, err := repo.UpsertArrived(ctx, s); err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(s.ID)

	so, err := repo.ClaimAccept(ctx, id, 20, false)
	if err != nil || so.Status.State != order.StateAccepted || so.Status.PrintedDate == nil {
		t.Fatalf("claim: %v %+v", err, so)
	}
	if _, err := repo.ClaimAccept(ctx, id, 20, false); err != order.ErrAlreadyAccepted {
		t.Fatalf("second claim: want ErrAlreadyAccepted, got %v", err)
	}
	if err := repo.RevertAccept(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(ctx, id)
	if got.Status.State != order.StateArrived || got.Status.PrintedDate != nil {
		t.Fatalf("revert: %+v", got.Status)
	}
	if _, err := repo.ClaimAccept(ctx, id, 20, false); err != nil {
		t.Fatalf("re-claim after revert: %v", err)
	}
	if _, err := repo.ClaimAccept(ctx, uuid.New(), 20, false); err != order.ErrNotFound {
		t.Fatalf("missing order: want ErrNotFound, got %v", err)
	}
}
