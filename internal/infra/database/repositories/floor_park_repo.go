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

	"github.com/google/uuid"
)

// Floor sync dead letters (contract/agent.md §2.4e): refusals of a kept event
// are counted; past the thresholds the event is parked (it keeps its place in
// floor_events, later events about the same subject wait behind it) with a
// floor_dead_letters row and a journal entry, in one transaction.

// RefuseEvent counts one more refusal of a kept event (also an attempt) and
// returns the count and the first refusal's time.
func (r *OrderRepository) RefuseEvent(ctx context.Context, id, result string, at time.Time) (ref order.Refusal, err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.refuse")
	defer end(&err)
	msg := "main answered " + result + " (kept, retried)"
	row, err := models.New(r.db).MarkFloorEventRefused(ctx, models.MarkFloorEventRefusedParams{LastError: msg, At: nullTS(at), ID: id})
	if err != nil {
		return ref, err
	}
	ref = order.Refusal{EventID: id, Type: row.Type, Subject: row.Subject, Refusals: row.Refusals, LastError: msg}
	if row.FirstRefusedAt.Valid {
		if ref.FirstAt, err = parseTS(row.FirstRefusedAt.String); err != nil {
			return ref, err
		}
	}
	return ref, nil
}

// ParkEvent parks a refused event: false when it was synced or parked
// already. The dead letter row and a journal entry (sync.parked) go in the
// same transaction.
func (r *OrderRepository) ParkEvent(ctx context.Context, ref order.Refusal, result string, at time.Time) (parked bool, err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.park")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		n, err := q.ParkFloorEvent(ctx, models.ParkFloorEventParams{ParkedAt: nullTS(at), ID: ref.EventID})
		if err != nil || n == 0 {
			return err
		}
		parked = true
		first := ""
		if !ref.FirstAt.IsZero() {
			first = fmtTS(ref.FirstAt)
		}
		if err := q.InsertDeadLetter(ctx, models.InsertDeadLetterParams{EventID: ref.EventID, Subject: ref.Subject, Type: ref.Type,
			Result: result, Refusals: ref.Refusals, FirstRefusedAt: first, ParkedAt: fmtTS(at), LastError: ref.LastError}); err != nil {
			return fmt.Errorf("dead letter: %w", err)
		}
		return insertJournal(ctx, q, at, []terminal.Entry{{DeviceID: "pi", Action: terminal.ActionSyncParked, OrderID: orderIDOf(ref.Subject),
			Detail: jsonDetail(map[string]any{"event_id": ref.EventID, "type": ref.Type, "subject": ref.Subject, "result": result,
				"refusals": ref.Refusals})}})
	})
	return parked, err
}

// jsonDetail marshals a journal detail object.
func jsonDetail(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// orderIDOf is a subject's order id (journal rows want a uuid or nothing).
func orderIDOf(subject string) string {
	if _, err := uuid.Parse(subject); err == nil {
		return subject
	}
	return ""
}

// ParkedEvents is what GET /api/sync/parked shows (at most limit rows).
func (r *OrderRepository) ParkedEvents(ctx context.Context, limit int) (v order.ParkedView, err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.parked")
	defer end(&err)
	q := models.New(r.db)
	rows, err := q.ListOpenDeadLetters(ctx, int64(limit))
	if err != nil {
		return v, err
	}
	v.Parked = make([]order.DeadLetter, 0, len(rows))
	for _, row := range rows {
		d := order.DeadLetter{EventID: row.EventID, Type: row.Type, Subject: row.Subject, Result: row.Result, Refusals: row.Refusals,
			LastError: row.LastError}
		d.FirstRefusedAt, _ = parseTS(row.FirstRefusedAt)
		d.ParkedAt, _ = parseTS(row.ParkedAt)
		d.At, _ = parseTS(row.At)
		v.Parked = append(v.Parked, d)
	}
	if v.Count, err = q.CountParkedFloorEvents(ctx); err != nil {
		return v, err
	}
	if v.Held, err = q.CountHeldFloorEvents(ctx); err != nil {
		return v, err
	}
	if v.Backlog, err = q.CountUnsyncedFloorEvents(ctx); err != nil {
		return v, err
	}
	v.Settings, err = syncPark(ctx, q)
	return v, err
}

// CountParkedEvents is how many events are parked now.
func (r *OrderRepository) CountParkedEvents(ctx context.Context) (n int64, err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.count_parked")
	defer end(&err)
	return models.New(r.db).CountParkedFloorEvents(ctx)
}

// ErrNotParked: the event isn't parked (synced, retried already, unknown).
var ErrNotParked = errors.New("that event isn't parked")

// RetryParked puts parked events back in the queue at their old place
// (refusals start again); ids empty = every parked event. Journaled
// sync.retry. Returns how many went back.
func (r *OrderRepository) RetryParked(ctx context.Context, ids []string, by string, at time.Time) (n int, err error) {
	ctx, _, end := dbSpan(ctx, "floor_events.retry_parked")
	defer end(&err)
	err = r.withTx(ctx, func(q *models.Queries) error {
		if len(ids) == 0 {
			if ids, err = q.ListParkedFloorEventIDs(ctx); err != nil {
				return err
			}
		}
		for _, id := range ids {
			k, err := q.UnparkFloorEvent(ctx, id)
			if err != nil {
				return err
			}
			if k == 0 {
				continue
			}
			n++
			if err := q.MarkDeadLetterRetried(ctx, models.MarkDeadLetterRetriedParams{RetriedAt: nullTS(at), RetriedBy: by, EventID: id}); err != nil {
				return err
			}
		}
		if n == 0 {
			return ErrNotParked
		}
		return insertJournal(ctx, q, at, []terminal.Entry{{DeviceID: by, Action: terminal.ActionSyncRetry,
			Detail: jsonDetail(map[string]any{"event_ids": ids, "count": n})}})
	})
	return n, err
}

func syncPark(ctx context.Context, q *models.Queries) (order.SyncPark, error) {
	row, err := q.GetSyncParkSettings(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return order.SyncPark{Refusals: order.DefaultSyncParkRefusals, Hours: order.DefaultSyncParkHours}, nil
	}
	if err != nil {
		return order.SyncPark{}, err
	}
	p := order.SyncPark{Refusals: int(row.SyncParkRefusals), Hours: int(row.SyncParkHours)}
	if row.SyncParkAlertedAt != "" {
		if t, err := parseTS(row.SyncParkAlertedAt); err == nil {
			p.AlertedAt = &t
		}
	}
	return p, nil
}

// SyncParkSettings are the parking thresholds.
func (r *OrderRepository) SyncParkSettings(ctx context.Context) (p order.SyncPark, err error) {
	ctx, _, end := dbSpan(ctx, "settings.sync_park")
	defer end(&err)
	return syncPark(ctx, models.New(r.db))
}

// SetSyncParkSettings saves the thresholds (validated by the caller).
func (r *OrderRepository) SetSyncParkSettings(ctx context.Context, refusals, hours int) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.set_sync_park")
	defer end(&err)
	return models.New(r.db).SetSyncParkSettings(ctx, models.SetSyncParkSettingsParams{SyncParkRefusals: int64(refusals), SyncParkHours: int64(hours)})
}

// SetSyncParkAlertedAt remembers the last parked-events Pushover.
func (r *OrderRepository) SetSyncParkAlertedAt(ctx context.Context, at time.Time) (err error) {
	ctx, _, end := dbSpan(ctx, "settings.sync_park_alerted")
	defer end(&err)
	return models.New(r.db).SetSyncParkAlertedAt(ctx, fmtTS(at))
}
