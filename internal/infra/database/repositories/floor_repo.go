package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// Floor transitions. Every mutating method runs in one transaction that
// changes the state AND appends the floor event(s) main will receive, so a
// change is synced iff it's committed (the floor_events table is the outbox).

// withTx runs fn inside a transaction on the single SQLite connection.
func (r *OrderRepository) withTx(ctx context.Context, fn func(q *models.Queries) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := fn(models.New(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// insertEvent marshals payload and appends it to floor_events.
func insertEvent(ctx context.Context, q *models.Queries, id, orderID, typ string, at time.Time, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal floor event: %w", err)
	}
	return q.InsertFloorEvent(ctx, models.InsertFloorEventParams{
		ID:      id,
		OrderID: sql.NullString{String: orderID, Valid: orderID != ""},
		Type:    typ,
		Payload: string(raw),
		At:      fmtTS(at),
	})
}

// recordOrderStatus appends an order.status event built from the order's
// current row. A local-only order (pulled from the POS) never reaches main.
func recordOrderStatus(ctx context.Context, q *models.Queries, so order.StoredOrder, auto bool, at time.Time) error {
	if so.Status.LocalOnly {
		return nil
	}
	ev := order.OrderStatusEvent{
		EventID:          uuid.NewString(),
		Type:             order.FloorEventOrderStatus,
		OrderID:          so.Order.ID,
		Status:           so.Status.State,
		KitchenNumber:    so.Status.KitchenNumber,
		PrepMinutes:      so.Status.PrepMinutes,
		EstimatedReadyAt: so.Status.EstimatedReadyAt,
		DriverName:       so.Status.DriverName,
		Auto:             auto,
		At:               at.UTC().Truncate(time.Millisecond),
		BatchID:          so.Status.BatchID,
	}
	st := so.Status
	ev.AgentStatus = &st
	return insertEvent(ctx, q, ev.EventID, so.Order.ID, ev.Type, at, ev)
}

// recordRoster appends a drivers.roster event with the whole active roster.
func recordRoster(ctx context.Context, q *models.Queries, at time.Time) error {
	rows, err := q.ListActiveDrivers(ctx)
	if err != nil {
		return fmt.Errorf("list drivers: %w", err)
	}
	ev := order.DriversRosterEvent{EventID: uuid.NewString(), Type: order.FloorEventDriversRoster, At: at.UTC().Truncate(time.Millisecond),
		Drivers: make([]order.RosterDriver, 0, len(rows))}
	for _, d := range rows {
		ev.Drivers = append(ev.Drivers, order.RosterDriver{ID: d.ID, Name: d.Name, Position: d.Position, Status: d.Status})
	}
	return insertEvent(ctx, q, ev.EventID, "", ev.Type, at, ev)
}

// loadOrder reads one order with its geo.
func loadOrder(ctx context.Context, q *models.Queries, id string) (order.StoredOrder, error) {
	row, err := q.GetOrderByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return order.StoredOrder{}, order.ErrNotFound
		}
		return order.StoredOrder{}, fmt.Errorf("load order: %w", err)
	}
	so, err := toStored(row)
	if err != nil {
		return so, err
	}
	switch g, err := q.GetOrderGeo(ctx, id); {
	case err == nil:
		so.Geo = geoFromRow(g)
	case !errors.Is(err, sql.ErrNoRows):
		return so, fmt.Errorf("load order geo: %w", err)
	}
	now := time.Now()
	so.FillMissingGeo(now)
	list := []order.StoredOrder{so}
	if err := markFuture(ctx, q, list, now); err != nil {
		return so, err
	}
	return list[0], nil
}

// RecordAccepted writes the accepted event once the tickets printed;
// insertExpected rides on the hub's order.accepted (see OrderAcceptedData).
func (r *OrderRepository) RecordAccepted(ctx context.Context, so order.StoredOrder, insertExpected bool) (err error) {
	ctx, _, end := dbSpan(ctx, "floor.record_accepted")
	defer end(&err)
	at := time.Now()
	if so.Status.AcceptedAt != nil {
		at = *so.Status.AcceptedAt
	}
	return r.withTx(ctx, func(q *models.Queries) error {
		if err := recordOrderStatus(ctx, q, so, false, at); err != nil {
			return err
		}
		if !so.IsPOS() { // the hub inserts online cash pickups from it
			if err := insertFeed(ctx, q, order.FeedOrderAccepted, at, order.OrderAcceptedData{Order: so, InsertExpected: insertExpected}); err != nil {
				return err
			}
		}
		return q.ClearAcceptPending(ctx, so.Order.ID)
	})
}

// ListInterruptedAccepts — see order.FloorRepository.
func (r *OrderRepository) ListInterruptedAccepts(ctx context.Context) (ids []string, err error) {
	ctx, _, end := dbSpan(ctx, "floor.list_interrupted_accepts")
	defer end(&err)
	return models.New(r.db).ListInterruptedAccepts(ctx)
}

