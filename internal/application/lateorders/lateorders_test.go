package lateorders

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

type fakeList struct{ orders []order.StoredOrder }

func (f *fakeList) ListByBatch(_ context.Context, _ int64, offset, _ int) ([]order.StoredOrder, error) {
	if offset > 0 {
		return nil, nil
	}
	return f.orders, nil
}

type fakePub struct{ events []Late }

func (f *fakePub) Publish(typ string, v any) {
	if typ == Event {
		f.events = append(f.events, v.(Late))
	}
}

func so(id, source string, state order.State, ready time.Time) order.StoredOrder {
	n := int64(1101)
	return order.StoredOrder{Order: order.Snapshot{ID: id, Source: source},
		Status: order.Status{State: state, EstimatedReadyAt: &ready, KitchenNumber: &n}}
}

func TestLateOnce(t *testing.T) {
	now := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	past, future := now.Add(-7*time.Minute), now.Add(5*time.Minute)
	list := &fakeList{orders: []order.StoredOrder{
		so("web-late", "web", order.StateAccepted, past),
		so("vendor-late", "online", order.StateAccepted, past),
		so("web-early", "web", order.StateAccepted, future),
		so("web-ready", "web", order.StateReady, past),      // already ready
		so("counter", "counter", order.StateAccepted, past), // the terminal's own
	}}
	pub := &fakePub{}
	w := New(list, pub, zerolog.Nop())
	got, err := w.Check(context.Background(), now)
	if err != nil || len(got) != 2 || got[0].Order.Order.ID != "web-late" || got[1].Order.Order.ID != "vendor-late" {
		t.Fatalf("first check: %+v %v", got, err)
	}
	if got[0].LateMinutes != 7 || got[0].Number != 1101 || len(pub.events) != 2 {
		t.Fatalf("event: %+v (%d published)", got[0], len(pub.events))
	}
	// Once each.
	if got, _ := w.Check(context.Background(), now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("announced again: %+v", got)
	}
	// The early one becomes late later.
	if got, _ := w.Check(context.Background(), now.Add(6*time.Minute)); len(got) != 1 || got[0].Order.Order.ID != "web-early" {
		t.Fatalf("later: %+v", got)
	}
	// An order gone from the open batch is forgotten.
	list.orders = list.orders[:1]
	w.Check(context.Background(), now.Add(7*time.Minute))
	if len(w.seen) != 1 {
		t.Fatalf("seen = %v", w.seen)
	}
}
