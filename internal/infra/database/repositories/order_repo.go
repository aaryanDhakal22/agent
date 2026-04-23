package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// compile-time interface check
var _ order.Repository = (*OrderRepository)(nil)

type OrderRepository struct {
	pool   *pgxpool.Pool
	logger zerolog.Logger
}

func NewOrderRepository(pool *pgxpool.Pool, lg zerolog.Logger) *OrderRepository {
	return &OrderRepository{
		pool:   pool,
		logger: lg.With().Str("module", "order-repo").Logger(),
	}
}

func (r *OrderRepository) UpsertArrived(ctx context.Context, o order.OrderRequest) error {
	payload, err := json.Marshal(o)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	q := models.New(r.pool)
	return q.UpsertArrivedOrder(ctx, models.UpsertArrivedOrderParams{
		OrderID: int32(o.OrderID),
		Payload: payload,
	})
}

func (r *OrderRepository) GetByID(ctx context.Context, id int) (*order.StoredOrder, error) {
	q := models.New(r.pool)
	row, err := q.GetOrderByID(ctx, int32(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, order.ErrNotFound
		}
		return nil, fmt.Errorf("get order: %w", err)
	}
	so, err := toStored(row)
	if err != nil {
		return nil, err
	}
	return &so, nil
}

func (r *OrderRepository) ListPage(ctx context.Context, offset, limit int) ([]order.StoredOrder, error) {
	q := models.New(r.pool)
	rows, err := q.ListOrdersPage(ctx, models.ListOrdersPageParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	return mapRows(rows)
}

func (r *OrderRepository) ListArrived(ctx context.Context) ([]order.StoredOrder, error) {
	q := models.New(r.pool)
	rows, err := q.ListArrivedOrders(ctx)
	if err != nil {
		return nil, fmt.Errorf("list arrived: %w", err)
	}
	return mapRows(rows)
}

// AcceptAndPrint runs the DB state transition and the physical print inside a
// single transaction. If `print` returns an error, we roll back — the order
// remains in the arrived state and can be retried. This is the contract the
// user asked for: no silent lost prints when the printer is unreachable.
//
// A row-level FOR UPDATE lock serializes concurrent accepts on the same order
// (e.g. mobile double-tap). Duration of the open tx is bounded by the
// printer's 10s print-timeout, which is acceptable at restaurant order rate.
func (r *OrderRepository) AcceptAndPrint(
	ctx context.Context,
	id int,
	print func(ctx context.Context, o order.OrderRequest) error,
) (*order.StoredOrder, error) {
	log := r.logger.With().Int("order_id", id).Logger()
	log.Debug().Ctx(ctx).Msg("accept+print: beginning transaction")

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — idempotent after Commit

	q := models.New(tx)

	row, err := q.GetOrderByIDForUpdate(ctx, int32(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, order.ErrNotFound
		}
		return nil, fmt.Errorf("lock row: %w", err)
	}

	if row.State == models.OrderStateAccepted {
		log.Info().Ctx(ctx).Msg("accept+print: order already accepted, skipping")
		return nil, order.ErrAlreadyAccepted
	}

	stored, err := toStored(row)
	if err != nil {
		return nil, err
	}

	// Mark accepted first so the DB write is committed atomically with the
	// physical print. If the print fails, the whole tx rolls back and the
	// order's state remains 'arrived' for a retry.
	if err := q.MarkAccepted(ctx, int32(id)); err != nil {
		return nil, fmt.Errorf("mark accepted: %w", err)
	}

	if err := print(ctx, stored.Order); err != nil {
		log.Warn().Ctx(ctx).Err(err).Msg("accept+print: printer failed, rolling back state")
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	// Re-read so the caller sees the new state + printed_date that the tx set.
	fresh, err := r.GetByID(ctx, id)
	if err != nil {
		// Committed but re-read failed — return a best-effort view.
		log.Warn().Ctx(ctx).Err(err).Msg("accept+print: committed but re-read failed")
		now := time.Now()
		stored.Status.State = order.StateAccepted
		stored.Status.PrintedDate = &now
		return &stored, nil
	}
	return fresh, nil
}

func (r *OrderRepository) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	q := models.New(r.pool)
	return q.DeleteOlderThan(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
}

// --- helpers --------------------------------------------------------------

func toStored(row models.Order) (order.StoredOrder, error) {
	var o order.OrderRequest
	if err := json.Unmarshal(row.Payload, &o); err != nil {
		return order.StoredOrder{}, fmt.Errorf("unmarshal payload for order %d: %w", row.OrderID, err)
	}
	s := order.Status{
		State:       order.State(row.State),
		ArrivalDate: row.ArrivalDate.Time,
	}
	if row.PrintedDate.Valid {
		t := row.PrintedDate.Time
		s.PrintedDate = &t
	}
	return order.StoredOrder{Order: o, Status: s}, nil
}

func mapRows(rows []models.Order) ([]order.StoredOrder, error) {
	out := make([]order.StoredOrder, 0, len(rows))
	for _, row := range rows {
		so, err := toStored(row)
		if err != nil {
			return nil, err
		}
		out = append(out, so)
	}
	return out, nil
}
