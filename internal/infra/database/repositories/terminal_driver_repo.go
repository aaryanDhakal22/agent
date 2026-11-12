package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"
)

// The terminal's driver view (contract/agent.md §2.10b) and the batch
// auto-close (§2.10c): a driver staff member's Pi driver, claiming late
// unassigned deliveries, closing one of his orders, a tip he got at the door, and the last auto-close slot.

// TerminalDriverLink is the Pi driver a staff member is linked to ("" =
// none yet).
func (r *OrderRepository) TerminalDriverLink(ctx context.Context, staffID string) (driverID string, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.driver_link")
	defer end(&err)
	id, err := models.New(r.db).GetTerminalDriverLink(ctx, staffID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// TerminalDriverLinks maps every linked staff member to their Pi driver.
func (r *OrderRepository) TerminalDriverLinks(ctx context.Context) (out map[string]string, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.driver_links")
	defer end(&err)
	rows, err := models.New(r.db).ListTerminalDriverLinks(ctx)
	if err != nil {
		return nil, err
	}
	out = make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.StaffID] = row.DriverID
	}
	return out, nil
}

// LinkTerminalDriver links a staff member to a Pi driver (a driver belongs
// to one staff member: an older link to it is dropped).
func (r *OrderRepository) LinkTerminalDriver(ctx context.Context, staffID, driverID string) (err error) {
	ctx, _, end := dbSpan(ctx, "terminal.link_driver")
	defer end(&err)
	now := time.Now()
	return r.withTx(ctx, func(q *models.Queries) error {
		if err := q.DeleteTerminalDriverLinkOfDriver(ctx, models.DeleteTerminalDriverLinkOfDriverParams{DriverID: driverID, StaffID: staffID}); err != nil {
			return err
		}
		return q.UpsertTerminalDriverLink(ctx, models.UpsertTerminalDriverLinkParams{StaffID: staffID, DriverID: driverID, LinkedAt: fmtTS(now)})
	})
}

// DeliverOne is the driver's "close" on one of his out orders: delivered,
// order.status to main. When it was his last stop he is back in the queue
// (returned), like "I have returned". ErrDriverNotFound, ErrDriverNotOut
// (he isn't out), order.ErrNotFound / ErrInvalidState (not one of his out
// orders).
func (r *OrderRepository) DeliverOne(ctx context.Context, driverID, orderID string) (so *order.StoredOrder, returned bool, err error) {
	ctx, _, end := dbSpan(ctx, "floor.deliver_one")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := q.GetDriver(ctx, driverID)
		if err != nil || !d.Active {
			return order.ErrDriverNotFound
		}
		cur, err := loadOrder(ctx, q, orderID)
		if err != nil {
			return err
		}
		if cur.Status.State != order.StateOnDelivery || cur.Status.DriverID != d.ID {
			return fmt.Errorf("order %s (%s): %w", orderID, cur.Status.State, order.ErrInvalidState)
		}
		n, err := q.MarkDelivered(ctx, models.MarkDeliveredParams{DeliveredAt: nullTS(now), ID: orderID})
		if err != nil {
			return err
		}
		if n == 0 {
			return order.ErrInvalidState
		}
		if fresh, err = loadOrder(ctx, q, orderID); err != nil {
			return err
		}
		if err := recordOrderStatus(ctx, q, fresh, false, now); err != nil {
			return err
		}
		returned, err = returnIfRunDone(ctx, q, d, now)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return &fresh, returned, nil
}

// ClaimDelivered - see order.DriverViewRepository. Each order is MARK
// DELIVERED (when it wasn't delivered yet) and then moved to him, like the
// Drivers tab moving an Unassigned delivery to a driver, in one tx: one
// order.status event per order (delivered, his name), the hub's
// order.driver_changed for a POS check. His status never changes.
func (r *OrderRepository) ClaimDelivered(ctx context.Context, driverID string, ids []string, rule order.ClaimRule, by order.MarkedBy) (claimed []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.claim_delivered")
	defer end(&err)
	ids = dedupe(ids)
	if len(ids) == 0 {
		return nil, order.ErrNoOrders
	}
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := q.GetDriver(ctx, driverID)
		if err != nil || !d.Active {
			return order.ErrDriverNotFound
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		// All or nothing, first come first served: check every order first.
		var todo []order.StoredOrder
		var taken []string
		for _, id := range ids {
			so, err := loadOrder(ctx, q, id)
			if err != nil {
				return err
			}
			switch {
			case so.Status.State == order.StateDelivered && so.Status.DriverID == d.ID:
				// already his
			case order.ClaimableLate(so, b.ID, rule):
				todo = append(todo, so)
			default:
				taken = append(taken, id)
			}
		}
		if len(taken) > 0 {
			return &order.ErrTaken{IDs: taken}
		}
		var names []string
		for _, so := range todo {
			md := so.Status.MarkedDelivered
			if so.Status.State != order.StateDelivered {
				if names == nil {
					if names, err = availableNames(ctx, q); err != nil {
						return err
					}
				}
				md = &order.MarkedDelivered{At: now.UTC(), By: by, PrevState: so.Status.State, Available: names}
				raw, err := markedJSON(md)
				if err != nil {
					return err
				}
				n, err := q.MarkDeliveredNoDriver(ctx, models.MarkDeliveredNoDriverParams{At: nullTS(now), Marked: raw, ID: so.Order.ID})
				if err != nil {
					return fmt.Errorf("mark delivered: %w", err)
				}
				if n == 0 {
					return &order.ErrTaken{IDs: []string{so.Order.ID}}
				}
			}
			raw, err := markedJSON(md)
			if err != nil {
				return err
			}
			n, err := q.SetOrderDriver(ctx, models.SetOrderDriverParams{DriverID: sql.NullString{String: d.ID, Valid: true},
				DriverName: d.Name, MarkedDelivered: raw, ID: so.Order.ID})
			if err != nil {
				return fmt.Errorf("set order driver: %w", err)
			}
			if n == 0 {
				return &order.ErrTaken{IDs: []string{so.Order.ID}}
			}
			fresh, err := loadOrder(ctx, q, so.Order.ID)
			if err != nil {
				return err
			}
			if err := recordOrderStatus(ctx, q, fresh, false, now); err != nil {
				return err
			}
			claimed = append(claimed, fresh)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// AutoCloseLast is the scheduled time of the last auto-close slot handled
// (zero = none yet).
func (r *OrderRepository) AutoCloseLast(ctx context.Context) (t time.Time, err error) {
	ctx, _, end := dbSpan(ctx, "settings.auto_close_last")
	defer end(&err)
	v, err := models.New(r.db).GetAutoCloseLast(ctx)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	return parseTS(v)
}

// SetAutoCloseLast stores the auto-close slot just handled.
func (r *OrderRepository) SetAutoCloseLast(ctx context.Context, slot time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_auto_close_last")
	defer end(&err)
	return models.New(r.db).SetAutoCloseLast(ctx, fmtTS(slot))
}
