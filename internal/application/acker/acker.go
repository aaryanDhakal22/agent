// Package acker tells main which orders this agent has stored (RPC report
// `order.ack`). main re-sends every order that stays unacked, so an order lost
// on the way (a dropped SSE stream, a crash between receive and store) comes
// back until the agent has it. Acks are batched (every 300 ms) and retried
// with backoff; an ack lost entirely only costs one duplicate delivery, which
// the arrival path ignores — and then acks again.
package acker

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// MsgOrderAck is the report type main handles.
const MsgOrderAck = "order.ack"

// Reporter sends one RPC report to main (rpc.Registry.Report).
type Reporter func(ctx context.Context, msgType string, payload any) error

type Acker struct {
	report     Reporter
	configured func() bool
	logger     zerolog.Logger

	mu      sync.Mutex
	pending map[string]struct{}
	wake    chan struct{}
}

func New(report Reporter, configured func() bool, logger zerolog.Logger) *Acker {
	return &Acker{report: report, configured: configured, pending: map[string]struct{}{},
		wake: make(chan struct{}, 1), logger: logger.With().Str("module", "acker").Logger()}
}

// Ack queues an order id (non-blocking).
func (a *Acker) Ack(orderID string) {
	if orderID == "" {
		return
	}
	a.mu.Lock()
	a.pending[orderID] = struct{}{}
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// Pending is how many acks wait to be sent (tests / health).
func (a *Acker) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// Run sends queued acks until ctx ends.
func (a *Acker) Run(ctx context.Context) {
	backoff := time.Duration(0)
	for {
		wait := 300 * time.Millisecond
		if backoff > 0 {
			wait = backoff
		}
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
			if backoff == 0 {
				// Let a burst (catch-up, reconnect backlog) collect first.
				select {
				case <-ctx.Done():
					return
				case <-time.After(300 * time.Millisecond):
				}
			}
		case <-time.After(wait):
		}
		if err := a.Flush(ctx); err != nil {
			if backoff == 0 {
				backoff = time.Second
			} else if backoff < 30*time.Second {
				backoff *= 2
			}
			a.logger.Debug().Err(err).Dur("retry_in", backoff).Msg("order acks not sent; retrying")
			continue
		}
		backoff = 0
	}
}

// Flush sends everything queued (up to 500 per report).
func (a *Acker) Flush(ctx context.Context) error {
	if a.configured != nil && !a.configured() {
		return nil
	}
	a.mu.Lock()
	ids := make([]string, 0, len(a.pending))
	for id := range a.pending {
		ids = append(ids, id)
		if len(ids) == 500 {
			break
		}
	}
	a.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := a.report(rctx, MsgOrderAck, map[string]any{"order_ids": ids}); err != nil {
		return err
	}
	a.mu.Lock()
	for _, id := range ids {
		delete(a.pending, id)
	}
	more := len(a.pending) > 0
	a.mu.Unlock()
	if more {
		select {
		case a.wake <- struct{}{}:
		default:
		}
	}
	return nil
}
