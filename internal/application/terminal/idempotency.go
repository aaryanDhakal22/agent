package terminal

import (
	"context"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"
)

// Idempotency of SEND, payments and cart changes (contract/agent.md §2.10s
// §1, HARDENING-PLAN M-1 / M-8): a client_key is required, and the request's
// fingerprint is stored with what it made. The same key with the same body
// answers the original; with another body it is 409 key_reused.

// orderFingerprint: the SEND without client_key / manager_pin.
func orderFingerprint(in OrderIn) string {
	pays := make([]PaymentIn, len(in.Payments))
	for i, p := range in.Payments {
		p.ClientKey, p.ManagerPIN = "", ""
		pays[i] = p
	}
	return terminal.Fingerprint(struct {
		Type        string      `json:"type"`
		Cart        Cart        `json:"cart"`
		Customer    *Customer   `json:"customer"`
		Payments    []PaymentIn `json:"payments"`
		Note        string      `json:"note"`
		PrepMinutes *int64      `json:"prep_minutes"`
	}{in.Type, in.Cart, in.Customer, pays, in.Note, in.PrepMinutes})
}

// paymentFingerprint: the payment without client_key / manager_pin.
func paymentFingerprint(in PaymentIn) string {
	in.ClientKey, in.ManagerPIN = "", ""
	return terminal.Fingerprint(in)
}

// changeFingerprint: the cart change without client_key / manager_pin.
func changeFingerprint(in ChangeIn) string {
	in.ClientKey, in.ManagerPIN = "", ""
	return terminal.Fingerprint(in)
}

// tipFingerprint: an added tip (the payment it is on + the amount).
func tipFingerprint(paymentID string, in PaymentTipIn) string {
	return terminal.Fingerprint(map[string]any{"payment_id": paymentID, "tip_cents": in.TipCents})
}

// requireKey checks a keyed route's client_key.
func requireKey(key string) error {
	if key == "" {
		return terminal.ErrClientKeyRequired
	}
	if !keyRe.MatchString(key) {
		return badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	return nil
}

// KeyRepo finds what a key made before (repositories.OrderRepository).
type KeyRepo interface {
	CounterKeyHash(ctx context.Context, key string) (orderID, hash string, found bool, err error)
	PaymentByKey(ctx context.Context, orderID, key string) (*terminal.Payment, error)
}

// duplicateOrder answers a SEND whose key made an order already: that
// order (duplicate, its change as taken), or 409 key_reused for another
// body; nil when the key is new.
func (s *Service) duplicateOrder(ctx context.Context, key, hash string) (*PlaceResult, error) {
	id, stored, found, err := s.repo.CounterKeyHash(ctx, key)
	if err != nil || !found {
		return nil, err
	}
	if !terminal.SameRequest(stored, hash) {
		return nil, &terminal.KeyReused{ClientKey: key, OrderID: id}
	}
	prev, err := s.repo.CounterOrderByKey(ctx, key)
	if err != nil || prev == nil {
		return nil, err
	}
	var change int64
	if pays, err := s.repo.OrderPayments(ctx, prev.Order.ID); err == nil {
		for _, p := range pays {
			if p.ClientKey == "" { // the SEND's own payments
				change += p.ChangeCents
			}
		}
	}
	return &PlaceResult{Order: *prev, Number: numberOf(*prev), Duplicate: true, ChangeCents: change}, nil
}

// duplicatePayment answers a payment whose key was taken already.
func (s *Service) duplicatePayment(ctx context.Context, orderID string, prev terminal.Payment) (*PaymentResult, error) {
	v, err := s.Order(ctx, orderID)
	if err != nil {
		return nil, err
	}
	return &PaymentResult{Order: v.Order, Payment: prev, Duplicate: true, BalanceCents: v.Order.Status.CashDueCents}, nil
}

// UnsettledRepo lists the unsettled checks (repositories.OrderRepository).
type UnsettledRepo interface {
	UnsettledChecks(ctx context.Context) (*order.UnsettledList, error)
}

// Unsettled is GET /api/terminal/unsettled (contract 2.10s §4).
func (s *Service) Unsettled(ctx context.Context) (*order.UnsettledList, error) {
	return s.repo.UnsettledChecks(ctx)
}
