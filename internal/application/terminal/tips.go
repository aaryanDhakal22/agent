package terminal

import (
	"context"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// A tip added to a card payment afterwards (contract/agent.md §2.10p, the
// owner 2026-11-12): a delivery's manually entered card comes back with a
// tip written on the receipt. The difference is its own payment on the same
// tender (amount 0, the tip; a lower tip = a tip refund), ref = the
// payment, so every report adds it up; the delivery's card tips are the
// driver's (his report).

// PaymentTipIn is POST /api/terminal/orders/{id}/payments/{payment_id}/tip.
type PaymentTipIn struct {
	TipCents  int64  `json:"tip_cents"`
	ClientKey string `json:"client_key"`
}

// PaymentTipResult answers it: Payment = the tip payment written (nil when the tip
// was already that).
type PaymentTipResult struct {
	Order     order.StoredOrder `json:"order"`
	Payment   *terminal.Payment `json:"payment"`
	TipCents  int64             `json:"tip_cents"`
	Duplicate bool              `json:"duplicate"`
}

// tipOf is a card payment's tip now: its own + the tip payments that point
// at it (refunds take off).
func tipOf(paid []terminal.Payment, id string) (terminal.Payment, int64, bool) {
	var orig terminal.Payment
	found := false
	var tip int64
	for _, p := range paid {
		switch {
		case p.ID == id:
			orig, found = p, true
			tip += p.TipCents
		case p.Ref == id && p.AmountCents == 0:
			_, t := p.Signed()
			tip += t
		}
	}
	return orig, tip, found
}

// AddTip sets a card payment's whole tip (absolute).
func (s *Service) AddTip(ctx context.Context, a Actor, orderID, paymentID string, in PaymentTipIn) (*PaymentTipResult, error) {
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(orderID); err != nil {
		return nil, order.ErrNotFound
	}
	if in.TipCents < 0 || in.TipCents > 100_000 {
		return nil, badRequest("tip_cents is 0-100000")
	}
	if in.ClientKey != "" && !keyRe.MatchString(in.ClientKey) {
		return nil, badRequest("client_key is 8-64 letters, digits, _ . : -")
	}
	before, err := s.repo.OrderPayments(ctx, orderID)
	if err != nil {
		return nil, err
	}
	orig, now, found := tipOf(before, paymentID)
	if !found {
		return nil, order.ErrNotFound
	}
	if orig.Refund || !terminal.IsCard(orig.Tender) || orig.AmountCents == 0 {
		return nil, ErrNotTippable
	}
	if now == in.TipCents {
		cur, err := s.Order(ctx, orderID)
		if err != nil {
			return nil, err
		}
		return &PaymentTipResult{Order: cur.Order, TipCents: now}, nil
	}
	at := s.now().UTC().Truncate(time.Millisecond)
	so, got, dup, err := s.repo.AddCounterPayment(ctx, orderID, in.ClientKey, func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		_, from, _ := tipOf(paid, paymentID)
		delta := in.TipCents - from
		if delta == 0 {
			return nil, nil, errNoTipChange
		}
		p := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), OrderID: orderID, ClientKey: in.ClientKey, At: at, Tender: orig.Tender,
			TipCents: max(delta, -delta), Refund: delta < 0, Ref: paymentID, Last4: orig.Last4, Brand: orig.Brand, StaffID: st.ID, DeviceID: a.DeviceID}
		if p.Refund {
			p.Reason = "tip lowered"
		}
		e := terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionPaymentTip, OrderID: orderID,
			Detail: detail(map[string]any{"payment_id": paymentID, "from_cents": from, "to_cents": in.TipCents, "tip_payment_id": p.ID})}
		p.DrawerStart = cfg.DrawerStartCents()
		return []terminal.Payment{p}, []terminal.Entry{e}, nil
	})
	if err == errNoTipChange {
		cur, err := s.Order(ctx, orderID)
		if err != nil {
			return nil, err
		}
		return &PaymentTipResult{Order: cur.Order, TipCents: in.TipCents}, nil
	}
	if err != nil {
		return nil, err
	}
	if !dup {
		s.orders.CounterOrderChanged(ctx, so, nil, false)
		s.publish(EventOrderPaid, map[string]any{"order": so, "payment": got})
	}
	pays, _ := s.repo.OrderPayments(ctx, orderID)
	_, total, _ := tipOf(pays, paymentID)
	return &PaymentTipResult{Order: so, Payment: &got, TipCents: total, Duplicate: dup}, nil
}
