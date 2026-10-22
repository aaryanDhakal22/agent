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

// Driver pools, order geo and AUTO plans (2.0.9). Pool changes follow the
// floor rule: one transaction per change, its floor events written inside it.

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// availableDriver loads an active driver and checks it is in the queue.
func availableDriver(ctx context.Context, q *models.Queries, id string) (models.Driver, error) {
	d, err := q.GetDriver(ctx, id)
	if err != nil || !d.Active {
		return d, order.ErrDriverNotFound
	}
	if d.Status != order.DriverAvailable {
		return d, order.ErrDriverNotAvailable
	}
	return d, nil
}

// poolable reports whether an order may sit in a pool.
func poolable(so order.StoredOrder) bool {
	return so.IsDelivery() && (so.Status.State == order.StateAccepted || so.Status.State == order.StateReady)
}

// setPool moves one order into driverID's pool ("" = unassigned) and records
// the order.status event (unchanged state, new agent_status). planID is the
// AUTO plan that pooled it (confirm; "" by hand). Its pool_seq is cleared (the
// service renumbers the pools). No-op when the order is already there.
func setPool(ctx context.Context, q *models.Queries, so order.StoredOrder, driverID, planID string, now time.Time) (order.StoredOrder, error) {
	if so.Status.PoolDriverID == driverID {
		return so, nil
	}
	n, err := q.SetOrderPool(ctx, models.SetOrderPoolParams{
		PoolDriverID: sql.NullString{String: driverID, Valid: driverID != ""},
		PooledAt:     nullTSIf(now, driverID != ""),
		PoolPlanID:   sql.NullString{String: planID, Valid: planID != "" && driverID != ""},
		ID:           so.Order.ID,
	})
	if err != nil {
		return so, fmt.Errorf("set pool: %w", err)
	}
	if n == 0 {
		return so, fmt.Errorf("order %s: %w", so.Order.ID, order.ErrInvalidState)
	}
	fresh, err := loadOrder(ctx, q, so.Order.ID)
	if err != nil {
		return so, err
	}
	return fresh, recordOrderStatus(ctx, q, fresh, false, now)
}

// poolInTx pools ids to driverID ("" = unassign), all or nothing (the caller's
// tx rolls back on error).
func poolInTx(ctx context.Context, q *models.Queries, ids []string, driverID string, now time.Time) ([]order.StoredOrder, error) {
	list := make([]order.StoredOrder, 0, len(ids))
	for _, id := range ids {
		so, err := loadOrder(ctx, q, id)
		if err != nil {
			return nil, err
		}
		if !poolable(so) {
			return nil, fmt.Errorf("order %s (%s): %w", id, so.Status.State, order.ErrInvalidState)
		}
		list = append(list, so)
	}
	out := make([]order.StoredOrder, 0, len(list))
	for _, so := range list {
		fresh, err := setPool(ctx, q, so, driverID, "", now)
		if err != nil {
			return nil, err
		}
		out = append(out, fresh)
	}
	return out, nil
}

