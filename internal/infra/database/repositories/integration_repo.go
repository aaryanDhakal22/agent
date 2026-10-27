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

// The store hub integration (2.1.0): POS orders, the hub's feed, feature /
// ticket / time settings, future orders. Feed events are written in the same
// transaction as the change they describe (integration_events is the hub's
// outbox, like floor_events is main's).

var _ order.IntegrationRepository = (*OrderRepository)(nil)

// insertFeed appends one event to the hub's feed.
func insertFeed(ctx context.Context, q *models.Queries, typ string, at time.Time, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal feed event: %w", err)
	}
	return q.InsertIntegrationEvent(ctx, models.InsertIntegrationEventParams{Type: typ, Payload: string(raw), CreatedAt: fmtTS(at)})
}

// feedReady appends order.ready for an order that just became ready (with
// the money the hub's order callout needs: a terminal takeout is called out
// only once it is paid).
func feedReady(ctx context.Context, q *models.Queries, so order.StoredOrder, at time.Time) error {
	return insertFeed(ctx, q, order.FeedOrderReady, at, order.OrderReadyData{FeedOrderRef: order.RefOf(so),
		Number: so.Number(), FulfillmentType: so.Order.Fulfillment.Type, Paid: so.Status.Paid, CashDueCents: so.Status.CashDueCents})
}

// feedCancelled appends order.cancelled.
func feedCancelled(ctx context.Context, q *models.Queries, so order.StoredOrder, at time.Time) error {
	return insertFeed(ctx, q, order.FeedOrderCancelled, at, order.RefOf(so))
}

// --- settings ------------------------------------------------------------------

func integrationSettings(ctx context.Context, q *models.Queries) (order.IntegrationSettings, error) {
	set := order.DefaultIntegrationSettings()
	row, err := q.GetIntegrationSettings(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return set, nil
		}
		return set, fmt.Errorf("integration settings: %w", err)
	}
	if set.Features, err = order.DecodeFeatures([]byte(row.Features)); err != nil {
		set.Features = order.DefaultFeatures()
	}
	if set.Tickets, err = order.DecodeTicketToggles([]byte(row.TicketToggles)); err != nil {
		set.Tickets = order.DefaultTicketToggles()
	}
	set.Times = order.Times{DefaultPickupMinutes: row.DefaultPickupMinutes, DefaultDeliveryMinutes: row.DefaultDeliveryMinutes}
	if set.Finishing, err = order.DecodeFinishing([]byte(row.Finishing)); err != nil {
		set.Finishing = order.DefaultFinishing()
	}
	if set.Callout, err = order.DecodeCallout([]byte(row.Callout)); err != nil {
		set.Callout = order.DefaultCallout()
	}
	return set, nil
}

// IntegrationSettings — see order.IntegrationRepository.
func (r *OrderRepository) IntegrationSettings(ctx context.Context) (set order.IntegrationSettings, err error) {
	ctx, _, end := dbSpan(ctx, "settings.integration")
	defer end(&err)
	return integrationSettings(ctx, models.New(r.db))
}

// SetIntegrationSettings stores every field (so a later default change can't
// flip a switch the manager chose) and writes the feed's settings.changed.
func (r *OrderRepository) SetIntegrationSettings(ctx context.Context, set order.IntegrationSettings) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_integration")
	defer end(&err)
	f, err := json.Marshal(set.Features)
	if err != nil {
		return err
	}
	t, err := json.Marshal(set.Tickets)
	if err != nil {
		return err
	}
	fin, err := json.Marshal(set.Finishing)
	if err != nil {
		return err
	}
	co, err := json.Marshal(set.Callout)
	if err != nil {
		return err
	}
	return r.withTx(ctx, func(q *models.Queries) error {
		if err := q.SetIntegrationSettings(ctx, models.SetIntegrationSettingsParams{Features: string(f), TicketToggles: string(t),
			DefaultPickupMinutes: set.Times.DefaultPickupMinutes, DefaultDeliveryMinutes: set.Times.DefaultDeliveryMinutes,
			Finishing: string(fin), Callout: string(co)}); err != nil {
			return err
		}
		return insertFeed(ctx, q, order.FeedSettingsChanged, time.Now(), set)
	})
}

