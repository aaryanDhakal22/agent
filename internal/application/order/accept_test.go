package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"

	"github.com/google/uuid"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// floor.accept.seconds: arrival → accept; a re-rung future order counts
// from its ring; a POS order is labelled pos.
func TestRecordAcceptTime(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	h, err := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t").Float64Histogram("floor.accept.seconds")
	if err != nil {
		t.Fatal(err)
	}
	svc, _, _, _ := newTestService(t, false)
	svc.meters.FloorAcceptSeconds = h

	now := time.Now()
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	ring := at(-30 * time.Second)
	svc.recordAcceptTime(context.Background(), order.StoredOrder{Status: order.Status{ArrivalDate: now.Add(-90 * time.Second), AcceptedAt: at(0)}})
	svc.recordAcceptTime(context.Background(), order.StoredOrder{Status: order.Status{ArrivalDate: now.Add(-3 * time.Hour), AcceptedAt: at(0), Rerung: true, RingAt: ring}})
	svc.recordAcceptTime(context.Background(), order.StoredOrder{Status: order.Status{ArrivalDate: now.Add(-10 * time.Second), AcceptedAt: at(0), ExtSource: order.ExtSourceSoftTouch}})
	svc.recordAcceptTime(context.Background(), order.StoredOrder{}) // no arrival time: skipped

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]float64{} // source → {count, sum}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, p := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				v, _ := p.Attributes.Value("source")
				got[v.AsString()] = [2]float64{float64(p.Count), p.Sum}
			}
		}
	}
	if got["online"] != [2]float64{2, 120} || got["pos"] != [2]float64{1, 10} {
		t.Fatalf("floor.accept.seconds = %v", got)
	}
}

// orders.print.seconds: one sample per print job, result ok / partial /
// failed by outcome; a plan with nothing to print records nothing.
func TestRecordPrintTime(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	h, err := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t").Float64Histogram("orders.print.seconds")
	if err != nil {
		t.Fatal(err)
	}
	svc, _, _, _ := newTestService(t, false)
	svc.meters.OrdersPrintSeconds = h

	ctx := context.Background()
	start := time.Now().Add(-2 * time.Second)
	online := order.StoredOrder{}
	pos := order.StoredOrder{Status: order.Status{ExtSource: order.ExtSourceSoftTouch}}
	one := []printer.TicketResult{{}}
	svc.recordPrintTime(ctx, online, start, printer.Outcome{Printed: one}, nil)
	svc.recordPrintTime(ctx, online, start, printer.Outcome{Printed: one, Failed: one}, nil)
	svc.recordPrintTime(ctx, online, start, printer.Outcome{Failed: one}, errors.New("all down"))
	svc.recordPrintTime(ctx, pos, start, printer.Outcome{Printed: one}, nil)
	svc.recordPrintTime(ctx, online, start, printer.Outcome{}, printer.ErrNoTickets)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]uint64{} // result/source → count
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, p := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				r, _ := p.Attributes.Value("result")
				s, _ := p.Attributes.Value("source")
				got[r.AsString()+"/"+s.AsString()] = p.Count
				if p.Sum < 2*float64(p.Count) {
					t.Fatalf("sum %v for %d samples", p.Sum, p.Count)
				}
			}
		}
	}
	want := map[string]uint64{"ok/online": 1, "partial/online": 1, "failed/online": 1, "ok/pos": 1}
	if len(got) != len(want) {
		t.Fatalf("orders.print.seconds = %v", got)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("orders.print.seconds = %v", got)
		}
	}
}
