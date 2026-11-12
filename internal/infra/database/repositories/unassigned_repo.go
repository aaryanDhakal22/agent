package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"
)

// Unassigned (2.4.0): MARK DELIVERED with no driver, Back to deliveries, and
// the Drivers tab moving delivered / out deliveries between drivers. Same
// floor rule as every transition: one transaction changes the orders and
// the drivers and writes main's floor events + the hub's feed events.

// availableNames are the names of the drivers in the queue right now (the
// UNASSIGNED report's "drivers in the store then").
func availableNames(ctx context.Context, q *models.Queries) ([]string, error) {
	rows, err := q.ListActiveDrivers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list drivers: %w", err)
	}
	names := []string{}
	for _, d := range rows {
		if d.Status == order.DriverAvailable {
			names = append(names, d.Name)
		}
	}
	return names, nil
}

func markedJSON(md *order.MarkedDelivered) (string, error) {
	if md == nil {
		return "", nil
	}
	raw, err := json.Marshal(md)
	if err != nil {
		return "", fmt.Errorf("marshal marked_delivered: %w", err)
	}
	return string(raw), nil
}

// MarkDeliveredNoDriver — see order.FloorRepository.
func (r *OrderRepository) MarkDeliveredNoDriver(ctx context.Context, id string, by order.MarkedBy) (result *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.mark_delivered_no_driver")
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
		if !so.IsDelivery() || so.Status.BatchID != b.ID {
			return order.ErrInvalidState
		}
		names, err := availableNames(ctx, q)
		if err != nil {
			return err
		}
		raw, err := markedJSON(&order.MarkedDelivered{At: now.UTC(), By: by, PrevState: so.Status.State, Available: names})
		if err != nil {
			return err
		}
		n, err := q.MarkDeliveredNoDriver(ctx, models.MarkDeliveredNoDriverParams{At: nullTS(now), Marked: raw, ID: id})
		if err != nil {
			return fmt.Errorf("mark delivered: %w", err)
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

// Undeliver — see order.FloorRepository. main hears it as the one backwards
// step it takes, order.not_delivered retry (back to ready with no driver).
func (r *OrderRepository) Undeliver(ctx context.Context, id string) (result *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.undeliver")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		so, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		if !so.IsUnassigned() {
			return order.ErrNotUnassigned
		}
		back := order.StateReady
		if so.Status.MarkedDelivered.PrevState == order.StateAccepted {
			back = order.StateAccepted
		}
		n, err := q.UndeliverOrder(ctx, models.UndeliverOrderParams{State: string(back), ID: id})
		if err != nil {
			return fmt.Errorf("undeliver: %w", err)
		}
		if n == 0 {
			return order.ErrNotUnassigned
		}
		if fresh, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		return recordNotDelivered(ctx, q, fresh, order.NotDeliveredRetry, "", "", false, now)
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// ReassignDeliveries — see order.FloorRepository.
func (r *OrderRepository) ReassignDeliveries(ctx context.Context, ids []string, driverID string, by order.MarkedBy) (moved []order.StoredOrder, driversChanged bool, err error) {
	ctx, _, end := dbSpan(ctx, "floor.reassign")
	defer end(&err)
	ids = dedupe(ids)
	if len(ids) == 0 {
		return nil, false, order.ErrNoOrders
	}
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		var to *models.Driver
		if driverID != "" {
			d, err := q.GetDriver(ctx, driverID)
			if err != nil || !d.Active {
				return order.ErrDriverNotFound
			}
			// Clocked out: nothing moves to them.
			if d.Status != order.DriverAvailable && d.Status != order.DriverOut {
				return order.ErrDriverNotAvailable
			}
			to = &d
		}
		// All or nothing: check every order first.
		orders := make([]order.StoredOrder, 0, len(ids))
		for _, id := range ids {
			so, err := loadOrder(ctx, q, id)
			if err != nil {
				return err
			}
			st := so.Status.State
			if !so.IsDelivery() || so.Status.BatchID != b.ID || (st != order.StateDelivered && st != order.StateOnDelivery) {
				return fmt.Errorf("order %s: %w", id, order.ErrNotOutOrDelivered)
			}
			if to == nil && st != order.StateDelivered {
				return fmt.Errorf("order %s is out: %w", id, order.ErrInvalidState) // only delivered ones go to Unassigned
			}
			orders = append(orders, so)
		}
		var names []string
		sources := map[string]bool{} // drivers an out order left
		for _, so := range orders {
			if to != nil && so.Status.DriverID == to.ID {
				continue // already theirs
			}
			if to == nil && so.IsUnassigned() {
				continue
			}
			p := models.SetOrderDriverParams{ID: so.Order.ID}
			md := so.Status.MarkedDelivered
			if to == nil {
				if names == nil {
					if names, err = availableNames(ctx, q); err != nil {
						return err
					}
				}
				md = &order.MarkedDelivered{At: now.UTC(), By: by, PrevState: order.StateReady,
					FromDriver: so.Status.DriverName, Available: names}
				p.RunSeq = optNull(so.Status.RunSeq)
			} else {
				p.DriverID, p.DriverName = sql.NullString{String: to.ID, Valid: true}, to.Name
				p.RunSeq = optNull(so.Status.RunSeq)
				if so.Status.State == order.StateOnDelivery {
					seq, err := q.NextRunSeq(ctx, sql.NullString{String: to.ID, Valid: true})
					if err != nil {
						return err
					}
					p.RunSeq = sql.NullInt64{Int64: seq, Valid: true}
					if to.Status == order.DriverAvailable { // out now, with this stop
						if err := q.SetDriverOut(ctx, to.ID); err != nil {
							return err
						}
						to.Status = order.DriverOut
						driversChanged = true
					}
					if so.Status.DriverID != "" {
						sources[so.Status.DriverID] = true
					}
				}
			}
			if p.MarkedDelivered, err = markedJSON(md); err != nil {
				return err
			}
			n, err := q.SetOrderDriver(ctx, p)
			if err != nil {
				return fmt.Errorf("set order driver: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("order %s: %w", so.Order.ID, order.ErrInvalidState)
			}
			fresh, err := loadOrder(ctx, q, so.Order.ID)
			if err != nil {
				return err
			}
			if err := recordOrderStatus(ctx, q, fresh, false, now); err != nil {
				return err
			}
			moved = append(moved, fresh)
		}
		// A driver whose last out stop moved away is back in the queue.
		for id := range sources {
			d, err := q.GetDriver(ctx, id)
			if err != nil || !d.Active || d.Status != order.DriverOut {
				continue
			}
			left, err := q.CountOnDeliveryByDriver(ctx, sql.NullString{String: id, Valid: true})
			if err != nil {
				return err
			}
			if left > 0 {
				continue
			}
			pos, err := q.NextDriverPosition(ctx)
			if err != nil {
				return err
			}
			if err := q.SetDriverReturned(ctx, models.SetDriverReturnedParams{LastReturnedAt: nullTS(now), Position: pos, ID: id}); err != nil {
				return err
			}
			if err := clockOutIfResetPassed(ctx, q, id, now); err != nil {
				return err
			}
			driversChanged = true
		}
		if driversChanged {
			return recordRoster(ctx, q, now)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return moved, driversChanged, nil
}

func optNull(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// ListUnassigned — see order.FloorRepository.
func (r *OrderRepository) ListUnassigned(ctx context.Context, batchID int64) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.list_unassigned")
	defer end(&err)
	q := models.New(r.db)
	if batchID == 0 {
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return nil, fmt.Errorf("open batch: %w", err)
		}
		batchID = b.ID
	}
	rows, err := q.ListBatchUnassigned(ctx, batchID)
	if err != nil {
		return nil, err
	}
	return mapRowsBare(rows)
}

// SetEarlyPrinted — see order.IntegrationRepository.
func (r *OrderRepository) SetEarlyPrinted(ctx context.Context, id string, kitchen, receipt bool) (err error) {
	ctx, _, end := dbSpan(ctx, "floor.set_early_printed")
	defer end(&err)
	return models.New(r.db).SetEarlyPrinted(ctx, models.SetEarlyPrintedParams{Kitchen: kitchen, Receipt: receipt, ID: id})
}
