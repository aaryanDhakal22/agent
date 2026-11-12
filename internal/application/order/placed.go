package order

import (
	"context"
	"errors"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Online orders = called-in orders (agent contract §2.10aa, the owner
// 2026-11-12): "the online delivery order should not be treated as any
// different ... Quibase doesnt need to ring, orders will just show up ... The
// prep still exists and is always defaulted." Every order main sends is
// PLACED on arrival like a terminal SEND of a called-in pickup / delivery: no
// accept step, prep = the store's default for its type, the station tickets
// (+ a delivery's Merchant / Customer Copy) printed at that moment, never
// ringing, never reverted to arrived.

// SetPrepSource registers the default prep minutes for a fulfillment type as
// a terminal SEND takes them (the terminal service: main's
// default_pickup_minutes for a pickup, else the Pi's default for the type).
// Unset = the Pi's defaults (PrepMinutesFor).
//
// It is also the start signal for the leftovers: cmd/main.go builds the
// terminal service (which calls it) after RecoverInterruptedAccepts put cut
// off accepts back to arrived, so the first call places every order still
// arrived in the background (PlaceArrived) - they never ring again.
func (s *Service) SetPrepSource(fn func(fulfillment string) int64) {
	s.prepSource.Store(&fn)
	s.placeOnce.Do(func() {
		if s.legacyArrival {
			return // the tests' kept accept path: leftovers wait for staff
		}
		s.background(context.Background(), func(ctx context.Context) { s.PlaceArrived(ctx) })
	})
}

// KeepAcceptStep (dev tests only, cmd/main.go AGENT_TEST_ACCEPT_STEP): an
// arrival waits arrived unless the auto-accept setting is on, as before
// §2.10aa - for the older suites of the kept accept path.
func (s *Service) KeepAcceptStep(on bool) { s.legacyArrival = on }

// placedPrep is the prep a placed order gets: a re-rung future order's lead,
// else the store's default for its type, bounded 0-240.
func (s *Service) placedPrep(so order.StoredOrder) int {
	if so.Status.Rerung && so.Status.ScheduledLeadMinutes != nil {
		return int(min(max(*so.Status.ScheduledLeadMinutes, 0), 240))
	}
	m := s.PrepMinutesFor(so.Order.Fulfillment.Type)
	if fn := s.prepSource.Load(); fn != nil && *fn != nil {
		m = (*fn)(so.Order.Fulfillment.Type)
	}
	return int(min(max(m, 0), 240))
}

// Place places a stored arrived order (§2.10aa): claimed accepted with the
// default prep (auto_accepted: nobody tapped accept), the tablets get
// order.updated, its tickets print (stations + a delivery's two copies) and
// the accepted floor event goes to main whatever printed - a print problem is
// a Pushover, the order stays placed. A delivery joins auto-assign like any.
func (s *Service) Place(ctx context.Context, id uuid.UUID) (*order.StoredOrder, error) {
	ctx, span := s.tracer.Start(ctx, "order.place", trace.WithAttributes(attribute.String("order.id", id.String())))
	defer span.End()
	fail := func(err error) (*order.StoredOrder, error) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Placing the order failed")
		return nil, err
	}
	cur, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return fail(err)
	}
	prep := s.placedPrep(*cur)
	so, err := s.repo.ClaimAccept(ctx, id, prep, true)
	if err != nil {
		return fail(err)
	}
	span.SetAttributes(attribute.Int64("order.kitchen_number", deref(so.Status.KitchenNumber)), attribute.Int("order.prep_minutes", prep))
	s.publishUpdated(*so) // it shows at once; the print follows
	pctx := context.WithoutCancel(ctx)
	start := time.Now()
	mode := printer.ModePlaced
	snap := printable(*so)
	if so.Status.EarlyReceipt && so.IsDelivery() {
		// A future delivery whose copies printed early: the stations only now.
		mode = printer.ModeKitchen
	}
	var out printer.Outcome
	perr := printer.ErrNoTickets // no printing wired (tests of other routes)
	if s.print != nil {
		out, perr = s.print(pctx, snap, mode)
	}
	if errors.Is(perr, printer.ErrNoTickets) {
		perr = nil
	}
	s.recordPrintTime(ctx, *so, start, out, perr)
	if perr != nil {
		s.devEvent(devlog.Error, devlog.KindPrint, "%s placed: nothing printed", label(*so))
		s.logger.Warn().Ctx(ctx).Err(perr).Str("order_id", id.String()).Msg("Placed order: nothing printed")
	}
	s.alertPrintIssues(ctx, *so, out, true)
	if err := s.repo.RecordAccepted(pctx, *so); err != nil {
		s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Failed to record the placed order's floor event (main won't see it)")
	}
	s.floorChanged()
	s.recordAcceptTime(ctx, *so)
	s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Int64("kitchen_number", deref(so.Status.KitchenNumber)).
		Int("tickets", len(out.Printed)).Int("prep_minutes", prep).Msg("Order placed (called-in style)")
	s.devEvent(devlog.Info, devlog.KindOrder, "%s placed (auto-accepted, prep %d min)", label(*so), prep)
	s.printEvents(label(*so), out)
	if back, gerr := s.repo.GetByID(pctx, id); gerr == nil {
		so = back
	}
	s.publishUpdated(*so)
	if so.IsDelivery() {
		s.autoAssignBG(ctx, "accepted")
	}
	return so, nil
}

// placeOrSchedule places a stored arrived order now, or schedules a future
// one with its lead (placed at its ring time by RaiseDue). It never leaves
// the order ringing unless the claim itself failed.
func (s *Service) placeOrSchedule(ctx context.Context, id uuid.UUID, snap order.Snapshot) {
	if cur, gerr := s.repo.GetByID(ctx, id); gerr == nil && cur.Status.Future && cur.Status.State == order.StateArrived && !cur.Status.Rerung {
		lead := s.FutureLead(ctx, *cur)
		if _, err := s.Schedule(ctx, id.String(), lead); err == nil {
			s.logger.Info().Ctx(ctx).Str("order_id", id.String()).Int("lead_minutes", lead).Msg("Future order scheduled (placed at its time)")
			return
		} else {
			s.logger.Error().Ctx(ctx).Err(err).Str("order_id", id.String()).Msg("Scheduling a future order failed - placing it now")
		}
	}
	if _, err := s.Place(ctx, id); err != nil {
		// Only a failed claim gets here (a DB error, or the order is no
		// longer arrived): the tablets see it as it is.
		s.announceStored(ctx, id, snap)
	}
}

// PlaceArrived runs once at start: every order of the open batch still
// arrived (stored by an older Pi, or an accept cut off mid-print and put
// back) is placed now the §2.10aa way. Returns how many.
func (s *Service) PlaceArrived(ctx context.Context) int {
	list, err := s.repo.ListArrived(ctx)
	if err != nil {
		s.logger.Error().Ctx(ctx).Err(err).Msg("Placing waiting orders at start failed")
		return 0
	}
	n := 0
	for _, so := range list {
		id, err := uuid.Parse(so.Order.ID)
		if err != nil || so.Order.Source == order.SourceCounter {
			continue
		}
		s.placeOrSchedule(ctx, id, so.Order)
		n++
	}
	if n > 0 {
		s.logger.Info().Ctx(ctx).Int("orders", n).Msg("Waiting orders placed at start")
	}
	return n
}

// placedDirectMeta marks a snapshot as placed on arrival (status.placed_direct).
func placedDirectMeta(snap *order.Snapshot) {
	snap.SourceMeta = snap.WithPiMeta(map[string]any{"placed_direct": true})
}
