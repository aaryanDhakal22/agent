package order

import (
	"context"
	"fmt"
	"sync/atomic"

	"quiccpos/agent/internal/domain/order"
)

// The Unsettled Check List at an auto-close (contract/agent.md §2.10s §4,
// the owner 2026-11-12): the auto-close is never blocked; it prints the list
// of checks still owing money on the driver reports' printer (the receipt
// printer, with failover) and pages the owner.

// UnsettledBuilder renders the UNSETTLED CHECKS slip (receipt.BuildUnsettledList).
type UnsettledBuilder = func(l order.UnsettledList, reroutedFrom string) []byte

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
	res, err := s.printDoc(ctx, "unsettled-checks", func(from string) []byte { return (*b)(list, from) })
	if err != nil || res.PrintedOn == "" {
		s.logger.Warn().Ctx(ctx).Err(err).Int64("batch", l.BatchID).Int("checks", l.Count).Msg("Unsettled Check List not printed")
		return line + " (the list did NOT print)"
	}
	s.logger.Info().Ctx(ctx).Int64("batch", l.BatchID).Int("checks", l.Count).Int64("due_cents", l.DueCents).
		Msg("Unsettled Check List printed")
	return line + " (list printed)"
}
