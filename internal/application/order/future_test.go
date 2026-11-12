package order

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
	"quiccpos/agent/internal/infra/database/repositories"
)

// 2.4.3: staff's actions on a waiting future order.

func futureOrder(t *testing.T, svc *Service, ftype string, in time.Duration) (order.Snapshot, time.Time) {
	t.Helper()
	promised := time.Now().Add(in).UTC().Truncate(time.Millisecond)
	s := order.Snapshot{ID: uuid.NewString(), TenantID: uuid.NewString(), LocationID: uuid.NewString(), Source: "online",
		SourceEventID: uuid.NewString(), Fulfillment: order.Fulfillment{Type: ftype, PromisedAt: &promised}}
	if ftype == "delivery" {
		s.Fulfillment.Address = &order.Address{Line1: "1 Main"}
	}
	if err := svc.OnArrival(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if !storedOf(t, svc, s.ID).Status.Future {
		t.Fatal("not future")
	}
	return s, promised
}

func statusEvents(t *testing.T, repo *repositories.OrderRepository, id string) []order.OrderStatusEvent {
	t.Helper()
	evs, err := repo.ListUnsyncedEvents(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []order.OrderStatusEvent
	for _, ev := range evs {
		if ev.Type != order.FloorEventOrderStatus || ev.OrderID != id {
			continue
		}
		var e order.OrderStatusEvent
		if err := json.Unmarshal(ev.Payload, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// MAKE NOW from scheduled (and from a first ring): accepted with the prep,
// a kitchen number, printed as a normal order (no promised_at), accepted sent
// to main; it no longer counts as future.
func TestMakeNow(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, prints, _ := newIntegService(t)
	s, _ := futureOrder(t, svc, "delivery", 23*time.Hour)
	if _, err := svc.Schedule(ctx, s.ID, 45); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MakeNow(ctx, s.ID, 0); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("prep 0 = %v", err)
	}
	so, err := svc.MakeNow(ctx, s.ID, 25)
	if err != nil {
		t.Fatal(err)
	}
	if so.Status.State != order.StateAccepted || so.Status.KitchenNumber == nil || *so.Status.PrepMinutes != 25 ||
		so.Status.Future || !so.Status.MadeNow || so.Order.Fulfillment.PromisedAt != nil {
		t.Fatalf("made now = %+v / %+v", so.Status, so.Order.Fulfillment)
	}
	p := prints.all()
	if len(p) != 1 || p[0].Mode != printer.ModeAccept || p[0].Promised != nil || p[0].Number == 0 {
		t.Fatalf("prints = %+v", p)
	}
	evs := statusEvents(t, repo, s.ID)
	if last := evs[len(evs)-1]; last.Status != order.StateAccepted {
		t.Fatalf("events = %+v", evs)
	}
	if _, err := svc.MakeNow(ctx, s.ID, 25); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("twice = %v", err)
	}

	// A future order still ringing (not confirmed yet).
	s2, _ := futureOrder(t, svc, "pickup", 5*time.Hour)
	if so, err := svc.MakeNow(ctx, s2.ID, 15); err != nil || so.Status.State != order.StateAccepted {
		t.Fatalf("make now arrived = %+v %v", so, err)
	}
	// Not a future order.
	asap := readyDelivery(t, svc, time.Minute)
	if _, err := svc.MakeNow(ctx, asap, 15); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("accepted = %v", err)
	}
}

// An early receipt already out: MAKE NOW prints the kitchen tickets only.
func TestMakeNowAfterEarlyReceipt(t *testing.T) {
	ctx := context.Background()
	svc, _, _, prints, _ := newIntegService(t)
	s, _ := futureOrder(t, svc, "pickup", 6*time.Hour)
	if _, _, err := svc.PrintNow(ctx, s.ID, false, true); err != nil {
		t.Fatal(err)
	}
	if !storedOf(t, svc, s.ID).Status.EarlyReceipt {
		t.Fatal("early receipt not recorded")
	}
	if _, err := svc.MakeNow(ctx, s.ID, 15); err != nil {
		t.Fatal(err)
	}
	p := prints.all()
	if len(p) != 2 || p[0].Mode != printer.ModeFutureReceipt || p[1].Mode != printer.ModeAcceptKitchen {
		t.Fatalf("prints = %+v", p)
	}
}

// CHANGE DUE TIME: the Pi's own due time, the ring time follows; moved
// close enough it rings at once; main hears nothing.
func TestChangeDue(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, _, _ := newIntegService(t)
	s, promised := futureOrder(t, svc, "delivery", 23*time.Hour)
	if _, err := svc.Schedule(ctx, s.ID, 45); err != nil {
		t.Fatal(err)
	}
	before := len(statusEvents(t, repo, s.ID))
	if _, err := svc.ChangeDue(ctx, s.ID, time.Now().Add(-time.Minute)); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("past = %v", err)
	}
	if _, err := svc.ChangeDue(ctx, s.ID, time.Now().Add(15*24*time.Hour)); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("too far = %v", err)
	}
	due := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Millisecond)
	so, err := svc.ChangeDue(ctx, s.ID, due)
	if err != nil {
		t.Fatal(err)
	}
	if !so.Order.Fulfillment.PromisedAt.Equal(due) || so.Status.DueMovedFrom == nil || !so.Status.DueMovedFrom.Equal(promised) ||
		!so.Status.RingAt.Equal(due.Add(-45*time.Minute)) || so.Status.State != order.StateScheduled {
		t.Fatalf("moved = %+v / %v", so.Status, so.Order.Fulfillment.PromisedAt)
	}
	if n := len(statusEvents(t, repo, s.ID)); n != before {
		t.Fatalf("main heard the move (%d events)", n-before)
	}
	// Due in 30 minutes with a 45-minute lead: it rings now.
	so, err = svc.ChangeDue(ctx, s.ID, time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := storedOf(t, svc, s.ID); got.Status.State != order.StateArrived || !got.Status.Rerung {
		t.Fatalf("not raised: %+v", got.Status)
	}
	_ = so
}

