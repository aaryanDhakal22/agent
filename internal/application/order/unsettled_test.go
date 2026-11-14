package order

import (
	"context"
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The auto-close prints the Unsettled Check List on the driver reports'
// printer (contract 2.10s §4) and says so on the owner's page.
func TestPrintUnsettledOnTheReportPrinter(t *testing.T) {
	svc, _, _ := newPoolService(t)
	var labels []string
	var built order.UnsettledList
	svc.SetDocumentPrinting(func(_ context.Context, label string, build func(printer.Paper) []byte) (printer.TicketResult, error) {
		labels = append(labels, label)
		build(printer.Paper{})
		return printer.TicketResult{PrintedOn: "Receipt"}, nil
	}, func(order.DriverReport, printer.Paper) []byte { return nil })
	svc.SetUnsettledReport(func(l order.UnsettledList, _ printer.Paper) []byte { built = l; return []byte("x") })
	if line := svc.printUnsettled(context.Background(), &order.UnsettledList{BatchID: 3}); line != "" || len(labels) != 0 {
		t.Fatalf("an empty list prints nothing: %q %v", line, labels)
	}
	l := &order.UnsettledList{BatchID: 3, Count: 2, DueCents: 3656}
	line := svc.printUnsettled(context.Background(), l)
	if len(labels) != 1 || labels[0] != "unsettled-checks" || built.Count != 2 || !strings.Contains(line, "2 unsettled check(s), $36.56 due") ||
		!strings.Contains(line, "list printed") {
		t.Fatalf("printed %v %+v %q", labels, built, line)
	}
}

// The NOT PICKED UP slip (the owner, 2026-11-14) prints like the Unpaid at
// close list: on the driver reports' printer through its own builder, by
// the Pi unless the closing terminal prints it (PrintedBy "terminal"); the
// auto-close prints it itself (printNotPickedUp).
func TestPrintNotPickedUpSlip(t *testing.T) {
	svc, _, _ := newPoolService(t)
	labels := make(chan string, 4)
	svc.SetDocumentPrinting(func(_ context.Context, label string, build func(printer.Paper) []byte) (printer.TicketResult, error) {
		build(printer.Paper{})
		labels <- label
		return printer.TicketResult{PrintedOn: "Receipt"}, nil
	}, func(order.DriverReport, printer.Paper) []byte { return nil })
	var unpaid, picked []order.UnpaidAtClose
	svc.SetUnpaidAtCloseReport(func(l order.UnpaidAtClose, _ printer.Paper) []byte { unpaid = append(unpaid, l); return []byte("u") })
	svc.SetNotPickedUpReport(func(l order.UnpaidAtClose, _ printer.Paper) []byte { picked = append(picked, l); return []byte("n") })
	l := &order.UnpaidAtClose{BatchID: 4, Reason: order.NotPickedUpReason, Count: 2, TotalCents: 5772, PrintedBy: "pi", Auto: true}
	line := svc.printNotPickedUp(context.Background(), l)
	if got := <-labels; got != "not-picked-up" || len(picked) != 1 || len(unpaid) != 0 ||
		line != "2 unpaid pickup(s) not picked up written off as waste ($57.72) (Not picked up slip printed)" {
		t.Fatalf("auto-close print: %q %q %d %d", got, line, len(picked), len(unpaid))
	}
	// A manual close: the Pi prints it in the background...
	manual := *l
	manual.Auto = false
	svc.notPickedUpWrittenOff(context.Background(), &manual)
	if got := <-labels; got != "not-picked-up" {
		t.Fatalf("manual close print: %q", got)
	}
	// ...unless the closing terminal prints it.
	manual.PrintedBy = "terminal"
	svc.notPickedUpWrittenOff(context.Background(), &manual)
	if line := svc.printNotPickedUp(context.Background(), nil); line != "" {
		t.Fatalf("nil list: %q", line)
	}
	select {
	case got := <-labels:
		t.Fatalf("printed for a terminal that prints it: %q", got)
	default:
	}
}
