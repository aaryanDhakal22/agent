package terminal

import (
	"context"

	"quiccpos/agent/internal/domain/terminal"
)

// Hooks for "Send to phone" (contract §2.10z, application/phonepay): who is
// logged in on a terminal, a manager for pairing a phone, and recording a
// phone's Square payment as the person who sent the charge (their terminal
// may have locked since: money is never dropped for want of a login).

// ActionPairPhone is the manager need for minting a phone pairing code.
const ActionPairPhone = "pair_phone"

// CurrentStaff is the staff member logged in on a's terminal
// (terminal.ErrLoginRequired when nobody is).
func (s *Service) CurrentStaff(a Actor) (terminal.Staff, error) {
	st, _, err := s.staffOf(a)
	return st, err
}

// RequireManager passes a logged-in manager (or anyone main made unlimited)
// at once; anyone else needs a manager's PIN or an approval for action.
func (s *Service) RequireManager(ctx context.Context, a Actor, pin string, approvals []string, action string) (terminal.Staff, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return st, err
	}
	if st.Role == terminal.RoleManager {
		return st, nil
	}
	ctx = WithApprovals(ctx, approvals)
	mgr, err := s.manager(ctx, a, cfg, pin, &terminal.ManagerRequired{Action: action})
	if err != nil {
		return st, err
	}
	return *mgr, nil
}

type staffAsKey struct{}

// RecordAs takes a payment (never a refund) on an order as staff member
// staffID on a's terminal, whether or not anyone is logged in there now.
func (s *Service) RecordAs(ctx context.Context, a Actor, staffID string, orderID string, in PaymentIn) (*PaymentResult, error) {
	if in.Refund {
		return nil, badRequest("a phone charge records payments only")
	}
	return s.AddPayment(context.WithValue(ctx, staffAsKey{}, staffID), a, orderID, in)
}

// staffFor is staffOf, unless RecordAs named the staff member: then that
// person from main's config (or, gone since, a bare record of their id).
func (s *Service) staffFor(ctx context.Context, a Actor) (terminal.Staff, *terminal.Config, error) {
	id, ok := ctx.Value(staffAsKey{}).(string)
	if !ok {
		return s.staffOf(a)
	}
	cfg, _ := s.store.Config()
	if cfg == nil {
		return terminal.Staff{}, nil, terminal.ErrLoginRequired
	}
	if st, found := cfg.Find(id); found {
		return st, cfg, nil
	}
	return terminal.Staff{ID: id, Role: terminal.RoleCashier}, cfg, nil
}
