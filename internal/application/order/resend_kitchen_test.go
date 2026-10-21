package order

import (
	"context"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
)

func TestReprintAndResendKitchenPrintTheirMode(t *testing.T) {
	svc, repo, _, _ := newTestService(t, false)
	var modes []printer.Mode
	svc.print = func(_ context.Context, _ order.Snapshot, m printer.Mode) (printer.Outcome, error) {
		modes = append(modes, m)
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketStation, Station: "Pizza", PrintedOn: "Pizza"}}}, nil
	}
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	id := uuid.MustParse(s.ID)

	// Still waiting: not accepted.
	if _, _, err := svc.ResendKitchen(context.Background(), id); !errors.Is(err, ErrNotAccepted) {
		t.Fatalf("waiting order: want ErrNotAccepted, got %v", err)
	}
	if _, err := svc.Accept(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	kn := int64(1100)
	repo.orders[s.ID].Status.KitchenNumber = &kn

	if _, err := svc.Reprint(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	so, out, err := svc.ResendKitchen(context.Background(), id)
	if err != nil || so == nil || len(out.Printed) != 1 {
		t.Fatalf("resend kitchen: so=%v out=%+v err=%v", so, out, err)
	}
	want := []printer.Mode{printer.ModeAccept, printer.ModeReceipt, printer.ModeKitchen}
	if len(modes) != 3 || modes[0] != want[0] || modes[1] != want[1] || modes[2] != want[2] {
		t.Fatalf("modes = %v", modes)
	}

	// Nothing for the kitchen / nothing printed.
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{}, printer.ErrNoTickets
	}
	if _, _, err := svc.ResendKitchen(context.Background(), id); !errors.Is(err, ErrNothingForKitchen) {
		t.Fatalf("want ErrNothingForKitchen, got %v", err)
	}
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{Failed: []printer.TicketResult{{Kind: printer.TicketStation, Station: "Pizza", Err: "down"}}}, errors.New("down")
	}
	if _, out, err := svc.ResendKitchen(context.Background(), id); !errors.Is(err, ErrKitchenNotPrinted) || len(out.Failed) != 1 {
		t.Fatalf("want ErrKitchenNotPrinted, got %v (%+v)", err, out)
	}
	if _, _, err := svc.ResendKitchen(context.Background(), uuid.New()); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}
