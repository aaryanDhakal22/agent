package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"

	"github.com/google/uuid"
)

// ChangeCounterOrder writes a kitchen change on a counter order of the open
// batch that is accepted or ready (contract §2.10d): check gets the order
// and its payments in the transaction and returns the new snapshot (lines +
// money; nil = nothing changes). The money columns follow, and the floor
// event order.changed carries the whole new snapshot to main.
func (r *OrderRepository) ChangeCounterOrder(ctx context.Context, id, staffID, deviceID string, check terminal.ChangeCheck) (so, prev order.StoredOrder, changed bool, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.change_counter_order")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		prev, so = cur, cur
		if cur.Order.Source != order.SourceCounter {
			return terminal.ErrNotCounterOrder
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		if cur.Status.BatchID != b.ID {
			return terminal.ErrNotEditable
		}
		switch cur.Status.State {
		case order.StateAccepted, order.StateReady:
		default:
			return terminal.ErrNotEditable
		}
		paid, err := listPayments(ctx, q, id)
		if err != nil {
			return err
		}
		snap, entries, err := check(cur, paid)
		if err != nil || snap == nil {
			return err
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		due := snap.CashDue()
		paidAt := sql.NullString{}
		switch {
		case due == 0 && !cur.Status.Paid:
			paidAt = nullTS(now)
		case due == 0 && cur.Status.PaidAt != nil:
			paidAt = nullTS(*cur.Status.PaidAt)
		}
		if err := q.SetCounterMoney(ctx, models.SetCounterMoneyParams{Snapshot: string(raw), Paid: due == 0,
			CashDueCents: sql.NullInt64{Int64: due, Valid: true}, TipCents: sql.NullInt64{Int64: tipsOf(ctx, q, id), Valid: true},
			PaidAt: paidAt, ID: id}); err != nil {
			return fmt.Errorf("change order: %w", err)
		}
		// A delivery to another place (CHG-14) or a takeout moved to one: main's
		// location of the old address is stale. Drop it here; main locates the
		// new one and pushes order.geo again (main contract §2.5zb order.changed).
		if deliveryMoved(cur.Order, *snap) {
			if err := q.DeleteOrderGeo(ctx, id); err != nil {
				return fmt.Errorf("drop old geo: %w", err)
			}
		}
		if so, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		changed = true
		wire, err := changedSnapshot(so, paid, staffID, deviceID)
		if err != nil {
			return err
		}
		evID := uuid.NewString()
		ev := map[string]any{"event_id": evID, "type": terminal.EventOrderChanged, "order_id": id,
			"at": now.UTC().Truncate(time.Millisecond), "snapshot": wire, "staff_id": nilIfEmpty(staffID)}
		if err := insertEvent(ctx, q, evID, id, terminal.EventOrderChanged, now, ev); err != nil {
			return err
		}
		if err := claimDraft(ctx, q, deviceID); err != nil { // the draft it came from (contract 2.10s §9; TAB-16)
			return err
		}
		return insertJournal(ctx, q, now, entries)
	})
	return so, prev, changed, err
}

// deliveryMoved: does after deliver somewhere before didn't (another street,
// town, state or zip; a line2 or a note alone is the same place)?
func deliveryMoved(before, after order.Snapshot) bool {
	if after.Fulfillment.Type != "delivery" || after.Fulfillment.Address == nil {
		return false
	}
	if before.Fulfillment.Type != "delivery" || before.Fulfillment.Address == nil {
		return true
	}
	key := func(a *order.Address) string {
		return strings.ToLower(strings.Join(strings.Fields(a.Line1+"|"+a.City+"|"+a.State+"|"+a.PostalCode), " "))
	}
	return key(before.Fulfillment.Address) != key(after.Fulfillment.Address)
}

// changedSnapshot is the changed order as main's order.changed takes it: the
// order.placed shape (fulfillment counter | pickup | delivery, the order's
// client_key as source_event_id, the payments so far).
func changedSnapshot(so order.StoredOrder, paid []terminal.Payment, staffID, deviceID string) (map[string]any, error) {
	raw, err := json.Marshal(so.Order)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if f, ok := m["fulfillment"].(map[string]any); ok && so.Order.Fulfillment.Type == terminal.FulfillmentOf(terminal.TypeCounter) {
		f["type"] = terminal.TypeCounter
	}
	pays := make([]map[string]any, 0, len(paid))
	for _, p := range paid {
		pays = append(pays, p.Wire())
	}
	m["payments"], m["staff_id"], m["device_id"] = pays, nilIfEmpty(staffID), deviceID
	if so.Status.EstimatedReadyAt != nil {
		m["estimated_ready_at"] = so.Status.EstimatedReadyAt.UTC()
	}
	return m, nil
}
