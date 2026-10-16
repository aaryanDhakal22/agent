// Package driverreset clocks every driver out once a day at the location's
// reset time (default 10:00 — the old tablet app's reset), so each day's
// driver queue starts empty and every shift record gets an end. Drivers out on
// a run are clocked out when they return. Checked every 30 s; the last run is
// stored, so restarts neither skip nor repeat it.
package driverreset

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

const tick = 30 * time.Second

// Resetter is the order service's DriverResetDue.
type Resetter interface {
	DriverResetDue(ctx context.Context, now time.Time, loc *time.Location) (int, error)
}

// Run blocks until ctx is done. loc returns the location's timezone (from
// store_config; it can change at runtime).
func Run(ctx context.Context, svc Resetter, loc func() *time.Location, logger zerolog.Logger) {
	log := logger.With().Str("module", "driverreset").Logger()
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if _, err := svc.DriverResetDue(ctx, now, loc()); err != nil {
				log.Warn().Err(err).Msg("Driver reset check failed")
			}
		}
	}
}
