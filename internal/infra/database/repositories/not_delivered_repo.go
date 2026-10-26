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

// Not delivered (2.2.1): a delivery that came back with its driver. Same
// floor rule as every transition: one transaction that changes the order,
// the driver and writes main's floor event + the hub's feed event.

// recordNotDelivered appends the order.not_delivered floor event (never for
// a local-only POS order).
func recordNotDelivered(ctx context.Context, q *models.Queries, so order.StoredOrder, action, driverID, driverName string, auto bool, at time.Time) error {
	if so.Status.LocalOnly {
		return nil
	}
	st := so.Status
	ev := order.NotDeliveredEvent{EventID: uuid.NewString(), Type: order.FloorEventNotDelivered, OrderID: so.Order.ID,
		Action: action, Status: so.Status.State, DriverID: driverID, DriverName: driverName, Auto: auto,
		At: at.UTC().Truncate(time.Millisecond), BatchID: so.Status.BatchID, AgentStatus: &st}
	return insertEvent(ctx, q, ev.EventID, so.Order.ID, ev.Type, at, ev)
}

// NotDelivered — see order.FloorRepository.
func (r *OrderRepository) NotDelivered(ctx context.Context, id, action string) (result *order.StoredOrder, returnedDriver bool, err error) {
	ctx, _, end := dbSpan(ctx, "floor.not_delivered")
	defer end(&err)
	if action != order.NotDeliveredRetry && action != order.NotDeliveredCancel {
		return nil, false, fmt.Errorf("action %q: %w", action, order.ErrInvalidState)
	}
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
		st := so.Status.State
		if !so.IsDelivery() || so.Status.BatchID != b.ID || (st != order.StateOnDelivery && st != order.StateDelivered) {
			return order.ErrNotOutOrDelivered
		}
		fromID, fromName := so.Status.DriverID, so.Status.DriverName
		p := models.MarkNotDeliveredParams{State: string(order.StateReady), CameBack: true, Action: action,
			At: nullTS(now), FromDriverID: fromID, ID: id}
		if action == order.NotDeliveredCancel {
			p.State, p.CameBack, p.CancelledAt = string(order.StateCancelled), false, nullTS(now)
		}
		n, err := q.MarkNotDelivered(ctx, p)
		if err != nil {
			return fmt.Errorf("mark not delivered: %w", err)
		}
		if n == 0 {
			return order.ErrNotOutOrDelivered
		}

		// The driver: still out with other stops → stays out; this was the
		// last stop of a run still out → back in the queue (they came back
		// with it); already returned → nothing (the order just leaves that
		// finished run).
		var drv models.Driver
		hasDriver := false
		if fromID != "" {
			if d, err := q.GetDriver(ctx, fromID); err == nil {
				drv, hasDriver = d, true
				if fromName == "" {
					fromName = d.Name
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if hasDriver && st == order.StateOnDelivery && drv.Active && drv.Status == order.DriverOut {
			left, err := q.CountOnDeliveryByDriver(ctx, sql.NullString{String: drv.ID, Valid: true})
			if err != nil {
				return err
			}
			if left == 0 {
				pos, err := q.NextDriverPosition(ctx)
				if err != nil {
					return err
				}
				if err := q.SetDriverReturned(ctx, models.SetDriverReturnedParams{LastReturnedAt: nullTS(now), Position: pos, ID: drv.ID}); err != nil {
					return err
				}
				if err := clockOutIfResetPassed(ctx, q, drv.ID, now); err != nil {
					return err
				}
				if err := recordRoster(ctx, q, now); err != nil {
					return err
				}
				returnedDriver = true
			}
		}

		// A POS check: forget the driver number the POS showed, so a later
		// assignment at the terminal mirrors again — except the very number
		// it came back from while the POS still shows it (the hub clears it
		// from order.not_delivered below; see order.CameBackMark).
		var posNum *string
		if hasDriver && drv.PosNumber != "" {
			v := drv.PosNumber
			posNum = &v
		}
		if so.IsPOS() {
			row, err := q.GetOrderByID(ctx, id)
			if err != nil {
				return err
			}
			if posNum == nil && row.ExtDriverPos != "" && row.ExtDriverPos[:1] != order.CameBackMark {
				v := row.ExtDriverPos // the driver has no number now: the one the POS showed
				posNum = &v
			}
			mark := ""
			if posNum != nil {
				mark = order.CameBackMark + *posNum
			}
			if err := q.SetExtDriverPos(ctx, models.SetExtDriverPosParams{ExtDriverPos: mark, PosDriverNote: "", ID: id}); err != nil {
				return err
			}
		}

		if fresh, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		if err := recordNotDelivered(ctx, q, fresh, action, fromID, fromName, false, now); err != nil {
			return err
		}
		return insertFeed(ctx, q, order.FeedOrderNotDelivered, now, order.OrderNotDeliveredData{
			FeedOrderRef: order.RefOf(fresh), Action: action, DriverID: fromID, DriverName: fromName, DriverPosNumber: posNum})
	})
	if err != nil {
		return nil, false, err
	}
	return &fresh, returnedDriver, nil
}

// ReleaseCameBack — see order.FloorRepository. main hears the release as an
// order.status with the same state (its agent_status changes).
func (r *OrderRepository) ReleaseCameBack(ctx context.Context, id string) (result *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.release_came_back")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		n, err := q.ReleaseCameBack(ctx, id)
		if err != nil {
			return fmt.Errorf("release came back: %w", err)
		}
		if fresh, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		if n == 0 {
			return order.ErrNotCameBack
		}
		return recordOrderStatus(ctx, q, fresh, false, now)
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}
