package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"
)

// 2.4.3: staff's actions on a waiting future order (MAKE NOW, CHANGE DUE
// TIME, CANCEL). The due time and MADE NOW stay on the Pi (toStored applies
// them); only a cancel is a floor change main hears about.

// PrepareMakeNow — see order.IntegrationRepository.
func (r *OrderRepository) PrepareMakeNow(ctx context.Context, id string) (err error) {
	ctx, _, end := dbSpan(ctx, "floor.prepare_make_now")
	defer end(&err)
	n, err := models.New(r.db).PrepareMakeNow(ctx, id)
	if err != nil {
		return fmt.Errorf("make now: %w", err)
	}
	if n == 0 {
		return order.ErrInvalidState
	}
	return nil
}

// SetDueOverride — see order.IntegrationRepository.
func (r *OrderRepository) SetDueOverride(ctx context.Context, id string, due time.Time, ringAt *time.Time) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.set_due")
	defer end(&err)
	ring := sql.NullString{}
	if ringAt != nil {
		ring = nullTS(*ringAt)
	}
	q := models.New(r.db)
	n, err := q.SetDueOverride(ctx, models.SetDueOverrideParams{Due: nullTS(due), RingAt: ring, ID: id})
	if err != nil {
		return nil, fmt.Errorf("set due: %w", err)
	}
	if n == 0 {
		return nil, order.ErrInvalidState
	}
	fresh, err := loadOrder(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// CancelWaiting — see order.IntegrationRepository.
func (r *OrderRepository) CancelWaiting(ctx context.Context, id string) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.cancel_waiting")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		n, err := q.CancelWaiting(ctx, models.CancelWaitingParams{CancelledAt: nullTS(now), ID: id})
		if err != nil {
			return fmt.Errorf("cancel: %w", err)
		}
		if n == 0 {
			return order.ErrInvalidState
		}
		if fresh, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		return recordOrderStatus(ctx, q, fresh, false, now)
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}
