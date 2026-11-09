package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// Website / channel orders on the floor (2.5.0, weborder stage 3): ADD TIP,
// the tip lock (main charges the card when the driver clocks out, and at
// Close batch) and CANCEL ORDER. Same floor rule as every transition: one
// transaction that changes the order and writes main's floor event.

// recordBatchClosed appends the batch.closed floor event.
func recordBatchClosed(ctx context.Context, q *models.Queries, batchID int64, at time.Time) error {
	ev := order.BatchClosedEvent{EventID: uuid.NewString(), Type: order.FloorEventBatchClosed, BatchID: batchID,
		At: at.UTC().Truncate(time.Millisecond)}
	return insertEvent(ctx, q, ev.EventID, "", ev.Type, at, ev)
}

// lockShiftTips locks the tips of the deliveries the driver took out during
// the shift (open batch): main charges those cards at the clock-out.
func lockShiftTips(ctx context.Context, q *models.Queries, sh models.DriverShift) ([]string, error) {
	b, err := q.GetOpenBatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("open batch: %w", err)
	}
	ids, err := q.LockShiftTips(ctx, models.LockShiftTipsParams{
		DriverID: sql.NullString{String: sh.DriverID, Valid: true},
		ClockIn:  sql.NullString{String: sh.ClockIn, Valid: true},
		BatchID:  b.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("lock shift tips: %w", err)
	}
	return ids, nil
}

// appendLocked adds the orders whose tip just locked to list (for the
// order.updated SSE), skipping any already in it.
func appendLocked(ctx context.Context, q *models.Queries, list []order.StoredOrder, ids []string) ([]order.StoredOrder, error) {
	have := make(map[string]bool, len(list))
	for _, so := range list {
		have[so.Order.ID] = true
	}
	for _, id := range ids {
		if have[id] {
			continue
		}
		so, err := loadOrder(ctx, q, id)
		if err != nil {
			return nil, err
		}
		list = append(list, so)
	}
	return list, nil
}

// SetAddedTip — see order.FloorRepository.
func (r *OrderRepository) SetAddedTip(ctx context.Context, id string, cents int64) (result *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.set_added_tip")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		so, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		if !so.IsChannel() || so.Order.CashDue() > 0 || so.Status.State == order.StateCancelled || so.Status.BatchID != b.ID {
			return order.ErrNotTippable
		}
		if so.Status.TipLocked {
			return order.ErrTipLocked
		}
		if so.Status.AddedTipCents == cents {
			fresh = so // unchanged: nothing for main
			return nil
		}
		n, err := q.SetAddedTip(ctx, models.SetAddedTipParams{TipCents: cents, ID: id, BatchID: b.ID})
		if err != nil {
			return fmt.Errorf("set added tip: %w", err)
		}
		if n == 0 {
			return order.ErrNotTippable
		}
		if fresh, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		ev := order.OrderTipEvent{EventID: uuid.NewString(), Type: order.FloorEventOrderTip, OrderID: id,
			TipCents: cents, At: now.UTC().Truncate(time.Millisecond)}
		return insertEvent(ctx, q, ev.EventID, id, ev.Type, now, ev)
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// CancelChannelOrder — see order.FloorRepository.
func (r *OrderRepository) CancelChannelOrder(ctx context.Context, id, reason string) (result *order.StoredOrder, prev order.StoredOrder, returnedDriver bool, err error) {
	ctx, _, end := dbSpan(ctx, "floor.cancel_channel_order")
	defer end(&err)
	if !order.ValidCancelReason(reason) {
		return nil, prev, false, fmt.Errorf("reason %q: %w", reason, order.ErrNotCancellable)
	}
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		so, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		prev = so
		if !so.IsChannel() {
			return order.ErrNotCancellable
		}
		n, err := q.CancelChannelOrder(ctx, models.CancelChannelOrderParams{CancelledAt: nullTS(now), Reason: reason, ID: id})
		if err != nil {
			return fmt.Errorf("cancel order: %w", err)
		}
		if n == 0 {
			return order.ErrNotCancellable
		}
		// Out on a run: it leaves the run; the last stop of a run still out
		// brings the driver back into the queue (as for not delivered).
		if so.Status.State == order.StateOnDelivery && so.Status.DriverID != "" {
			drv, err := q.GetDriver(ctx, so.Status.DriverID)
			switch {
			case err == nil:
				if returnedDriver, err = returnIfRunDone(ctx, q, drv, now); err != nil {
					return err
				}
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		if fresh, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		return recordOrderStatus(ctx, q, fresh, false, now)
	})
	if err != nil {
		return nil, prev, false, err
	}
	return &fresh, prev, returnedDriver, nil
}

// returnIfRunDone: a driver out on a run with no stop left on it is back in
// the queue (and clocked out if the daily reset passed meanwhile). Reports
// whether they came back.
func returnIfRunDone(ctx context.Context, q *models.Queries, drv models.Driver, now time.Time) (bool, error) {
	if !drv.Active || drv.Status != order.DriverOut {
		return false, nil
	}
	left, err := q.CountOnDeliveryByDriver(ctx, sql.NullString{String: drv.ID, Valid: true})
	if err != nil || left > 0 {
		return false, err
	}
	pos, err := q.NextDriverPosition(ctx)
	if err != nil {
		return false, err
	}
	if err := q.SetDriverReturned(ctx, models.SetDriverReturnedParams{LastReturnedAt: nullTS(now), Position: pos, ID: drv.ID}); err != nil {
		return false, err
	}
	if err := clockOutIfResetPassed(ctx, q, drv.ID, now); err != nil {
		return false, err
	}
	return true, recordRoster(ctx, q, now)
}
