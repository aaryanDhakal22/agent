package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// AddGeneralRefund records a general refund (contract 2.10x: cash back with
// no order) in one transaction: the order_payments row (order_id "", kind
// general; a cash refund goes in the terminal's open drawer, which opens by
// itself like at a first payment), the floor event refund.general for main
// and the journal entries. A client_key already used for a general refund
// returns that refund (duplicate, nothing written); with another body it is
// a *terminal.KeyReused.
func (r *OrderRepository) AddGeneralRefund(ctx context.Context, p terminal.Payment, entries []terminal.Entry) (got terminal.Payment, duplicate bool, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.general_refund")
	defer end(&err)
	if !terminal.IsGeneralRefund(p) {
		return got, false, fmt.Errorf("not a general refund")
	}
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		if p.ClientKey != "" {
			switch row, err := q.GetOrderPaymentByKey(ctx, models.GetOrderPaymentByKeyParams{OrderID: "", ClientKey: p.ClientKey}); {
			case err == nil:
				if !terminal.SameRequest(row.RequestHash, p.RequestHash) {
					return &terminal.KeyReused{ClientKey: p.ClientKey}
				}
				duplicate = true
				got, err = paymentFromRow(row)
				return err
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
		}
		if _, err := q.GetOpenBatch(ctx); err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		if err := insertPayment(ctx, q, &p); err != nil {
			return err
		}
		got = p
		id := uuid.NewString()
		ev := map[string]any{"event_id": id, "type": terminal.EventRefundGeneral, "at": now.UTC().Truncate(time.Millisecond),
			"payment": p.Wire(), "reason": p.Reason, "kind": terminal.RefundGeneral}
		if p.BatchID > 0 {
			ev["batch_id"] = p.BatchID
		}
		if err := insertSubjectEvent(ctx, q, id, "", "refund:"+p.ID, terminal.EventRefundGeneral, now, ev); err != nil {
			return err
		}
		return insertJournal(ctx, q, now, entries)
	})
	return got, duplicate, err
}

// GeneralRefundByKey is the general refund a client_key made (nil = none).
func (r *OrderRepository) GeneralRefundByKey(ctx context.Context, key string) (p *terminal.Payment, err error) {
	return r.PaymentByKey(ctx, "", key)
}
