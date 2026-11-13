package order

import (
	"context"
	"fmt"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
)

// Batch auto-close (contract/agent.md §2.10c): if nobody pressed Close batch,
// the Pi closes the open batch at the store's auto_close_batch_at (main's
// terminal settings, "HH:MM" store time, default 04:00, "" = off) exactly as
// Close batch does (force: leftovers auto-finished; holds charged by main,
// driver reports printed, numbering reset), logs it and tells the owner.

const (
	// AutoCloseWindow: a slot is only acted on this long after its time (a
	// Pi that was off at 4:00 and boots at 4:30 still closes; one that boots
	// mid-service never does).
	AutoCloseWindow = 2 * time.Hour
	// AutoCloseMinAge: a batch opened less than this long ago is never
	// auto-closed (someone just closed one by hand).
	AutoCloseMinAge = time.Hour
)

// AutoCloseOutcome is what one check did.
type AutoCloseOutcome string

const (
	AutoCloseOff    AutoCloseOutcome = "off"     // no setting / bad setting
	AutoCloseNotDue AutoCloseOutcome = "not_due" // this slot is handled already
	AutoCloseMissed AutoCloseOutcome = "missed"  // past the window (Pi was off): skipped for the day
	AutoCloseRecent AutoCloseOutcome = "recent"  // the batch opened < 1 h ago: skipped for the day
	AutoCloseEmpty  AutoCloseOutcome = "empty"   // nothing in the batch: skipped for the day
	AutoCloseClosed AutoCloseOutcome = "closed"  // closed now
	AutoCloseFailed AutoCloseOutcome = "failed"  // Close batch failed: tried again next tick
)

// AutoCloseResult is one check's outcome (Orders: the batch's orders when it
// was closed).
type AutoCloseResult struct {
	Outcome AutoCloseOutcome
	Slot    time.Time
	BatchID int64
	Orders  int64
}

// ParseHHMM reads an "HH:MM" (24 h) store time.
func ParseHHMM(v string) (time.Time, bool) {
	t, err := time.Parse("15:04", v)
	return t, err == nil && len(v) == 5
}

// AutoCloseDue runs the auto-close check for now (loc = the store's
// timezone, at = the setting, "" = off). Called on a ticker; restart-safe:
// the slot handled is stored, and a batch opened in the last hour is never
// closed, so a slot is acted on at most once.
func (s *Service) AutoCloseDue(ctx context.Context, now time.Time, loc *time.Location, at string) (AutoCloseResult, error) {
	if at == "" {
		return AutoCloseResult{Outcome: AutoCloseOff}, nil
	}
	hm, ok := ParseHHMM(at)
	if !ok {
		return AutoCloseResult{Outcome: AutoCloseOff}, nil
	}
	slot := lastScheduledReset(now, hm, loc)
	res := AutoCloseResult{Slot: slot}
	last, err := s.repo.AutoCloseLast(ctx)
	if err != nil {
		return res, err
	}
	if !last.Before(slot) {
		res.Outcome = AutoCloseNotDue
		return res, nil
	}
	skip := func(o AutoCloseOutcome, why string) (AutoCloseResult, error) {
		res.Outcome = o
		s.logger.Info().Ctx(ctx).Str("at", at).Time("slot", slot).Str("outcome", string(o)).Msg("Batch auto-close skipped: " + why)
		return res, s.repo.SetAutoCloseLast(ctx, slot)
	}
	if now.Sub(slot) > AutoCloseWindow {
		// The slot was over 2 h ago when the Pi first looked at it: it was off
		// then, or had no terminal settings yet (Setting "" = off, the slot
		// not stored), or this is its first check after a start. Never a
		// close mid-service; the next slot closes as usual.
		return skip(AutoCloseMissed, "the slot passed more than 2 h before the Pi checked it (off, or no terminal settings yet)")
	}
	b, err := s.repo.OpenBatch(ctx)
	if err != nil {
		return res, err
	}
	res.BatchID = b.ID
	if now.Sub(b.OpenedAt) < AutoCloseMinAge {
		return skip(AutoCloseRecent, "the batch was opened less than an hour ago")
	}
	total, _, err := s.repo.CountOpenBatchOrders(ctx)
	if err != nil {
		return res, err
	}
	res.Orders = total
	if total == 0 {
		return skip(AutoCloseEmpty, "the batch has no orders")
	}
	closed, finished, err := s.CloseBatch(order.WithCloseKind(ctx, order.CloseAuto), true)
	if err != nil {
		res.Outcome = AutoCloseFailed
		s.logger.Error().Ctx(ctx).Err(err).Int64("batch", b.ID).Msg("Batch auto-close failed")
		return res, err
	}
	res.Outcome, res.BatchID = AutoCloseClosed, closed.ID
	if err := s.repo.SetAutoCloseLast(ctx, slot); err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("auto-close: saving the slot failed (the 1 h rule still stops a second close)")
	}
	msg := fmt.Sprintf("Batch auto-closed at %s: %d orders", at, total)
	// contract 2.10s §4: never blocked. Unpaid counter checks follow the
	// store's close_unpaid_counter_action (the owner, 2026-11-13; before,
	// the auto-close always carried them): waste = written off, the Unpaid
	// at close list prints; the Unsettled Check List (what was carried)
	// prints too. Both on the driver reports' printer and the owner's page.
	if line := s.printUnpaid(ctx, closed.UnpaidAtClose); line != "" {
		msg += ". " + line
	}
	if line := s.printUnsettled(ctx, closed.Unsettled); line != "" {
		msg += ". " + line
	}
	s.logger.Info().Ctx(ctx).Int64("batch", closed.ID).Int64("orders", total).Int("auto_finished", len(finished)).
		Str("at", at).Msg("Batch auto-closed")
	s.devEvent(devlog.Info, devlog.KindOrder, "%s", msg)
	if s.notifier != nil {
		s.background(ctx, func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := s.notifier.Send(ctx, msg, "classical"); err != nil {
				s.logger.Warn().Ctx(ctx).Err(err).Msg("auto-close Pushover failed (non-fatal)")
			}
		})
	}
	return res, nil
}
