package order

import (
	"context"
	"errors"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// Terminal stage 0d: what the terminal service (application/terminal) needs
// from the floor - printing a counter order's station tickets, the VOID slip,
// telling the tablets and main about a change, the default times.

// PrintCounterTickets prints a new counter order's station tickets in the
// background (the Resend-to-kitchen plan: stations only, never the receipt,
// which prints on the terminal's own printer). Nothing printed only alerts.
func (s *Service) PrintCounterTickets(ctx context.Context, so order.StoredOrder) {
	s.background(ctx, func(ctx context.Context) {
		start := time.Now()
		out, err := s.print(ctx, printable(so), printer.ModeKitchen)
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

// PrintVoidSlip prints the VOID slip of a voided order on the stations its
// tickets went to (background).
func (s *Service) PrintVoidSlip(ctx context.Context, so order.StoredOrder) {
	if printedTickets(so) {
		s.printCancelVoid(ctx, so)
	}
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
