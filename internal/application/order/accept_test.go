package order

import (
	"context"
	"errors"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
)

func TestAccept_NothingPrintedRevertsClaim(t *testing.T) {
	svc, repo, _, _ := newTestService(t, false)
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{Failed: []printer.TicketResult{{Kind: printer.TicketFull, Station: "Front", Err: "down"}}}, errors.New("nothing printed")
	}
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	if _, err := svc.Accept(context.Background(), uuid.MustParse(s.ID), nil); err == nil {
		t.Fatal("accept should fail when nothing printed")
	}
	if repo.orders[s.ID].Status.State != order.StateArrived {
		t.Fatalf("claim not reverted: %s", repo.orders[s.ID].Status.State)
	}
	// …so it can be accepted again once a printer is back.
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketFull}}}, nil
	}
	if _, err := svc.Accept(context.Background(), uuid.MustParse(s.ID), nil); err != nil {
		t.Fatalf("re-accept: %v", err)
	}
}

func TestAccept_PartialPrintStaysAccepted(t *testing.T) {
	svc, repo, _, _ := newTestService(t, false)
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		return printer.Outcome{
			Printed: []printer.TicketResult{{Kind: printer.TicketStation, Station: "Pizza", PrintedOn: "Pizza"}},
			Failed:  []printer.TicketResult{{Kind: printer.TicketReceipt, Station: "Front", Err: "down"}},
		}, nil
	}
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	if _, err := svc.Accept(context.Background(), uuid.MustParse(s.ID), nil); err != nil {
		t.Fatal(err)
	}
	if repo.orders[s.ID].Status.State != order.StateAccepted {
		t.Fatal("a partially printed order must stay accepted")
	}
}

func TestAccept_DoubleAcceptLoses(t *testing.T) {
	svc, _, prints, _ := newTestService(t, false)
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	if _, err := svc.Accept(context.Background(), uuid.MustParse(s.ID), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Accept(context.Background(), uuid.MustParse(s.ID), nil); !errors.Is(err, order.ErrAlreadyAccepted) {
		t.Fatalf("second accept: %v", err)
	}
	if *prints != 1 {
		t.Fatalf("printed %d times", *prints)
	}
}

type recordingRepo struct {
	*fakeRepo
	recordedCtxErr error
	recorded       bool
}

func (r *recordingRepo) RecordAccepted(ctx context.Context, _ order.StoredOrder, _ bool) error {
	r.recorded, r.recordedCtxErr = true, ctx.Err()
	return nil
}

// The tablet gave up (request cancelled) while the tickets printed: the
// accepted floor event must still be written so main learns of it.
func TestAcceptRecordsEventEvenIfRequestCancelled(t *testing.T) {
	svc, base, _, _ := newTestService(t, false)
	repo := &recordingRepo{fakeRepo: base}
	svc.repo = repo
	ctx, cancel := context.WithCancel(context.Background())
	svc.print = func(context.Context, order.Snapshot, printer.Mode) (printer.Outcome, error) {
		cancel() // client disconnects mid-print
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketFull}}}, nil
	}
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), nil); err != nil {
		t.Fatal(err)
	}
	if !repo.recorded || repo.recordedCtxErr != nil {
		t.Fatalf("accepted event: recorded=%v ctxErr=%v", repo.recorded, repo.recordedCtxErr)
	}
}

// main redelivering an order the agent already holds (SSE reconnect) must not
// re-announce it as new or re-accept it.
func TestRedeliveryIsIgnored(t *testing.T) {
	svc, repo, prints, events := newTestService(t, true) // auto-accept on
	s := snap()
	if err := svc.OnArrival(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if *prints != 1 {
		t.Fatalf("first arrival auto-accepts once, prints=%d", *prints)
	}
	for len(events) > 0 {
		<-events
	}
	if err := svc.OnArrival(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if *prints != 1 || gotEvent(events) || repo.orders[s.ID].Status.State != order.StateAccepted {
		t.Fatalf("redelivery must be a no-op: prints=%d state=%s", *prints, repo.orders[s.ID].Status.State)
	}
}
