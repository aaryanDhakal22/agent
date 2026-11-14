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
	return listDrafts(ctx, models.New(r.db), locationID)
}

// draftOrigins reads the drafts' origin column (00043; hand-written: the
// sqlc queries name their columns): id -> origin, only the non-empty ones.
func draftOrigins(ctx context.Context, q *models.Queries, locationID string) (map[string]string, error) {
	rows, err := q.Raw().QueryContext(ctx, `SELECT id, origin FROM terminal_drafts WHERE location_id = ? AND origin <> ''`, locationID)
	if err != nil {
		return nil, fmt.Errorf("draft origins: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, origin string
		if err := rows.Scan(&id, &origin); err != nil {
			return nil, err
		}
		out[id] = origin
	}
	return out, rows.Err()
}

// listDrafts are a store's drafts with their origin, oldest first.
func listDrafts(ctx context.Context, q *models.Queries, locationID string) ([]terminal.Draft, error) {
	rows, err := q.ListTerminalDrafts(ctx, locationID)
	if err != nil {
		return nil, err
	}
	origins, err := draftOrigins(ctx, q, locationID)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.Draft, 0, len(rows))
	for _, row := range rows {
		d, err := draftFromRow(row)
		if err != nil {
			return nil, err
		}
		d.Origin = origins[d.ID]
		out = append(out, d)
	}
	return out, nil
}

// PutTerminalDraft creates or replaces a draft under the soft lock (one
// transaction: two terminals racing for the same draft get one winner).
// The draft's holder and the one it was taken from (nil = none) come from
// put.Decide. A PUT that names a sent order also runs put.OrderLock against
// the order's other drafts (contract 2.10ab, the store wins): a seat is
// refused while a store terminal has the order open; a store terminal's PUT
// deletes the call-center drafts of that order (Displaced).
func (r *OrderRepository) PutTerminalDraft(ctx context.Context, in terminal.Draft, put terminal.DraftPut, now time.Time) (res terminal.DraftPutResult, err error) {
	ctx, _, end := dbSpan(ctx, "terminal.put_draft")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		res = terminal.DraftPutResult{}
		all, err := listDrafts(ctx, q, in.LocationID)
		if err != nil {
			return err
		}
		cur, err := getDraft(ctx, q, in.ID)
		if err != nil {
			return err
		}
		if cur != nil && cur.LocationID != in.LocationID {
			cur = nil // another store's leftover: replaced
		}
		var others []terminal.Draft
		for _, d := range all {
			if d.ID != in.ID {
				others = append(others, d)
			}
		}
		displaced, err := put.OrderLock(in.OrderID, others)
		if err != nil {
			return err
		}
		holder, tk, err := put.Decide(cur)
		if err != nil {
			return err
		}
		res.Taken = tk
		for _, d := range displaced {
			if _, err := q.DeleteTerminalDraft(ctx, d.ID); err != nil {
				return fmt.Errorf("displace draft: %w", err)
			}
		}
		res.Displaced = displaced
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
		// Whose it is now: the PUT's sender (a store terminal taking a seat's
		// draft over makes it the store's, with the terminal's own body).
		origin := ""
		if put.BySeat() {
			origin = terminal.DraftOriginCallcenter
		}
		if _, err := q.Raw().ExecContext(ctx, `UPDATE terminal_drafts SET origin = ? WHERE id = ?`, origin, in.ID); err != nil {
			return fmt.Errorf("draft origin: %w", err)
		}
		d, err := getDraft(ctx, q, in.ID)
		if err != nil {
			return err
		}
		d.Origin = origin
		res.Draft = *d
		return nil
	})
	return res, err
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
		// A check reopened since it was finished (contract 2.10ak) is
		// finished again; any other finished one answers already.
		if cur.Status.FinishedAt != nil && !cur.Status.OpenAgain() {
			so, already = cur, true
			return nil
		}
		if cur.Status.State == order.StateAccepted {
			if _, err := q.MarkReady(ctx, models.MarkReadyParams{ReadyAt: nullTS(now), ID: id}); err != nil {
				return fmt.Errorf("mark ready: %w", err)
			}
		}
		var n int64
		if cur.Status.FinishedAt != nil { // reopened (2.10ak): finished again
			n, err = q.SetOrderRefinished(ctx, models.SetOrderRefinishedParams{FinishedAt: nullTS(now), FinishedBy: terminal.FinishedByTerminal, ID: id})
		} else {
			n, err = q.SetOrderFinished(ctx, models.SetOrderFinishedParams{FinishedAt: nullTS(now), FinishedBy: terminal.FinishedByTerminal, ID: id})
		}
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

// claimDraft deletes the shared draft a SEND / cart change came from
// (contract 2.10s §9), in the order's transaction. A draft another terminal
// has open (it was TAKEN OVER since this one opened it) refuses the SEND:
// *terminal.OpenElsewhere, so the old holder's late SEND can't place a
// second order or write a stale cart over the new holder's
// (TERMINAL-FLOWS TAB-16). A held draft (nobody has it open), this
// terminal's own, or one already gone is fine.
func claimDraft(ctx context.Context, q *models.Queries, deviceID string) error {
	id := terminal.DraftIDOf(ctx)
	if id == "" {
		return nil
	}
	cur, err := getDraft(ctx, q, id)
	if err != nil {
		return err
	}
	if cur != nil && cur.Holder != nil && deviceID != "" && cur.Holder.DeviceID != deviceID {
		return &terminal.OpenElsewhere{Holder: *cur.Holder}
	}
	if _, err := q.DeleteTerminalDraft(ctx, id); err != nil {
		return fmt.Errorf("delete draft: %w", err)
	}
	return nil
}