// MarkReady: accepted → ready.
func (r *OrderRepository) MarkReady(ctx context.Context, id string) (result *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.mark_ready")
	defer end(&err)
	now := time.Now()
	var so order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		n, err := q.MarkReady(ctx, models.MarkReadyParams{ReadyAt: nullTS(now), ID: id})
		if err != nil {
			return fmt.Errorf("mark ready: %w", err)
		}
		if so, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		if n == 0 {
			return order.ErrInvalidState
		}
		if err := recordOrderStatus(ctx, q, so, false, now); err != nil {
			return err
		}
		if err := feedReady(ctx, q, so, now); err != nil {
			return err
		}
		// A POS pickup / counter check the POS already closed is done with
		// the moment it is ready (00019).
		if so.IsPOS() && so.Status.PosClosed && !so.IsDelivery() {
			if _, err := q.SetOrderFinished(ctx, models.SetOrderFinishedParams{FinishedAt: nullTS(now), FinishedBy: order.FinishedByPOS, ID: id}); err != nil {
				return err
			}
			so, err = loadOrder(ctx, q, id)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &so, nil
}

// AssignDriver sends ready delivery orders out with a driver (all or nothing).
// The sent orders leave any pool they were in (the old Quibase path).
func (r *OrderRepository) AssignDriver(ctx context.Context, driverID string, orderIDs []string) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.assign_driver")
	defer end(&err)
	orderIDs = dedupe(orderIDs)
	if len(orderIDs) == 0 {
		return nil, order.ErrNoOrders
	}
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := availableDriver(ctx, q, driverID)
		if err != nil {
			return err
		}
		for _, id := range orderIDs { // a came-back order waits for its release
			if so, err := loadOrder(ctx, q, id); err == nil && so.Status.CameBack {
				return fmt.Errorf("order %s: %w", id, order.ErrCameBack)
			}
		}
		if out, err = sendRun(ctx, q, d, orderIDs, now); err != nil {
			return err
		}
		return recordDeliveryRun(ctx, q, d, out, order.RunMeta{}, now)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DriverReturned marks the driver's out orders delivered and requeues them.
func (r *OrderRepository) DriverReturned(ctx context.Context, driverID string) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.driver_returned")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := q.GetDriver(ctx, driverID)
		if err != nil || !d.Active {
			return order.ErrDriverNotFound
		}
		if d.Status != order.DriverOut {
			return order.ErrInvalidState // not on a run (double tap / stale screen)
		}
		rows, err := q.ListOnDeliveryByDriver(ctx, sql.NullString{String: d.ID, Valid: true})
		if err != nil {
			return err
		}
		for _, row := range rows {
			if _, err := q.MarkDelivered(ctx, models.MarkDeliveredParams{DeliveredAt: nullTS(now), ID: row.ID}); err != nil {
				return err
			}
			so, err := loadOrder(ctx, q, row.ID)
			if err != nil {
				return err
			}
			if err := recordOrderStatus(ctx, q, so, false, now); err != nil {
				return err
			}
			out = append(out, so)
		}
		pos, err := q.NextDriverPosition(ctx)
		if err != nil {
			return err
		}
		// Remember the return so a mistaken tap can be undone (UndoReturn).
		if err := q.SetLastReturn(ctx, models.SetLastReturnParams{LastReturnDriverID: d.ID, LastReturnPosition: d.Position, LastReturnAt: fmtTS(now)}); err != nil {
			return err
		}
		if err := q.SetDriverReturned(ctx, models.SetDriverReturnedParams{LastReturnedAt: nullTS(now), Position: pos, ID: d.ID}); err != nil {
			return err
		}
		if err := clockOutIfResetPassed(ctx, q, d.ID, now); err != nil {
			return err
		}
		return recordRoster(ctx, q, now)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CloseBatch closes the open batch (see order.FloorRepository).
func (r *OrderRepository) CloseBatch(ctx context.Context, force bool) (closed *order.Batch, finished []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.close_batch")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		rows, err := q.ListBatchLeftovers(ctx, b.ID)
		if err != nil {
			return err
		}
		left, err := mapRowsGeo(ctx, q, rows) // served in the 409 leftovers list
		if err != nil {
			return err
		}
		if err := markFuture(ctx, q, left, now); err != nil {
			return err
		}
		// Only online orders that would be lost block a plain close: POS
		// checks are the POS's (cancelled or finished below) and a future
		// order still waiting for its time moves on with the scheduled ones.
		var blocking []order.StoredOrder
		var waiting []string
		for _, so := range left {
			switch {
			case waitsForLater(so, now):
				waiting = append(waiting, so.Order.ID)
			case !so.IsPOS():
				blocking = append(blocking, so)
			}
		}
		if len(blocking) > 0 && !force {
			return &order.LeftoversError{Orders: blocking}
		}
		ts := nullTS(now)
		for i, so := range left {
			row := rows[i] // mapRows keeps row order
			if waitsForLater(so, now) {
				continue // moved to the next batch below
			}
			if so.IsPOS() && so.Status.State != order.StateOnDelivery {
				// The POS owns its checks: an unfinished one is cancelled
				// (local only, nothing goes to main).
				if _, err := q.CancelExternalOrder(ctx, models.CancelExternalOrderParams{CancelledAt: ts, ID: row.ID}); err != nil {
					return err
				}
				fresh, err := loadOrder(ctx, q, row.ID)
				if err != nil {
					return err
				}
				if err := feedCancelled(ctx, q, fresh, now); err != nil {
					return err
				}
				finished = append(finished, fresh)
				continue
			}
			if so.Status.CameBack {
				// Came back with its driver and nobody took it out again: it
				// never reached the customer — cancelled, not auto-delivered
				// (main gets it as a not-delivered cancel).
				if _, err := q.CancelCameBack(ctx, models.CancelCameBackParams{CancelledAt: ts, ID: row.ID}); err != nil {
					return err
				}
				fresh, err := loadOrder(ctx, q, row.ID)
				if err != nil {
					return err
				}
				fromID := ""
				if nd := fresh.Status.NotDelivered; nd != nil {
					fromID = nd.DriverID
				}
				if err := recordNotDelivered(ctx, q, fresh, order.NotDeliveredCancel, fromID, "", true, now); err != nil {
					return err
				}
				finished = append(finished, fresh)
				continue
			}
			if so.Status.State == order.StateArrived {
				// Nobody accepted it: never printed, no kitchen number — cancel.
				if err := q.CancelOrder(ctx, models.CancelOrderParams{CancelledAt: ts, ID: row.ID}); err != nil {
					return err
				}
				fresh, err := loadOrder(ctx, q, row.ID)
				if err != nil {
					return err
				}
				if err := recordOrderStatus(ctx, q, fresh, true, now); err != nil {
					return err
				}
				finished = append(finished, fresh)
				continue
			}
			state := order.StateReady
			delivered := row.DeliveredAt
			if so.IsDelivery() {
				state = order.StateDelivered
				delivered = ts
			}
			accepted, ready := row.AcceptedAt, row.ReadyAt
			if !accepted.Valid {
				accepted = ts
			}
			if !ready.Valid {
				ready = ts
			}
			if err := q.FinishOrder(ctx, models.FinishOrderParams{State: string(state), AcceptedAt: accepted,
				ReadyAt: ready, DeliveredAt: delivered, ID: row.ID}); err != nil {
				return err
			}
			fresh, err := loadOrder(ctx, q, row.ID)
			if err != nil {
				return err
			}
			if err := recordOrderStatus(ctx, q, fresh, true, now); err != nil {
				return err
			}
			finished = append(finished, fresh)
		}
		// POS customer data never outlives the day: name, phone and address
		// are wiped (items, money, number, driver and times stay).
		if err := wipePOS(ctx, q, b.ID); err != nil {
			return err
		}
		if err := q.CloseBatch(ctx, models.CloseBatchParams{ClosedAt: ts, ID: b.ID}); err != nil {
			return err
		}
		next, err := q.OpenBatch(ctx, fmtTS(now))
		if err != nil {
			return err
		}
		// Future orders wait in the next batch (still scheduled).
		if err := q.MoveScheduledToBatch(ctx, models.MoveScheduledToBatchParams{NewBatch: next.ID, OldBatch: b.ID}); err != nil {
			return err
		}
		// So do future orders still ringing for their first confirm, or rung
		// again and not accepted yet, while promised_at is ahead.
		for _, id := range waiting {
			if err := q.MoveOrderToBatch(ctx, models.MoveOrderToBatchParams{NewBatch: next.ID, ID: id, OldBatch: b.ID}); err != nil {
				return err
			}
		}
		if err := insertFeed(ctx, q, order.FeedBatchClosed, now, order.BatchClosedData{BatchID: b.ID, ClosedAt: now.UTC().Truncate(time.Millisecond)}); err != nil {
			return err
		}
		if err := q.ResetKitchenNumber(ctx); err != nil {
			return err
		}
		outDrivers, err := q.ListActiveDrivers(ctx)
		if err != nil {
			return err
		}
		if err := q.ResetDriversAvailable(ctx); err != nil {
			return err
		}
		for _, d := range outDrivers {
			if d.Status == order.DriverOut {
				if err := clockOutIfResetPassed(ctx, q, d.ID, now); err != nil {
					return err
				}
			}
		}
		if err := recordRoster(ctx, q, now); err != nil {
			return err
		}
		opened, _ := parseTS(b.OpenedAt)
		closed = &order.Batch{ID: b.ID, OpenedAt: opened, ClosedAt: &now}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return closed, finished, nil
}

// waitsForLater: a future online order not accepted yet (still arrived:
// never scheduled, or rung again) whose promised time is still ahead. At
// Close batch it moves into the next batch like a scheduled one; a past-due
// one is a leftover as before.
func waitsForLater(so order.StoredOrder, now time.Time) bool {
	if so.IsPOS() || so.Status.State != order.StateArrived || !(so.Status.Future || so.Status.Rerung) {
		return false
	}
	p := so.Order.Fulfillment.PromisedAt
	return p != nil && p.After(now)
}

// wipePOS clears the customer name, phone and address of every POS order in
// the batch, and drops their located points.
func wipePOS(ctx context.Context, q *models.Queries, batchID int64) error {
	rows, err := q.ListLocalOnlyByBatch(ctx, batchID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var snap order.Snapshot
		if err := json.Unmarshal([]byte(row.Snapshot), &snap); err != nil {
			return fmt.Errorf("unmarshal snapshot for order %s: %w", row.ID, err)
		}
		snap.WipePII()
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if err := q.SetOrderSnapshot(ctx, models.SetOrderSnapshotParams{Snapshot: string(raw), ID: row.ID}); err != nil {
			return err
		}
		if err := q.DeleteOrderGeo(ctx, row.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *OrderRepository) OpenBatch(ctx context.Context) (b *order.Batch, err error) {
	ctx, _, end := dbSpan(ctx, "floor.open_batch")
	defer end(&err)
	row, err := models.New(r.db).GetOpenBatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("open batch: %w", err)
	}
	opened, err := parseTS(row.OpenedAt)
	if err != nil {
		return nil, err
	}
	return &order.Batch{ID: row.ID, OpenedAt: opened}, nil
}

func (r *OrderRepository) ListBatches(ctx context.Context, limit int) (out []order.Batch, err error) {
	ctx, _, end := dbSpan(ctx, "floor.list_batches")
	defer end(&err)
	rows, err := models.New(r.db).ListBatches(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		opened, err := parseTS(row.OpenedAt)
		if err != nil {
			return nil, err
		}
		closed, err := optTS(row.ClosedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, order.Batch{ID: row.ID, OpenedAt: opened, ClosedAt: closed, OrderCount: row.OrderCount})
	}
	return out, nil
}

func (r *OrderRepository) ListByBatch(ctx context.Context, batchID int64, offset, limit int) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.list_by_batch")
	defer end(&err)
	q := models.New(r.db)
	rows, err := q.ListOrdersByBatch(ctx, models.ListOrdersByBatchParams{
		BatchID: batchID, Limit: int64(limit), Offset: int64(offset),
	})
	if err != nil {
		return nil, err
	}
	return mapRowsGeo(ctx, q, rows)
}

func (r *OrderRepository) FloorSettings(ctx context.Context) (fs order.FloorSettings, err error) {
	ctx, _, end := dbSpan(ctx, "floor.settings")
	defer end(&err)
	row, err := models.New(r.db).GetFloorSettings(ctx)
	if err != nil {
		return fs, err
	}
	return order.FloorSettings{KitchenNext: row.KitchenNext, LastPrepMinutes: row.LastPrepMinutes}, nil
}

// --- drivers ---------------------------------------------------------------

func (r *OrderRepository) ListDrivers(ctx context.Context) (out []order.Driver, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.list")
	defer end(&err)
	q := models.New(r.db)
	rows, err := q.ListActiveDrivers(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	if b, err := q.GetOpenBatch(ctx); err == nil {
		cs, err := q.CountDeliveriesInBatch(ctx, b.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range cs {
			counts[c.DriverID.String] = c.Deliveries
		}
	}
	open, err := q.ListOpenShifts(ctx)
	if err != nil {
		return nil, err
	}
	runs := map[string][]string{} // driver -> its run that is out, in stop order
	stops, err := q.ListRunStops(ctx)
	if err != nil {
		return nil, err
	}
	for _, st := range stops {
		runs[st.DriverID.String] = append(runs[st.DriverID.String], st.ID)
	}
	clockedIn := map[string]string{}
	for _, sh := range open {
		clockedIn[sh.DriverID] = sh.ClockIn
	}
	out = make([]order.Driver, 0, len(rows))
	for _, d := range rows {
		ret, err := optTS(d.LastReturnedAt)
		if err != nil {
			return nil, err
		}
		in, err := optTS(sql.NullString{String: clockedIn[d.ID], Valid: clockedIn[d.ID] != ""})
		if err != nil {
			return nil, err
		}
		dr := order.Driver{ID: d.ID, Name: d.Name, Position: d.Position, Status: d.Status,
			LastReturnedAt: ret, Deliveries: counts[d.ID], ClockedInAt: in, PosNumber: d.PosNumber}
		if d.Status == order.DriverOut {
			dr.RunOrderIDs = runs[d.ID]
		}
		out = append(out, dr)
	}
	return out, nil
}

func (r *OrderRepository) CreateDriver(ctx context.Context, name, posNumber string) (d *order.Driver, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.create")
	defer end(&err)
	id := uuid.NewString()
	var pos int64
	err = r.withTx(ctx, func(q *models.Queries) error {
		if pos, err = q.NextDriverPosition(ctx); err != nil {
			return err
		}
		if err := q.CreateDriver(ctx, models.CreateDriverParams{ID: id, Name: name, Position: pos, PosNumber: posNumber}); err != nil {
			return posNumberErr(err)
		}
		return recordRoster(ctx, q, time.Now())
	})
	if err != nil {
		return nil, err
	}
	return &order.Driver{ID: id, Name: name, Position: pos, Status: order.DriverOff, PosNumber: posNumber}, nil
}

// SetDriverPosNumber — see order.FloorRepository.
func (r *OrderRepository) SetDriverPosNumber(ctx context.Context, id, posNumber string) (err error) {
	ctx, _, end := dbSpan(ctx, "drivers.set_pos_number")
	defer end(&err)
	n, err := models.New(r.db).SetDriverPosNumber(ctx, models.SetDriverPosNumberParams{PosNumber: posNumber, ID: id})
	if err != nil {
		return posNumberErr(err)
	}
	if n == 0 {
		return order.ErrDriverNotFound
	}
	return nil
}

// posNumberErr maps the unique index on drivers.pos_number to
// ErrPosNumberTaken.
func posNumberErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "pos_number") {
		return order.ErrPosNumberTaken
	}
	return err
}

func (r *OrderRepository) RenameDriver(ctx context.Context, id, name string) (err error) {
	ctx, _, end := dbSpan(ctx, "drivers.rename")
	defer end(&err)
	return r.withTx(ctx, func(q *models.Queries) error {
		n, err := q.RenameDriver(ctx, models.RenameDriverParams{Name: name, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return order.ErrDriverNotFound
		}
		return recordRoster(ctx, q, time.Now())
	})
}

func (r *OrderRepository) RemoveDriver(ctx context.Context, id string) (unpooled []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.remove")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := q.GetDriver(ctx, id)
		if err != nil || !d.Active {
			return order.ErrDriverNotFound
		}
		if d.Status == order.DriverOut {
			return order.ErrDriverNotAvailable // their run must come back first
		}
		if _, err := q.DeactivateDriver(ctx, id); err != nil {
			return err
		}
		if unpooled, err = clearDriverPool(ctx, q, id, now); err != nil {
			return err
		}
		if d.Status == order.DriverAvailable { // clocked in: close the shift
			if _, err := q.ClockOutDriver(ctx, id); err != nil {
				return err
			}
			if sh, err := q.GetOpenShift(ctx, id); err == nil {
				if err := closeShift(ctx, q, sh, false, now); err != nil {
					return err
				}
			}
		}
		return recordRoster(ctx, q, now)
	})
	if err != nil {
		return nil, err
	}
	return unpooled, nil
}

