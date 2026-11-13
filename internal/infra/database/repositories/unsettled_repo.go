package repositories

import (
	"context"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// The Unsettled Check List (contract/agent.md §2.10s §4, HARDENING-PLAN
// M-10): checks still owing money at a close, and the carried ones.

// unsettledOf lists the open batch's unsettled checks plus every carried
// one still unsettled, oldest first.
func unsettledOf(ctx context.Context, q *models.Queries, batchID int64) ([]order.StoredOrder, error) {
	rows, err := q.ListAllBatchOrders(ctx, batchID)
	if err != nil {
		return nil, err
	}
	all, err := mapRowsBare(rows)
	if err != nil {
		return nil, err
	}
	carriedRows, err := q.ListCarriedOrders(ctx)
	if err != nil {
		return nil, err
	}
	carried, err := mapRowsBare(carriedRows)
	if err != nil {
		return nil, err
	}
	var out []order.StoredOrder
	for _, so := range carried {
		if so.Status.BatchID != batchID && order.IsUnsettled(so) {
			out = append(out, so)
		}
	}
	for _, so := range all {
		if order.IsUnsettled(so) {
			out = append(out, so)
		}
	}
	return out, nil
}

func listOf(batchID int64, checks []order.StoredOrder, now time.Time) *order.UnsettledList {
	l := &order.UnsettledList{BatchID: batchID, GeneratedAt: now.UTC().Truncate(time.Millisecond), Checks: []order.UnsettledCheck{}}
	for _, so := range checks {
		c := order.UnsettledCheckOf(so, nil)
		l.Checks = append(l.Checks, c)
		l.DueCents += c.DueCents
		if c.BlocksClose {
			l.Blocking++
		}
	}
	l.Count = len(l.Checks)
	return l
}

// closeUnsettled runs in Close batch's transaction. An unpaid counter check
// (blocking) is refused by a manual close without force (order.UnsettledError,
// saying what force would do); a forced close and the auto-close follow the
// store's setting (order.CloseUnpaidOf, the owner 2026-11-13): waste = each
// one with no payment row is written off as Void waste "Unpaid at close"
// (wasteAtClose), carry (or a partly paid one) = carried like a pickup.
// Then it marks the checks the close carries, journals the remaining list
// (batch.unsettled) and returns it, the write-offs and the carried ids.
func closeUnsettled(ctx context.Context, q *models.Queries, batchID int64, auto, force bool, now time.Time) (*order.UnsettledList, *order.UnpaidAtClose, map[string]bool, error) {
	checks, err := unsettledOf(ctx, q, batchID)
	if err != nil {
		return nil, nil, nil, err
	}
	action := order.CloseUnpaidOf(ctx)
	plan := map[string]string{} // blocking check -> waste | carry
	var block []order.UnsettledCheck
	var preview order.ForcePreview
	for _, so := range checks {
		if !order.BlocksClose(so) {
			continue
		}
		paid, err := listPayments(ctx, q, so.Order.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		c := order.UnsettledCheckOf(so, nil)
		c.OnForce = order.ForceOutcome(action, len(paid))
		plan[so.Order.ID] = c.OnForce
		if c.OnForce == order.UnpaidWaste {
			preview.WasteCount++
			preview.WasteCents += c.TotalCents
		} else {
			preview.CarryCount++
			preview.CarryDueCents += c.DueCents
		}
		block = append(block, c)
	}
	if len(block) > 0 && !auto && !force && !order.ForceUnpaidOf(ctx) {
		return nil, nil, nil, &order.UnsettledError{Checks: block, ForceAction: action, Force: preview}
	}
	var wasted *order.UnpaidAtClose
	if preview.WasteCount > 0 {
		by := order.CloseActorOf(ctx)
		wasted = &order.UnpaidAtClose{BatchID: batchID, Reason: order.UnpaidAtCloseReason, At: now.UTC().Truncate(time.Millisecond),
			Auto: auto, ClosedBy: by, PrintedBy: "pi", Checks: []order.UnsettledCheck{}}
		if by.PrintsHere {
			wasted.PrintedBy = "terminal"
		}
		var entries []terminal.Entry
		kept := checks[:0:0]
		for _, so := range checks {
			if plan[so.Order.ID] != order.UnpaidWaste {
				kept = append(kept, so)
				continue
			}
			c, e, err := wasteAtClose(ctx, q, so, by, auto, now)
			if err != nil {
				return nil, nil, nil, err
			}
			if c == nil {
				kept = append(kept, so) // changed under the close: left as it was
				continue
			}
			wasted.Checks = append(wasted.Checks, *c)
			wasted.TotalCents += c.TotalCents
			entries = append(entries, e)
		}
		wasted.Count = len(wasted.Checks)
		checks = kept
		if err := insertJournal(ctx, q, now, entries); err != nil {
			return nil, nil, nil, err
		}
		if wasted.Count == 0 {
			wasted = nil
		}
	}
	carry := map[string]bool{}
	for i, so := range checks {
		if so.Status.BatchID != batchID || !(order.Carries(so, auto) || plan[so.Order.ID] == order.UnpaidCarry) {
			continue
		}
		if err := q.SetOrderCarried(ctx, models.SetOrderCarriedParams{Carried: true, ID: so.Order.ID}); err != nil {
			return nil, nil, nil, fmt.Errorf("carry check: %w", err)
		}
		checks[i].Status.Carried = true
		carry[so.Order.ID] = true
	}
	l := listOf(batchID, checks, now)
	l.Blocking = 0 // nothing blocks once closed
	for i := range l.Checks {
		l.Checks[i].BlocksClose = false
	}
	if l.Count > 0 {
		ids := make([]string, 0, l.Count)
		for _, c := range l.Checks {
			ids = append(ids, c.OrderID)
		}
		if err := insertJournal(ctx, q, now, []terminal.Entry{{DeviceID: "pi", Action: terminal.ActionUnsettled,
			Detail: jsonDetail(map[string]any{"batch_id": batchID, "count": l.Count, "due_cents": l.DueCents, "order_ids": ids,
				"auto": auto})}}); err != nil {
			return nil, nil, nil, err
		}
	}
	return l, wasted, carry, nil
}

// wasteAtClose writes one unpaid counter check off as Void waste "Unpaid at
// close" inside the close's transaction (contract 2.10s §4): cancelled with
// the waste cancel reason, nothing prints, order.void {kind: waste,
// waste_reason} for main, a carried one settled; the journal row (void.waste,
// the Z / till Waste section) is returned for the caller to write. nil = the
// check changed under the close (left alone).
func wasteAtClose(ctx context.Context, q *models.Queries, so order.StoredOrder, by order.CloseActor, auto bool, now time.Time) (*order.UnsettledCheck, terminal.Entry, error) {
	id := so.Order.ID
	c := order.UnsettledCheckOf(so, nil)
	c.BlocksClose, c.OnForce = false, order.UnpaidWaste
	n, err := q.CancelChannelOrder(ctx, models.CancelChannelOrderParams{CancelledAt: nullTS(now), Reason: terminal.WasteReason, ID: id})
	if err != nil {
		return nil, terminal.Entry{}, fmt.Errorf("write off check: %w", err)
	}
	if n == 0 {
		return nil, terminal.Entry{}, nil
	}
	reason := order.UnpaidAtCloseReason
	evID := uuid.NewString()
	ev := map[string]any{"event_id": evID, "type": terminal.EventOrderVoid, "order_id": id, "at": now.UTC().Truncate(time.Millisecond),
		"reason": reason, "staff_id": nilIfEmpty(by.StaffID), "kind": "waste", "waste_reason": reason}
	if by.ApprovedBy != "" {
		ev["approved_by"] = by.ApprovedBy
	}
	if err := insertEvent(ctx, q, evID, id, terminal.EventOrderVoid, now, ev); err != nil {
		return nil, terminal.Entry{}, err
	}
	if err := settleCarried(ctx, q, id); err != nil {
		return nil, terminal.Entry{}, err
	}
	items := make([]map[string]any, 0, len(so.Order.Items))
	for _, it := range so.Order.Items {
		items = append(items, map[string]any{"qty": max(it.Quantity, 1), "name": it.Name})
	}
	d := map[string]any{"reason": reason, "refunded_cents": 0, "number": c.Number, "state": so.Status.State,
		"total_cents": so.Order.Money.Total, "items": items, "at_close": true, "auto": auto, "closed_by": by}
	if by.ApprovedBy != "" {
		d["approved_by"] = by.ApprovedBy
	}
	return &c, terminal.Entry{StaffID: by.StaffID, DeviceID: by.DeviceID, Action: terminal.ActionVoidWaste, OrderID: id, Detail: jsonDetail(d)}, nil
}

// UnsettledChecks is GET /api/terminal/unsettled: the open batch's
// unsettled checks and the carried ones.
func (r *OrderRepository) UnsettledChecks(ctx context.Context) (l *order.UnsettledList, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.unsettled")
	defer end(&err)
	q := models.New(r.db)
	b, err := q.GetOpenBatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("open batch: %w", err)
	}
	checks, err := unsettledOf(ctx, q, b.ID)
	if err != nil {
		return nil, err
	}
	l = listOf(b.ID, checks, time.Now())
	if l.Blocking == 0 {
		return l, nil
	}
	// What a forced close would do with each blocking check (the store's
	// setting on ctx; a partly paid one is carried).
	action := order.CloseUnpaidOf(ctx)
	f := &order.ForcePreview{}
	for i := range l.Checks {
		c := &l.Checks[i]
		if !c.BlocksClose {
			continue
		}
		paid, err := listPayments(ctx, q, c.OrderID)
		if err != nil {
			return nil, err
		}
		if c.OnForce = order.ForceOutcome(action, len(paid)); c.OnForce == order.UnpaidWaste {
			f.WasteCount++
			f.WasteCents += c.TotalCents
		} else {
			f.CarryCount++
			f.CarryDueCents += c.DueCents
		}
	}
	l.ForceAction, l.Force = action, f
	return l, nil
}

// CarriedOrders are the checks carried past their batch's close that are
// still open (the Pickup list adds them).
func (r *OrderRepository) CarriedOrders(ctx context.Context) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.carried")
	defer end(&err)
	q := models.New(r.db)
	rows, err := q.ListCarriedOrders(ctx)
	if err != nil {
		return nil, err
	}
	return mapRowsGeo(ctx, q, rows)
}
