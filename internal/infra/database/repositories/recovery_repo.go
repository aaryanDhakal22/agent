package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printroute"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
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
	if st.CounterNext, err = q.GetCounterNext(ctx); err != nil {
		return st, err
	}
	if p, err := q.GetFloorPrefs(ctx); err == nil {
		st.Prefs = order.FloorPrefs{GreenTillMinutes: p.GreenTillMinutes, TipTaxBps: p.TipTaxBps}
	}
	if raw, err := q.GetReceiptPrefs(ctx); err == nil {
		if rp, err := order.DecodeReceiptPrefs([]byte(raw)); err == nil {
			st.ReceiptPrefs = &rp
		}
	}
	if raw, err := q.GetPrintRouting(ctx); err == nil && raw != "" {
		if pr, err := printroute.Decode([]byte(raw)); err == nil {
			st.PrintRouting = &pr
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
		if err := restoreDrawers(ctx, q, data.Drawers, batchID, &res); err != nil {
			return err
		}

		// Orders.
		maxKitchen, maxCounter := int64(0), int64(0)
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
			if snap.Source == order.SourceCounter && snap.Fulfillment.Type == "counter" {
				snap.Fulfillment.Type = "dine_in" // main keeps "counter"; the tablets know dine_in
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
			poolID, pooledAt, poolSeq, poolPlan := sql.NullString{}, sql.NullString{}, sql.NullInt64{}, sql.NullString{}
			if st.PoolDriverID != "" && (st.State == order.StateAccepted || st.State == order.StateReady) {
				poolID, pooledAt, poolSeq = sql.NullString{String: st.PoolDriverID, Valid: true}, nullTSPtr(st.PooledAt), nullInt(st.PoolSeq)
				poolPlan = sql.NullString{String: st.PoolPlanID, Valid: st.PoolPlanID != ""}
			}
			p := models.RestoreOrderParams{
				ID: snap.ID, TenantID: snap.TenantID, LocationID: snap.LocationID, Source: snap.Source,
				SourceEventID: snap.SourceEventID, Snapshot: string(raw), State: string(st.State),
				ArrivalDate: fmtTS(st.ArrivalDate), PrintedDate: nullTSPtr(st.PrintedDate), BatchID: batchID,
				KitchenNumber: nullInt(st.KitchenNumber), PrepMinutes: nullInt(st.PrepMinutes),
				EstimatedReadyAt: nullTSPtr(st.EstimatedReadyAt), AcceptedAt: nullTSPtr(st.AcceptedAt),
				ReadyAt: nullTSPtr(st.ReadyAt), OutAt: nullTSPtr(st.OutAt), DeliveredAt: nullTSPtr(st.DeliveredAt),
				DriverID: driverID, DriverName: st.DriverName, CancelledAt: nullTSPtr(st.CancelledAt),
				PoolDriverID: poolID, PooledAt: pooledAt, PoolSeq: poolSeq, PoolPlanID: poolPlan,
				ScheduledLeadMinutes: nullInt(st.ScheduledLeadMinutes), RingAt: nullTSPtr(st.RingAt), Rerung: st.Rerung,
				CameBack: st.CameBack && st.State == order.StateReady, RunSeq: nullInt(st.RunSeq),
				AddedTipCents: st.AddedTipCents, TipLocked: st.TipLocked, CancelReason: st.CancelReason,
			}
			if snap.Source == order.SourceCounter { // terminal stage 0d: paid state + the terminal's key
				p.Paid, p.PaidAt = st.Paid, nullTSPtr(st.PaidAt)
				p.TipCents = sql.NullInt64{Int64: snap.Money.Tip, Valid: true}
				p.ClientKey = snap.SourceEventID
			}
			if nd := st.NotDelivered; nd != nil {
				p.NotDeliveredAction, p.NotDeliveredAt, p.NotDeliveredDriverID = nd.Action, nullTSPtr(&nd.At), nd.DriverID
			}
			if err := q.RestoreOrder(ctx, p); err != nil {
				return fmt.Errorf("restore order %s: %w", snap.ID, err)
			}
			if err := restorePayments(ctx, q, ro, batchID, &res); err != nil {
				return err
			}
			res.Orders++
			if st.State == order.StateArrived {
				res.Waiting++
			}
			switch {
			case st.KitchenNumber == nil:
			case snap.Source == order.SourceCounter: // the counter sequence is its own
				maxCounter = max(maxCounter, *st.KitchenNumber)
			case *st.KitchenNumber > maxKitchen:
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
		var counter int64
		if data.State != nil {
			counter = data.State.CounterNext
		}
		if maxCounter > 0 && maxCounter >= counter {
			counter = maxCounter + 1 // never hand out a counter number twice
		}
		if err := q.SetCounterNext(ctx, counter); err != nil {
			return err
		}
		res.CounterNext = counter
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
			// The routing setting comes back as it was (its printer ids are
			// store_config's, the same store's); an unreadable one is dropped.
			if pr := data.State.PrintRouting; pr != nil {
				if err := setPrintRouting(ctx, q, pr.Normalize()); err != nil {
					return err
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

// restorePayments brings a restored order's payments back (HARDENING F-9):
// the refund / void / change checks and the Z read order_payments, so a Pi
// swapped mid-batch must have them. main's payments when it sends them; else
// rebuilt from the snapshot's payments (amounts only: no tips, no drawer).
// Counter orders, and online orders a terminal took money on.
func restorePayments(ctx context.Context, q *models.Queries, ro order.RestoreOrder, batchID int64, res *order.RestoreResult) error {
	snap := ro.Order
	insert := func(p terminal.Payment) error {
		if p.AmountCents == 0 && p.TipCents == 0 {
			return nil
		}
		if err := q.InsertOrderPayment(ctx, models.InsertOrderPaymentParams{ID: p.ID, OrderID: snap.ID, At: fmtTS(p.At), Tender: p.Tender,
			AmountCents: p.AmountCents, TipCents: p.TipCents, Refund: p.Refund, Reason: p.Reason, Ref: p.Ref, Last4: p.Last4,
			StaffID: p.StaffID, DeviceID: p.DeviceID, ApprovedBy: p.ApprovedBy, DrawerID: p.DrawerID, Brand: p.Brand,
			RefundKind: p.RefundKind}); err != nil {
			return fmt.Errorf("restore payment of %s: %w", snap.ID, err)
		}
		return nil
	}
	tenderOf := func(method string) string {
		if terminal.ValidTender(method) {
			return method
		}
		return terminal.TenderCard
	}
	if len(ro.Payments) > 0 {
		for _, mp := range ro.Payments {
			p := terminal.Payment{ID: mp.ID, At: mp.At, Tender: tenderOf(mp.Method), AmountCents: mp.AmountCents, TipCents: mp.TipCents,
				Last4: mp.Last4, Ref: mp.Ref, StaffID: mp.StaffID, DeviceID: mp.DeviceID, DrawerID: mp.DrawerID, Brand: mp.Brand,
				RefundKind: mp.RefundKind, Reason: mp.Reason, ApprovedBy: mp.ApprovedBy}
			if p.ID == "" {
				p.ID = uuid.NewString()
			}
			if p.AmountCents < 0 {
				p.Refund, p.AmountCents = true, -p.AmountCents
			}
			if p.At.IsZero() {
				p.At = time.Now()
			}
			if err := insert(p); err != nil {
				return err
			}
			res.Payments++
		}
		return nil
	}
	if snap.Source != order.SourceCounter {
		return nil // an online order's payments are main's (the cash rule)
	}
	for i, ps := range snap.Payments {
		p := terminal.Payment{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s/%d", snap.ID, i))).String(),
			At: snap.PlacedAt, Tender: terminal.TenderOf(ps.Method), AmountCents: ps.AmountCents, Last4: ps.Last4,
			Reason: "restored"}
		if p.AmountCents < 0 {
			p.Refund, p.AmountCents = true, -p.AmountCents
		}
		if p.At.IsZero() {
			p.At = time.Now()
		}
		if err := insert(p); err != nil {
			return err
		}
		res.RebuiltPayments++
	}
	return nil
}

// restoreDrawers brings the open batch's open drawer sessions back (F-9).
func restoreDrawers(ctx context.Context, q *models.Queries, drawers []order.RestoreDrawer, batchID int64, res *order.RestoreResult) error {
	for _, d := range drawers {
		if d.Closed || d.ID == "" || d.DeviceID == "" {
			continue
		}
		at := d.OpenedAt
		if at.IsZero() {
			at = time.Now()
		}
		if err := q.InsertDrawerSession(ctx, models.InsertDrawerSessionParams{ID: d.ID, DeviceID: d.DeviceID, DeviceName: d.DeviceName,
			StaffID: d.StaffID, StaffName: d.StaffName, BatchID: batchID, OpenedAt: fmtTS(at), StartCents: max(d.StartCents, 0),
			HandoverFrom: d.HandoverFrom, Auto: d.Auto}); err != nil {
			return fmt.Errorf("restore drawer %s: %w", d.ID, err)
		}
		res.Drawers++
	}
	return nil
}
