package repositories

import (
	"context"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"
)

// ReopenCheck reopens a check from All checks (contract 2.10ak, batch B):
// reopened_at = now and the journal rows in one transaction, after
// terminal.CheckReopenable against the open batch. Nothing else moves.
func (r *OrderRepository) ReopenCheck(ctx context.Context, id string, now time.Time, journal func(cur order.StoredOrder) []terminal.Entry) (so order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.reopen")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		if err := terminal.CheckReopenable(cur, b.ID); err != nil {
			return err
		}
		n, err := q.SetOrderReopened(ctx, models.SetOrderReopenedParams{ReopenedAt: nullTS(now), ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return order.ErrNotFound
		}
		if so, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		return insertJournal(ctx, q, now, journal(cur))
	})
	return so, err
}