// ReorderDrivers sets the queue order: ids first (in the given order), any
// active driver not listed keeps its relative order after them.
func (r *OrderRepository) ReorderDrivers(ctx context.Context, ids []string) (err error) {
	ctx, _, end := dbSpan(ctx, "drivers.reorder")
	defer end(&err)
	return r.withTx(ctx, func(q *models.Queries) error {
		rows, err := q.ListActiveDrivers(ctx)
		if err != nil {
			return err
		}
		active := map[string]bool{}
		for _, d := range rows {
			active[d.ID] = true
		}
		order2 := make([]string, 0, len(rows))
		seen := map[string]bool{}
		for _, id := range ids {
			if !active[id] {
				return order.ErrDriverNotFound
			}
			if !seen[id] {
				order2 = append(order2, id)
				seen[id] = true
			}
		}
		for _, d := range rows {
			if !seen[d.ID] {
				order2 = append(order2, d.ID)
			}
		}
		for i, id := range order2 {
			if err := q.SetDriverPosition(ctx, models.SetDriverPositionParams{Position: int64(i + 1), ID: id}); err != nil {
				return err
			}
		}
		return recordRoster(ctx, q, time.Now())
	})
}

// --- clock in / out --------------------------------------------------------

// recordShift appends a driver.shift event for the shift's current state.
func recordShift(ctx context.Context, q *models.Queries, sh models.DriverShift, at time.Time) error {
	in, err := parseTS(sh.ClockIn)
	if err != nil {
		return err
	}
	out, err := optTS(sh.ClockOut)
	if err != nil {
		return err
	}
	ev := order.DriverShiftEvent{EventID: uuid.NewString(), Type: order.FloorEventDriverShift, ShiftID: sh.ID,
		DriverID: sh.DriverID, DriverName: sh.DriverName, ClockIn: in.UTC(), ClockOut: out, Auto: sh.AutoOut,
		At: at.UTC().Truncate(time.Millisecond)}
	return insertEvent(ctx, q, ev.EventID, "", ev.Type, at, ev)
}

