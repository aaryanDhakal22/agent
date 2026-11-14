package order

import (
	"context"
	"fmt"
	"strings"
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

// UnpaidBuilder renders the UNPAID AT CLOSE slip (receipt.BuildUnpaidAtClose)
// and, wired by SetNotPickedUpReport, the NOT PICKED UP slip
// (receipt.BuildNotPickedUp): the same report shape.
type UnpaidBuilder = func(l order.UnpaidAtClose, on printer.Paper) []byte

var unpaidBuilder, notPickedUpBuilder atomic.Pointer[UnpaidBuilder]

// SetUnpaidAtCloseReport wires the slip (cmd/main.go).
func (s *Service) SetUnpaidAtCloseReport(build UnpaidBuilder) { unpaidBuilder.Store(&build) }

// SetNotPickedUpReport wires the NOT PICKED UP slip (cmd/main.go; the
// owner, 2026-11-14).
func (s *Service) SetNotPickedUpReport(build UnpaidBuilder) { notPickedUpBuilder.Store(&build) }

// writeOff names one of a close's write-off reports: the Unpaid at close
// list (unpaid counter checks) or the Not picked up slip (unpaid pickups).
type writeOff struct {
	doc     string // the print job's name
	what    string // "unpaid counter check(s)"
	list    string // the list's name in logs and on the owner's page
	builder *atomic.Pointer[UnpaidBuilder]
}

var (
	unpaidWriteOff  = writeOff{doc: "unpaid-at-close", what: "unpaid counter check(s)", list: "Unpaid at close list", builder: &unpaidBuilder}
	notPickedUpSlip = writeOff{doc: "not-picked-up", what: "unpaid pickup(s) not picked up", list: "Not picked up slip", builder: &notPickedUpBuilder}
)

// unpaidWrittenOff follows a close that wrote unpaid counter checks off as
// waste (the owner, 2026-11-13): the tablets and terminals hear each one
// (order.updated + terminal.order.voided), and the Pi prints the Unpaid at
// close list on the driver reports' printer unless the closing terminal
// prints it itself (PrintedBy "terminal"). The auto-close prints and pages
// on its own (printUnpaid).
func (s *Service) unpaidWrittenOff(ctx context.Context, l *order.UnpaidAtClose) {
	s.writtenOff(ctx, l, unpaidWriteOff)
}

// notPickedUpWrittenOff is unpaidWrittenOff for the unpaid pickups the close
// wrote off as waste "Not picked up" (the owner, 2026-11-14): the NOT PICKED
// UP slip prints the same way (the Pi, unless the closing terminal asked to
// print it).
func (s *Service) notPickedUpWrittenOff(ctx context.Context, l *order.UnpaidAtClose) {
	s.writtenOff(ctx, l, notPickedUpSlip)
}

func (s *Service) writtenOff(ctx context.Context, l *order.UnpaidAtClose, kind writeOff) {
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
		Str("reason", l.Reason).Str("trigger", l.ClosedBy.Trigger).Str("staff_id", l.ClosedBy.StaffID).
		Msg("Unpaid checks written off as waste at close")
	s.devEvent(devlog.Info, devlog.KindOrder, "%d %s written off at close (%s)", l.Count, kind.what, cents2(l.TotalCents))
	if l.Auto || l.PrintedBy != "pi" {
		return
	}
	list := *l
	safego.Go("order."+strings.ReplaceAll(kind.doc, "-", "_"), func() { s.printWriteOff(context.WithoutCancel(ctx), &list, kind) })
}

// printUnpaid prints the Unpaid at close list (nothing when empty or not
// wired); the answer is a line for the owner's page.
func (s *Service) printUnpaid(ctx context.Context, l *order.UnpaidAtClose) string {
	return s.printWriteOff(ctx, l, unpaidWriteOff)
}

// printNotPickedUp prints the NOT PICKED UP slip (the auto-close's).
func (s *Service) printNotPickedUp(ctx context.Context, l *order.UnpaidAtClose) string {
	return s.printWriteOff(ctx, l, notPickedUpSlip)
}

func (s *Service) printWriteOff(ctx context.Context, l *order.UnpaidAtClose, kind writeOff) string {
	if l == nil || l.Count == 0 {
		return ""
	}
	line := fmt.Sprintf("%d %s written off as waste (%s)", l.Count, kind.what, cents2(l.TotalCents))
	b := kind.builder.Load()
	if s.printDoc == nil || b == nil || *b == nil {
		return line + " (list not printed: no printer)"
	}
	list := *l
	res, err := s.printDoc(ctx, kind.doc, func(on printer.Paper) []byte { return (*b)(list, on) })
	if err != nil || res.PrintedOn == "" {
		s.logger.Warn().Ctx(ctx).Err(err).Int64("batch", l.BatchID).Int("checks", l.Count).Msg(kind.list + " not printed")
		s.alertFloor(ctx, fmt.Sprintf("Batch %d: the %s (%s) did not print", l.BatchID, kind.list, line))
		return line + " (the list did NOT print)"
	}
	s.logger.Info().Ctx(ctx).Int64("batch", l.BatchID).Int("checks", l.Count).Int64("total_cents", l.TotalCents).
		Msg(kind.list + " printed")
	return line + " (" + kind.list + " printed)"
}
