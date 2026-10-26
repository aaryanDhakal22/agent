// Package escalation nags about orders nobody has accepted: every order still
// `arrived` after 2 minutes gets a Pushover + an SSE order.escalated (the
// tablet keeps ringing), repeating every 2 minutes until it's accepted. The
// last alert time is stored on the order, so a restart doesn't re-alert early
// or forget an overdue order.
//
// The same tick (2.1.0) rings scheduled future orders again once their ring
// time comes (they then escalate like any waiting order) and notices the
// store hub going quiet (POS link down), and (2.2.1) runs the simple
// auto-assign check.
package escalation

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

const (
	// After is how long an order may wait unaccepted before (re)alerting.
	After = 2 * time.Minute
	tick  = 15 * time.Second
)

// Escalator is the order service's EscalateOverdue, RaiseDue and
// CheckIntegrationLink.
type Escalator interface {
	EscalateOverdue(ctx context.Context, now time.Time, after time.Duration) (int, error)
	RaiseDue(ctx context.Context, now time.Time) (int, error)
	CheckIntegrationLink(now time.Time)
	// AutoAssignTick (2.2.1): the next idle driver gets ready deliveries
	// (a cooking one crossing into soon-ready included).
	AutoAssignTick(ctx context.Context)
}

// Run blocks until ctx is done, checking every 15s.
func Run(ctx context.Context, svc Escalator, logger zerolog.Logger) {
	log := logger.With().Str("module", "escalation").Logger()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if _, err := svc.RaiseDue(ctx, now); err != nil {
				log.Warn().Err(err).Msg("Scheduled-order check failed")
			}
			if _, err := svc.EscalateOverdue(ctx, now, After); err != nil {
				log.Warn().Err(err).Msg("Escalation check failed")
			}
			svc.CheckIntegrationLink(now)
			svc.AutoAssignTick(ctx)
		}
	}
}
