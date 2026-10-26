package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
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
		if so.Status.CameBack && driverID != "" {
			return nil, fmt.Errorf("order %s: %w", id, order.ErrCameBack)
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
		for _, id := range dedupe(meta.Exclude) { // left behind: back to unassigned
			so, err := loadOrder(ctx, q, id)
			switch {
			case errors.Is(err, order.ErrNotFound):
				continue
			case err != nil:
				return err
			case so.Status.PoolDriverID != d.ID:
				continue
			}
			if _, err := setPool(ctx, q, so, "", "", now); err != nil {
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
			if err := feedReady(ctx, q, so, now); err != nil {
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
	suggested := make([]string, 0, len(meta.SuggestedIDs)) // a copy: rewritten below
	ev := order.DeliveryRunEvent{
		EventID: uuid.NewString(), Type: order.FloorEventDeliveryRun, At: now.UTC().Truncate(time.Millisecond),
		BatchID: sent[0].Status.BatchID, DriverID: d.ID, DriverName: d.Name, PlanID: meta.PlanID,
		SuggestedIDs: suggested,
		Stops:        order.EstimateStops(ids, meta.StopOrder, now, storeTo, times),
	}
	// The run's stop order is kept on the orders (run_seq, the tablet's
	// address list under a driver who is out) and on the caller's sent list.
	for i, st := range ev.Stops {
		seq := int64(i + 1)
		if err := q.SetRunSeq(ctx, models.SetRunSeqParams{RunSeq: sql.NullInt64{Int64: seq, Valid: true}, ID: st.OrderID}); err != nil {
			return fmt.Errorf("set run seq: %w", err)
		}
		for j := range sent {
			if sent[j].Order.ID == st.OrderID {
				sent[j].Status.RunSeq = &seq
			}
		}
	}
	// POS stops reach main anonymous: an opaque id and times, never the
	// order id, a name or an address.
	byID := map[string]order.StoredOrder{}
	for _, so := range sent {
		byID[so.Order.ID] = so
	}
	opaque := map[string]string{}
	for i, st := range ev.Stops {
		so := byID[st.OrderID]
		if !so.Status.LocalOnly {
			continue
		}
		stop := order.RunStop{OpaqueID: order.PosOpaqueID(so.Status.BatchID, so.Status.ExtRef), Source: order.SourcePOS,
			ETA: st.ETA, SentAt: so.Status.SentAt, OutAt: so.Status.OutAt, DeliveredAt: so.Status.DeliveredAt}
		if !so.Order.PlacedAt.IsZero() {
			placed := so.Order.PlacedAt.UTC()
			stop.PlacedAt = &placed
		}
		if so.Geo.Located() {
			dist := so.Geo.DistanceM
			stop.DistanceM = &dist
		}
		opaque[st.OrderID] = stop.OpaqueID
		ev.Stops[i] = stop
	}
	// AUTO's suggestions too: a POS order (sent on this run or not) by its
	// opaque id, never its local id; an id that is no order here is dropped.
	for _, id := range meta.SuggestedIDs {
		if o, ok := opaque[id]; ok {
			ev.SuggestedIDs = append(ev.SuggestedIDs, o)
			continue
		}
		if _, ok := byID[id]; ok {
			ev.SuggestedIDs = append(ev.SuggestedIDs, id) // an online stop
			continue
		}
		row, err := q.GetOrderByID(ctx, id)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("suggested order %s: %w", id, err)
		case row.LocalOnly:
			ev.SuggestedIDs = append(ev.SuggestedIDs, order.PosOpaqueID(row.BatchID, row.ExtRef))
		default:
			ev.SuggestedIDs = append(ev.SuggestedIDs, id)
		}
	}
	for _, s := range ev.Stops {
		if s.OpaqueID != "" {
			ev.SentIDs = append(ev.SentIDs, s.OpaqueID)
		} else {
			ev.SentIDs = append(ev.SentIDs, s.OrderID)
		}
	}
	if err := insertEvent(ctx, q, ev.EventID, "", ev.Type, now, ev); err != nil {
		return err
	}
	// The hub writes the driver back onto the POS checks of this run.
	rs := order.RunSentData{Driver: order.FeedDriver{ID: d.ID, Name: d.Name}, RunID: ev.EventID, Orders: make([]order.FeedOrderRef, 0, len(sent))}
	if d.PosNumber != "" {
		p := d.PosNumber
		rs.Driver.PosNumber = &p
	}
	for _, so := range sent {
		rs.Orders = append(rs.Orders, order.RefOf(so))
	}
	return insertFeed(ctx, q, order.FeedRunSent, now, rs)
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
		before, err := loadOrder(ctx, q, g.OrderID)
		if err != nil {
			return err
		}
		p := models.UpsertOrderGeoParams{OrderID: g.OrderID, State: g.State, Quality: g.Quality,
			DistanceM: g.DistanceM, DurationS: g.DurationS, Polyline: g.Polyline, OutsideArea: g.OutsideArea,
			StoreToS: nullInt(g.StoreToS), ToStoreS: nullInt(g.ToStoreS), UpdatedAt: fmtTS(now)}
		if g.Lat != nil && g.Lng != nil {
			p.Lat = sql.NullFloat64{Float64: *g.Lat, Valid: true}
			p.Lng = sql.NullFloat64{Float64: *g.Lng, Valid: true}
		}
		if len(g.Candidates) > 0 {
			b, err := json.Marshal(g.Candidates)
			if err != nil {
				return err
			}
			p.Candidates = string(b)
		}
		if g.ConfirmedAddress != nil {
			b, err := json.Marshal(g.ConfirmedAddress)
			if err != nil {
				return err
			}
			p.ConfirmedAddress = string(b)
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
		if fresh, err = loadOrder(ctx, q, g.OrderID); err != nil {
			return err
		}
		// Staff confirmed the place (main's direct answer to geo.choose or
		// geo.pin, flagged confirmed — a cache hit that comes back pinned is
		// not): the hub heals the POS address from it. The same place again
		// writes no second event.
		if g.Confirmed && fresh.Geo.Confirmed() && !sameConfirmed(before.Geo, fresh.Geo) {
			ev := order.AddressConfirmedData{OrderID: fresh.Order.ID, ExtRef: order.RefOf(fresh).ExtRef, How: "pin",
				Lat: *fresh.Geo.Lat, Lng: *fresh.Geo.Lng}
			if fresh.Geo.ConfirmedAddress != nil {
				ev.How, ev.Address = "candidate", fresh.Geo.ConfirmedAddress
			}
			return insertFeed(ctx, q, order.FeedAddressConfirmed, now, ev)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// sameConfirmed reports whether two geos are the same confirmed place.
func sameConfirmed(a, b *order.OrderGeo) bool {
	if !a.Confirmed() || !b.Confirmed() {
		return false
	}
	if *a.Lat != *b.Lat || *a.Lng != *b.Lng || a.Quality != b.Quality {
		return false
	}
	switch {
	case a.ConfirmedAddress == nil && b.ConfirmedAddress == nil:
		return true
	case a.ConfirmedAddress == nil || b.ConfirmedAddress == nil:
		return false
	}
	return *a.ConfirmedAddress == *b.ConfirmedAddress
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

// LatestAutoPlan — see order.PoolRepository.
func (r *OrderRepository) LatestAutoPlan(ctx context.Context) (createdAt time.Time, confirmed bool, err error) {
	row, err := models.New(r.db).GetLatestAutoPlan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	at, err := parseTS(row.CreatedAt)
	return at, row.ConfirmedAt.Valid, err
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
				case so.Status.CameBack:
					skipped = append(skipped, order.PoolSkip{OrderID: oid, Reason: order.SkipCameBack})
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
	return order.DeliveryPrefs{SoonReadyMinutes: row.SoonReadyMinutes, DeliveryTargetMinutes: row.DeliveryTargetMinutes,
		NearbyRule: row.NearbyRule, NearbyDetourMinutes: row.NearbyDetourMinutes, NearbyDistanceMi: row.NearbyDistanceMi,
		AutoAssign: row.AutoAssign}, nil
}

func (r *OrderRepository) SetDeliveryPrefs(ctx context.Context, p order.DeliveryPrefs) error {
	return models.New(r.db).SetDeliveryPrefs(ctx, models.SetDeliveryPrefsParams{
		SoonReadyMinutes: p.SoonReadyMinutes, DeliveryTargetMinutes: p.DeliveryTargetMinutes,
		NearbyRule: p.NearbyRule, NearbyDetourMinutes: p.NearbyDetourMinutes, NearbyDistanceMi: p.NearbyDistanceMi,
		AutoAssign: p.AutoAssign})
}

// --- store map file (settings.map_file) ----------------------------------------

// GetMapFile / SetMapFile persist the downloaded store map's metadata (JSON).
func (r *OrderRepository) GetMapFile(ctx context.Context) (string, error) {
	return models.New(r.db).GetMapFile(ctx)
}

func (r *OrderRepository) SetMapFile(ctx context.Context, raw string) error {
	return models.New(r.db).SetMapFile(ctx, raw)
}
