package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/terminal"
)

// Refunds of in-app card payments (contract 2.10r §4): a refund on a card
// (reader) or manual_card payment that has Square's payment id goes back
// through Square (the Pi asks main: square.refund with the store's token) and
// is recorded only once Square answered PENDING / COMPLETED, with Square's
// refund id. A Square-app card, or a card recorded without Square's id, is
// recorded as before (outside: true = given back in Square's dashboard).

// SquareRefunder gives money back on a Square payment (squareterm.Service).
type SquareRefunder interface {
	Refund(ctx context.Context, paymentID string, cents int64, key, reason string) (squareterm.RefundResult, error)
}

// SetSquare wires refunds through Square.
func (s *Service) SetSquare(r SquareRefunder) { s.square = r }

// ErrSquareUnavailable: the Pi can't reach Square for a refund right now.
var ErrSquareUnavailable = errors.New("this Pi can't refund through Square right now")

// squareSource is the order's Square payment a refund gives back on: the one
// in.SquarePaymentID names, else the newest of the refund's tender (and last
// 4 when given) that still holds enough. nil = none: the refund is recorded
// as before.
func squareSource(paid []terminal.Payment, pay terminal.Payment, want string) (*terminal.Payment, error) {
	back := map[string]int64{}
	for _, p := range paid {
		if p.Refund && p.RefundOf != "" {
			back[p.RefundOf] += p.AmountCents + p.TipCents
		}
	}
	need := pay.AmountCents + pay.TipCents
	var found, most int64 = 0, 0
	for i := len(paid) - 1; i >= 0; i-- {
		p := paid[i]
		if p.Refund || p.SquarePaymentID == "" || p.Tender != pay.Tender {
			continue
		}
		if want != "" && p.SquarePaymentID != want {
			continue
		}
		if want == "" && pay.Last4 != "" && p.Last4 != "" && p.Last4 != pay.Last4 {
			continue
		}
		found++
		left := p.AmountCents + p.TipCents - back[p.ID]
		most = max(most, left)
		if left >= need {
			return &paid[i], nil
		}
	}
	switch {
	case want != "" && found == 0:
		return nil, badRequest("square_payment_id is not a " + pay.Tender + " payment of this order")
	case found > 0:
		return nil, fmt.Errorf("%w: at most %d back on one card", terminal.ErrRefundTooBig, most)
	}
	return nil, nil
}

func (s *Service) squareRefund(ctx context.Context, orderID string, in PaymentIn, pay *terminal.Payment, paid []terminal.Payment) error {
	if pay.Tender != terminal.TenderCard && pay.Tender != terminal.TenderManualCard {
		return nil
	}
	src, err := squareSource(paid, *pay, strings.TrimSpace(in.SquarePaymentID))
	if err != nil || src == nil {
		return err
	}
	if s.square == nil {
		return ErrSquareUnavailable
	}
	res, err := s.square.Refund(ctx, src.SquarePaymentID, pay.AmountCents+pay.TipCents, squareterm.RefundKey(orderID, in.ClientKey), pay.Reason)
	if err != nil {
		s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", orderID).Str("square_payment_id", src.SquarePaymentID).
			Int64("amount_cents", pay.AmountCents+pay.TipCents).Msg("Square refund not given (nothing recorded)")
		return err
	}
	pay.CardInfo = terminal.CardInfo{SquarePaymentID: src.SquarePaymentID, SquareRefundID: res.RefundID, RefundOf: src.ID}
	if pay.Last4 == "" {
		pay.Last4 = src.Last4
	}
	if pay.Brand == "" {
		pay.Brand = src.Brand
	}
	s.logger.Info().Ctx(ctx).Str("order_id", orderID).Str("square_refund_id", res.RefundID).Str("status", res.Status).
		Int64("amount_cents", pay.AmountCents+pay.TipCents).Msg("Square refund given")
	return nil
}

// KeyedHold (contract 2.10ao) decides whether a typed card is taken as a
// hold (authorized only, captured by main with the driver's typed tip): a
// delivery's card. A sent order decides by its own fulfilment, whatever the
// client says; a cart not sent yet (no order id, or none the Pi knows) by
// the client's order type. A wrong "delivery" from the client costs nothing:
// main captures a hold whose order isn't a delivery at once.
func (s *Service) KeyedHold(ctx context.Context, orderID, fulfillment string) bool {
	if id := strings.TrimSpace(orderID); id != "" {
		if v, err := s.Order(ctx, id); err == nil && v != nil {
			return v.Order.IsDelivery()
		}
	}
	return strings.TrimSpace(fulfillment) == "delivery"
}