// clockOutIfResetPassed: a driver back from a run whose shift started before
// the last daily reset is clocked out (the reset skipped them while out).
func clockOutIfResetPassed(ctx context.Context, q *models.Queries, driverID string, now time.Time) error {
	rs, err := q.GetDriverReset(ctx)
	if err != nil || rs.LastDriverResetAt == "" {
		return nil
	}
	sh, err := q.GetOpenShift(ctx, driverID)
	if err != nil || sh.ClockIn >= rs.LastDriverResetAt {
		return nil
	}
	if _, err := q.ClockOutDriver(ctx, driverID); err != nil {
		return err
	}
	if _, err := clearDriverPool(ctx, q, driverID, now); err != nil {
		return err
	}
	return closeShift(ctx, q, sh, true, now)
}

func closeShift(ctx context.Context, q *models.Queries, sh models.DriverShift, auto bool, at time.Time) error {
	if err := q.CloseShift(ctx, models.CloseShiftParams{ClockOut: nullTS(at), AutoOut: auto, ID: sh.ID}); err != nil {
		return err
	}
	sh.ClockOut, sh.AutoOut = nullTS(at), auto
	return recordShift(ctx, q, sh, at)
}

func (r *OrderRepository) driverByID(ctx context.Context, id string) (*order.Driver, error) {
	ds, err := r.ListDrivers(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.ID == id {
			return &d, nil
		}
	}
	return nil, order.ErrDriverNotFound
}