// IntegrationStatus returns the hub's last stored status (zero when none).
func (r *OrderRepository) IntegrationStatus(ctx context.Context) (order.IntegrationStatus, error) {
	var st order.IntegrationStatus
	raw, err := models.New(r.db).GetIntegrationStatus(ctx)
	if err != nil || raw == "" {
		return st, err
	}
	err = json.Unmarshal([]byte(raw), &st)
	return st, err
}

// SetIntegrationStatus stores the hub's last status.
func (r *OrderRepository) SetIntegrationStatus(ctx context.Context, st order.IntegrationStatus) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return models.New(r.db).SetIntegrationStatus(ctx, string(raw))
}

// --- POS orders ------------------------------------------------------------------

// UpsertExternal — see order.IntegrationRepository.
//
// 00020: a terminal change to a check already on the tablet is diffed
// (lines added / removed); a kitchen line rings the tablet until OK
// (pos_change), a ready order with a kitchen line added goes back to
// cooking (out of its pool), lines removed stay struck through
// (removed_items). Visibility on Kitchen is sticky: a hidden check that now
// qualifies is shown as new. A check the POS closed is finished once ready.
func (r *OrderRepository) UpsertExternal(ctx context.Context, extRef string, in order.ExternalOrder, meta order.ExternalMeta) (res order.ExternalResult, err error) {
	ctx, _, end := dbSpan(ctx, "orders.upsert_external")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetOrderByExtRef(ctx, models.GetOrderByExtRefParams{ExtSource: in.Source, ExtRef: extRef})
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return insertExternal(ctx, q, extRef, in, meta, &res)
		case err != nil:
			return fmt.Errorf("find pos order: %w", err)
		}
		cur, err := toStored(row)
		if err != nil {
			return err
		}
		res.PrevDriverPos = row.ExtDriverPos
		if row.ExtHash == in.Hash {
			res.Order, err = loadOrder(ctx, q, row.ID)
			return err
		}
		snap := in.Snapshot(row.ID, row.TenantID, row.LocationID, extRef, meta.Now)
		snap.ReceivedAt = cur.Order.ReceivedAt
		state := cur.Status.State
		done := state == order.StateDelivered || state == order.StateCancelled
		wasShown := !row.BoardHidden
		added, removed, _, removedItems := order.POSDiff(cur.Order.Items, snap.Items)
		reopen := !done && state == order.StateReady && wasShown && order.AnyKitchen(added) && !cur.Status.CameBack
		frozen := done || (cur.Status.FinishedAt != nil && !reopen)
		hidden := in.BoardHidden && !wasShown // once on Kitchen, it stays
		if frozen {
			// Done on the floor: only the money and paid move (never re-opened).
			kept := cur.Order
			kept.Money, kept.Payments = snap.Money, snap.Payments
			snap = kept
			hidden = row.BoardHidden
		} else if wasShown {
			markPOSLines(&snap, cur.Order, removedItems)
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if err := q.UpdateExternalOrder(ctx, models.UpdateExternalOrderParams{Snapshot: string(raw), ExtHash: in.Hash,
			DisplayNumber: sql.NullInt64{Int64: in.DisplayNumber, Valid: in.DisplayNumber > 0}, Paid: in.Paid,
			CashDueCents: sql.NullInt64{Int64: in.CashDueCents, Valid: true}, TipCents: sql.NullInt64{Int64: in.TipCents, Valid: true},
			BoardHidden: hidden, ID: row.ID}); err != nil {
			return fmt.Errorf("update pos order: %w", err)
		}
		if in.Closed != row.PosClosed {
			if err := q.SetPosClosed(ctx, models.SetPosClosedParams{PosClosed: in.Closed, ID: row.ID}); err != nil {
				return err
			}
		}
		res.Changed = true
		if !frozen {
			wasDelivery, isDelivery := cur.IsDelivery(), snap.Fulfillment.Type == "delivery"
			// Became a delivery, or a delivery whose address changed: locate
			// it. Stopped being one: main forgets its point, and it leaves
			// its pool and its geo here.
			res.AddressChanged = isDelivery && (!wasDelivery || !sameAddress(cur.Order.Fulfillment.Address, snap.Fulfillment.Address))
			res.DeliveryEnded = wasDelivery && !isDelivery
			if res.DeliveryEnded {
				if err := q.ClearOrderPool(ctx, row.ID); err != nil {
					return err
				}
				if err := q.DeleteOrderGeo(ctx, row.ID); err != nil {
					return err
				}
			}
			if row.BoardHidden && !in.BoardHidden && state == order.StateAccepted {
				// Too small before, it qualifies now: on Kitchen as a new order.
				if err := q.ShowPosOrder(ctx, models.ShowPosOrderParams{At: nullTS(meta.Now),
					EstimatedReadyAt: nullTS(meta.Now.Add(time.Duration(meta.PrepMinutes) * time.Minute)), ID: row.ID}); err != nil {
					return err
				}
				res.Shown = true
			}
			if reopen {
				n, err := q.ReopenPosOrder(ctx, models.ReopenPosOrderParams{
					EstimatedReadyAt: nullTS(meta.Now.Add(time.Duration(meta.PrepMinutes) * time.Minute)), ID: row.ID})
				if err != nil {
					return fmt.Errorf("back to cooking: %w", err)
				}
				res.Reopened = n > 0
			}
			if wasShown && !done && (len(added) > 0 || len(removed) > 0) {
				if order.AnyKitchen(added) || order.AnyKitchen(removed) {
					if err := recordPOSChange(ctx, q, cur, added, removed, res.Reopened, meta.Now, &res); err != nil {
						return err
					}
				}
				if state == order.StateAccepted || state == order.StateReady {
					for _, it := range removedItems {
						if it.Kitchen {
							res.VoidPrint = append(res.VoidPrint, it)
						}
					}
				}
			}
		}
		if in.Closed && !(snap.Fulfillment.Type == "delivery") {
			fin, err := finishClosed(ctx, q, row.ID, hidden, meta.Now)
			if err != nil {
				return err
			}
			res.Finished = fin
		}
		res.Order, err = loadOrder(ctx, q, row.ID)
		return err
	})
	return res, err
}

