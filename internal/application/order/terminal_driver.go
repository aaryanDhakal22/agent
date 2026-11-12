package order

import (
	"context"
	"fmt"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
)

// The terminal's driver view (contract/agent.md §2.10b): what the terminal
// service (application/terminal) needs from the floor beyond the hooks in
// terminal_hooks.go. Every change goes through the same floor transitions as
// Quibase's (floor events, SSE order.updated / drivers.updated /
// drivers.dispatched), so the tablets stay in step.

// OpenDeliveries are the open batch's deliveries still on the floor
// (cooking, ready or out), with their geo.
func (s *Service) OpenDeliveries(ctx context.Context) ([]order.StoredOrder, error) {
	return s.repo.ListOpenDeliveries(ctx)
}

// GetOrder is one stored order (order.ErrNotFound).
func (s *Service) GetOrder(ctx context.Context, id string) (*order.StoredOrder, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, order.ErrNotFound
	}
	return s.repo.GetByID(ctx, uid)
}

// UnassignedDelivered is the open batch's deliveries delivered with no
// driver (MARK DELIVERED; the Drivers tab's Unassigned).
func (s *Service) UnassignedDelivered(ctx context.Context) ([]order.StoredOrder, error) {
	return s.repo.ListUnassigned(ctx, 0)
}

// ClaimDelivered is a driver assigning himself deliveries nobody took (the
// Dispatch section): they become his and delivered (delivered now when they
// weren't), in one transaction, all or nothing (*order.ErrTaken). Not a
// send-out: he never goes out, nothing is printed and no route is sent. The
// orders count on his driver report (cash, tips) like any delivery of his.
func (s *Service) ClaimDelivered(ctx context.Context, driverID string, ids []string, rule order.ClaimRule, by order.MarkedBy) ([]order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.claim_delivered", attribute.String("driver.id", driverID), attribute.Int("orders", len(ids)))
	defer span.End()
	claimed, err := s.repo.ClaimDelivered(ctx, driverID, dedupeIDs(ids), rule, by)
	if err != nil {
		return nil, spanErr(span, err)
	}
	if len(claimed) == 0 {
		return claimed, nil
	}
	s.floorChanged()
	s.publishAll(claimed)
	for _, so := range claimed {
		s.devEvent(devlog.Info, devlog.KindOrder, "%s claimed as delivered by %s on a terminal", label(so), so.Status.DriverName)
	}
	return claimed, nil
}

// DeliverOne is the driver's "close" on one of his out orders: delivered (it
// is closed for good only at Close batch or by a manager on the Pi). His
// last stop brings him back to the queue like "I have returned" (and runs
// the simple auto-assign).
func (s *Service) DeliverOne(ctx context.Context, driverID, orderID string) (*order.StoredOrder, bool, error) {
	ctx, span := s.span(ctx, "delivery.deliver_one", attribute.String("driver.id", driverID), attribute.String("order.id", orderID))
	defer span.End()
	if _, err := uuid.Parse(orderID); err != nil {
		return nil, false, spanErr(span, order.ErrNotFound)
	}
	so, returned, err := s.repo.DeliverOne(ctx, driverID, orderID)
	if err != nil {
		return nil, false, spanErr(span, err)
	}
	span.SetAttributes(attribute.Bool("returned", returned))
	s.devEvent(devlog.Info, devlog.KindOrder, "Delivered: %s (closed by the driver)", label(*so))
	s.floorChanged()
	s.publishUpdated(*so)
	if returned {
		s.autoAssign(ctx, "returned")
	}
	s.publishDrivers(ctx)
	return so, returned, nil
}

// SetDoorTip records a tip the driver got at the door on a cash or counter
// delivery (absolute, 0-MaxAddedTipCents). It shows on his report; nothing
// is charged or sent to main.
func (s *Service) SetDoorTip(ctx context.Context, id string, cents int64) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "order.set_door_tip", attribute.String("order.id", id), attribute.Int64("tip_cents", cents))
	defer span.End()
	if _, err := uuid.Parse(id); err != nil {
		return nil, spanErr(span, fmt.Errorf("%w: invalid order id", ErrBadRequest))
	}
	if cents < 0 || cents > order.MaxAddedTipCents {
		return nil, spanErr(span, fmt.Errorf("%w: tip_cents must be 0-%d", ErrBadRequest, order.MaxAddedTipCents))
	}
	so, err := s.repo.SetDoorTip(ctx, id, cents)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.publishUpdated(*so)
	s.devEvent(devlog.Info, devlog.KindOrder, "%s door tip recorded: %s", label(*so), cents2(cents))
	return so, nil
}
