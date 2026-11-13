package terminal

import (
	"context"
	"strings"
	"unicode/utf8"

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

// ---- an unsent cart (the owner, 2026-11-12: nothing goes to the kitchen
// before a Send to phone charge is paid) ------------------------------------

// AlreadyPlaced: an order with that client_key exists (use the order's route).
type AlreadyPlaced struct{ OrderID string }

func (e *AlreadyPlaced) Error() string { return "an order with that client_key was already placed" }

// Precheck is what a cart would place as now.
type Precheck struct {
	TotalCents int64
	ApprovedBy string // the manager who approved its discount / price changes ("" = none needed)
}

// PrecheckOrder runs PlaceOrder's checks on a cart a phone charge will place
// later (nothing stored, numbered or printed): the key, type, customer,
// prices, and the manager needs, resolved now (manager_pin / approvals in
// in); the approver comes back so PlaceAs needs no PIN later.
func (s *Service) PrecheckOrder(ctx context.Context, a Actor, in OrderIn) (*Precheck, error) {
	ctx = WithApprovals(ctx, in.Approvals)
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if err := requireKey(in.ClientKey); err != nil {
		return nil, err
	}
	if len(in.Payments) > 0 {
		return nil, badRequest("a phone charge's order takes no payments: the charge is its payment")
	}
	if id, _, found, err := s.repo.CounterKeyHash(ctx, in.ClientKey); err != nil {
		return nil, err
	} else if found {
		return nil, &AlreadyPlaced{OrderID: id}
	}
	typ, err := checkType(in.Type)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(strings.TrimSpace(in.Note)) > 500 {
		return nil, badRequest("note is at most 500 characters")
	}
	if in.PrepMinutes != nil && (*in.PrepMinutes < 0 || *in.PrepMinutes > MaxPrepMinutes) {
		return nil, badRequest("prep_minutes is 0-240")
	}
	cust := Customer{}
	if in.Customer != nil {
		cust = *in.Customer
	}
	if utf8.RuneCountInString(cust.Name) > 80 || utf8.RuneCountInString(cust.Phone) > 32 {
		return nil, badRequest("customer name is at most 80 characters, phone 32")
	}
	if typ == terminal.TypeDelivery && (cust.Address == nil || strings.TrimSpace(cust.Address.Line1) == "" || strings.TrimSpace(cust.Address.City) == "") {
		return nil, badRequest("a delivery needs the customer's address (line1 and city)")
	}
	m, _ := s.store.Menu()
	if m == nil {
		return nil, terminal.ErrNoMenu
	}
	in.Cart = s.withDistance(ctx, typ, &cust, in.Cart) // the delivery fee by distance (2.10ae)
	p, _, err := priceRung(m, s.orders.StoreLocation(), typ, nil, in.Cart, s.now(), s.toppingMax())
	if err != nil {
		return nil, asBadRequest(err)
	}
	if len(p.quote.Errors) > 0 {
		return nil, &terminal.CartInvalid{Issues: p.quote.Errors}
	}
	dNeed := discountNeed(cfg, st, in.Cart.Discount, p)
	priceNeed, compNeed := checkPrices(cfg, st, p.changes, nil)
	out := &Precheck{}
	if dNeed != nil || priceNeed != nil || compNeed != nil {
		mgr, _, err := s.approve(ctx, a, st, cfg, in.ManagerPIN, "", dNeed, priceNeed, compNeed)
		if err != nil {
			return nil, err
		}
		if mgr != nil {
			out.ApprovedBy = mgr.ID
		}
	}
	items, off, fees, err := buildItems(m, p)
	if err != nil {
		return nil, err
	}
	money := moneyOf(items, p, off, fees)
	if err := s.checkMoney(ctx, money, items); err != nil {
		return nil, err
	}
	out.TotalCents = money.Total
	return out, nil
}

type preApprovedKey struct{}

// PlaceAs places a phone charge's cart with its payment (PlaceOrder: one
// transaction, the station tickets print then) as staff member staffID on
// a's terminal, the manager approvedBy standing in for the PIN asked at the
// charge's creation.
func (s *Service) PlaceAs(ctx context.Context, a Actor, staffID, approvedBy string, in OrderIn) (*PlaceResult, error) {
	ctx = context.WithValue(ctx, staffAsKey{}, staffID)
	if approvedBy != "" {
		ctx = context.WithValue(ctx, preApprovedKey{}, approvedBy)
	}
	in.ManagerPIN, in.Approvals = "", nil
	return s.PlaceOrder(ctx, a, in)
}

// preApproved is the manager PlaceAs carries (still a manager in main's config).
func preApproved(ctx context.Context, cfg *terminal.Config) (*terminal.Staff, bool) {
	id, ok := ctx.Value(preApprovedKey{}).(string)
	if !ok || id == "" || cfg == nil {
		return nil, false
	}
	if cur, found := cfg.Find(id); found && cur.Role == terminal.RoleManager {
		return &cur, true
	}
	return nil, false
}

// OrderByKey is the counter order a client_key placed (nil = none).
func (s *Service) OrderByKey(ctx context.Context, key string) (*OrderView, error) {
	so, err := s.repo.CounterOrderByKey(ctx, key)
	if err != nil || so == nil {
		return nil, err
	}
	return s.Order(ctx, so.Order.ID)
}