// ClockIn — see order.FloorRepository.
func (r *OrderRepository) ClockIn(ctx context.Context, id string) (d *order.Driver, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.clock_in")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetDriver(ctx, id)
		if err != nil || !row.Active {
			return order.ErrDriverNotFound
		}
		pos, err := q.NextDriverPosition(ctx)
		if err != nil {
			return err
		}
		n, err := q.SetDriverStatus(ctx, models.SetDriverStatusParams{NewStatus: order.DriverAvailable, Position: pos, ID: id, OldStatus: order.DriverOff})
		if err != nil {
			return err
		}
		if n == 0 {
			return order.ErrInvalidState // already clocked in
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return err
		}
		sh := models.DriverShift{ID: uuid.NewString(), DriverID: id, DriverName: row.Name, BatchID: b.ID, ClockIn: fmtTS(now)}
		if err := q.OpenShift(ctx, models.OpenShiftParams{ID: sh.ID, DriverID: id, DriverName: row.Name, BatchID: b.ID, ClockIn: sh.ClockIn}); err != nil {
			return err
		}
		if err := recordShift(ctx, q, sh, now); err != nil {
			return err
		}
		return recordRoster(ctx, q, now)
	})
	if err != nil {
		return nil, err
	}
	return r.driverByID(ctx, id)
}