// clearDriverPool sends every order in the driver's pool back to unassigned
// (clock-out, daily reset, removal) and returns them.
func clearDriverPool(ctx context.Context, q *models.Queries, driverID string, now time.Time) ([]order.StoredOrder, error) {
	rows, err := q.ListPoolByDriver(ctx, sql.NullString{String: driverID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list pool: %w", err)
	}
	var out []order.StoredOrder
	for _, row := range rows {
		if err := q.ClearOrderPool(ctx, row.ID); err != nil {
			return nil, err
		}
		fresh, err := loadOrder(ctx, q, row.ID)
		if err != nil {
			return nil, err
		}
		if err := recordOrderStatus(ctx, q, fresh, false, now); err != nil {
			return nil, err
		}
		out = append(out, fresh)
	}
	return out, nil
}

// PoolOrders — see order.PoolRepository.
func (r *OrderRepository) PoolOrders(ctx context.Context, ids []string, driverID string) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.pool_orders")
	defer end(&err)
	ids = dedupe(ids)
	if len(ids) == 0 {
		return nil, order.ErrNoOrders
	}
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		if driverID != "" {
			if _, err := availableDriver(ctx, q, driverID); err != nil {
				return err
			}
		}
		out, err = poolInTx(ctx, q, ids, driverID, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SendOut — see order.PoolRepository.
func (r *OrderRepository) SendOut(ctx context.Context, driverID string, include []string, markReady bool, meta order.RunMeta) (sent, readied []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.send_out")
	defer end(&err)
	include = dedupe(include)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := availableDriver(ctx, q, driverID)
		if err != nil {
			return err
		}
		if len(include) > 0 {
			if _, err := poolInTx(ctx, q, include, d.ID, now); err != nil {
				return err
			}
		}
		rows, err := q.ListPoolByDriver(ctx, sql.NullString{String: d.ID, Valid: true})
		if err != nil {
			return err
		}
		pool, err := mapRowsBare(rows)
		if err != nil {
			return err
		}
		var cooking []string
		ids := make([]string, 0, len(pool))
		for _, so := range pool {
			if !poolable(so) {
				continue // can't happen (the pool empties when an order leaves)
			}
			ids = append(ids, so.Order.ID)
			if so.Status.State == order.StateAccepted {
				cooking = append(cooking, so.Order.ID)
			}
		}
		if len(ids) == 0 {
			return order.ErrPoolEmpty
		}
		if len(cooking) > 0 && !markReady {
			return &order.ErrNotReady{IDs: cooking}
		}
		for _, id := range cooking {
			n, err := q.MarkReady(ctx, models.MarkReadyParams{ReadyAt: nullTS(now), ID: id})
			if err != nil {
				return fmt.Errorf("mark ready: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("order %s: %w", id, order.ErrInvalidState)
			}
			so, err := loadOrder(ctx, q, id)
			if err != nil {
				return err
			}
			if err := recordOrderStatus(ctx, q, so, false, now); err != nil {
				return err
			}
			readied = append(readied, so)
		}
		sent, err = sendRun(ctx, q, d, ids, now)
		if err != nil {
			return err
		}
		return recordDeliveryRun(ctx, q, d, sent, meta, now)
	})
	if err != nil {
		return nil, nil, err
	}
	return sent, readied, nil
}

// sendRun is the shared tail of AssignDriver and SendOut: the orders go out
// with the driver (on_delivery, pool slot emptied), the driver is out.
func sendRun(ctx context.Context, q *models.Queries, d models.Driver, ids []string, now time.Time) ([]order.StoredOrder, error) {
	out := make([]order.StoredOrder, 0, len(ids))
	for _, id := range ids {
		n, err := q.MarkOnDelivery(ctx, models.MarkOnDeliveryParams{
			OutAt: nullTS(now), DriverID: sql.NullString{String: d.ID, Valid: true}, DriverName: d.Name, ID: id,
		})
		if err != nil {
			return nil, fmt.Errorf("mark on delivery: %w", err)
		}
		so, err := loadOrder(ctx, q, id)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("order %s: %w", id, order.ErrInvalidState)
		}
		if err := recordOrderStatus(ctx, q, so, false, now); err != nil {
			return nil, err
		}
		out = append(out, so)
	}
	if err := q.SetDriverOut(ctx, d.ID); err != nil {
		return nil, err
	}
	return out, recordRoster(ctx, q, now)
}

// recordDeliveryRun appends the delivery.run event for a run that just left.
func recordDeliveryRun(ctx context.Context, q *models.Queries, d models.Driver, sent []order.StoredOrder, meta order.RunMeta, now time.Time) error {
	if len(sent) == 0 {
		return nil
	}
	ids := make([]string, 0, len(sent))
	for _, so := range sent {
		ids = append(ids, so.Order.ID)
	}
	times, err := driveTimes(ctx, q, ids)
	if err != nil {
		return err
	}
	storeTo := map[string]int64{}
	for _, so := range sent {
		if so.Geo != nil && so.Geo.Located() {
			if so.Geo.StoreToS != nil {
				storeTo[so.Order.ID] = *so.Geo.StoreToS
			} else if so.Geo.DurationS > 0 {
				storeTo[so.Order.ID] = so.Geo.DurationS
			}
		}
	}
	suggested := meta.SuggestedIDs
	if suggested == nil {
		suggested = []string{}
	}
	ev := order.DeliveryRunEvent{
		EventID: uuid.NewString(), Type: order.FloorEventDeliveryRun, At: now.UTC().Truncate(time.Millisecond),
		BatchID: sent[0].Status.BatchID, DriverID: d.ID, DriverName: d.Name, PlanID: meta.PlanID,
		SuggestedIDs: suggested,
		Stops:        order.EstimateStops(ids, meta.StopOrder, now, storeTo, times),
	}
	for _, s := range ev.Stops {
		ev.SentIDs = append(ev.SentIDs, s.OrderID)
	}
	return insertEvent(ctx, q, ev.EventID, "", ev.Type, now, ev)
}

