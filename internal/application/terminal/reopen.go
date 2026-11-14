package terminal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Reopen a check from All checks (contract/agent.md §2.10ak, batch B, the
// owner 2026-11-14).

// ReopenRepo is the reopen's storage (repositories.OrderRepository); a repo
// without it answers ErrReopenUnsupported.
type ReopenRepo interface {
	ReopenCheck(ctx context.Context, id string, now time.Time, journal func(cur order.StoredOrder) []terminal.Entry) (order.StoredOrder, error)
}

// ErrReopenUnsupported: the repo can't reopen (tests' fakes).
var ErrReopenUnsupported = errors.New("reopening checks isn't available")

// ReopenResult is POST /api/terminal/checks/{id}/reopen's answer: the check
// as GET /api/terminal/checks/{id} has it, its payments (the tenders) and
// reopened: true.
type ReopenResult struct {
	Check    Check              `json:"check"`
	Order    order.StoredOrder  `json:"order"`
	Payments []terminal.Payment `json:"payments"`
	Reopened bool               `json:"reopened"`
}

// reopenType is the overview's type of an order.
func reopenType(so order.StoredOrder) string {
	switch {
	case so.IsDelivery():
		return terminal.TypeDelivery
	case so.Order.Fulfillment.Type == "pickup":
		return terminal.TypePickup
	}
	return terminal.TypeCounter
}

// Reopen puts a check of the open batch back on the Pickup list / Dispatch's
// New: any staff login but a driver's, no manager. The order's money and state
// don't move; the terminals hear terminal.order.reopened.
func (s *Service) Reopen(ctx context.Context, a Actor, id string) (*ReopenResult, error) {
	st, _, err := s.reportStaff(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, order.ErrNotFound
	}
	repo, ok := s.repo.(ReopenRepo)
	if !ok {
		return nil, ErrReopenUnsupported
	}
	so, err := repo.ReopenCheck(ctx, id, s.now(), func(cur order.StoredOrder) []terminal.Entry {
		return []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionReopen, OrderID: id,
			Detail: detail(map[string]any{"number": numberOf(cur), "type": reopenType(cur), "state": cur.Status.State,
				"paid": terminal.DueOf(cur) == 0, "due_cents": terminal.DueOf(cur)})}}
	})
	if err != nil {
		return nil, err
	}
	s.publish(terminal.EventOrderReopened, map[string]any{"order": so, "device_id": a.DeviceID})
	if s.orders != nil {
		s.orders.DevEvent(fmt.Sprintf("Check #%d reopened", numberOf(so)))
	}
	s.logger.Info().Ctx(ctx).Str("order_id", id).Int64("number", numberOf(so)).Str("staff_id", st.ID).
		Str("device_id", a.DeviceID).Msg("Check reopened")
	d, err := s.CheckByID(ctx, a, id)
	if err != nil {
		return nil, err
	}
	return &ReopenResult{Check: d.Check, Order: d.Order, Payments: d.Payments, Reopened: true}, nil
}
