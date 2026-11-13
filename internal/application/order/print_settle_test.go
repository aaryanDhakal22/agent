package order

import (
	"context"
	"slices"
	"sync"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/database/repositories"

	"github.com/google/uuid"
)

// A print a restart would do again settles its marker the moment the paper is
// out, before order.printed is written (2.5.5): the gate's flow_offline caught
// a counter order's kitchen tickets printing twice after a SIGKILL that landed
// between the print and kitchen_pending's clear, a window e60c8f2 widened by
// writing order.printed first. Each probe below looks at the marker when the
// first write after the paper (order.printed) happens.

// settleProbe is the real repository with a hook on the order.printed write.
type settleProbe struct {
	*repositories.OrderRepository
	mu       sync.Mutex
	onRecord func(orderID string)
}

func (p *settleProbe) RecordOrderPrinted(ctx context.Context, ev order.OrderPrintedEvent) error {
	p.mu.Lock()
	fn := p.onRecord
	p.mu.Unlock()
	if fn != nil {
		fn(ev.OrderID)
	}
	return p.OrderRepository.RecordOrderPrinted(ctx, ev)
}

func interrupted(t *testing.T, repo *repositories.OrderRepository, id string) bool {
	t.Helper()
	ids, err := repo.ListInterruptedAccepts(context.Background())
	if err != nil {
		t.Error(err)
	}
	return slices.Contains(ids, id)
}

func TestPrintSettlesTheRestartMarkerBeforeOrderPrinted(t *testing.T) {
	ctx := context.Background()

	t.Run("an online order placed on arrival: accept_pending cleared first", func(t *testing.T) {
		svc, repo, _, _, _ := newIntegService(t)
		svc.legacyArrival = false
		probe := &settleProbe{OrderRepository: repo}
		svc.repo = probe
		var seen, pending bool
		s := onlineSnap("pickup")
		probe.onRecord = func(id string) {
			if id == s.ID {
				seen, pending = true, interrupted(t, repo, id)
			}
		}
		if err := svc.OnArrival(ctx, s); err != nil {
			t.Fatal(err)
		}
		svc.WaitBackground()
		if !seen {
			t.Fatal("no order.printed written")
		}
		if pending {
			t.Fatal("order.printed was written while the accept was still pending: a restart then rings it again for a second set")
		}
	})

	t.Run("a tablet ACCEPT: accept_pending cleared first", func(t *testing.T) {
		svc, repo, _, _, _ := newIntegService(t)
		svc.legacyArrival = true
		probe := &settleProbe{OrderRepository: repo}
		svc.repo = probe
		var seen, pending bool
		s := onlineSnap("pickup")
		probe.onRecord = func(id string) {
			if id == s.ID {
				seen, pending = true, interrupted(t, repo, id)
			}
		}
		if err := svc.OnArrival(ctx, s); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Accept(ctx, uuid.MustParse(s.ID), nil); err != nil {
			t.Fatal(err)
		}
		svc.WaitBackground()
		if !seen {
			t.Fatal("no order.printed written")
		}
		if pending {
			t.Fatal("order.printed was written while the accept was still pending: a restart then rings it again for a second set")
		}
		if interrupted(t, repo, s.ID) {
			t.Fatal("the accept is still pending after the print")
		}
	})

	t.Run("a counter order's station tickets: done (kitchen_pending) runs first", func(t *testing.T) {
		svc, repo, _, _, _ := newIntegService(t)
		probe := &settleProbe{OrderRepository: repo}
		svc.repo = probe
		s := onlineSnap("pickup")
		so := order.StoredOrder{Order: s, Status: order.Status{State: order.StateAccepted}}
		var mu sync.Mutex
		var steps []string
		step := func(x string) { mu.Lock(); steps = append(steps, x); mu.Unlock() }
		probe.onRecord = func(string) { step("order.printed") }
		svc.PrintCounterTicketsThen(ctx, so, func(context.Context) { step("done") })
		svc.WaitBackground()
		if want := []string{"done", "order.printed"}; !slices.Equal(steps, want) {
			t.Fatalf("steps %v, want %v (kitchen_pending must be cleared before anything else is written)", steps, want)
		}
	})
}