// SetPoolSeq — see order.PoolRepository.
func (r *OrderRepository) SetPoolSeq(ctx context.Context, driverID string, ids []string) (changed []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.set_pool_seq")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		for i, id := range ids {
			n, err := q.SetPoolSeq(ctx, models.SetPoolSeqParams{PoolSeq: sql.NullInt64{Int64: int64(i + 1), Valid: true},
				ID: id, PoolDriverID: sql.NullString{String: driverID, Valid: true}})
			if err != nil {
				return fmt.Errorf("set pool seq: %w", err)
			}
			if n == 0 {
				continue // unchanged, or left the pool meanwhile
			}
			so, err := loadOrder(ctx, q, id)
			if err != nil {
				return err
			}
			if err := recordOrderStatus(ctx, q, so, false, now); err != nil {
				return err
			}
			changed = append(changed, so)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// ListOpenDeliveries — see order.PoolRepository.
func (r *OrderRepository) ListOpenDeliveries(ctx context.Context) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.list_open_deliveries")
	defer end(&err)
	q := models.New(r.db)
	rows, err := q.ListOpenDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	return mapRowsGeo(ctx, q, rows)
}

// --- geo -------------------------------------------------------------------

// UpsertGeo — see order.PoolRepository.
func (r *OrderRepository) UpsertGeo(ctx context.Context, g order.GeoUpdate) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.upsert_geo")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		if _, err := q.GetOrderByID(ctx, g.OrderID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return order.ErrNotFound
			}
			return err
		}
		p := models.UpsertOrderGeoParams{OrderID: g.OrderID, State: g.State, Quality: g.Quality,
			DistanceM: g.DistanceM, DurationS: g.DurationS, Polyline: g.Polyline, OutsideArea: g.OutsideArea,
			StoreToS: nullInt(g.StoreToS), ToStoreS: nullInt(g.ToStoreS), UpdatedAt: fmtTS(now)}
		if g.Lat != nil && g.Lng != nil {
			p.Lat = sql.NullFloat64{Float64: *g.Lat, Valid: true}
			p.Lng = sql.NullFloat64{Float64: *g.Lng, Valid: true}
		}
		if err := q.UpsertOrderGeo(ctx, p); err != nil {
			return fmt.Errorf("upsert order geo: %w", err)
		}
		for _, t := range g.Times {
			if t.Other == "" || t.Other == g.OrderID {
				continue
			}
			if t.ToS != nil {
				if err := q.UpsertOrderTime(ctx, models.UpsertOrderTimeParams{A: g.OrderID, B: t.Other, Seconds: *t.ToS}); err != nil {
					return err
				}
			}
			if t.FromS != nil {
				if err := q.UpsertOrderTime(ctx, models.UpsertOrderTimeParams{A: t.Other, B: g.OrderID, Seconds: *t.FromS}); err != nil {
					return err
				}
			}
		}
		fresh, err = loadOrder(ctx, q, g.OrderID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// DriveTimes — see order.PoolRepository.
func (r *OrderRepository) DriveTimes(ctx context.Context, ids []string) (m map[string]map[string]int64, err error) {
	ctx, _, end := dbSpan(ctx, "orders.drive_times")
	defer end(&err)
	return driveTimes(ctx, models.New(r.db), ids)
}

func driveTimes(ctx context.Context, q *models.Queries, ids []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.ListOrderTimes(ctx, models.ListOrderTimesParams{FromIds: ids, ToIds: ids})
	if err != nil {
		return nil, fmt.Errorf("list order times: %w", err)
	}
	for _, t := range rows {
		if out[t.A] == nil {
			out[t.A] = map[string]int64{}
		}
		out[t.A][t.B] = t.Seconds
	}
	return out, nil
}

// --- AUTO plans --------------------------------------------------------------

func (r *OrderRepository) SaveAutoPlan(ctx context.Context, id string, at time.Time, planJSON []byte) (err error) {
	ctx, _, end := dbSpan(ctx, "auto_plans.save")
	defer end(&err)
	return models.New(r.db).InsertAutoPlan(ctx, models.InsertAutoPlanParams{ID: id, CreatedAt: fmtTS(at), PlanJson: string(planJSON)})
}

func (r *OrderRepository) GetAutoPlan(ctx context.Context, id string) (raw []byte, err error) {
	ctx, _, end := dbSpan(ctx, "auto_plans.get")
	defer end(&err)
	row, err := models.New(r.db).GetAutoPlan(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, order.ErrPlanNotFound
		}
		return nil, err
	}
	return []byte(row.PlanJson), nil
}

