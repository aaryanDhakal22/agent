// Package retention enforces the agent's batch window.
//
// The agent is a cache for the floor, not the system of record — main keeps
// the full history. It keeps the open batch plus the newest N closed batches
// (N = AGENT_KEEP_BATCHES, default 3); older batches' orders are pruned so the
// SQLite file stays small, and synced floor events older than a week go too
// (unsynced ones are never pruned).
package retention

import (
	"context"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/observability"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "quiccpos/agent/retention"
	// Hourly is plenty: batches close at most a few times a day.
	sweepInterval = 1 * time.Hour
	// Synced floor events are kept this long for debugging, then dropped.
	syncedEventTTL = 7 * 24 * time.Hour
)

// Run blocks until ctx is cancelled. Sweeps once immediately on start so a
// long-downed agent trims down promptly. `keep` is the number of closed batches
// to retain.
func Run(ctx context.Context, repo order.Repository, keep int, logger zerolog.Logger, meters observability.Meters) {
	log := logger.With().Str("module", "retention").Logger()
	log.Info().Dur("interval", sweepInterval).Int("keep", keep).Msg("Retention sweeper started")

	sweep(ctx, repo, keep, log, meters)

	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Retention sweeper stopped")
			return
		case <-ticker.C:
			sweep(ctx, repo, keep, log, meters)
		}
	}
}

func sweep(ctx context.Context, repo order.Repository, keep int, log zerolog.Logger, meters observability.Meters) {
	tracer := otel.Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "retention.sweep",
		trace.WithAttributes(attribute.Int("keep", keep)),
	)
	defer span.End()

	n, err := repo.TrimBatches(ctx, keep, time.Now().Add(-syncedEventTTL))
	if err != nil {
		span.RecordError(err)
		log.Error().Ctx(ctx).Err(err).Msg("Retention sweep failed")
		return
	}
	if n > 0 {
		meters.OrdersRetentionTrimmed.Add(ctx, n)
	}
	span.SetAttributes(attribute.Int64("rows_deleted", n))
	log.Info().Ctx(ctx).Int64("rows_deleted", n).Msg("Retention sweep complete")
}
