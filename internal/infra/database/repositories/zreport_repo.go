package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// The Z report (contract/agent.md §2.10l): Close batch queues a pending row
// in its transaction; the report is built from the closed batch right after
// (and at start for one a crash left pending), stored, and sent as the floor
// event z.report (subject "z:<batch>") in one transaction.

// PendingZReports are the Z reports still to build, oldest batch first.
func (r *OrderRepository) PendingZReports(ctx context.Context) (out []terminal.ZPending, err error) {
	ctx, _, end := dbSpan(ctx, "z.pending")
	defer end(&err)
	rows, err := models.New(r.db).ListPendingZReports(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out = append(out, terminal.ZPending{BatchID: row.BatchID, ID: row.ID, CloseKind: row.CloseKind, Forced: row.Forced})
	}
	return out, nil
}

// ZInputs reads what a closed batch's Z report is built from (no driver
// reports: the caller adds them).
func (r *OrderRepository) ZInputs(ctx context.Context, batchID int64) (in terminal.ZInput, err error) {
	ctx, _, end := dbSpan(ctx, "z.inputs")
	defer end(&err)
	q := models.New(r.db)
	b, err := q.GetBatch(ctx, batchID)
	if err != nil {
		return in, fmt.Errorf("batch %d: %w", batchID, err)
	}
	in.BatchID = b.ID
	if in.OpenedAt, err = parseTS(b.OpenedAt); err != nil {
		return in, err
	}
	in.ClosedAt, in.Open = time.Now(), !b.ClosedAt.Valid
	if b.ClosedAt.Valid {
		if in.ClosedAt, err = parseTS(b.ClosedAt.String); err != nil {
			return in, err
		}
	}
	rows, err := q.ListAllBatchOrders(ctx, batchID)
	if err != nil {
		return in, err
	}
	if in.Orders, err = mapRowsBare(rows); err != nil {
		return in, err
	}
	pays, err := q.ListBatchPayments(ctx, sql.NullInt64{Int64: batchID, Valid: true})
	if err != nil {
		return in, err
	}
	held := make(map[string]bool, len(in.Orders))
	for _, so := range in.Orders {
		held[so.Order.ID] = true
	}
	for _, row := range pays {
		p, err := paymentFromRow(row)
		if err != nil {
			return in, err
		}
		in.Payments = append(in.Payments, p)
		// A payment taken here on an earlier batch's (carried) check
		// (contract 2.10s §4): its order comes along, not as a sale.
		if !held[p.OrderID] {
			held[p.OrderID] = true
			if so, err := loadOrder(ctx, q, p.OrderID); err == nil {
				in.Earlier = append(in.Earlier, so)
			}
		}
	}
	if in.Drawers, err = drawersByBatch(ctx, q, batchID); err != nil {
		return in, err
	}
	jr, err := q.ListJournalBetween(ctx, models.ListJournalBetweenParams{FromAt: b.OpenedAt, ToAt: fmtTS(in.ClosedAt)})
	if err != nil {
		return in, err
	}
	for _, row := range jr {
		at, err := parseTS(row.At)
		if err != nil {
			return in, err
		}
		in.Journal = append(in.Journal, terminal.Entry{ID: row.ID, At: at, StaffID: row.StaffID, DeviceID: row.DeviceID,
			Action: row.Action, OrderID: row.OrderID, Detail: json.RawMessage(row.Detail)})
	}
	return in, nil
}

// SaveZReport stores a built Z report and writes its z.report floor event
// (+ a journal row), in one transaction. A report already done is left as
// it is (false).
func (r *OrderRepository) SaveZReport(ctx context.Context, z terminal.Z, at time.Time) (saved bool, err error) {
	ctx, _, end := dbSpan(ctx, "z.save")
	defer end(&err)
	raw, err := json.Marshal(z)
	if err != nil {
		return false, err
	}
	err = r.withTx(ctx, func(q *models.Queries) error {
		row, err := q.GetZReport(ctx, z.BatchID)
		if err != nil {
			return fmt.Errorf("z report %d: %w", z.BatchID, err)
		}
		if row.Status == "done" {
			return nil
		}
		if err := q.SaveZReport(ctx, models.SaveZReportParams{Report: string(raw), BuiltAt: nullTS(at), BatchID: z.BatchID}); err != nil {
			return err
		}
		saved = true
		evID := uuid.NewString()
		ev := map[string]any{"event_id": evID, "type": terminal.EventZReport, "at": at.UTC().Truncate(time.Millisecond), "report": json.RawMessage(raw)}
		if err := insertSubjectEvent(ctx, q, evID, "", "z:"+strconv.FormatInt(z.BatchID, 10), terminal.EventZReport, at, ev); err != nil {
			return err
		}
		return insertJournal(ctx, q, at, []terminal.Entry{{DeviceID: "pi", Action: terminal.ActionZReport,
			Detail: jsonDetail(map[string]any{"batch_id": z.BatchID, "z_id": z.ID, "total_cents": z.Sales.TotalCents,
				"orders": z.Counts.Orders, "difference_cents": z.Reconcile.DifferenceCents})}})
	})
	return saved, err
}

// ZReport is a batch's stored Z report (nil = none, or not built yet).
func (r *OrderRepository) ZReport(ctx context.Context, batchID int64) (z *terminal.Z, err error) {
	ctx, _, end := dbSpan(ctx, "z.get")
	defer end(&err)
	row, err := models.New(r.db).GetZReport(ctx, batchID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || row.Status != "done" {
		return nil, err
	}
	var out terminal.Z
	if err := json.Unmarshal([]byte(row.Report), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ZReports are the newest stored Z reports (built ones), newest first.
func (r *OrderRepository) ZReports(ctx context.Context, limit int) (out []terminal.Z, err error) {
	ctx, _, end := dbSpan(ctx, "z.list")
	defer end(&err)
	rows, err := models.New(r.db).ListZReports(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	out = []terminal.Z{}
	for _, row := range rows {
		if row.Status != "done" {
			continue
		}
		var z terminal.Z
		if err := json.Unmarshal([]byte(row.Report), &z); err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, nil
}
