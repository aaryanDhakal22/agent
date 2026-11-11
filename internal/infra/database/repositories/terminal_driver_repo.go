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
// auto-close (§2.10c): a driver staff member's Pi driver, closing one of his
// orders, a tip he got at the door, and the last auto-close slot.

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

// SetDoorTip records a tip the driver got at the door (absolute cents) on a
// delivery of the open batch that is out or delivered (not a POS check:
// ErrNotTippable). Nothing reaches main: no card is charged.
func (r *OrderRepository) SetDoorTip(ctx context.Context, id string, cents int64) (result *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.set_door_tip")
	defer end(&err)
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		n, err := q.SetDoorTip(ctx, models.SetDoorTipParams{TipCents: cents, ID: id, BatchID: b.ID})
		if err != nil {
			return fmt.Errorf("set door tip: %w", err)
		}
		if n == 0 {
			if _, err := loadOrder(ctx, q, id); err != nil {
				return err
			}
			return order.ErrNotTippable
		}
		fresh, err = loadOrder(ctx, q, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
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
