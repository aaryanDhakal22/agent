package repositories

// Card tips the driver types before Close batch (contract/agent.md §2.10am,
// batch F): table driver_tips (migration 00046). Hand-written SQL (no sqlc
// queries) so this batch's work stays out of the shared generated files.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"
)

const driverTipCols = `order_id, batch_id, driver_id, state, tip_cents, applied_cents, set_at, set_by, applied_at,
	square_state, square_error, square_payment_id`


func scanDriverTip(r rowScanner) (order.DriverTip, error) {
	var t order.DriverTip
	var setAt string
	var applied sql.NullString
	if err := r.Scan(&t.OrderID, &t.BatchID, &t.DriverID, &t.State, &t.TipCents, &t.AppliedCents, &setAt, &t.SetBy, &applied,
		&t.SquareState, &t.SquareError, &t.SquarePaymentID); err != nil {
		return t, err
	}
	at, err := parseTS(setAt)
	if err != nil {
		return t, err
	}
	t.SetAt = at
	if t.AppliedAt, err = optTS(applied); err != nil {
		return t, err
	}
	return t, nil
}

// openBatchID is the open batch's id.
func openBatchID(ctx context.Context, q *models.Queries) (int64, error) {
	b, err := q.GetOpenBatch(ctx)
	if err != nil {
		return 0, fmt.Errorf("open batch: %w", err)
	}
	return b.ID, nil
}

