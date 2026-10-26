package order

import (
	"fmt"
	"strings"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// Developer-mode events (GET /api/dev/events): order ids, kitchen numbers,
// printer names and error codes only — never a customer's name, phone or
// address.

// SetDevLog wires the developer-mode event ring.
func (s *Service) SetDevLog(r *devlog.Ring) { s.dev.Store(r) }

func (s *Service) devEvent(level, kind, format string, args ...any) {
	if r := s.dev.Load(); r != nil {
		r.Add(level, kind, fmt.Sprintf(format, args...))
	}
}

// label names an order in events and PII-free log lines: its kitchen (or
// POS check) number, else the start of its id.
func label(so order.StoredOrder) string {
	if n := so.Number(); n != nil {
		return fmt.Sprintf("#%d", *n)
	}
	return "order " + devlog.Short(so.Order.ID)
}

// snapLabel is label for a bare snapshot (a reprint from main's history).
func snapLabel(snap order.Snapshot) string {
	if snap.KitchenNumber != nil {
		return fmt.Sprintf("#%d", *snap.KitchenNumber)
	}
	return "order " + devlog.Short(snap.ID)
}

func labels(list []order.StoredOrder) string {
	parts := make([]string, 0, len(list))
	for _, so := range list {
		parts = append(parts, label(so))
	}
	return strings.Join(parts, ", ")
}

// printEvents records where an order's tickets went.
func (s *Service) printEvents(what string, out printer.Outcome) {
	var on []string
	seen := map[string]bool{}
	for _, p := range out.Printed {
		if !seen[p.PrintedOn] {
			seen[p.PrintedOn] = true
			on = append(on, p.PrintedOn)
		}
	}
	if len(on) > 0 {
		s.devEvent(devlog.Info, devlog.KindPrint, "%s printed on %s", what, strings.Join(on, ", "))
	}
}
