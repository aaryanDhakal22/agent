package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"
)

// SetReadyAt moves an accepted / ready order's ready time (a terminal's
// pickup ready time or delivery time change, contract 2.10v): the estimate,
// the prep minutes it implies (from when it was accepted) and an
// order.status floor event with the same state, in one transaction. prev =
// the ready time before (nil when none). order.ErrInvalidState for any other
// state.
func (r *OrderRepository) SetReadyAt(ctx context.Context, id string, at, now time.Time) (so *order.StoredOrder, prev *time.Time, err error) {
	ctx, _, end := dbSpan(ctx, "floor.set_ready_at")
	defer end(&err)
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		prev = cur.Status.EstimatedReadyAt
		from := now
		if cur.Status.AcceptedAt != nil {
			from = *cur.Status.AcceptedAt
		}
		prep := int64(math.Ceil(at.Sub(from).Minutes()))
		prep = max(prep, 0)
		n, err := q.SetReadyAt(ctx, models.SetReadyAtParams{EstimatedReadyAt: nullTS(at), PrepMinutes: sql.NullInt64{Int64: prep, Valid: true}, ID: id})
		if err != nil {
			return fmt.Errorf("set ready at: %w", err)
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
		return nil, nil, err
	}
	return &fresh, prev, nil
}