// markPOSLines carries the ADDED marks over a terminal edit (and marks lines
// new since the check was on the tablet) and keeps the removed lines struck
// through.
func markPOSLines(snap *order.Snapshot, before order.Snapshot, removedItems []order.LineItem) {
	oldKeys := order.POSLineKeys(before.Items)
	wasAdded := map[string]bool{}
	seen := map[string]bool{}
	for i, it := range before.Items {
		seen[oldKeys[i]] = true
		wasAdded[oldKeys[i]] = it.Added
	}
	newKeys := order.POSLineKeys(snap.Items)
	for i := range snap.Items {
		k := newKeys[i]
		snap.Items[i].Added = !seen[k] || wasAdded[k]
	}
	snap.RemovedItems = append(append([]order.LineItem(nil), before.RemovedItems...), removedItems...)
}

// recordPOSChange stores (or merges into the unacknowledged one) a terminal
// change that rings the tablet.
func recordPOSChange(ctx context.Context, q *models.Queries, cur order.StoredOrder, added, removed []order.ChangeLine, reopened bool, now time.Time, res *order.ExternalResult) error {
	kind := order.ChangeCooking
	switch {
	case reopened:
		kind = order.ChangeReopened
	case cur.Status.State == order.StateOnDelivery:
		kind = order.ChangeOut
	case cur.Status.State == order.StateReady:
		kind = order.ChangeReady
	}
	n := order.PosChange{ID: uuid.NewString(), At: now.UTC(), Kind: kind, Added: added, Removed: removed}
	if kind == order.ChangeOut {
		n.DriverName = cur.Status.DriverName
	}
	if n.Added == nil {
		n.Added = []order.ChangeLine{}
	}
	if n.Removed == nil {
		n.Removed = []order.ChangeLine{}
	}
	c := n
	if prev := cur.Status.PosChange; prev.Pending() {
		c = *prev // not OK'd yet: one bar for both
		c.Merge(n)
	}
	c.RenderText(cur.Number())
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := q.SetPosChange(ctx, models.SetPosChangeParams{PosChange: string(raw), ID: cur.Order.ID}); err != nil {
		return err
	}
	res.Change = &c
	return nil
}

// finishClosed finishes a pickup / counter check the POS closed: a ready one
// (not finished yet), or one that never was on Kitchen (hidden: ready and
// finished at once). One still cooking finishes when marked ready.
func finishClosed(ctx context.Context, q *models.Queries, id string, hidden bool, now time.Time) (bool, error) {
	n, err := q.SetOrderFinished(ctx, models.SetOrderFinishedParams{FinishedAt: nullTS(now), FinishedBy: order.FinishedByPOS, ID: id})
	if err != nil || n > 0 || !hidden {
		return n > 0, err
	}
	n, err = q.FinishHiddenClosed(ctx, models.FinishHiddenClosedParams{At: nullTS(now), ID: id})
	return n > 0, err
}

