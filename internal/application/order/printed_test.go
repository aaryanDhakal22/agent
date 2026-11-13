package order

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// 2.5.5 (contract §2.10ag): every print job of an order writes an
// order.printed floor event with each ticket's printer and result.

func printedEvents(t *testing.T, repo interface {
	ListUnsyncedEvents(ctx context.Context, limit int) ([]order.FloorEvent, error)
}, orderID string) []order.OrderPrintedEvent {
	t.Helper()
	evs, err := repo.ListUnsyncedEvents(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []order.OrderPrintedEvent
	for _, e := range evs {
		if e.Type != order.FloorEventOrderPrinted || e.OrderID != orderID {
			continue
		}
		var p order.OrderPrintedEvent
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if e.Subject != orderID {
			t.Fatalf("order.printed subject %q, want the order", e.Subject)
		}
		out = append(out, p)
	}
	return out
}

func TestPrintJobRecordsOrderPrinted(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, _, _ := newIntegService(t)
	svc.legacyArrival = false
	at := time.Now()
	svc.print = func(_ context.Context, _ order.Snapshot, mode printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{
			Printed: []printer.TicketResult{
				{Kind: printer.TicketStation, Label: printer.LabelStation, Station: "Pizza", PrintedOn: "Pizza", Attempts: 1, At: at},
				{Kind: printer.TicketDeliveryCopies, Label: printer.LabelDeliveryCopies, Station: "Counter", PrintedOn: "Online",
					ReroutedFrom: "Counter", RerouteBy: "auto", Attempts: 2, At: at},
			},
			Failed: []printer.TicketResult{{Kind: printer.TicketStation, Label: printer.LabelStation, Station: "Wings", Attempts: 5,
				Err: "printer unreachable", At: at}},
		}, nil
	}
	s := onlineSnap("delivery")
	if err := svc.OnArrival(ctx, s); err != nil {
		t.Fatal(err)
	}
	svc.WaitBackground()
	got := printedEvents(t, repo, s.ID)
	if len(got) != 1 {
		t.Fatalf("order.printed events: %d, want 1", len(got))
	}
	ev := got[0]
	if ev.Mode != string(printer.ModePlaced) || ev.EventID == "" || ev.At.IsZero() || len(ev.Tickets) != 3 {
		t.Fatalf("event: %+v", ev)
	}
	want := []order.PrintedTicket{
		{Kind: "station", Printer: "Pizza", Result: "printed", PrintedOn: "Pizza", Attempts: 1},
		{Kind: "delivery_copies", Printer: "Counter", Result: "rerouted", PrintedOn: "Online", By: "auto", Attempts: 2},
		{Kind: "station", Printer: "Wings", Result: "failed", Attempts: 5, Error: "printer unreachable"},
	}
	for i, w := range want {
		g := ev.Tickets[i]
		g.At = time.Time{}
		if g != w {
			t.Fatalf("ticket %d: %+v, want %+v", i, g, w)
		}
	}

	// A reprint is a job of its own.
	if _, _, err := svc.ReprintOn(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(printedEvents(t, repo, s.ID)); n != 2 {
		t.Fatalf("after a reprint: %d order.printed events, want 2", n)
	}
}

func TestPrintedEventNoPrinterAndNothingPlanned(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 1, 0, 0, time.UTC)
	ev := PrintedEvent("o1", printer.ModeReceipt, printer.Outcome{}, errors.New("no printers configured"), now)
	if len(ev.Tickets) != 1 || ev.Tickets[0].Result != order.PrintSkipped || ev.Tickets[0].Kind != printer.LabelReprint ||
		ev.Tickets[0].Error != "no printers configured" || !ev.Tickets[0].At.Equal(now) {
		t.Fatalf("no roster: %+v", ev.Tickets)
	}
	ev = PrintedEvent("o1", printer.ModeKitchen, printer.Outcome{Failed: []printer.TicketResult{{Kind: printer.TicketStation,
		Station: "Bar", Skipped: true, Err: "printer not in roster"}}}, errors.New("nothing printed"), now)
	if len(ev.Tickets) != 1 || ev.Tickets[0].Result != order.PrintSkipped || ev.Tickets[0].Printer != "Bar" {
		t.Fatalf("not in roster: %+v", ev.Tickets)
	}

	// Nothing planned (a kitchen resend of receipt-only lines): no event.
	svc, repo, _, _, _ := newIntegService(t)
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{}, printer.ErrNoTickets
	}
	_, _ = svc.printJob(context.Background(), onlineSnap("pickup"), printer.ModeKitchen)
	evs, _ := repo.ListUnsyncedEvents(context.Background(), 1000)
	for _, e := range evs {
		if e.Type == order.FloorEventOrderPrinted {
			t.Fatalf("an empty plan recorded %+v", e)
		}
	}
}