// DriverTipsOfBatch is every typed tip of a batch (0 = the open one), by order id.
func (r *OrderRepository) DriverTipsOfBatch(ctx context.Context, batchID int64) (out map[string]order.DriverTip, err error) {
	ctx, _, end := dbSpan(ctx, "driver_tips.of_batch")
	defer end(&err)
	if batchID == 0 {
		if batchID, err = openBatchID(ctx, models.New(r.db)); err != nil {
			return nil, err
		}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+driverTipCols+` FROM driver_tips WHERE batch_id = ?`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out = map[string]order.DriverTip{}
	for rows.Next() {
		t, err := scanDriverTip(rows)
		if err != nil {
			return nil, err
		}
		out[t.OrderID] = t
	}
	return out, rows.Err()
}

// DriverTipOf is one order's typed tip (nil = none saved).
func (r *OrderRepository) DriverTipOf(ctx context.Context, orderID string) (t *order.DriverTip, err error) {
	ctx, _, end := dbSpan(ctx, "driver_tips.get")
	defer end(&err)
	got, err := scanDriverTip(r.db.QueryRowContext(ctx, `SELECT `+driverTipCols+` FROM driver_tips WHERE order_id = ?`, orderID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &got, nil
}

// SaveDriverTip saves a tippable card delivery's tip state (tipped with
// cents, no_tip, none). The order must be TipTakable on the open batch:
// order.ErrTipBatchClosed (another batch), order.ErrTipLocked (a website
// card order main charged), order.ErrNotTippable (anything else). What a
// press of Close batch applied so far is kept (the next press puts the
// difference). prev = the state before (zero when none was saved).
func (r *OrderRepository) SaveDriverTip(ctx context.Context, orderID, state string, cents int64, setBy string) (so *order.StoredOrder, saved, prev order.DriverTip, err error) {
	ctx, _, end := dbSpan(ctx, "driver_tips.save")
	defer end(&err)
	if state != order.TipStateTipped {
		cents = 0
	}
	now := time.Now()
	err = r.withTxRaw(ctx, func(tx *sql.Tx, q *models.Queries) error {
		cur, err := loadOrder(ctx, q, orderID)
		if err != nil {
			return err
		}
		b, err := openBatchID(ctx, q)
		if err != nil {
			return err
		}
		switch {
		case cur.Status.BatchID != b:
			return order.ErrTipBatchClosed
		case cur.IsChannel() && cur.Order.Source != order.SourceCounter && cur.Status.TipLocked:
			return order.ErrTipLocked
		case !order.TipTakable(cur, b):
			return order.ErrNotTippable
		}
		so = &cur
		old, err := scanDriverTip(tx.QueryRowContext(ctx, `SELECT `+driverTipCols+` FROM driver_tips WHERE order_id = ?`, orderID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			prev = old
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO driver_tips (order_id, batch_id, driver_id, state, tip_cents, set_at, set_by)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (order_id) DO UPDATE SET batch_id = excluded.batch_id, driver_id = excluded.driver_id,
				state = excluded.state, tip_cents = excluded.tip_cents, set_at = excluded.set_at, set_by = excluded.set_by`,
			orderID, b, cur.Status.DriverID, state, cents, fmtTS(now), setBy)
		if err != nil {
			return fmt.Errorf("save driver tip: %w", err)
		}
		saved, err = scanDriverTip(tx.QueryRowContext(ctx, `SELECT `+driverTipCols+` FROM driver_tips WHERE order_id = ?`, orderID))
		return err
	})
	return so, saved, prev, err
}

// PendingDriverTips are the open batch's typed tips not on their orders yet
// (target != applied): driverID "" = every driver's.
func (r *OrderRepository) PendingDriverTips(ctx context.Context, driverID string) (out []order.DriverTip, err error) {
	ctx, _, end := dbSpan(ctx, "driver_tips.pending")
	defer end(&err)
	b, err := openBatchID(ctx, models.New(r.db))
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+driverTipCols+` FROM driver_tips
		WHERE batch_id = ? AND (? = '' OR driver_id = ?)
		  AND (CASE WHEN state = 'tipped' THEN tip_cents ELSE 0 END) <> applied_cents
		ORDER BY set_at`, b, driverID, driverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanDriverTip(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DriverTipApplied records what a press of Close batch put on the order
// (applied = the typed target now on it) and how Square took it.
func (r *OrderRepository) DriverTipApplied(ctx context.Context, orderID string, applied int64, squareState, squareErr, squarePaymentID string) (err error) {
	ctx, _, end := dbSpan(ctx, "driver_tips.applied")
	defer end(&err)
	_, err = r.db.ExecContext(ctx, `UPDATE driver_tips SET applied_cents = ?, applied_at = ?, square_state = ?, square_error = ?,
		square_payment_id = CASE WHEN ? <> '' THEN ? ELSE square_payment_id END WHERE order_id = ?`,
		applied, fmtTS(time.Now()), squareState, squareErr, squarePaymentID, squarePaymentID, orderID)
	return err
}

// DriverTipSquare records the Square tip adjustment's outcome.
func (r *OrderRepository) DriverTipSquare(ctx context.Context, orderID, state, errText string) (err error) {
	ctx, _, end := dbSpan(ctx, "driver_tips.square")
	defer end(&err)
	_, err = r.db.ExecContext(ctx, `UPDATE driver_tips SET square_state = ?, square_error = ? WHERE order_id = ?`, state, errText, orderID)
	return err
}

// withTxRaw runs fn in one transaction with both the raw tx (this file's
// hand-written SQL) and the sqlc queries on it.
func (r *OrderRepository) withTxRaw(ctx context.Context, fn func(tx *sql.Tx, q *models.Queries) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(tx, models.New(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// claimOld makes an old undelivered delivery (#27, §2.10am) the claimer's
// and delivered in the claim's transaction: off any pool or run, delivered
// now. 0 rows = it isn't an undelivered delivery of the batch any more.
func claimOld(ctx context.Context, tx *sql.Tx, id string, batchID int64, driverID, driverName, marked string, at time.Time) (int64, error) {
	res, err := tx.ExecContext(ctx, `UPDATE orders
		SET state = 'delivered', delivered_at = ?, marked_delivered = ?, driver_id = ?, driver_name = ?,
		    run_seq = NULL, came_back = false, pool_driver_id = NULL, pooled_at = NULL, pool_seq = NULL, pool_plan_id = NULL
		WHERE id = ? AND batch_id = ? AND state IN ('accepted', 'ready', 'on_delivery')
		  AND json_extract(snapshot, '$.fulfillment.type') = 'delivery'`,
		fmtTS(at), marked, driverID, driverName, id, batchID)
	if err != nil {
		return 0, fmt.Errorf("claim old delivery: %w", err)
	}
	return res.RowsAffected()
}
