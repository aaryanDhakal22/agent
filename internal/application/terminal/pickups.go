package terminal

import (
	"context"
	"fmt"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// The Pickup list (TERMINAL-PLAN "Pick Up list", "Pickups and payment";
// contract §2.10i): every open pickup / counter order of the open batch from
// every source (counter, phone, online, website; never the old POS's
// checks), paid ones marked picked up, "pay at pickup" ones paid at the
// terminal (AddPayment takes online cash pickups too).

// PickupRepo is the Pickup list's storage (repositories.OrderRepository).
type PickupRepo interface {
	PickupOrders(ctx context.Context) ([]order.StoredOrder, error)
	MarkPickedUp(ctx context.Context, id string, journal func(cur order.StoredOrder) []terminal.Entry) (order.StoredOrder, bool, error)
}

// PickupList is GET /api/terminal/pickups' answer.
type PickupList struct {
	Orders []order.StoredOrder `json:"orders"`
}

// Pickups are the open batch's pickups not done with, oldest first: each a
// StoredOrder with its source (order.source) and money state
// (status.cash_due_cents, status.paid; an online order the old POS marked
// paid shows 0 due).
func (s *Service) Pickups(ctx context.Context) (*PickupList, error) {
	all, err := s.repo.PickupOrders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]order.StoredOrder, 0, len(all))
	for _, so := range all {
		if !terminal.OnPickupList(so) {
			continue
		}
		due := terminal.DueOf(so)
		so.Status.CashDueCents, so.Status.Paid = due, due == 0
		out = append(out, so)
	}
	return &PickupList{Orders: out}, nil
}

// PickedUpResult is POST /api/terminal/orders/{id}/picked-up's answer.
type PickedUpResult struct {
	Order   order.StoredOrder `json:"order"`
	Already bool              `json:"already"`
}

// PickedUp marks a paid pickup handed over (any source but the old POS): it
// is finished (status.finished_at, finished_by "terminal"; an accepted one
// is made ready at the same time), main hears order.status, the tablets
// order.updated and the terminals terminal.order.picked_up. Money still due
// → 409 cash_due. A second time answers already: true.
func (s *Service) PickedUp(ctx context.Context, a Actor, id string) (*PickedUpResult, error) {
	st, _, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.ErrNotFound
	}
	so, already, err := s.repo.MarkPickedUp(ctx, id, func(cur order.StoredOrder) []terminal.Entry {
		return []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionPickedUp, OrderID: id,
			Detail: detail(map[string]any{"number": numberOf(cur), "source": cur.Order.Source, "state": cur.Status.State})}}
	})
	if err != nil {
		return nil, err
	}
	if already {
		return &PickedUpResult{Order: so, Already: true}, nil
	}
	s.orders.CounterOrderChanged(ctx, so, nil, false) // floor sync + order.updated
	s.publish(terminal.EventOrderPickedUp, map[string]any{"order": so, "device_id": a.DeviceID})
	s.orders.DevEvent(fmt.Sprintf("Order #%d picked up", numberOf(so)))
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int64("number", numberOf(so)).Str("staff_id", st.ID).
		Str("device_id", a.DeviceID).Msg("Pickup handed over")
	return &PickedUpResult{Order: so}, nil
}

// pickupOrder is an online / website pickup of the open batch (Order's
// fallback: the terminal reads it before taking its payment).
func (s *Service) pickupOrder(ctx context.Context, id string) (*order.StoredOrder, error) {
	all, err := s.repo.PickupOrders(ctx)
	if err != nil {
		return nil, err
	}
	for _, so := range all {
		if so.Order.ID == id && so.Order.Source != order.SourceCounter {
			return &so, nil
		}
	}
	return nil, order.ErrNotFound
}
