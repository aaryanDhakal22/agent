package order

import (
	"context"
	"fmt"
	"strings"

	"quiccpos/agent/internal/application/devlog"
	"quiccpos/agent/internal/domain/order"

	"go.opentelemetry.io/otel/attribute"
)

// Unassigned (2.4.0): MARK DELIVERED on a delivery's details delivers it
// with no driver; it waits under Unassigned on the Drivers tab until staff
// move it to the driver who took it (ReassignDeliveries), and Close batch
// prints it on the UNASSIGNED report. Back to deliveries undoes it.

// MarkDeliveredNoDriver delivers an open delivery with no driver.
func (s *Service) MarkDeliveredNoDriver(ctx context.Context, id string, by order.MarkedBy) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.mark_delivered", attribute.String("order.id", id), attribute.String("by", by.Kind))
	defer span.End()
	so, err := s.repo.MarkDeliveredNoDriver(ctx, id, by)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*so)
	for _, x := range s.resequencePools(ctx, nil) { // it left a pool
		s.publishUpdated(x)
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "%s marked delivered with no driver (%s)", label(*so), byText(by))
	return so, nil
}

// Undeliver puts an Unassigned delivery back on the deliveries (Back to
// deliveries / the toast's UNDO).
func (s *Service) Undeliver(ctx context.Context, id string) (*order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.undeliver", attribute.String("order.id", id))
	defer span.End()
	so, err := s.repo.Undeliver(ctx, id)
	if err != nil {
		return nil, spanErr(span, err)
	}
	s.floorChanged()
	s.publishUpdated(*so)
	s.devEvent(devlog.Info, devlog.KindOrder, "%s back to the deliveries", label(*so))
	if so.Status.State == order.StateReady {
		s.autoAssignBG(ctx, "undeliver")
	}
	return so, nil
}

// ReassignDeliveries moves delivered / out deliveries to a clocked-in driver
// (driverID "" = Unassigned, delivered ones only). Driver reports follow the
// orders; a POS check's driver changes in the POS through the hub.
func (s *Service) ReassignDeliveries(ctx context.Context, ids []string, driverID string, by order.MarkedBy) ([]order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.reassign", attribute.String("driver.id", driverID), attribute.Int("orders", len(ids)))
	defer span.End()
	moved, driversChanged, err := s.repo.ReassignDeliveries(ctx, ids, driverID, by)
	if err != nil {
		return nil, spanErr(span, err)
	}
	if len(moved) == 0 {
		return moved, nil
	}
	s.floorChanged()
	s.publishAll(moved)
	if driversChanged {
		s.publishDrivers(ctx)
	}
	to := "Unassigned"
	if driverID != "" {
		to = moved[0].Status.DriverName
	}
	labels := make([]string, 0, len(moved))
	for _, so := range moved {
		labels = append(labels, label(so))
	}
	s.devEvent(devlog.Info, devlog.KindOrder, "%s moved to %s (%s)", strings.Join(labels, ", "), to, byText(by))
	return moved, nil
}

// SetPoolOrder puts a driver's pool in the given stop order (the tablet's
// up / down arrows). ids must be exactly the pool; the order holds until the
// pool's members change (then the fastest order again).
func (s *Service) SetPoolOrder(ctx context.Context, driverID string, ids []string) ([]order.StoredOrder, error) {
	ctx, span := s.span(ctx, "delivery.pool_order", attribute.String("driver.id", driverID), attribute.Int("orders", len(ids)))
	defer span.End()
	open, err := s.repo.ListOpenDeliveries(ctx)
	if err != nil {
		return nil, spanErr(span, err)
	}
	pool := map[string]bool{}
	for _, so := range open {
		if so.Status.PoolDriverID == driverID && (so.Status.State == order.StateAccepted || so.Status.State == order.StateReady) {
			pool[so.Order.ID] = true
		}
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !pool[id] || seen[id] {
			return nil, spanErr(span, fmt.Errorf("%w: order_ids must be the driver's pool, each once", ErrBadRequest))
		}
		seen[id] = true
	}
	if len(seen) != len(pool) {
		return nil, spanErr(span, fmt.Errorf("%w: order_ids must be the driver's whole pool", ErrBadRequest))
	}
	changed, err := s.repo.SetPoolSeq(ctx, driverID, ids)
	if err != nil {
		return nil, spanErr(span, err)
	}
	if len(changed) > 0 {
		s.floorChanged()
		s.publishAll(changed)
	}
	return changed, nil
}

// byText is "tablet Counter" / "Pi web page" for the developer events.
func byText(by order.MarkedBy) string {
	if by.Kind == order.ByTablet {
		return "tablet " + by.Name
	}
	if by.Name != "" {
		return by.Name
	}
	return "Pi web page"
}
