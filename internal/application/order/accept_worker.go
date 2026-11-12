package order

import (
	"context"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/safego"

	"github.com/google/uuid"
)

// The auto-accept worker (hardening F-7). Prod runs with auto-accept on, and
// an accept prints (up to the dispatcher's 25 s budget with a dead printer).
// Done inline that would hold the SSE reader: the next order, RPC command or
// store_config waits behind the print. OnArrival stores and acks the order
// inline, then hands the accept to this worker.

// acceptQueueSize bounds the waiting accepts; a full queue accepts inline
// (back-pressure on the stream rather than an unbounded backlog).
const acceptQueueSize = 64

type acceptJob struct {
	ctx  context.Context
	id   uuid.UUID
	snap order.Snapshot
}

// StartAcceptWorker starts the auto-accept worker (cmd/main.go). Without it
// (tests) OnArrival accepts inline. The worker stops with ctx; StopAccepts
// waits for the job in hand.
func (s *Service) StartAcceptWorker(ctx context.Context) {
	q := make(chan acceptJob, acceptQueueSize)
	s.acceptQ.Store(&q)
	s.acceptWG.Add(1)
	go func() {
		defer s.acceptWG.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case j := <-q:
				_ = safego.Do(j.ctx, "order.auto_accept", func() { s.autoAcceptOrRing(j.ctx, j.id, j.snap) })
			}
		}
	}()
}

// WaitAccepts waits for the accept worker to stop (shutdown, after ctx is
// cancelled): an accept in hand finishes its print first.
func (s *Service) WaitAccepts() { s.acceptWG.Wait() }

// queueAutoAccept hands the accept to the worker; false = no worker or the
// queue is full (the caller accepts inline).
func (s *Service) queueAutoAccept(ctx context.Context, id uuid.UUID, snap order.Snapshot) bool {
	qp := s.acceptQ.Load()
	if qp == nil {
		return false
	}
	select {
	case *qp <- acceptJob{ctx: context.WithoutCancel(ctx), id: id, snap: snap}:
		return true
	default:
		s.logger.Warn().Ctx(ctx).Str("order_id", id.String()).Msg("Accept queue full — accepting inline")
		return false
	}
}
