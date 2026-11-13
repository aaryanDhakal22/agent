package order

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Terminal stage 0d: what the terminal service (application/terminal) needs
// from the floor - printing a counter order's station tickets, the VOID slip, the change
// tickets,
// telling the tablets and main about a change, the default times.

// PrintCounterTickets prints a new counter order's station tickets in the
// background (the Resend-to-kitchen plan: stations only, never the receipt,
// which prints on the terminal's own printer). Nothing printed only alerts.
func (s *Service) PrintCounterTickets(ctx context.Context, so order.StoredOrder) {
	s.PrintCounterTicketsThen(ctx, so, nil)
}

// PrintCounterTicketsThen is PrintCounterTickets with done run right after
// the print was tried, before anything else is written (M-11: the order's
// kitchen_pending is cleared then).
func (s *Service) PrintCounterTicketsThen(ctx context.Context, so order.StoredOrder, done func(ctx context.Context)) {
	s.background(ctx, func(ctx context.Context) {
		start := time.Now()
		// done runs as soon as the paper is out (before order.printed and the
		// alerts): a restart after the print must find kitchen_pending cleared.
		var settle func(context.Context, printer.Outcome, error)
		if done != nil {
			settle = func(ctx context.Context, _ printer.Outcome, _ error) { done(ctx) }
		}
		out, err := s.printJobThen(ctx, printable(so), printer.ModeKitchen, settle)
		s.recordPrintTime(ctx, so, start, out, err)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		s.alertPrintIssues(ctx, so, out, true)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("Counter order: no kitchen ticket printed")
			return
		}
		s.printEvents(label(so)+" (counter)", out)
	})
}

// PrintOrderReceipt prints an order's customer paper on the store's receipt
// printer in the background (a call-center order has no terminal printer,
// contract §2.10ab): a delivery's Merchant + Customer Copy, a pickup's
// customer receipt. Nothing printed only alerts.
func (s *Service) PrintOrderReceipt(ctx context.Context, so order.StoredOrder) {
	s.background(ctx, func(ctx context.Context) {
		out, err := s.printJob(ctx, printable(so), printer.ModeOrderReceipt)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		s.alertPrintIssues(ctx, so, out, false)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("Call-center order: no receipt printed")
			return
		}
		s.printEvents(label(so)+" receipt (call center)", out)
	})
}

// ReprintOn is Reprint that also says where the receipt printed (the
// terminals' Reprint, contract §2.10ab): the actual printer's name.
// ErrPrintingUnavailable when there is no printer at all.
func (s *Service) ReprintOn(ctx context.Context, id string) (*order.StoredOrder, string, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, "", order.ErrNotFound
	}
	so, err := s.repo.GetByID(ctx, uid)
	if err != nil {
		return nil, "", err
	}
	if s.print == nil {
		return nil, "", ErrPrintingUnavailable
	}
	out, err := s.printJob(ctx, printable(*so), printer.ModeReceipt)
	if errors.Is(err, printer.ErrNoTickets) || (err != nil && len(out.Printed) == 0 && len(out.Failed) == 0) {
		return nil, "", ErrPrintingUnavailable // no roster: nothing was even tried
	}
	s.alertPrintIssues(ctx, *so, out, false)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrKitchenNotPrinted, err)
	}
	on := ""
	if len(out.Printed) > 0 {
		on = out.Printed[0].PrintedOn
	}
	s.meters.OrdersReprinted.Add(ctx, 1, metric.WithAttributes(attribute.String("source", "terminal")))
	s.logger.Info().Ctx(ctx).Str("order_id", id).Str("printed_on", on).Msg("Order receipt reprinted (terminal)")
	return so, on, nil
}

// PrintVoidSlip prints the VOID slip of a voided order on the stations its
// tickets went to (background).
func (s *Service) PrintVoidSlip(ctx context.Context, so order.StoredOrder) {
	if printedTickets(so) {
		s.printCancelVoid(ctx, so)
	}
}

// PrintChange prints the change tickets of a counter order changed after
// SEND (background, terminal §2.10d): one per station with the lines added
// there under ADD / AGREGAR and the lines removed there under REMOVE /
// QUITAR (struck through). so is the order now; added / removed are the
// changed lines with their stations.
//
// A type change (so.Order.TypeChangedFrom, terminal §2.10d `type`) puts the
// NOW / WAS band on the change tickets; with no line changed, every station
// with lines of the order gets a type-change ticket (ModeTypeChange).
func (s *Service) PrintChange(ctx context.Context, so order.StoredOrder, added, removed []order.LineItem) {
	typeOnly := len(added) == 0 && len(removed) == 0
	if typeOnly && so.Order.TypeChangedFrom == "" {
		return
	}
	snap := printable(so)
	mode := printer.ModeTypeChange
	if !typeOnly {
		mode = printer.ModeChange
		snap.Items = append([]order.LineItem(nil), added...)
		snap.RemovedItems = append([]order.LineItem(nil), removed...)
	}
	s.background(ctx, func(ctx context.Context) {
		out, err := s.printJob(ctx, snap, mode)
		if errors.Is(err, printer.ErrNoTickets) {
			return
		}
		s.alertPrintIssues(ctx, so, out, false)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", so.Order.ID).Msg("Counter change ticket not printed")
			return
		}
		s.printEvents(label(so)+" change (counter)", out)
	})
	s.deferDeliveryUpdate(ctx, so) // the driver's copies: one UPDATED pair, after the charge (delivery_update.go)
}

// CounterOrderChanged tells main (floor sync) and the tablets about a counter
// order change; prev is the order before it (a pool / run it left).
func (s *Service) CounterOrderChanged(ctx context.Context, so order.StoredOrder, prev *order.StoredOrder, driverReturned bool) {
	s.floorChanged()
	s.publishUpdated(so)
	if prev == nil {
		return
	}
	if prev.Status.PoolDriverID != "" {
		s.publishAll(s.resequencePools(ctx, nil))
	}
	if prev.Status.PoolDriverID != "" || prev.Status.DriverID != "" || driverReturned {
		s.publishDrivers(ctx)
	}
}

// JournalWritten wakes the floor sync (a journal.entries event is queued).
func (s *Service) JournalWritten() { s.floorChanged() }

// PrepMinutesFor is the default prep time for a fulfillment type (the
// store's default pickup / delivery minutes).
func (s *Service) PrepMinutesFor(fulfillment string) int64 {
	return s.IntegrationSettings().Times.For(fulfillment)
}

// StoreIDs are the tenant + location this Pi is linked to.
func (s *Service) StoreIDs() (tenantID, locationID string) { return s.currentStoreIDs() }

// StoreLocation is the store's time zone.
func (s *Service) StoreLocation() *time.Location { return s.tz() }

// DevEvent notes a terminal event in the developer-mode ring (no PII).
func (s *Service) DevEvent(text string) { s.devEvent(devlog.Info, devlog.KindOrder, "%s", text) }

// SetReadyAt moves an accepted / ready order's ready time (a terminal's
// pickup ready time / delivery time change, contract 2.10v): main gets the
// order.status event, the tablets order.updated. prev = the time before.
func (s *Service) SetReadyAt(ctx context.Context, id string, at time.Time) (*order.StoredOrder, *time.Time, error) {
	ctx, span := s.span(ctx, "order.set_ready_at")
	defer span.End()
	so, prev, err := s.repo.SetReadyAt(ctx, id, at.UTC(), time.Now())
	if err != nil {
		return nil, nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*so)
	return so, prev, nil
}
