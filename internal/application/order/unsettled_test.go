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
	svc.SetDocumentPrinting(func(_ context.Context, label string, build func(string) []byte) (printer.TicketResult, error) {
		labels = append(labels, label)
		build("")
		return printer.TicketResult{PrintedOn: "Receipt"}, nil
	}, func(order.DriverReport, string) []byte { return nil })
	svc.SetUnsettledReport(func(l order.UnsettledList, _ string) []byte { built = l; return []byte("x") })
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