// ClockOut — see order.FloorRepository.
func (r *OrderRepository) ClockOut(ctx context.Context, id string) (d *order.Driver, unpooled []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.clock_out")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetDriver(ctx, id)
		if err != nil || !row.Active {
			return order.ErrDriverNotFound
		}
		if row.Status != order.DriverAvailable {
			return order.ErrDriverNotAvailable // off already, or out on a run
		}
		if _, err := q.ClockOutDriver(ctx, id); err != nil {
			return err
		}
		if unpooled, err = clearDriverPool(ctx, q, id, now); err != nil {
			return err
		}
		if sh, err := q.GetOpenShift(ctx, id); err == nil {
			if err := closeShift(ctx, q, sh, false, now); err != nil {
				return err
			}
		}
		return recordRoster(ctx, q, now)
	})
	if err != nil {
		return nil, nil, err
	}
	d, err = r.driverByID(ctx, id)
	return d, unpooled, err
}

// AutoClockOut — see order.FloorRepository.
func (r *OrderRepository) AutoClockOut(ctx context.Context, at time.Time) (n int, unpooled []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.auto_clock_out")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		ds, err := q.ListAvailableDrivers(ctx)
		if err != nil {
			return err
		}
		for _, d := range ds {
			if _, err := q.ClockOutDriver(ctx, d.ID); err != nil {
				return err
			}
			cleared, err := clearDriverPool(ctx, q, d.ID, at)
			if err != nil {
				return err
			}
			unpooled = append(unpooled, cleared...)
			if sh, err := q.GetOpenShift(ctx, d.ID); err == nil {
				if err := closeShift(ctx, q, sh, true, at); err != nil {
					return err
				}
			}
			n++
		}
		if err := q.SetLastDriverReset(ctx, fmtTS(at)); err != nil {
			return err
		}
		if n > 0 {
			return recordRoster(ctx, q, at)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return n, unpooled, nil
}

func (r *OrderRepository) GetDriverReset(ctx context.Context) (order.DriverReset, error) {
	row, err := models.New(r.db).GetDriverReset(ctx)
	if err != nil {
		return order.DriverReset{}, err
	}
	last, err := optTS(sql.NullString{String: row.LastDriverResetAt, Valid: row.LastDriverResetAt != ""})
	if err != nil {
		return order.DriverReset{}, err
	}
	return order.DriverReset{Time: row.DriverResetTime, LastRun: last}, nil
}

func (r *OrderRepository) MarkDriverReset(ctx context.Context, at time.Time) error {
	return models.New(r.db).SetLastDriverReset(ctx, fmtTS(at))
}

func (r *OrderRepository) SetDriverResetTime(ctx context.Context, hhmm string) error {
	return models.New(r.db).SetDriverResetTime(ctx, hhmm)
}

func (r *OrderRepository) ListShifts(ctx context.Context, since time.Time) (out []order.Shift, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.list_shifts")
	defer end(&err)
	rows, err := models.New(r.db).ListShiftsSince(ctx, fmtTS(since))
	if err != nil {
		return nil, err
	}
	out = make([]order.Shift, 0, len(rows))
	for _, sh := range rows {
		in, err := parseTS(sh.ClockIn)
		if err != nil {
			return nil, err
		}
		o, err := optTS(sh.ClockOut)
		if err != nil {
			return nil, err
		}
		out = append(out, order.Shift{ID: sh.ID, DriverID: sh.DriverID, DriverName: sh.DriverName, BatchID: sh.BatchID,
			ClockIn: in, ClockOut: o, AutoOut: sh.AutoOut})
	}
	return out, nil
}

// --- escalation ------------------------------------------------------------

func (r *OrderRepository) ListOverdueArrived(ctx context.Context, cutoff time.Time) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.list_overdue")
	defer end(&err)
	q := models.New(r.db)
	rows, err := q.ListOverdueArrived(ctx, fmtTS(cutoff))
	if err != nil {
		return nil, err
	}
	return mapRowsGeo(ctx, q, rows) // published as order.escalated
}