func insertExternal(ctx context.Context, q *models.Queries, extRef string, in order.ExternalOrder, meta order.ExternalMeta, res *order.ExternalResult) error {
	snap := in.Snapshot(meta.OrderID, meta.TenantID, meta.LocationID, extRef, meta.Now)
	raw, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	sent := meta.Now
	if in.SentAt != nil {
		sent = *in.SentAt
	}
	if err := q.InsertExternalOrder(ctx, models.InsertExternalOrderParams{
		ID: meta.OrderID, TenantID: meta.TenantID, LocationID: meta.LocationID, Source: snap.Source,
		SourceEventID: snap.SourceEventID, Snapshot: string(raw), ArrivalDate: fmtTS(meta.Now),
		PrepMinutes:      sql.NullInt64{Int64: meta.PrepMinutes, Valid: true},
		EstimatedReadyAt: nullTS(sent.Add(time.Duration(meta.PrepMinutes) * time.Minute)),
		AcceptedAt:       nullTS(sent), ExtSource: in.Source, ExtRef: extRef, ExtHash: in.Hash,
		DisplayNumber: sql.NullInt64{Int64: in.DisplayNumber, Valid: in.DisplayNumber > 0}, Paid: in.Paid,
		CashDueCents: sql.NullInt64{Int64: in.CashDueCents, Valid: true}, TipCents: sql.NullInt64{Int64: in.TipCents, Valid: true},
		BoardHidden: in.BoardHidden, SentAt: nullTS(sent),
	}); err != nil {
		return fmt.Errorf("insert pos order: %w", err)
	}
	if in.Closed {
		if err := q.SetPosClosed(ctx, models.SetPosClosedParams{PosClosed: true, ID: meta.OrderID}); err != nil {
			return err
		}
		if in.CheckType != "delivery" {
			if res.Finished, err = finishClosed(ctx, q, meta.OrderID, in.BoardHidden, meta.Now); err != nil {
				return err
			}
		}
	}
	res.Created = true
	res.Order, err = loadOrder(ctx, q, meta.OrderID)
	return err
}

func sameAddress(a, b *order.Address) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return a.Line1 == b.Line1 && a.Line2 == b.Line2 && a.City == b.City && a.State == b.State && a.PostalCode == b.PostalCode
}

// SetExtDriver records the driver number the POS set on a check and the note
// shown when it couldn't be mirrored ("" = mirrored).
func (r *OrderRepository) SetExtDriver(ctx context.Context, id, posNumber, note string) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.set_ext_driver")
	defer end(&err)
	q := models.New(r.db)
	if err := q.SetExtDriverPos(ctx, models.SetExtDriverPosParams{ExtDriverPos: posNumber, PosDriverNote: note, ID: id}); err != nil {
		return nil, err
	}
	fresh, err := loadOrder(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// VoidExternal — see order.IntegrationRepository.
func (r *OrderRepository) VoidExternal(ctx context.Context, source, extRef string) (so *order.StoredOrder, changed bool, err error) {
	ctx, _, end := dbSpan(ctx, "orders.void_external")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetOrderByExtRef(ctx, models.GetOrderByExtRefParams{ExtSource: source, ExtRef: extRef})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return order.ErrNotFound
			}
			return err
		}
		n, err := q.CancelExternalOrder(ctx, models.CancelExternalOrderParams{CancelledAt: nullTS(now), ID: row.ID})
		if err != nil {
			return err
		}
		if fresh, err = loadOrder(ctx, q, row.ID); err != nil {
			return err
		}
		if n == 0 {
			return nil // already cancelled, or delivered
		}
		changed = true
		return feedCancelled(ctx, q, fresh, now)
	})
	if err != nil {
		return nil, false, err
	}
	return &fresh, changed, nil
}

