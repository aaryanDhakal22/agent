package order

import (
	"context"
	"time"
)

// Repository is the persistence port for orders. Kept in the domain layer so
// the application service depends only on the interface; the pgx-backed
// implementation lives in infra.
type Repository interface {
	// UpsertArrived inserts a freshly-arrived order. Duplicates (same OrderID)
	// are silently ignored — the existing row is preserved.
	UpsertArrived(ctx context.Context, o OrderRequest) error

	// GetByID returns the stored order with its current status. Returns
	// ErrNotFound if no row matches.
	GetByID(ctx context.Context, id int) (*StoredOrder, error)

	// ListPage returns orders newest-first for the mobile history view.
	ListPage(ctx context.Context, offset, limit int) ([]StoredOrder, error)

	// ListArrived returns every order currently awaiting acceptance,
	// oldest-first (the queue order mobile should display).
	ListArrived(ctx context.Context) ([]StoredOrder, error)

	// AcceptAndPrint transitions an order from arrived→accepted, running
	// `print` inside the same DB transaction. If `print` returns an error,
	// the transaction rolls back and the order stays arrived — so a printer
	// outage doesn't cause silent lost prints.
	//
	// Returns ErrNotFound if no row, ErrAlreadyAccepted if already accepted.
	AcceptAndPrint(ctx context.Context, id int, print func(ctx context.Context, o OrderRequest) error) (*StoredOrder, error)

	// DeleteOlderThan removes orders whose arrival_date is before the cutoff.
	// Returns the number of rows deleted.
	DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
}

// SettingsRepository owns the singleton settings row. Split from Repository so
// callers that only care about the toggle don't depend on the orders API.
type SettingsRepository interface {
	GetAutoAccept(ctx context.Context) (bool, error)
	SetAutoAccept(ctx context.Context, enabled bool) error
}
