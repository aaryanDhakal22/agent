package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/infra/database/models"
)

// The terminals' shared drafts (hold + TAKE OVER) and the Pickup list
// (contract/agent.md §2.10i). Drafts never reach main: no floor event.

func draftFromRow(row models.TerminalDraft) (terminal.Draft, error) {
	created, err := parseTS(row.CreatedAt)
	if err != nil {
		return terminal.Draft{}, err
	}
	updated, err := parseTS(row.UpdatedAt)
	if err != nil {
		return terminal.Draft{}, err
	}
	d := terminal.Draft{ID: row.ID, LocationID: row.LocationID, Type: row.Type, Name: row.Name, Held: row.Held,
		StaffID: row.StaffID, StaffName: row.StaffName, OrderID: row.OrderID, Draft: json.RawMessage(row.Draft),
		CreatedAt: created, UpdatedAt: updated}
	if row.HolderDeviceID != "" {
		d.Holder = &terminal.Holder{DeviceID: row.HolderDeviceID, DeviceName: row.HolderDeviceName,
			StaffID: row.HolderStaffID, StaffName: row.HolderStaffName}
	}
	return d, nil
}

func getDraft(ctx context.Context, q *models.Queries, id string) (*terminal.Draft, error) {
	row, err := q.GetTerminalDraft(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d, err := draftFromRow(row)
	return &d, err
}

// TerminalDrafts are a store's shared drafts, oldest first.
func (r *OrderRepository) TerminalDrafts(ctx context.Context, locationID string) (out []terminal.Draft, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.drafts")
	defer end(&err)
	rows, err := models.New(r.db).ListTerminalDrafts(ctx, locationID)
	if err != nil {
		return nil, err
	}
	out = make([]terminal.Draft, 0, len(rows))
	for _, row := range rows {
		d, err := draftFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// PutTerminalDraft creates or replaces a draft under the soft lock (one
// transaction: two terminals racing for the same draft get one winner).
// The draft's holder and the one it was taken from (nil = none) come from
// put.Decide.
func (r *OrderRepository) PutTerminalDraft(ctx context.Context, in terminal.Draft, put terminal.DraftPut, now time.Time) (out terminal.Draft, taken *terminal.Holder, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.put_draft")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := getDraft(ctx, q, in.ID)
		if err != nil {
			return err
		}
		if cur != nil && cur.LocationID != in.LocationID {
			cur = nil // another store's leftover: replaced
		}
		holder, tk, err := put.Decide(cur)
		if err != nil {
			return err
		}
		taken = tk
		created := now
		if cur != nil {
			created = cur.CreatedAt
		} else if n, err := q.CountTerminalDrafts(ctx, in.LocationID); err != nil {
			return err
		} else if n >= terminal.MaxDrafts {
			return terminal.ErrTooManyDrafts
		}
		p := models.UpsertTerminalDraftParams{ID: in.ID, LocationID: in.LocationID, Type: in.Type, Name: in.Name, Held: in.Held,
			StaffID: put.By.StaffID, StaffName: put.By.StaffName, OrderID: in.OrderID, Draft: string(in.Draft),
			CreatedAt: fmtTS(created), UpdatedAt: fmtTS(now)}
		if holder != nil {
			p.HolderDeviceID, p.HolderDeviceName, p.HolderStaffID, p.HolderStaffName = holder.DeviceID, holder.DeviceName, holder.StaffID, holder.StaffName
		}
		if err := q.UpsertTerminalDraft(ctx, p); err != nil {
			return fmt.Errorf("put draft: %w", err)
		}
		d, err := getDraft(ctx, q, in.ID)
		if err != nil {
			return err
		}
		out = *d
		return nil
	})
	return out, taken, err
}

// DeleteTerminalDraft removes a draft unless another terminal has it open.
// deleted false = there was none.
func (r *OrderRepository) DeleteTerminalDraft(ctx context.Context, id, locationID, deviceID string) (deleted bool, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.delete_draft")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := getDraft(ctx, q, id)
		if err != nil || cur == nil || cur.LocationID != locationID {
			return err
		}
		if err := terminal.CheckDelete(cur, deviceID); err != nil {
			return err
		}
		n, err := q.DeleteTerminalDraft(ctx, id)
		deleted = n > 0
		return err
	})
	return deleted, err
}

// PickupOrders are the open batch's pickup / counter orders of every source
// but the old POS, oldest first (the service keeps the Pickup list's).
func (r *OrderRepository) PickupOrders(ctx context.Context) (out []order.StoredOrder, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.pickup_orders")
	defer end(&err)
	q := models.New(r.db)
	b, err := q.GetOpenBatch(ctx)
	if err != nil {
		return nil, fmt.Errorf("open batch: %w", err)
	}
	rows, err := q.ListPickupOrders(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	// Checks carried past their batch's close stay on the list until
	// settled (contract 2.10s §4), oldest first.
	carried, err := q.ListCarriedOrders(ctx)
	if err != nil {
		return nil, err
	}
	var all []models.Order
	for _, c := range carried {
		if c.BatchID != b.ID {
			all = append(all, c)
		}
	}
	return mapRowsGeo(ctx, q, append(all, rows...))
}

// MarkPickedUp finishes a pickup handed over at the counter (any source but
// the old POS): an accepted one is made ready at the same time. The
// order.status event (ready, finished_at in agent_status) and the journal
// rows go in the same transaction. already = it was finished before
// (nothing written).
func (r *OrderRepository) MarkPickedUp(ctx context.Context, id string, journal func(cur order.StoredOrder) []terminal.Entry) (so order.StoredOrder, already bool, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.picked_up")
	defer end(&err)
	now := time.Now()
	err = r.withTx(ctx, func(q *models.Queries) error {
		cur, err := loadOrder(ctx, q, id)
		if err != nil {
			return err
		}
		b, err := q.GetOpenBatch(ctx)
		if err != nil {
			return fmt.Errorf("open batch: %w", err)
		}
		if !terminal.IsPickup(cur) || (cur.Status.BatchID != b.ID && !cur.Status.Carried) {
			return terminal.ErrNotPickable
		}
		switch cur.Status.State {
		case order.StateAccepted, order.StateReady:
		default:
			return terminal.ErrNotPickable
		}
		if due := terminal.DueOf(cur); due > 0 {
			return &terminal.CashDue{BalanceCents: due}
		}
		if cur.Status.FinishedAt != nil {
			so, already = cur, true
			return nil
		}
		if cur.Status.State == order.StateAccepted {
			if _, err := q.MarkReady(ctx, models.MarkReadyParams{ReadyAt: nullTS(now), ID: id}); err != nil {
				return fmt.Errorf("mark ready: %w", err)
			}
		}
		n, err := q.SetOrderFinished(ctx, models.SetOrderFinishedParams{FinishedAt: nullTS(now), FinishedBy: terminal.FinishedByTerminal, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return terminal.ErrNotPickable
		}
		if so, err = loadOrder(ctx, q, id); err != nil {
			return err
		}
		if err := recordOrderStatus(ctx, q, so, false, now); err != nil {
			return err
		}
		if err := settleCarried(ctx, q, id); err != nil {
			return err
		}
		return insertJournal(ctx, q, now, journal(cur))
	})
	return so, already, err
}
