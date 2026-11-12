// Package autoclose closes the open batch once a day at the store's
// auto_close_batch_at (main's terminal settings, default 04:00 store time)
// if nobody pressed Close batch (contract/agent.md §2.10c). The decision is
// the order service's AutoCloseDue (restart-safe: the slot handled is
// stored); this loop only ticks it.
package autoclose

import (
	"context"
	"time"

	orderApp "quiccpos/agent/internal/application/order"

	"github.com/rs/zerolog"
)

// Tick is how often the loop checks.
const Tick = 30 * time.Second

// Closer is the order service's AutoCloseDue.
type Closer interface {
	AutoCloseDue(ctx context.Context, now time.Time, loc *time.Location, at string) (orderApp.AutoCloseResult, error)
}

// Deps are the loop's inputs: Setting = the auto_close_batch_at in use (""
// = off), Loc = the store's timezone, Now = the clock, Ticks = when to check
// (a time.Ticker's channel in production; a test sends its own times).
type Deps struct {
	Setting func() string
	Loc     func() *time.Location
	Now     func() time.Time
	Ticks   <-chan time.Time
}

// Run blocks until ctx is done, checking at every tick.
func Run(ctx context.Context, svc Closer, d Deps, logger zerolog.Logger) {
	log := logger.With().Str("module", "autoclose").Logger()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.Ticks:
			if _, err := svc.AutoCloseDue(ctx, d.Now(), d.Loc(), d.Setting()); err != nil {
				log.Warn().Err(err).Msg("Batch auto-close check failed")
			}
		}
	}
}