// PRINT NOW: kitchen / receipt / both, whatever the store's switches say.
func TestPrintNow(t *testing.T) {
	ctx := context.Background()
	svc, _, _, prints, _ := newIntegService(t)
	s, _ := futureOrder(t, svc, "delivery", 23*time.Hour)
	if _, _, err := svc.PrintNow(ctx, s.ID, false, false); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("nothing = %v", err)
	}
	so, out, err := svc.PrintNow(ctx, s.ID, true, false)
	if err != nil || !so.Status.EarlyKitchen || so.Status.EarlyReceipt || len(out.Printed) != 1 {
		t.Fatalf("kitchen = %+v %+v %v", so, out, err)
	}
	if so, _, err = svc.PrintNow(ctx, s.ID, true, true); err != nil || !so.Status.EarlyReceipt {
		t.Fatalf("both = %+v %v", so, err)
	}
	p := prints.all()
	if len(p) != 2 || p[0].Mode != printer.ModeFutureKitchen || p[1].Mode != printer.ModeFutureBoth || p[0].Promised == nil {
		t.Fatalf("prints = %+v", p)
	}
	if so.Status.State != order.StateArrived || !so.Status.Future {
		t.Fatalf("print now changed the order: %+v", so.Status)
	}
}

// CANCEL: a waiting future order only; main hears cancelled (not auto).
func TestCancelFuture(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, _, _ := newIntegService(t)
	s, _ := futureOrder(t, svc, "pickup", 23*time.Hour)
	if _, err := svc.Schedule(ctx, s.ID, 30); err != nil {
		t.Fatal(err)
	}
	so, err := svc.CancelFuture(ctx, s.ID)
	if err != nil || so.Status.State != order.StateCancelled || so.Status.RingAt != nil {
		t.Fatalf("cancel = %+v %v", so, err)
	}
	evs := statusEvents(t, repo, s.ID)
	if last := evs[len(evs)-1]; last.Status != order.StateCancelled || last.Auto {
		t.Fatalf("events = %+v", evs)
	}
	if _, err := svc.CancelFuture(ctx, s.ID); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("twice = %v", err)
	}
	if _, err := svc.CancelFuture(ctx, readyDelivery(t, svc, time.Minute)); !errors.Is(err, order.ErrInvalidState) {
		t.Fatalf("accepted = %v", err)
	}
}

// The store's colours: defaults, a partial PUT keeps the rest, bad hex refused.
func TestLookSettings(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _, _ := newIntegService(t)
	if got := svc.IntegrationSettings().Look; got != order.DefaultLook() {
		t.Fatalf("defaults = %+v", got)
	}
	set, err := svc.UpdateIntegrationSettings(ctx, func(x *order.IntegrationSettings) error {
		return json.Unmarshal([]byte(`{"tickets":{"delivery":"#7C3AED"}}`), &x.Look)
	})
	if err != nil || set.Look.Tickets.Delivery != "#7C3AED" || set.Look.Tickets.Pickup != order.DefaultTicketLook || set.Look.Accent != order.DefaultAccent {
		t.Fatalf("partial = %+v %v", set.Look, err)
	}
	if _, err := svc.UpdateIntegrationSettings(ctx, func(x *order.IntegrationSettings) error {
		x.Look.Accent = "purple"
		return nil
	}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("bad hex = %v", err)
	}
	svc.LoadIntegrationSettings(ctx)
	if got := svc.IntegrationSettings().Look.Tickets.Delivery; got != "#7C3AED" {
		t.Fatalf("not stored: %s", got)
	}
}
