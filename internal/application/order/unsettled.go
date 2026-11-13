package order

import (
	"context"
	"fmt"
	"sync/atomic"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/domain/terminal"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
)

// The Unsettled Check List at an auto-close (contract/agent.md §2.10s §4,
// the owner 2026-11-12): the auto-close is never blocked; it prints the list
// of checks still owing money on the driver reports' printer (the receipt
// printer, with failover) and pages the owner.

// UnsettledBuilder renders the UNSETTLED CHECKS slip (receipt.BuildUnsettledList).
type UnsettledBuilder = func(l order.UnsettledList, on printer.Paper) []byte

var unsettledBuilder atomic.Pointer[UnsettledBuilder]

// SetUnsettledReport wires the slip (cmd/main.go).
func (s *Service) SetUnsettledReport(build UnsettledBuilder) { unsettledBuilder.Store(&build) }

// printUnsettled prints the list (nothing when empty or not wired); the
// answer is a line for the owner's page.
func (s *Service) printUnsettled(ctx context.Context, l *order.UnsettledList) string {
	if l == nil || l.Count == 0 {
		return ""
	}
	line := fmt.Sprintf("%d unsettled check(s), %s due, kept open", l.Count, cents2(l.DueCents))
	b := unsettledBuilder.Load()
	if s.printDoc == nil || b == nil || *b == nil {
		return line + " (list not printed: no printer)"
	}
	list := *l
	res, err := s.printDoc(ctx, "unsettled-checks", func(on printer.Paper) []byte { return (*b)(list, on) })
	if err != nil || res.PrintedOn == "" {
		s.logger.Warn().Ctx(ctx).Err(err).Int64("batch", l.BatchID).Int("checks", l.Count).Msg("Unsettled Check List not printed")
		return line + " (the list did NOT print)"
	}
	s.logger.Info().Ctx(ctx).Int64("batch", l.BatchID).Int("checks", l.Count).Int64("due_cents", l.DueCents).
		Msg("Unsettled Check List printed")
	return line + " (list printed)"
}

// UnpaidBuilder renders the UNPAID AT CLOSE slip (receipt.BuildUnpaidAtClose).
type UnpaidBuilder = func(l order.UnpaidAtClose, on printer.Paper) []byte

var unpaidBuilder atomic.Pointer[UnpaidBuilder]

// SetUnpaidAtCloseReport wires the slip (cmd/main.go).
func (s *Service) SetUnpaidAtCloseReport(build UnpaidBuilder) { unpaidBuilder.Store(&build) }

// unpaidWrittenOff follows a close that wrote unpaid counter checks off as
// waste (the owner, 2026-11-13): the tablets and terminals hear each one
// (order.updated + terminal.order.voided), and the Pi prints the Unpaid at
// close list on the driver reports' printer unless the closing terminal
// prints it itself (PrintedBy "terminal"). The auto-close prints and pages
// on its own (printUnpaid).
func (s *Service) unpaidWrittenOff(ctx context.Context, l *order.UnpaidAtClose) {
	if l == nil || l.Count == 0 {
		return
	}
	for _, c := range l.Checks {
		uid, err := uuid.Parse(c.OrderID)
		if err != nil {
			continue
		}
		if so, err := s.repo.GetByID(ctx, uid); err == nil && so != nil {
			s.publishUpdated(*so)
			if s.broker != nil {
				s.broker.Publish(terminal.EventOrderVoided, map[string]any{"order": *so})
			}
		}
	}
	s.logger.Info().Ctx(ctx).Int64("batch", l.BatchID).Int("checks", l.Count).Int64("total_cents", l.TotalCents).
		Str("trigger", l.ClosedBy.Trigger).Str("staff_id", l.ClosedBy.StaffID).Msg("Unpaid counter checks written off as waste at close")
	s.devEvent(devlog.Info, devlog.KindOrder, "%d unpaid counter check(s) written off at close (%s)", l.Count, cents2(l.TotalCents))
	if l.Auto || l.PrintedBy != "pi" {
		return
	}
	list := *l
	safego.Go("order.unpaid_at_close", func() { s.printUnpaid(context.WithoutCancel(ctx), &list) })
}

// printUnpaid prints the Unpaid at close list (nothing when empty or not
// wired); the answer is a line for the owner's page.
func (s *Service) printUnpaid(ctx context.Context, l *order.UnpaidAtClose) string {
	if l == nil || l.Count == 0 {
		return ""
	}
	line := fmt.Sprintf("%d unpaid counter check(s) written off as waste (%s)", l.Count, cents2(l.TotalCents))
	b := unpaidBuilder.Load()
	if s.printDoc == nil || b == nil || *b == nil {
		return line + " (list not printed: no printer)"
	}
	list := *l
	res, err := s.printDoc(ctx, "unpaid-at-close", func(on printer.Paper) []byte { return (*b)(list, on) })
	if err != nil || res.PrintedOn == "" {
		s.logger.Warn().Ctx(ctx).Err(err).Int64("batch", l.BatchID).Int("checks", l.Count).Msg("Unpaid at close list not printed")
		s.alertFloor(ctx, fmt.Sprintf("Batch %d: the Unpaid at close list (%s) did not print", l.BatchID, line))
		return line + " (the list did NOT print)"
	}
	s.logger.Info().Ctx(ctx).Int64("batch", l.BatchID).Int("checks", l.Count).Int64("total_cents", l.TotalCents).
		Msg("Unpaid at close list printed")
	return line + " (Unpaid at close list printed)"
}
