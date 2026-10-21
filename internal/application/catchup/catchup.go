// Package catchup closes the gap SSE leaves: main marks an order "delivered"
// once its frame is queued for the agent's stream, so frames still buffered
// when the connection drops are never re-sent. After every (re)connect the
// agent asks main for orders still `received` (nobody on the floor has seen
// them) since the open batch began (at most 12 h back) and runs the normal
// arrival path for any it doesn't hold. OnArrival ignores orders it already
// has, so this is safe to repeat.
package catchup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
)

const (
	maxLookback = 12 * time.Hour
	pageSize    = 100
	maxPages    = 10
	// settle lets the stream's own backlog arrive before we compare.
	settle = 3 * time.Second
)

// Fetcher is mainclient.Client.Get bound to the current store key.
type Fetcher func(ctx context.Context, path, rawQuery string) (int, []byte, error)

// Arriver is the order service's arrival path.
type Arriver interface {
	OnArrival(ctx context.Context, snap order.Snapshot) error
}

// Batches gives the open batch (the catch-up window starts there).
type Batches interface {
	OpenBatch(ctx context.Context) (*order.Batch, error)
}

// Run does one catch-up pass; returns how many orders it handed to OnArrival.
func Run(ctx context.Context, fetch Fetcher, svc Arriver, batches Batches, now time.Time, logger zerolog.Logger) (int, error) {
	since := now.Add(-maxLookback)
	if b, err := batches.OpenBatch(ctx); err == nil && b.OpenedAt.After(since) {
		since = b.OpenedAt
	}
	n := 0
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("status", "received")
		q.Set("from", since.UTC().Format(time.RFC3339))
		q.Set("sort", "asc")
		q.Set("num", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(page*pageSize))
		status, body, err := fetch(ctx, "/api/v1/orders", q.Encode())
		if err != nil {
			return n, err
		}
		if status != 200 {
			return n, fmt.Errorf("main answered %d", status)
		}
		var snaps []order.Snapshot
		if err := json.Unmarshal(body, &snaps); err != nil {
			return n, fmt.Errorf("decode orders: %w", err)
		}
		for _, s := range snaps {
			if err := svc.OnArrival(ctx, s); err != nil {
				logger.Warn().Err(err).Str("order_id", s.ID).Msg("catch-up: arrival failed")
				continue
			}
			n++
		}
		if len(snaps) < pageSize {
			break
		}
	}
	return n, nil
}

// Hook returns the OnConnected callback: wait for the stream's own backlog,
// then run one pass.
func Hook(parent context.Context, fetch Fetcher, svc Arriver, batches Batches, logger zerolog.Logger) func() {
	log := logger.With().Str("module", "catchup").Logger()
	return func() {
		select {
		case <-parent.Done():
			return
		case <-time.After(settle):
		}
		ctx, cancel := context.WithTimeout(parent, 60*time.Second)
		defer cancel()
		n, err := Run(ctx, fetch, svc, batches, time.Now(), log)
		if err != nil {
			log.Warn().Err(err).Msg("Catch-up after reconnect failed (orders still reach the agent on the next reconnect)")
			return
		}
		log.Info().Int("checked", n).Msg("Catch-up after reconnect done")
	}
}

// Reconcile runs a quiet pass every interval while the stream is connected —
// the safety net under main's ack-driven redelivery (an order main thinks is
// delivered but the agent never stored shows up here within a minute).
func Reconcile(ctx context.Context, every time.Duration, fetch Fetcher, svc Arriver, batches Batches, connected func() bool, logger zerolog.Logger) {
	log := logger.With().Str("module", "reconcile").Logger()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if connected != nil && !connected() {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		n, err := Run(rctx, fetch, svc, batches, time.Now(), log)
		cancel()
		if err != nil {
			log.Debug().Err(err).Msg("reconciliation pass failed")
			continue
		}
		log.Debug().Int("checked", n).Msg("reconciliation pass done")
	}
}
