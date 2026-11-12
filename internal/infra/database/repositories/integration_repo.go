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
)

// The store hub (2.1.0; the callout box since H3): the hub's feed, feature /
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
func feedReady(ctx context.Context, q *models.Queries, so order.StoredOrder, at time.Time, callout bool) error {
	d := order.OrderReadyData{FeedOrderRef: order.RefOf(so),
		Number: so.Number(), FulfillmentType: so.Order.Fulfillment.Type, Paid: so.Status.Paid, CashDueCents: so.Status.CashDueCents}
	if !callout {
		d.Callout = &callout
	}
	return insertFeed(ctx, q, order.FeedOrderReady, at, d)
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
	if set.Look, err = order.DecodeLook([]byte(row.Look)); err != nil {
		set.Look = order.DefaultLook()
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
	lk, err := json.Marshal(set.Look)
	if err != nil {
		return err
	}
	return r.withTx(ctx, func(q *models.Queries) error {
		if err := q.SetIntegrationSettings(ctx, models.SetIntegrationSettingsParams{Features: string(f), TicketToggles: string(t),
			DefaultPickupMinutes: set.Times.DefaultPickupMinutes, DefaultDeliveryMinutes: set.Times.DefaultDeliveryMinutes,
			Finishing: string(fin), Callout: string(co), Look: string(lk)}); err != nil {
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

// AppendFeed — see order.IntegrationRepository.
func (r *OrderRepository) AppendFeed(ctx context.Context, typ string, at time.Time, data any) (err error) {
	ctx, _, end := dbSpan(ctx, "integration_events.append")
	defer end(&err)
	return insertFeed(ctx, models.New(r.db), typ, at, data)
}

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

// --- finishing, terminal changes, callout (00019) ---------------------------------

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

// InsertCalloutRender — see order.IntegrationRepository.
func (r *OrderRepository) InsertCalloutRender(ctx context.Context, t order.CalloutRenderData) (err error) {
	ctx, _, end := dbSpan(ctx, "integration_events.callout_render")
	defer end(&err)
	return insertFeed(ctx, models.New(r.db), order.FeedCalloutRender, time.Now(), t)
}
