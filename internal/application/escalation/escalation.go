// Package escalation nags about orders nobody has accepted: every order still
// `arrived` after 2 minutes gets a Pushover + an SSE order.escalated (the
// tablet keeps ringing), repeating every 2 minutes until it's accepted. The
// last alert time is stored on the order, so a restart doesn't re-alert early
// or forget an overdue order.
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

// Escalator is the order service's EscalateOverdue.
type Escalator interface {
	EscalateOverdue(ctx context.Context, now time.Time, after time.Duration) (int, error)
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
			if _, err := svc.EscalateOverdue(ctx, now, After); err != nil {
				log.Warn().Err(err).Msg("Escalation check failed")
			}
		}
	}
}
