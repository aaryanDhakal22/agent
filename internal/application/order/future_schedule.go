package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// Future orders (2.1.0 / 2.4.0): an online order due later than its default
// time waits (scheduled) after its first confirm and rings again at its
// lead. Staff's actions on a waiting one are in future.go.

// ErrNotFuture: schedule of an order that isn't a future order (409).
var ErrNotFuture = errors.New("not a future order")

// Schedule is a future order's first confirm (or a lead edit while it
// waits): no print, no accepted event; it waits in scheduled until
// promised_at - lead, then rings again.
func (s *Service) Schedule(ctx context.Context, id string, leadMinutes int) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.schedule", attribute.String("order.id", id), attribute.Int("lead_minutes", leadMinutes))
	defer span.End()
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	if leadMinutes < 1 || leadMinutes > 240 {
		return nil, spanErr(span, fmt.Errorf("%w: lead_minutes must be 1-240", ErrBadRequest))
	}
	so, err := s.repo.GetByID(ctx, uid)
	if err != nil {
		return nil, spanErr(span, err)
	}
	if so.Status.State != order.StateArrived && so.Status.State != order.StateScheduled {
		return nil, spanErr(span, fmt.Errorf("%w (state %s)", order.ErrInvalidState, so.Status.State))
	}
	if !so.Status.Future || so.Order.Fulfillment.PromisedAt == nil {
		return nil, spanErr(span, ErrNotFuture)
	}
	first := so.Status.State == order.StateArrived
	ringAt := so.Order.Fulfillment.PromisedAt.Add(-time.Duration(leadMinutes) * time.Minute)
	out, err := s.repo.Schedule(ctx, uid.String(), int64(leadMinutes), ringAt)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*out)
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int("lead_minutes", leadMinutes).Time("ring_at", ringAt).Msg("Future order scheduled")
	if first {
		s.printEarly(ctx, *out)
	}
	return out, nil
}

// FutureLead is a future order's default lead (2.4.0): a delivery starts
// the store's delivery target before it is due (AUTO's placed-to-door
// minutes: due 6:40 with a 40-min target rings at 6:00), anything else its
// default time.
func (s *Service) FutureLead(ctx context.Context, so order.StoredOrder) int {
	if so.IsDelivery() {
		if t := s.DeliveryPrefs(ctx).DeliveryTargetMinutes; t > 0 {
			return int(t)
		}
	}
	return int(s.IntegrationSettings().Times.For(so.Order.Fulfillment.Type))
}

// printEarly prints a future order's early tickets on its first ACCEPT
// (2.4.0), in the background: with the store's future switches on, the
// kitchen tickets (DO NOT MAKE, when it is due) and / or the customer
// receipt (FUTURE on top). What printed is recorded - the CONFIRM at its
// time then skips the receipt.
func (s *Service) printEarly(ctx context.Context, so order.StoredOrder) {
	t := s.IntegrationSettings().Tickets
	if !t.FutureKitchen && !t.FutureReceipt {
		return
	}
	s.background(ctx, func(ctx context.Context) {
		out, err := s.print(ctx, printable(so), printer.ModeFutureEarly)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		kitchen, receipt := earlyPrinted(out)
		if kitchen || receipt {
			if err := s.repo.SetEarlyPrinted(ctx, so.Order.ID, kitchen, receipt); err != nil {
				s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("record early future tickets failed")
			}
			s.printEvents(label(so)+" (future, early)", out)
		}
		s.alertPrintIssues(ctx, so, out, false)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("Future order: early tickets not printed")
		}
	})
}

// RaiseDue puts scheduled orders whose ring time came back to arrived
// (rerung) and rings the tablets; the 2-minute escalation then applies. With
// auto-accept on, a raised order is accepted (and printed) right away, with
// its lead as the prep, as a new order would be. Run on the escalation ticker.
func (s *Service) RaiseDue(ctx context.Context, now time.Time) (int, error) {
	list, err := s.repo.RaiseDue(ctx, now)
	if err != nil {
		return 0, err
	}
	// §2.10aa: a due future order is placed at its ring time (it never
	// rings); only a failed claim leaves it waiting for the tablets.
	wait := len(list) > 0 && s.waitsArrived(ctx)
	for _, so := range list {
		id := uuid.MustParse(so.Order.ID)
		if wait {
			s.broadcastArrived(ctx, so)
			continue
		}
		if _, err := s.Place(ctx, id); err == nil {
			s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Msg("Future order is due: placed")
			continue
		}
		if cur, gerr := s.repo.GetByID(ctx, id); gerr == nil {
			if cur.Status.State != order.StateArrived {
				s.publishUpdated(*cur)
				continue
			}
			so = *cur
		}
		s.logger.Error().Ctx(ctx).Str("order_id", so.Order.ID).Msg("Placing a due future order failed - it waits on the tablets")
		s.broadcastArrived(ctx, so)
	}
	return len(list), nil
}
