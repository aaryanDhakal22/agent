package order

import (
	"context"
	"errors"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
)

// What each print job of an order actually printed goes to main as the
// floor event order.printed (contract §2.10ag, the owner 2026-11-13: main's
// order panel shows "Pizza ✓ · Receipt → rerouted to Online").

// printRecorder is the repository's order.printed writer (a test fake
// without it records nothing).
type printRecorder interface {
	RecordOrderPrinted(ctx context.Context, ev order.OrderPrintedEvent) error
}

// printJob prints an order's tickets for a mode (s.print) and records what
// printed (order.printed). Every order print goes through it.
func (s *Service) printJob(ctx context.Context, snap order.Snapshot, mode printer.Mode) (printer.Outcome, error) {
	return s.printJobThen(ctx, snap, mode, nil)
}

// printJobThen is printJob with settle run the moment the paper is out,
// before order.printed is written: a job a restart would print again (a
// counter order's kitchen_pending, an online order's accept claim) settles
// its marker first, so a power cut after the print finds it settled and
// never prints the tickets twice (the window is the marker's one write, not
// the record's too; agent 2.5.5, contract §2.10ag).
func (s *Service) printJobThen(ctx context.Context, snap order.Snapshot, mode printer.Mode,
	settle func(ctx context.Context, out printer.Outcome, err error)) (printer.Outcome, error) {
	out, err := s.print(ctx, snap, mode)
	if settle != nil {
		settle(ctx, out, err)
	}
	s.recordPrinted(ctx, snap.ID, mode, out, err)
	return out, err
}

// recordPrinted writes the job's order.printed event and wakes the floor
// sync. Nothing planned (ErrNoTickets) records nothing; no printer at all
// records one skipped ticket. A failed write only logs: the print happened.
func (s *Service) recordPrinted(ctx context.Context, orderID string, mode printer.Mode, out printer.Outcome, err error) {
	rec, ok := s.repo.(printRecorder)
	if !ok || orderID == "" || errors.Is(err, printer.ErrNoTickets) {
		return
	}
	ev := PrintedEvent(orderID, mode, out, err, time.Now())
	if len(ev.Tickets) == 0 {
		return
	}
	if werr := rec.RecordOrderPrinted(context.WithoutCancel(ctx), ev); werr != nil {
		s.logger.Warn().Ctx(ctx).Err(werr).Str("order_id", orderID).Str("mode", string(mode)).
			Msg("order.printed not recorded (the print itself is unaffected)")
		return
	}
	s.floorChanged()
}

// PrintedEvent builds a print job's order.printed event: the printed
// tickets, then the ones that printed nowhere. err with no ticket tried (no
// printers at all) is one skipped ticket of the job's kind.
func PrintedEvent(orderID string, mode printer.Mode, out printer.Outcome, err error, now time.Time) order.OrderPrintedEvent {
	now = now.UTC().Truncate(time.Millisecond)
	ev := order.OrderPrintedEvent{EventID: uuid.NewString(), Type: order.FloorEventOrderPrinted, OrderID: orderID,
		Mode: string(mode), At: now, Tickets: []order.PrintedTicket{}}
	at := func(t time.Time) time.Time {
		if t.IsZero() {
			return now
		}
		return t.UTC().Truncate(time.Millisecond)
	}
	for _, r := range out.Printed {
		t := order.PrintedTicket{Kind: labelOf(r, mode), Printer: r.Station, Result: order.PrintPrinted, PrintedOn: r.PrintedOn,
			Attempts: max(r.Attempts, 1), At: at(r.At)}
		if r.ReroutedFrom != "" {
			t.Result, t.By = order.PrintRerouted, r.RerouteBy
		}
		ev.Tickets = append(ev.Tickets, t)
	}
	for _, r := range out.Failed {
		t := order.PrintedTicket{Kind: labelOf(r, mode), Printer: r.Station, Result: order.PrintFailed, Attempts: r.Attempts,
			Error: clip(r.Err, 300), At: at(r.At)}
		if r.Skipped {
			t.Result = order.PrintSkipped
		}
		ev.Tickets = append(ev.Tickets, t)
	}
	if err != nil && len(ev.Tickets) == 0 {
		ev.Tickets = append(ev.Tickets, order.PrintedTicket{Kind: modeLabel(mode), Result: order.PrintSkipped,
			Error: clip(err.Error(), 300), At: now})
	}
	return ev
}

// labelOf is a result's ticket kind (a result built before 2.5.5 has none).
func labelOf(r printer.TicketResult, mode printer.Mode) string {
	if r.Label != "" {
		return r.Label
	}
	if r.Kind == printer.TicketStation {
		return printer.LabelStation
	}
	return modeLabel(mode)
}

// modeLabel is the ticket kind a job of mode mostly prints.
func modeLabel(mode printer.Mode) string {
	switch mode {
	case printer.ModeReceipt:
		return printer.LabelReprint
	case printer.ModeOrderReceipt:
		return printer.LabelReceipt
	case printer.ModeVoid:
		return printer.LabelVoid
	case printer.ModeChange, printer.ModeTypeChange:
		return printer.LabelChange
	case printer.ModeAddOn:
		return printer.LabelAddOn
	case printer.ModeDeliveryUpdate:
		return printer.LabelDeliveryCopies
	}
	return printer.LabelStation
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
