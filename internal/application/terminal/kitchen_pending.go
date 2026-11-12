package terminal

import (
	"context"
	"strconv"

	"quiccpos/agent/internal/domain/order"
)

// M-11: a counter order's station tickets print after its transaction, in
// the background. The order is stored with kitchen_pending set; the flag is
// cleared once the print was tried. A Pi restart in between finds it at the
// next start and prints the tickets then (the kitchen never got them), with
// a warning in the log and the Pi page's events.

// KitchenPendingRepo is the flag's storage (repositories.OrderRepository).
type KitchenPendingRepo interface {
	KitchenPending(ctx context.Context) ([]order.StoredOrder, error)
	ClearKitchenPending(ctx context.Context, id string) error
}

func (s *Service) printKitchen(ctx context.Context, so order.StoredOrder) {
	kp, ok := s.repo.(KitchenPendingRepo)
	if !ok {
		s.orders.PrintCounterTickets(ctx, so)
		return
	}
	id := so.Order.ID
	s.orders.PrintCounterTicketsThen(ctx, so, func(ctx context.Context) {
		if err := kp.ClearKitchenPending(ctx, id); err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", id).Msg("kitchen_pending: clear failed (prints again at the next start)")
		}
	})
}

// ResumeKitchenPrints prints the station tickets of counter orders a restart
// cut off between their commit and their print (call once at start, after
// the printer roster is loaded). Answers how many.
func (s *Service) ResumeKitchenPrints(ctx context.Context) int {
	kp, ok := s.repo.(KitchenPendingRepo)
	if !ok {
		return 0
	}
	list, err := kp.KitchenPending(ctx)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Msg("kitchen_pending: list failed")
		return 0
	}
	for _, so := range list {
		if so.Status.State == order.StateCancelled {
			_ = kp.ClearKitchenPending(ctx, so.Order.ID)
			continue
		}
		s.logger.Warn().Ctx(ctx).Str("order_id", so.Order.ID).Int64("number", numberOf(so)).
			Msg("Counter order's kitchen tickets never printed (restart): printing them now")
		s.orders.DevEvent("Counter order #" + strconv.FormatInt(numberOf(so), 10) + ": kitchen tickets printed after a restart")
		s.printKitchen(ctx, so)
	}
	return len(list)
}
