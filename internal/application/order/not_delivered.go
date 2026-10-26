package order

import (
	"context"
	"errors"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// Not delivered (2.2.1): the driver came back with the food. Staff either
// try again (the order waits in the "came back" list until released) or
// cancel it. Either way it leaves the driver's run and no driver report
// counts it; main hears order.not_delivered, the store hub
// order.not_delivered (it clears the driver off a POS check).

// ErrBadAction: not-delivered with an action other than retry | cancel.
var ErrBadAction = errors.New("action must be retry or cancel")

// NotDelivered marks a delivery that came back (see order.FloorRepository).
func (s *Service) NotDelivered(ctx context.Context, id, action string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.not_delivered", attribute.String("order.id", id), attribute.String("action", action))
	defer span.End()
	if action != order.NotDeliveredRetry && action != order.NotDeliveredCancel {
		return nil, spanErr(span, ErrBadAction)
	}
	so, returned, err := s.repo.NotDelivered(ctx, id, action)
	if err != nil {
		return nil, spanErr(span, err)
	}
	span.SetAttributes(attribute.Bool("driver.returned", returned))
	s.floorChanged()
	s.publishUpdated(*so)
	s.publishDrivers(ctx) // the run (and maybe the driver) changed
	what := "came back, waiting to go again"
	if action == order.NotDeliveredCancel {
		what = "came back, cancelled"
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "%s not delivered: %s", label(*so), what)
	s.logger.Info().Ctx(ctx).Str("order_id", so.Order.ID).Str("action", action).Bool("driver_returned", returned).Msg("Order not delivered")
	return so, nil
}

// ReleaseCameBack puts a came-back order back with the unassigned ready
// deliveries.
func (s *Service) ReleaseCameBack(ctx context.Context, id string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.release_came_back", attribute.String("order.id", id))
	defer span.End()
	so, err := s.repo.ReleaseCameBack(ctx, id)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*so)
	s.publishDrivers(ctx)
	s.devEvent(devlog.Info, devlog.KindOrder, "%s released: back with the deliveries", label(*so))
	s.autoAssignBG(ctx, "released")
	return so, nil
}
