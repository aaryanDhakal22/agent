package repositories

import (
	"context"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"
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

// closeUnsettled runs in Close batch's transaction: refuses a manual close
// over an unpaid counter check (order.UnsettledError), marks the checks the
// close carries, journals the list (batch.unsettled) and returns it with
// the carried ids.
func closeUnsettled(ctx context.Context, q *models.Queries, batchID int64, auto bool, now time.Time) (*order.UnsettledList, map[string]bool, error) {
	checks, err := unsettledOf(ctx, q, batchID)
	if err != nil {
		return nil, nil, err
	}
	if !auto {
		var block []order.UnsettledCheck
		for _, so := range checks {
			if order.BlocksClose(so) {
				block = append(block, order.UnsettledCheckOf(so, nil))
			}
		}
		if len(block) > 0 {
			return nil, nil, &order.UnsettledError{Checks: block}
		}
	}
	carry := map[string]bool{}
	for i, so := range checks {
		if so.Status.BatchID != batchID || !order.Carries(so, auto) {
			continue
		}
		if err := q.SetOrderCarried(ctx, models.SetOrderCarriedParams{Carried: true, ID: so.Order.ID}); err != nil {
			return nil, nil, fmt.Errorf("carry check: %w", err)
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
			return nil, nil, err
		}
	}
	return l, carry, nil
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
	return listOf(b.ID, checks, time.Now()), nil
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
