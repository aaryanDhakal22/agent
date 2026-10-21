package order

import (
	"context"
	"testing"
	"time"

	"quiccpos/agent/internal/domain/order"
)

// overdueRepo serves ListOverdueArrived from arrival + last-alert times, like
// the SQLite query.
type overdueRepo struct {
	*fakeRepo
	alerted map[string]time.Time
}

func (r *overdueRepo) ListOverdueArrived(_ context.Context, cutoff time.Time) ([]order.StoredOrder, error) {
	var out []order.StoredOrder
	for _, so := range r.orders {
		last, alerted := r.alerted[so.Order.ID]
		if so.Status.State == order.StateArrived && !so.Status.ArrivalDate.After(cutoff) && (!alerted || !last.After(cutoff)) {
			out = append(out, *so)
		}
	}
	return out, nil
}

func (r *overdueRepo) MarkAlerted(_ context.Context, id string, at time.Time) error {
	r.alerted[id] = at
	return nil
}

func TestEscalateOverdueRepeatsEveryInterval(t *testing.T) {
	svc, base, _, events := newTestService(t, false)
	repo := &overdueRepo{fakeRepo: base, alerted: map[string]time.Time{}}
	svc.repo = repo
	t0 := time.Now()
	s := snap()
	base.orders[s.ID] = &order.StoredOrder{Order: s, Status: order.Status{State: order.StateArrived, ArrivalDate: t0}}

	count := func(at time.Time) int {
		n, err := svc.EscalateOverdue(context.Background(), at, 2*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(t0.Add(time.Minute)) != 0 {
		t.Fatal("not overdue after 1 min")
	}
	if count(t0.Add(2*time.Minute)) != 1 {
		t.Fatal("overdue at 2 min")
	}
	select {
	case ev := <-events:
		if ev.Type != "order.escalated" {
			t.Fatalf("event %s", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("no order.escalated event")
	}
	if count(t0.Add(3*time.Minute)) != 0 {
		t.Fatal("alerted a minute ago: wait")
	}
	if count(t0.Add(4*time.Minute)) != 1 {
		t.Fatal("2 min after the last alert: again")
	}
	base.orders[s.ID].Status.State = order.StateAccepted
	if count(t0.Add(10*time.Minute)) != 0 {
		t.Fatal("accepted orders never escalate")
	}
}
