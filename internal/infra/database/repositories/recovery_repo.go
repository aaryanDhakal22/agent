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

var _ order.RecoveryRepository = (*OrderRepository)(nil)

// AgentState — see order.RecoveryRepository.
func (r *OrderRepository) AgentState(ctx context.Context) (st order.AgentState, err error) {
	ctx, _, end := dbSpan(ctx, "recovery.agent_state")
	defer end(&err)
	q := models.New(r.db)
	st.Version = 1
	b, err := q.GetOpenBatch(ctx)
	if err != nil {
		return st, fmt.Errorf("open batch: %w", err)
	}
	opened, err := parseTS(b.OpenedAt)
	if err != nil {
		return st, err
	}
	st.Batch = order.StateBatch{ID: b.ID, OpenedAt: opened}
	fs, err := q.GetFloorSettings(ctx)
	if err != nil {
		return st, err
	}
	st.KitchenNext, st.LastPrepMinutes = fs.KitchenNext, fs.LastPrepMinutes
	if p, err := q.GetFloorPrefs(ctx); err == nil {
		st.Prefs = order.FloorPrefs{GreenTillMinutes: p.GreenTillMinutes, TipTaxBps: p.TipTaxBps}
	}
	if raw, err := q.GetReceiptPrefs(ctx); err == nil {
		if rp, err := order.DecodeReceiptPrefs([]byte(raw)); err == nil {
			st.ReceiptPrefs = &rp
		}
	}
	if rs, err := q.GetDriverReset(ctx); err == nil {
		st.DriverResetTime = rs.DriverResetTime
		if last, err := optTS(sql.NullString{String: rs.LastDriverResetAt, Valid: rs.LastDriverResetAt != ""}); err == nil {
			st.LastDriverResetAt = last
		}
	}
	if aa, err := q.GetAutoAccept(ctx); err == nil {
		st.AutoAccept = aa
	}
	drivers, err := q.ListActiveDrivers(ctx)
	if err != nil {
		return st, err
	}
	st.Drivers = make([]order.StateDriver, 0, len(drivers))
	for _, d := range drivers {
		ret, err := optTS(d.LastReturnedAt)
		if err != nil {
			return st, err
		}
		st.Drivers = append(st.Drivers, order.StateDriver{ID: d.ID, Name: d.Name, Position: d.Position, Status: d.Status, LastReturnedAt: ret})
	}
	shifts, err := q.ListOpenShifts(ctx)
	if err != nil {
		return st, err
	}
	st.OpenShifts = make([]order.StateShift, 0, len(shifts))
	for _, sh := range shifts {
		in, err := parseTS(sh.ClockIn)
		if err != nil {
			return st, err
		}
		st.OpenShifts = append(st.OpenShifts, order.StateShift{ID: sh.ID, DriverID: sh.DriverID, DriverName: sh.DriverName, BatchID: sh.BatchID, ClockIn: in})
	}
	return st, nil
}

func nullTSPtr(t *time.Time) sql.NullString {
	if t == nil || t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: fmtTS(*t), Valid: true}
}

func nullInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// Restore — see order.RecoveryRepository. One transaction: the open batch (with
// the old agent's id and opening time), every order with its floor state, the
// kitchen counter, the settings, the drivers (same ids as main's mirror) and
// their open shifts. No floor events are written — main already has all of it.
func (r *OrderRepository) Restore(ctx context.Context, data order.RestoreData) (res order.RestoreResult, err error) {
	ctx, _, end := dbSpan(ctx, "recovery.restore")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		nOrders, err := q.CountAllOrders(ctx)
		if err != nil {
			return err
		}
		nDrivers, err := q.CountAllDrivers(ctx)
		if err != nil {
			return err
		}
		if nOrders > 0 || nDrivers > 0 {
			return order.ErrNotEmpty
		}

		// Batch: continue the old agent's open batch.
		batchID := int64(0)
		if data.State != nil && data.State.Batch.ID > 0 {
			if err := q.DeleteAllBatches(ctx); err != nil {
				return err
			}
			opened := data.State.Batch.OpenedAt
			if opened.IsZero() {
				opened = time.Now()
			}
			if err := q.InsertBatchWithID(ctx, models.InsertBatchWithIDParams{ID: data.State.Batch.ID, OpenedAt: fmtTS(opened)}); err != nil {
				return fmt.Errorf("restore batch: %w", err)
			}
			batchID = data.State.Batch.ID
		} else {
			b, err := q.GetOpenBatch(ctx)
			if err != nil {
				return err
			}
			batchID = b.ID
		}
		res.BatchID = batchID

		// Orders.
		maxKitchen := int64(0)
		for _, ro := range data.Orders {
			snap := ro.Order
			var st order.Status
			if ro.AgentStatus != nil && ro.AgentStatus.State.Valid() {
				st = *ro.AgentStatus
			} else {
				st = order.StatusFromSnapshot(snap)
			}
			if st.ArrivalDate.IsZero() {
				st.ArrivalDate = snap.ReceivedAt
			}
			if st.ArrivalDate.IsZero() {
				st.ArrivalDate = time.Now()
			}
			raw, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			driverID := sql.NullString{}
			if st.DriverID != "" {
				driverID = sql.NullString{String: st.DriverID, Valid: true}
			}
			// A pool slot comes back with the order (2.0.9) while it can still
			// be sent out.
			poolID, pooledAt := sql.NullString{}, sql.NullString{}
			if st.PoolDriverID != "" && (st.State == order.StateAccepted || st.State == order.StateReady) {
				poolID, pooledAt = sql.NullString{String: st.PoolDriverID, Valid: true}, nullTSPtr(st.PooledAt)
			}
			if err := q.RestoreOrder(ctx, models.RestoreOrderParams{
				ID: snap.ID, TenantID: snap.TenantID, LocationID: snap.LocationID, Source: snap.Source,
				SourceEventID: snap.SourceEventID, Snapshot: string(raw), State: string(st.State),
				ArrivalDate: fmtTS(st.ArrivalDate), PrintedDate: nullTSPtr(st.PrintedDate), BatchID: batchID,
				KitchenNumber: nullInt(st.KitchenNumber), PrepMinutes: nullInt(st.PrepMinutes),
				EstimatedReadyAt: nullTSPtr(st.EstimatedReadyAt), AcceptedAt: nullTSPtr(st.AcceptedAt),
				ReadyAt: nullTSPtr(st.ReadyAt), OutAt: nullTSPtr(st.OutAt), DeliveredAt: nullTSPtr(st.DeliveredAt),
				DriverID: driverID, DriverName: st.DriverName, CancelledAt: nullTSPtr(st.CancelledAt),
				PoolDriverID: poolID, PooledAt: pooledAt,
			}); err != nil {
				return fmt.Errorf("restore order %s: %w", snap.ID, err)
			}
			res.Orders++
			if st.State == order.StateArrived {
				res.Waiting++
			}
			if st.KitchenNumber != nil && *st.KitchenNumber > maxKitchen {
				maxKitchen = *st.KitchenNumber
			}
		}

		// Counters + settings.
		next, prep := int64(1100), int64(20)
		if data.State != nil {
			if data.State.KitchenNext > 0 {
				next = data.State.KitchenNext
			}
			if data.State.LastPrepMinutes > 0 {
				prep = data.State.LastPrepMinutes
			}
		}
		if maxKitchen >= next {
			next = maxKitchen + 1 // never hand out a number twice
		}
		if err := q.SetKitchenCounters(ctx, models.SetKitchenCountersParams{KitchenNext: next, LastPrepMinutes: prep}); err != nil {
			return err
		}
		res.KitchenNext = next
		if data.State != nil {
			if p := data.State.Prefs; p.GreenTillMinutes > 0 {
				if err := q.SetFloorPrefs(ctx, models.SetFloorPrefsParams{GreenTillMinutes: p.GreenTillMinutes, TipTaxBps: p.TipTaxBps}); err != nil {
					return err
				}
			}
			// Receipt settings from an older agent's report are absent (keep
			// the defaults); invalid ones are dropped rather than failing the
			// whole restore.
			if rp := data.State.ReceiptPrefs; rp != nil {
				if p := rp.Normalize(); p.Validate() == nil {
					if err := setReceiptPrefs(ctx, q, p); err != nil {
						return err
					}
				}
			}
			if data.State.DriverResetTime != "" {
				if err := q.SetDriverResetTime(ctx, data.State.DriverResetTime); err != nil {
					return err
				}
			}
			if data.State.LastDriverResetAt != nil {
				if err := q.SetLastDriverReset(ctx, fmtTS(*data.State.LastDriverResetAt)); err != nil {
					return err
				}
			}
			if err := q.SetAutoAccept(ctx, data.State.AutoAccept); err != nil {
				return err
			}
		}

		// Drivers (state first; main's mirror as the fallback) + open shifts.
		if data.State != nil && len(data.State.Drivers) > 0 {
			for _, d := range data.State.Drivers {
				if err := q.RestoreDriver(ctx, models.RestoreDriverParams{ID: d.ID, Name: d.Name, Position: d.Position,
					Status: driverStatus(d.Status), LastReturnedAt: nullTSPtr(d.LastReturnedAt)}); err != nil {
					return fmt.Errorf("restore driver %s: %w", d.Name, err)
				}
				res.Drivers++
			}
			for _, sh := range data.State.OpenShifts {
				if err := q.RestoreShift(ctx, models.RestoreShiftParams{ID: sh.ID, DriverID: sh.DriverID, DriverName: sh.DriverName,
					BatchID: batchID, ClockIn: fmtTS(sh.ClockIn)}); err != nil {
					return fmt.Errorf("restore shift: %w", err)
				}
				res.Shifts++
			}
		} else {
			for _, d := range data.Drivers {
				if err := q.RestoreDriver(ctx, models.RestoreDriverParams{ID: d.ID, Name: d.Name, Position: d.Position,
					Status: driverStatus(d.Status)}); err != nil {
					return fmt.Errorf("restore driver %s: %w", d.Name, err)
				}
				res.Drivers++
			}
		}
		return nil
	})
	return res, err
}

func driverStatus(s string) string {
	switch s {
	case order.DriverAvailable, order.DriverOut:
		return s
	}
	return order.DriverOff
}