// ConfirmAutoPlan — see order.PoolRepository. Each suggestion is checked on
// its own (one sent meanwhile doesn't block the rest).
func (r *OrderRepository) ConfirmAutoPlan(ctx context.Context, id string, runs []order.PlanRun, at time.Time) (applied []order.StoredOrder, skipped []order.PoolSkip, err error) {
	ctx, _, end := dbSpan(ctx, "auto_plans.confirm")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		if _, err := q.GetAutoPlan(ctx, id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return order.ErrPlanNotFound
			}
			return err
		}
		for _, run := range runs {
			_, derr := availableDriver(ctx, q, run.DriverID)
			for _, oid := range run.SuggestedIDs {
				if derr != nil {
					skipped = append(skipped, order.PoolSkip{OrderID: oid, Reason: order.SkipDriverNotAvailable})
					continue
				}
				so, err := loadOrder(ctx, q, oid)
				switch {
				case errors.Is(err, order.ErrNotFound):
					skipped = append(skipped, order.PoolSkip{OrderID: oid, Reason: order.SkipNotFound})
					continue
				case err != nil:
					return err
				case !poolable(so):
					skipped = append(skipped, order.PoolSkip{OrderID: oid, Reason: order.SkipInvalidState})
					continue
				case so.Status.PoolDriverID != "":
					skipped = append(skipped, order.PoolSkip{OrderID: oid, Reason: order.SkipAlreadyPooled})
					continue
				}
				fresh, err := setPool(ctx, q, so, run.DriverID, id, at)
				if err != nil {
					return err
				}
				applied = append(applied, fresh)
			}
		}
		return q.ConfirmAutoPlan(ctx, models.ConfirmAutoPlanParams{ConfirmedAt: nullTS(at), ID: id})
	})
	if err != nil {
		return nil, nil, err
	}
	return applied, skipped, nil
}

// --- delivery settings -------------------------------------------------------

func (r *OrderRepository) DeliveryPrefs(ctx context.Context) (order.DeliveryPrefs, error) {
	row, err := models.New(r.db).GetDeliveryPrefs(ctx)
	if err != nil {
		return order.DefaultDeliveryPrefs, err
	}
	return order.DeliveryPrefs{SoonReadyMinutes: row.SoonReadyMinutes, DeliveryTargetMinutes: row.DeliveryTargetMinutes}, nil
}

func (r *OrderRepository) SetDeliveryPrefs(ctx context.Context, p order.DeliveryPrefs) error {
	return models.New(r.db).SetDeliveryPrefs(ctx, models.SetDeliveryPrefsParams{
		SoonReadyMinutes: p.SoonReadyMinutes, DeliveryTargetMinutes: p.DeliveryTargetMinutes})
}

// --- store map file (settings.map_file) ----------------------------------------

// GetMapFile / SetMapFile persist the downloaded store map's metadata (JSON).
func (r *OrderRepository) GetMapFile(ctx context.Context) (string, error) {
	return models.New(r.db).GetMapFile(ctx)
}

func (r *OrderRepository) SetMapFile(ctx context.Context, raw string) error {
	return models.New(r.db).SetMapFile(ctx, raw)
}