func (r *OrderRepository) MarkAlerted(ctx context.Context, id string, at time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "floor.mark_alerted")
	defer end(&err)
	return models.New(r.db).MarkAlerted(ctx, models.MarkAlertedParams{LastAlertAt: nullTS(at), ID: id})
}

// --- sync outbox -----------------------------------------------------------

func (r *OrderRepository) ListUnsyncedEvents(ctx context.Context, limit int) (out []order.FloorEvent, err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.list_unsynced")
	defer end(&err)
	rows, err := models.New(r.db).ListUnsyncedFloorEvents(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		at, err := parseTS(row.At)
		if err != nil {
			return nil, err
		}
		out = append(out, order.FloorEvent{ID: row.ID, OrderID: row.OrderID.String, Type: row.Type,
			Payload: json.RawMessage(row.Payload), At: at, Attempts: row.Attempts})
	}
	return out, nil
}

func (r *OrderRepository) MarkEventSynced(ctx context.Context, id string, at time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.mark_synced")
	defer end(&err)
	return models.New(r.db).MarkFloorEventSynced(ctx, models.MarkFloorEventSyncedParams{SyncedAt: nullTS(at), ID: id})
}

func (r *OrderRepository) MarkEventFailed(ctx context.Context, id, msg string) (err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.mark_failed")
	defer end(&err)
	return models.New(r.db).MarkFloorEventFailed(ctx, models.MarkFloorEventFailedParams{LastError: msg, ID: id})
}

func (r *OrderRepository) CountUnsyncedEvents(ctx context.Context) (n int64, err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.count_unsynced")
	defer end(&err)
	return models.New(r.db).CountUnsyncedFloorEvents(ctx)
}

// --- retention -------------------------------------------------------------

func (r *OrderRepository) TrimBatches(ctx context.Context, keepClosed int, syncedBefore time.Time) (n int64, err error) {
	ctx, _, end := dbSpan(ctx, "orders.trim_batches")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		if n, err = q.DeleteOrdersOutsideKeptBatches(ctx, int64(keepClosed)); err != nil {
			return err
		}
		if err := q.DeleteOldClosedBatches(ctx, int64(keepClosed)); err != nil {
			return err
		}
		if _, err := q.DeleteOldSyncedFloorEvents(ctx, sql.NullString{String: fmtTS(syncedBefore), Valid: true}); err != nil {
			return err
		}
		// The hub's feed is kept 3 days.
		if err := q.DeleteOldIntegrationEvents(ctx, fmtTS(time.Now().Add(-order.FeedRetention))); err != nil {
			return err
		}
		// Geo + drive times of trimmed orders, and AUTO plans as old as the
		// synced events (they only matter for the tuning log).
		if err := q.DeleteOrphanOrderGeo(ctx); err != nil {
			return err
		}
		if err := q.DeleteOrphanOrderTimes(ctx); err != nil {
			return err
		}
		return q.DeleteOldAutoPlans(ctx, fmtTS(syncedBefore))
	})
	return n, err
}

// UndoReturn — see order.FloorRepository.
func (r *OrderRepository) UndoReturn(ctx context.Context, driverID string, now time.Time) (d *order.Driver, err error) {
	ctx, _, end := dbSpan(ctx, "drivers.undo_return")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		last, err := q.GetLastReturn(ctx)
		if err != nil {
			return err
		}
		at, err := optTS(sql.NullString{String: last.LastReturnAt, Valid: last.LastReturnAt != ""})
		if err != nil {
			return err
		}
		if last.LastReturnDriverID != driverID || at == nil || now.Sub(*at) > order.UndoReturnWindow {
			return order.ErrInvalidState // not the latest return, or too late
		}
		n, err := q.SetDriverOutAt(ctx, models.SetDriverOutAtParams{Position: last.LastReturnPosition, ID: driverID})
		if err != nil {
			return err
		}
		if n == 0 {
			return order.ErrInvalidState // sent out again, clocked out or removed since
		}
		if err := q.SetLastReturn(ctx, models.SetLastReturnParams{}); err != nil {
			return err
		}
		return recordRoster(ctx, q, now)
	})
	if err != nil {
		return nil, err
	}
	return r.driverByID(ctx, driverID)
}

