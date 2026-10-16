package order

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/ssebroker"
	"quiccpos/agent/internal/observability"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// fakeRepo is an in-memory order.Repository with the SQLite one's claim/revert
// semantics.
type fakeRepo struct {
	order.Repository // floor methods unused by these tests
	orders           map[string]*order.StoredOrder
	upserted         int
}

func (f *fakeRepo) UpsertArrived(_ context.Context, s order.Snapshot) (bool, error) {
	if _, ok := f.orders[s.ID]; ok {
		return false, nil // insert-only, like ON CONFLICT DO NOTHING
	}
	f.upserted++
	f.orders[s.ID] = &order.StoredOrder{Order: s, Status: order.Status{State: order.StateArrived}}
	return true, nil
}
func (f *fakeRepo) GetByID(_ context.Context, id uuid.UUID) (*order.StoredOrder, error) {
	so, ok := f.orders[id.String()]
	if !ok {
		return nil, order.ErrNotFound
	}
	cp := *so
	return &cp, nil
}
func (f *fakeRepo) ListPage(context.Context, int, int) ([]order.StoredOrder, error) { return nil, nil }
func (f *fakeRepo) ListArrived(context.Context) ([]order.StoredOrder, error)        { return nil, nil }
func (f *fakeRepo) TrimToNewest(context.Context, int) (int64, error)                { return 0, nil }
func (f *fakeRepo) ClaimAccept(_ context.Context, id uuid.UUID, _ int) (*order.StoredOrder, error) {
	so, ok := f.orders[id.String()]
	if !ok {
		return nil, order.ErrNotFound
	}
	if so.Status.State != order.StateArrived {
		return nil, order.ErrAlreadyAccepted
	}
	so.Status.State = order.StateAccepted
	cp := *so
	return &cp, nil
}
func (f *fakeRepo) RecordAccepted(context.Context, order.StoredOrder) error { return nil }
func (f *fakeRepo) FloorSettings(context.Context) (order.FloorSettings, error) {
	return order.FloorSettings{KitchenNext: 1100, LastPrepMinutes: 20}, nil
}
func (f *fakeRepo) RevertAccept(_ context.Context, id uuid.UUID) error {
	f.orders[id.String()].Status.State = order.StateArrived
	return nil
}

type fakeSettings struct {
	order.SettingsRepository
	autoAccept bool
}

func (f fakeSettings) GetAutoAccept(context.Context) (bool, error) { return f.autoAccept, nil }

func newTestService(t *testing.T, autoAccept bool) (*Service, *fakeRepo, *int, chan ssebroker.Event) {
	t.Helper()
	meters, err := observability.NewMeters()
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepo{orders: map[string]*order.StoredOrder{}}
	prints := 0
	print := func(context.Context, order.Snapshot) (printer.Outcome, error) {
		prints++
		return printer.Outcome{Printed: []printer.TicketResult{{Kind: printer.TicketFull}}}, nil
	}
	broker := ssebroker.New()
	events, _ := broker.Subscribe()
	svc := NewService(repo, fakeSettings{autoAccept: autoAccept}, print, nil, broker, zerolog.Nop(), meters)
	return svc, repo, &prints, events
}

func snap() order.Snapshot {
	return order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(),
		Source: "online", SourceEventID: "1", PlacedAt: time.Now()}
}

func gotEvent(ch chan ssebroker.Event) bool {
	select {
	case <-ch:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

func TestResend_UnknownOrderRunsFullArrival(t *testing.T) {
	svc, repo, prints, events := newTestService(t, false)
	if err := svc.Resend(context.Background(), snap()); err != nil {
		t.Fatal(err)
	}
	if repo.upserted != 1 || *prints != 0 || !gotEvent(events) {
		t.Fatalf("want stored + announced (no print, manual accept): upserted=%d prints=%d", repo.upserted, *prints)
	}
}

func TestResend_ArrivedOrderIsReannounced(t *testing.T) {
	svc, repo, prints, events := newTestService(t, false)
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	gotEvent(events) // drain the original announcement
	if err := svc.ResendByID(context.Background(), uuid.MustParse(s.ID)); err != nil {
		t.Fatal(err)
	}
	if !gotEvent(events) || *prints != 0 || repo.orders[s.ID].Status.State != order.StateArrived {
		t.Fatalf("arrived order should be re-announced, not printed (prints=%d)", *prints)
	}
}

func TestResend_AcceptedOrderIsReprinted(t *testing.T) {
	svc, _, prints, _ := newTestService(t, false)
	s := snap()
	_ = svc.OnArrival(context.Background(), s)
	if _, err := svc.Accept(context.Background(), uuid.MustParse(s.ID), nil); err != nil {
		t.Fatal(err)
	}
	before := *prints
	// Resend from the History tab passes the snapshot fetched from main.
	if err := svc.Resend(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if *prints != before+1 {
		t.Fatalf("accepted order should be reprinted: prints %d → %d", before, *prints)
	}
}

func TestResend_ArrivedWithAutoAcceptPrints(t *testing.T) {
	svc, repo, prints, _ := newTestService(t, false)
	s := snap()
	_ = svc.OnArrival(context.Background(), s) // auto-accept off at arrival
	svc.settings = fakeSettings{autoAccept: true}
	if err := svc.ResendByID(context.Background(), uuid.MustParse(s.ID)); err != nil {
		t.Fatal(err)
	}
	if *prints != 1 || repo.orders[s.ID].Status.State != order.StateAccepted {
		t.Fatalf("auto-accept resend should accept + print once (prints=%d)", *prints)
	}
}
