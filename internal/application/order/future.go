package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// Staff's actions on a waiting future order (2.4.3): MAKE NOW, CHANGE DUE
// TIME, PRINT NOW and CANCEL, on the tablet's order details and the web UI's
// Later strip. Each takes an online order that is still a future order,
// arrived (not confirmed yet) or scheduled.

// MaxDueAhead caps CHANGE DUE TIME.
const MaxDueAhead = 14 * 24 * time.Hour

// ErrFuturePrintFailed: PRINT NOW printed nothing anywhere (502).
var ErrFuturePrintFailed = errors.New("nothing printed")

// waitingFuture loads id and checks it is a waiting future order.
func (s *Service) waitingFuture(ctx context.Context, id string) (*order.StoredOrder, uuid.UUID, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, uid, fmt.Errorf("%w: invalid order id", ErrBadRequest)
	}
	so, err := s.repo.GetByID(ctx, uid)
	if err != nil {
		return nil, uid, err
	}
	if so.Status.State != order.StateArrived && so.Status.State != order.StateScheduled {
		return nil, uid, fmt.Errorf("%w (state %s)", order.ErrInvalidState, so.Status.State)
	}
	if !so.Status.Future {
		return nil, uid, ErrNotFuture
	}
	return so, uid, nil
}

// MakeNow takes a waiting future order out of the future and accepts it
// with prepMinutes, as a new order would be: kitchen number, tickets and
// receipt (no FUTURE / DO NOT MAKE - its promised_at is dropped), accepted
// sent to main. When nothing prints the accept is undone and it waits as a
// plain new order (no longer future), ringing.
func (s *Service) MakeNow(ctx context.Context, id string, prepMinutes int) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.make_now", attribute.String("order.id", id), attribute.Int("prep_minutes", prepMinutes))
	defer span.End()
	if prepMinutes < 1 || prepMinutes > 240 {
		return nil, spanErr(span, fmt.Errorf("%w: prep_minutes must be 1-240", ErrBadRequest))
	}
	so, uid, err := s.waitingFuture(ctx, id)
	if err != nil {
		return nil, spanErr(span, err)
	}
	if err := s.repo.PrepareMakeNow(ctx, uid.String()); err != nil {
		return nil, spanErr(span, err)
	}
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int("prep_minutes", prepMinutes).Msg("Future order made now")
	s.devEvent(devlog.Info, devlog.KindOrder, "%s made now (was a future order)", label(*so))
	out, err := s.Accept(ctx, uid, &prepMinutes)
	if err != nil {
		// Not accepted (the printers): it rings as a new order now.
		if back, gerr := s.repo.GetByID(context.WithoutCancel(ctx), uid); gerr == nil && back.Status.State == order.StateArrived {
			s.floorChanged()
			s.broadcastArrived(ctx, *back)
		}
		return nil, spanErr(span, err)
	}
	return out, nil
}

// ChangeDue moves a waiting future order's due time on the Pi (main keeps
// the vendor's): a scheduled order's ring time moves with it (due - lead),
// and rings at once when that has passed already.
func (s *Service) ChangeDue(ctx context.Context, id string, due time.Time) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.change_due", attribute.String("order.id", id))
	defer span.End()
	now := time.Now()
	if !due.After(now) || due.After(now.Add(MaxDueAhead)) {
		return nil, spanErr(span, fmt.Errorf("%w: promised_at must be in the next 14 days", ErrBadRequest))
	}
	so, uid, err := s.waitingFuture(ctx, id)
	if err != nil {
		return nil, spanErr(span, err)
	}
	var ringAt *time.Time
	if so.Status.State == order.StateScheduled && so.Status.ScheduledLeadMinutes != nil {
		r := due.Add(-time.Duration(*so.Status.ScheduledLeadMinutes) * time.Minute)
		ringAt = &r
	}
	out, err := s.repo.SetDueOverride(ctx, uid.String(), due, ringAt)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*out)
	s.logger.Info().Ctx(ctx).Str("order_id", id).Time("due", due).Msg("Future order moved")
	s.devEvent(devlog.Info, devlog.KindOrder, "%s moved to %s", label(*out), due.In(s.tz()).Format("Jan 2 3:04 PM"))
	if ringAt != nil && !ringAt.After(now) {
		if _, err := s.RaiseDue(ctx, now); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Msg("raise a moved future order failed (the next tick retries)")
		}
	}
	return out, nil
}

// PrintNow prints a waiting future order's early tickets on staff's ask:
// the kitchen tickets (DO NOT MAKE, when it is due) and / or the customer
// receipt (FUTURE on top), whatever the store's future switches say. What
// printed is recorded like the automatic early print.
func (s *Service) PrintNow(ctx context.Context, id string, kitchen, receipt bool) (*order.StoredOrder, printer.Outcome, error) {
	ctx, span := s.span(ctx, "order.print_now", attribute.String("order.id", id),
		attribute.Bool("kitchen", kitchen), attribute.Bool("receipt", receipt))
	defer span.End()
	if !kitchen && !receipt {
		return nil, printer.Outcome{}, spanErr(span, fmt.Errorf("%w: pick kitchen and / or receipt", ErrBadRequest))
	}
	so, uid, err := s.waitingFuture(ctx, id)
	if err != nil {
		return nil, printer.Outcome{}, spanErr(span, err)
	}
	mode := printer.ModeFutureBoth
	switch {
	case !receipt:
		mode = printer.ModeFutureKitchen
	case !kitchen:
		mode = printer.ModeFutureReceipt
	}
	out, err := s.printJob(context.WithoutCancel(ctx), printable(*so), mode)
	if errors.Is(err, printer.ErrNoTickets) {
		return nil, out, spanErr(span, ErrNothingForKitchen)
	}
	s.alertPrintIssues(ctx, *so, out, false)
	k, r := earlyPrinted(out)
	if !k && !r {
		if err == nil {
			err = ErrFuturePrintFailed
		}
		return nil, out, spanErr(span, fmt.Errorf("%w: %w", ErrFuturePrintFailed, err))
	}
	if err := s.repo.SetEarlyPrinted(ctx, uid.String(), k, r); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", id).Msg("record early future tickets failed")
	}
	s.printEvents(label(*so)+" (future, printed now)", out)
	fresh, err := s.repo.GetByID(ctx, uid)
	if err != nil {
		return nil, out, spanErr(span, err)
	}
	s.publishUpdated(*fresh)
	s.logger.Info().Ctx(ctx).Str("order_id", id).Bool("kitchen", k).Bool("receipt", r).Msg("Future order printed now")
	return fresh, out, nil
}

// CancelFuture cancels a waiting future order (the customer called): it
// leaves the board for good and main hears cancelled.
func (s *Service) CancelFuture(ctx context.Context, id string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.cancel_future", attribute.String("order.id", id))
	defer span.End()
	_, uid, err := s.waitingFuture(ctx, id)
	if err != nil {
		return nil, spanErr(span, err)
	}
	out, err := s.repo.CancelWaiting(ctx, uid.String())
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*out)
	s.logger.Info().Ctx(ctx).Str("order_id", id).Msg("Future order cancelled")
	s.devEvent(devlog.Info, devlog.KindOrder, "%s cancelled (future order)", label(*out))
	return out, nil
}

// earlyPrinted reads which early tickets printed.
func earlyPrinted(out printer.Outcome) (kitchen, receipt bool) {
	for _, r := range out.Printed {
		switch r.Kind {
		case printer.TicketStation:
			kitchen = true
		case printer.TicketReceipt, printer.TicketFull:
			receipt = true
		}
	}
	return kitchen, receipt
}
