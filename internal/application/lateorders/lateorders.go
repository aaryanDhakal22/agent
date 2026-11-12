// Package lateorders announces an accepted online order that is still being
// cooked past its estimated ready time: SSE terminal.order.late, once per
// order (contract/agent.md §2.10f). The POS terminals show it in their Info
// feed. Online = not a counter order and not a POS check (the hub's).
package lateorders

import (
	"context"
	"sync"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

// Tick is how often the loop checks.
const Tick = 30 * time.Second

// Event is the SSE event type.
const Event = "terminal.order.late"

// Lister lists the open batch's orders (orderApp.Service.ListByBatch, 0 =
// the open batch).
type Lister interface {
	ListByBatch(ctx context.Context, batchID int64, offset, limit int) ([]order.StoredOrder, error)
}

// Publisher is the SSE broker.
type Publisher interface {
	Publish(typ string, v any)
}

// Late is the event body.
type Late struct {
	Order       order.StoredOrder `json:"order"`
	Number      int64             `json:"number"`
	ReadyAt     time.Time         `json:"estimated_ready_at"`
	LateMinutes int64             `json:"late_minutes"`
}

// Watcher remembers which orders it announced (in memory: after a Pi
// restart an order still late is announced once more; the terminal keeps
// its own once-per-order set).
type Watcher struct {
	list   Lister
	pub    Publisher
	logger zerolog.Logger
	mu     sync.Mutex
	seen   map[string]bool
}

// New makes a watcher.
func New(list Lister, pub Publisher, logger zerolog.Logger) *Watcher {
	return &Watcher{list: list, pub: pub, logger: logger.With().Str("module", "lateorders").Logger(), seen: map[string]bool{}}
}

// IsLate: an accepted online order (not a counter order) whose estimated
// ready time has passed.
func IsLate(so order.StoredOrder, now time.Time) bool {
	return so.Status.State == order.StateAccepted && so.Order.Source != order.SourceCounter &&
		so.Status.EstimatedReadyAt != nil && now.After(*so.Status.EstimatedReadyAt)
}

// Check announces every order that became late since the last check and
// forgets orders no longer in the open batch. Returns the ones announced.
func (w *Watcher) Check(ctx context.Context, now time.Time) ([]Late, error) {
	const page = 500
	var out []Late
	present := map[string]bool{}
	for off := 0; ; off += page {
		list, err := w.list.ListByBatch(ctx, 0, off, page)
		if err != nil {
			return out, err
		}
		for _, so := range list {
			present[so.Order.ID] = true
			if !IsLate(so, now) {
				continue
			}
			w.mu.Lock()
			done := w.seen[so.Order.ID]
			w.seen[so.Order.ID] = true
			w.mu.Unlock()
			if done {
				continue
			}
			l := Late{Order: so, ReadyAt: so.Status.EstimatedReadyAt.UTC(), LateMinutes: int64(now.Sub(*so.Status.EstimatedReadyAt) / time.Minute)}
			if so.Status.KitchenNumber != nil {
				l.Number = *so.Status.KitchenNumber
			}
			out = append(out, l)
			if w.pub != nil {
				w.pub.Publish(Event, l)
			}
			w.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Int64("number", l.Number).Int64("late_minutes", l.LateMinutes).Msg("Online order late")
		}
		if len(list) < page {
			break
		}
	}
	w.mu.Lock()
	for id := range w.seen {
		if !present[id] {
			delete(w.seen, id)
		}
	}
	w.mu.Unlock()
	return out, nil
}

// Run checks at every tick until ctx is done.
func (w *Watcher) Run(ctx context.Context, ticks <-chan time.Time, now func() time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if _, err := w.Check(ctx, now()); err != nil {
				w.logger.Warn().Err(err).Msg("Late order check failed")
			}
		}
	}
}
