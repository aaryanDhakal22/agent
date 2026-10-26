package order

import (
	"context"
	"strings"
	"time"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/application/dispatch"
	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// Simple auto-assign (2.2.1, setting auto_assign, default on): whenever the
// NEXT driver (the first available one in queue order) is idle with an empty
// pool, he gets the ready (or soon-ready) unassigned deliveries the planner
// gives him alone — at most dispatch.AutoAssignMaxStops, the hold rule and
// the out drivers' returns respected, came-back orders never. They are
// pooled to him (like POST /api/deliveries/pool), never sent: he taps SEND
// OUT. Triggers: a driver returned, a delivery ready / accepted / released
// from came-back, a clock-in, the setting switched on, and the 15 s floor
// tick (a cooking order crossing into soon-ready). One at a time; a pool
// staff built is never touched, and nothing happens while an AUTO preview
// may be on a tablet.

// autoPreviewHold: an unconfirmed AUTO plan this recent may still be shown
// (yellow) on a tablet — auto-assign waits for it.
const autoPreviewHold = 2 * time.Minute

// EventAutoAssigned is the SSE event of an auto-assign.
const EventAutoAssigned = "deliveries.auto_assigned"

// AutoAssigned is SSE deliveries.auto_assigned.
type AutoAssigned struct {
	DriverID   string   `json:"driver_id"`
	DriverName string   `json:"driver_name"`
	OrderIDs   []string `json:"order_ids"`
}

// AutoAssignTick is the floor tick's check (application/escalation).
func (s *Service) AutoAssignTick(ctx context.Context) { s.autoAssign(ctx, "tick") }

// autoAssignBG runs a check in the background (a trigger in a request).
func (s *Service) autoAssignBG(ctx context.Context, trigger ...string) {
	t := "change"
	if len(trigger) > 0 {
		t = trigger[0]
	}
	s.background(ctx, func(ctx context.Context) { s.autoAssign(ctx, t) })
}

// autoAssign gives the next idle driver his orders; returns the ids pooled
// (nil when nothing was).
func (s *Service) autoAssign(ctx context.Context, trigger string) []string {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	if !s.DeliveryPrefs(ctx).AutoAssign {
		return nil
	}
	now := time.Now()
	if at, confirmed, err := s.repo.LatestAutoPlan(ctx); err != nil || (!at.IsZero() && !confirmed && now.Sub(at) < autoPreviewHold) {
		return nil
	}
	drivers, err := s.repo.ListDrivers(ctx)
	if err != nil {
		return nil
	}
	var next *order.Driver
	for i := range drivers { // queue order
		if drivers[i].Status == order.DriverAvailable {
			next = &drivers[i]
			break
		}
	}
	if next == nil || len(s.poolOf(ctx, next.ID)) > 0 {
		return nil
	}
	ctx, span := s.span(ctx, "delivery.auto_assign", attribute.String("trigger", trigger), attribute.String("driver.id", next.ID))
	defer span.End()
	in, err := s.planInput(ctx, now)
	if err != nil {
		spanErr(span, err)
		return nil
	}
	in.BudgetMs = nearbyBudgetMs
	ids := dispatch.PlanNext(*in, next.ID)
	if len(ids) == 0 {
		return nil
	}
	pooled, err := s.repo.PoolOrders(ctx, ids, next.ID)
	if err != nil {
		// Changed meanwhile (sent, pooled by hand): the next trigger retries.
		s.logger.Warn().Ctx(ctx).Err(err).Str("driver_id", next.ID).Msg("auto-assign: pooling failed")
		spanErr(span, err)
		return nil
	}
	s.publishAll(latest(pooled, s.resequencePools(ctx, map[string][]string{next.ID: ids})))
	if s.broker != nil {
		s.broker.Publish(EventAutoAssigned, AutoAssigned{DriverID: next.ID, DriverName: next.Name, OrderIDs: ids})
	}
	first := next.Name
	if f := strings.Fields(first); len(f) > 0 {
		first = f[0]
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "Auto-assigned %s to %s", labels(pooled), first)
	s.logger.Info().Ctx(ctx).Str("driver_id", next.ID).Int("orders", len(ids)).Str("trigger", trigger).Msg("Auto-assigned deliveries")
	span.SetAttributes(attribute.Int("orders", len(ids)))
	return ids
}
