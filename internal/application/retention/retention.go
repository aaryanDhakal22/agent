// Package retention runs the 7-day order-history cleanup.
//
// The agent is a rolling cache for the mobile interface, not the system of
// record — main/ keeps the full history. Orders older than 7 days are pruned
// here so the local DB stays small and fast.
package retention

import (
	"context"
	"time"

	"quiccpos/agent/internal/domain/order"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "quiccpos/agent/retention"
	ttl        = 7 * 24 * time.Hour
	// Run the sweep a few times a day — cheap compared to the write path.
	sweepInterval = 6 * time.Hour
)

// Run blocks until ctx is cancelled. Sweeps once immediately on start so a
// long-downed agent doesn't carry a stale backlog of expired rows into the
// active window.
func Run(ctx context.Context, repo order.Repository, logger zerolog.Logger) {
	log := logger.With().Str("module", "retention").Logger()
	log.Info().Dur("interval", sweepInterval).Dur("ttl", ttl).Msg("Retention sweeper started")

	sweep(ctx, repo, log)

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Retention sweeper stopped")
			return
		case <-ticker.C:
			sweep(ctx, repo, log)
		}
	}
}

func sweep(ctx context.Context, repo order.Repository, log zerolog.Logger) {
	tracer := otel.Tracer(tracerName)
	cutoff := time.Now().Add(-ttl)
	ctx, span := tracer.Start(ctx, "retention.sweep",
		trace.WithAttributes(attribute.String("cutoff", cutoff.Format(time.RFC3339))),
	)
	defer span.End()

	n, err := repo.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		span.RecordError(err)
		log.Error().Ctx(ctx).Err(err).Msg("Retention sweep failed")
		return
	}
	span.SetAttributes(attribute.Int64("rows_deleted", n))
	log.Info().Ctx(ctx).Int64("rows_deleted", n).Msg("Retention sweep complete")
}