// PatchOnline — see order.IntegrationRepository.
func (r *OrderRepository) PatchOnline(ctx context.Context, id string, p order.OnlinePatch) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.patch_online")
	defer end(&err)
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.IsPOS() {
			return fmt.Errorf("a POS order: %w", order.ErrInvalidState)
		}
		if p.Paid != nil {
			if err := q.SetOrderPaid(ctx, models.SetOrderPaidParams{Paid: *p.Paid, ID: id}); err != nil {
				return err
			}
		}
		if p.PosCheckNumber != nil {
			if err := q.SetPosCheckNumber(ctx, models.SetPosCheckNumberParams{PosCheckNumber: sql.NullInt64{Int64: *p.PosCheckNumber, Valid: true}, ID: id}); err != nil {
				return err
			}
		}
		if p.SetFlag {
			raw := ""
			if p.Flag != nil {
				b, err := json.Marshal(p.Flag)
				if err != nil {
					return err
				}
				raw = string(b)
			}
			if err := q.SetOrderFlag(ctx, models.SetOrderFlagParams{Flag: raw, ID: id}); err != nil {
				return err
			}
		}
		fresh, err = loadOrder(ctx, q, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// DriverByPosNumber returns the active driver with that POS number.
func (r *OrderRepository) DriverByPosNumber(ctx context.Context, pos string) (*order.Driver, error) {
	row, err := models.New(r.db).GetActiveDriverByPosNumber(ctx, pos)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, order.ErrDriverNotFound
		}
		return nil, err
	}
	return r.driverByID(ctx, row.ID)
}

// MirrorSendOut — see order.IntegrationRepository.
func (r *OrderRepository) MirrorSendOut(ctx context.Context, driverID, orderID string) (sent, readied, unpooled []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.mirror_send_out")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := availableDriver(ctx, q, driverID)
		if err != nil {
			return err
		}
		if readied, err = readyForRun(ctx, q, orderID, now); err != nil {
			return err
		}
		if sent, err = sendRun(ctx, q, d, []string{orderID}, now); err != nil {
			return err
		}
		// The driver left: whatever else they had pooled is unassigned again.
		if unpooled, err = clearDriverPool(ctx, q, d.ID, now); err != nil {
			return err
		}
		return recordDeliveryRun(ctx, q, d, sent, order.RunMeta{SuggestedIDs: []string{}}, now)
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return sent, readied, unpooled, nil
}

// MirrorJoinRun — see order.IntegrationRepository.
func (r *OrderRepository) MirrorJoinRun(ctx context.Context, driverID, orderID string) (so *order.StoredOrder, readied []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.mirror_join_run")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		d, err := q.GetDriver(ctx, driverID)
		if err != nil || !d.Active {
			return order.ErrDriverNotFound
		}
		if d.Status != order.DriverOut {
			return order.ErrDriverNotAvailable
		}
		if readied, err = readyForRun(ctx, q, orderID, now); err != nil {
			return err
		}
		seq, err := q.NextRunSeq(ctx, sql.NullString{String: d.ID, Valid: true})
		if err != nil {
			return err
		}
		n, err := q.MarkOnDelivery(ctx, models.MarkOnDeliveryParams{OutAt: nullTS(now),
			DriverID: sql.NullString{String: d.ID, Valid: true}, DriverName: d.Name, ID: orderID})
		if err != nil {
			return fmt.Errorf("mark on delivery: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("order %s: %w", orderID, order.ErrInvalidState)
		}
		if err := q.SetRunSeq(ctx, models.SetRunSeqParams{RunSeq: sql.NullInt64{Int64: seq, Valid: true}, ID: orderID}); err != nil {
			return err
		}
		if fresh, err = loadOrder(ctx, q, orderID); err != nil {
			return err
		}
		return recordOrderStatus(ctx, q, fresh, false, now)
	})
	if err != nil {
		return nil, nil, err
	}
	return &fresh, readied, nil
}

// readyForRun marks a cooking delivery ready (with its events) before it
// leaves with a driver; a ready one is left as is.
func readyForRun(ctx context.Context, q *models.Queries, id string, now time.Time) ([]order.StoredOrder, error) {
	so, err := loadOrder(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if !poolable(so) {
		return nil, fmt.Errorf("order %s (%s): %w", id, so.Status.State, order.ErrInvalidState)
	}
	if so.Status.State != order.StateAccepted {
		return nil, nil
	}
	if _, err := q.MarkReady(ctx, models.MarkReadyParams{ReadyAt: nullTS(now), ID: id}); err != nil {
		return nil, fmt.Errorf("mark ready: %w", err)
	}
	if so, err = loadOrder(ctx, q, id); err != nil {
		return nil, err
	}
	if err := recordOrderStatus(ctx, q, so, false, now); err != nil {
		return nil, err
	}
	return []order.StoredOrder{so}, feedReady(ctx, q, so, now)
}

// --- the feed ------------------------------------------------------------------

// ListFeed — see order.IntegrationRepository.
func (r *OrderRepository) ListFeed(ctx context.Context, after int64, limit int) (out []order.FeedEvent, maxSeq int64, err error) {
	ctx, _, end := dbSpan(ctx, "integration_events.list")
	defer end(&err)
	q := models.New(r.db)
	if maxSeq, err = q.MaxIntegrationSeq(ctx); err != nil {
		return nil, 0, err
	}
	rows, err := q.ListIntegrationEvents(ctx, models.ListIntegrationEventsParams{Seq: after, Limit: int64(limit)})
	if err != nil {
		return nil, 0, err
	}
	out = make([]order.FeedEvent, 0, len(rows))
	for _, row := range rows {
		at, err := parseTS(row.CreatedAt)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, order.FeedEvent{Seq: row.Seq, Type: row.Type, At: at.UTC(), Data: json.RawMessage(row.Payload)})
	}
	return out, maxSeq, nil
}

