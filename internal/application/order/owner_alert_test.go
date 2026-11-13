package order

import (
	"context"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
)

// 2.5.2: an order whose first print came out nowhere tells the owner's
// hook (once per print); a partial print or a reroute doesn't.
func TestNothingPrintedTellsTheOwnerHook(t *testing.T) {
	svc, _, _, _ := newTestService(t, false)
	var got []string
	svc.OnNothingPrinted(func(id, label string) { got = append(got, id) })
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{Failed: []printer.TicketResult{{Kind: printer.TicketFull, Station: "Front", Err: "down"}}}, errors.New("nothing printed")
	}
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	_, _ = svc.Accept(context.Background(), uuid.MustParse(s.ID), nil)
	if len(got) != 1 || got[0] != s.ID {
		t.Fatalf("hook = %v", got)
	}
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{
			Printed: []printer.TicketResult{{Kind: printer.TicketStation, Station: "Pizza", PrintedOn: "Front", ReroutedFrom: "Pizza"}},
			Failed:  []printer.TicketResult{{Kind: printer.TicketReceipt, Station: "Front", Err: "down"}},
		}, nil
	}
	if _, err := svc.Accept(context.Background(), uuid.MustParse(s.ID), nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a partial print reached the owner hook: %v", got)
	}
}
