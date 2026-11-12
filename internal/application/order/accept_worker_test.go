package order

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
)

// F-7: with the accept worker on, a print that hangs doesn't hold OnArrival:
// the next frame is stored at once, and both orders end up accepted.
func TestAutoAcceptRunsOffTheReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, repo, _ := newPoolService(t)
	release := make(chan struct{})
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		<-release
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketFull}}}, nil
	}
	if err := svc.SetAutoAccept(ctx, true); err != nil {
		t.Fatal(err)
	}
	svc.StartAcceptWorker(ctx)
	snap := func() order.Snapshot {
		return order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
			SourceEventID: "w", Fulfillment: order.Fulfillment{Type: "pickup"}}
	}
	a, b := snap(), snap()
	start := time.Now()
	if err := svc.OnArrival(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := svc.OnArrival(ctx, b); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("OnArrival waited on the print: %v", d)
	}
	if got, _ := repo.GetByID(ctx, uuid.MustParse(b.ID)); got == nil {
		t.Fatal("second order not stored while the first one printed")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ga, _ := repo.GetByID(ctx, uuid.MustParse(a.ID))
		gb, _ := repo.GetByID(ctx, uuid.MustParse(b.ID))
		if ga.Status.State == order.StateAccepted && gb.Status.State == order.StateAccepted {
			cancel()
			svc.WaitAccepts()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("orders not accepted by the worker")
}