// --- future orders ---------------------------------------------------------------

// Schedule — see order.IntegrationRepository.
func (r *OrderRepository) Schedule(ctx context.Context, id string, leadMinutes int64, ringAt time.Time) (so *order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.schedule")
	defer end(&err)
	now := time.Now()
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		n, err := q.ScheduleOrder(ctx, models.ScheduleOrderParams{ScheduledLeadMinutes: sql.NullInt64{Int64: leadMinutes, Valid: true},
			RingAt: nullTS(ringAt), ID: id})
		if err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		if fresh, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		if n == 0 {
			return order.ErrInvalidState
		}
		return recordOrderStatus(ctx, q, fresh, false, now)
	})
	if err != nil {
		return nil, err
	}
	return &fresh, nil
}

// RaiseDue — see order.IntegrationRepository.
func (r *OrderRepository) RaiseDue(ctx context.Context, now time.Time) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "floor.raise_due")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		rows, err := q.ListDueScheduled(ctx, nullTS(now))
		if err != nil {
			return err
		}
		for _, row := range rows {
			n, err := q.RaiseScheduled(ctx, models.RaiseScheduledParams{LastAlertAt: nullTS(now), ID: row.ID})
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			// No order.status to main: it ranks scheduled above arrived and
			// keeps scheduled until the accept arrives.
			so, err := loadOrder(ctx, q, row.ID)
			if err != nil {
				return err
			}
			out = append(out, so)
		}
		return nil
	})
	return out, err
}

// --- finishing, terminal changes, callout (00020) ---------------------------------

// AckPosChange — see order.IntegrationRepository.
func (r *OrderRepository) AckPosChange(ctx context.Context, id, changeID string) (so *order.StoredOrder, acked bool, err error) {
	ctx, _, end := dbSpan(ctx, "orders.ack_pos_change")
	defer end(&err)
	var fresh order.StoredOrder
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		if c := cur.Status.PosChange; c.Pending() && (changeID == "" || changeID == c.ID) {
			now := time.Now().UTC()
			c.AckedAt = &now
			raw, err := json.Marshal(c)
			if err != nil {
				return err
			}
			if err := q.SetPosChange(ctx, models.SetPosChangeParams{PosChange: string(raw), ID: id}); err != nil {
				return err
			}
			acked = true
		}
		fresh, err = loadOrder(ctx, q, id)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return &fresh, acked, nil
}

// FinishDueOnlinePickups — see order.IntegrationRepository.
func (r *OrderRepository) FinishDueOnlinePickups(ctx context.Context, cutoff, now time.Time) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "orders.finish_due_pickups")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		ids, err := q.ListDueOnlinePickups(ctx, nullTS(cutoff))
		if err != nil {
			return err
		}
		for _, id := range ids {
			n, err := q.SetOrderFinished(ctx, models.SetOrderFinishedParams{FinishedAt: nullTS(now), FinishedBy: order.FinishedByAuto, ID: id})
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			so, err := loadOrder(ctx, q, id)
			if err != nil {
				return err
			}
			out = append(out, so)
		}
		return nil
	})
	return out, err
}

// InsertCalloutTest — see order.IntegrationRepository.
func (r *OrderRepository) InsertCalloutTest(ctx context.Context, t order.CalloutTestData) (err error) {
	ctx, _, end := dbSpan(ctx, "integration_events.callout_test")
	defer end(&err)
	return insertFeed(ctx, models.New(r.db), order.FeedCalloutTest, time.Now(), t)
}