func (r *OrderRepository) FloorPrefs(ctx context.Context) (order.FloorPrefs, error) {
	row, err := models.New(r.db).GetFloorPrefs(ctx)
	if err != nil {
		return order.DefaultFloorPrefs, err
	}
	return order.FloorPrefs{GreenTillMinutes: row.GreenTillMinutes, TipTaxBps: row.TipTaxBps}, nil
}

func (r *OrderRepository) SetFloorPrefs(ctx context.Context, p order.FloorPrefs) error {
	return models.New(r.db).SetFloorPrefs(ctx, models.SetFloorPrefsParams{GreenTillMinutes: p.GreenTillMinutes, TipTaxBps: p.TipTaxBps})
}

// ReceiptPrefs — see order.FloorRepository. Stored as JSON; keys the stored
// object lacks (older agents: '{}') come back as their defaults.
func (r *OrderRepository) ReceiptPrefs(ctx context.Context) (order.ReceiptPrefs, error) {
	raw, err := models.New(r.db).GetReceiptPrefs(ctx)
	if err != nil {
		return order.DefaultReceiptPrefs(), err
	}
	return order.DecodeReceiptPrefs([]byte(raw))
}

// SetReceiptPrefs stores every field (so a later default change can't flip a
// setting the manager chose).
func (r *OrderRepository) SetReceiptPrefs(ctx context.Context, p order.ReceiptPrefs) error {
	return setReceiptPrefs(ctx, models.New(r.db), p)
}

func setReceiptPrefs(ctx context.Context, q *models.Queries, p order.ReceiptPrefs) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return q.SetReceiptPrefs(ctx, string(b))
}

// DriverReports — see order.FloorRepository.
func (r *OrderRepository) DriverReports(ctx context.Context, batchID int64) (out []order.DriverReport, err error) {
	ctx, _, end := dbSpan(ctx, "floor.driver_reports")
	defer end(&err)
	q := models.New(r.db)
	var b models.Batch
	if batchID == 0 {
		ob, err := q.GetOpenBatch(ctx)
		if err != nil {
			return nil, fmt.Errorf("open batch: %w", err)
		}
		b = models.Batch{ID: ob.ID, OpenedAt: ob.OpenedAt}
	} else if b, err = q.GetBatch(ctx, batchID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, order.ErrNotFound
		}
		return nil, err
	}
	opened, err := parseTS(b.OpenedAt)
	if err != nil {
		return nil, err
	}
	closed, err := optTS(b.ClosedAt)
	if err != nil {
		return nil, err
	}

	byDriver := map[string]*order.DriverReport{}
	get := func(id, name string) *order.DriverReport {
		rep := byDriver[id]
		if rep == nil {
			rep = &order.DriverReport{BatchID: b.ID, BatchOpenedAt: opened, BatchClosedAt: closed,
				DriverID: id, DriverName: name, Lines: []order.DriverReportLine{}}
			byDriver[id] = rep
		}
		if rep.DriverName == "" {
			rep.DriverName = name
		}
		return rep
	}

	rows, err := q.ListBatchDeliveries(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	orders, err := mapRowsBare(rows)
	if err != nil {
		return nil, err
	}
	for _, so := range orders {
		rep := get(so.Status.DriverID, so.Status.DriverName)
		rep.Lines = append(rep.Lines, order.NewDriverReportLine(so))
	}

	// Drivers clocked in during the batch with no deliveries still get a slip.
	windowEnd := fmtTS(time.Now())
	if closed != nil {
		windowEnd = fmtTS(*closed)
	}
	shifts, err := q.ListShiftsOverlapping(ctx, models.ListShiftsOverlappingParams{
		WindowEnd: windowEnd, WindowStart: sql.NullString{String: b.OpenedAt, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	for _, sh := range shifts {
		get(sh.DriverID, sh.DriverName)
	}

	// Prefer the driver's current name (a rename after the run).
	if ds, err := q.ListActiveDrivers(ctx); err == nil {
		for _, d := range ds {
			if rep := byDriver[d.ID]; rep != nil {
				rep.DriverName = d.Name
			}
		}
	}
	out = make([]order.DriverReport, 0, len(byDriver))
	for _, rep := range byDriver {
		out = append(out, *rep)
	}
	sort.Slice(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].DriverName, out[j].DriverName) {
			return strings.ToLower(out[i].DriverName) < strings.ToLower(out[j].DriverName)
		}
		return out[i].DriverID < out[j].DriverID
	})
	return out, nil
}

// GetAppRelease / SetAppRelease persist the cached tablet-app release (JSON).
func (r *OrderRepository) GetAppRelease(ctx context.Context) (string, error) {
	return models.New(r.db).GetAppRelease(ctx)
}

func (r *OrderRepository) SetAppRelease(ctx context.Context, raw string) error {
	return models.New(r.db).SetAppRelease(ctx, raw)
}
