package terminal

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/application/squareterm"
	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/terminal"

	"github.com/google/uuid"
)

// Void a tender from Cash Out (batch C, the owner 2026-11-14; contract
// §2.10al, domain/terminal/tender_void.go): any cashier gives one payment
// back as a refund of kind tender_void pointing at it. Cash comes back out of
// the drawer (kick_drawer), a card with Square's payment id is refunded on
// Square first (recorded only once Square took it), a Square-app card (or one
// recorded without Square's id) is recorded and the answer says to give it
// back in Square's app. Never a manager, never under the refund limit.

// TenderVoidIn is POST /api/terminal/checks/{id}/tenders/{tid}/void.
type TenderVoidIn struct {
	ClientKey string `json:"client_key"`
	Reason    string `json:"reason"`
}

// TenderVoidResult answers it.
type TenderVoidResult struct {
	Order        order.StoredOrder `json:"order"`
	Voided       terminal.Payment  `json:"voided"`
	Refund       terminal.Payment  `json:"refund"`
	Already      bool              `json:"already"`
	Duplicate    bool              `json:"duplicate"`
	BalanceCents int64             `json:"balance_cents"`
	KickDrawer   bool              `json:"kick_drawer"`
	// RefundInSquare: nothing could be given back from here (a Square-app
	// card, or a card recorded without Square's id): give it back in
	// Square's app / dashboard if the card was charged.
	RefundInSquare bool `json:"refund_in_square"`
}

// errTenderVoidedMeanwhile: another void of the same tender landed between
// the look and the transaction (answered as already).
var errTenderVoidedMeanwhile = errors.New("the tender was voided meanwhile")

// refundsInSquare: a void whose money the Pi could not give back itself.
func refundsInSquare(r terminal.Payment) bool {
	return terminal.IsCard(r.Tender) && r.SquareRefundID == ""
}

// tenderVoidReason checks the reason (default "Tender voided").
func tenderVoidReason(in string) (string, error) {
	r := strings.TrimSpace(in)
	if r == "" {
		return terminal.DefaultTenderVoidReason, nil
	}
	if utf8.RuneCountInString(r) > 140 {
		return "", badRequest("reason is at most 140 characters")
	}
	return r, nil
}

// VoidTender voids the tender tid of the order orderID.
func (s *Service) VoidTender(ctx context.Context, a Actor, orderID, tid string, in TenderVoidIn) (*TenderVoidResult, error) {
	res, err := s.voidTender(ctx, a, orderID, tid, in)
	result := resultOf(err)
	switch {
	case err == nil && (res.Duplicate || res.Already):
		result = "duplicate"
	case errors.Is(err, terminal.ErrNotVoidableTender), errors.Is(err, terminal.ErrTenderRefunded):
		result = "not_voidable"
	}
	tender := "other"
	if err == nil && terminal.ValidTender(res.Refund.Tender) {
		tender = res.Refund.Tender
	}
	s.cm.Payment(ctx, tender, "tender_void", result)
	if err == nil && !res.Duplicate && !res.Already {
		st, _, _ := s.staffOf(a)
		s.logger.Warn().Ctx(ctx).Str("order_id", orderID).Int64("number", numberOf(res.Order)).Str("staff_id", st.ID).
			Str("staff_name", st.Name()).Str("role", st.Role).Str("device_id", a.DeviceID).Str("tender", res.Refund.Tender).
			Str("payment_id", tid).Int64("amount_cents", res.Refund.AmountCents).Int64("tip_cents", res.Refund.TipCents).
			Bool("refund_in_square", res.RefundInSquare).Int64("balance_cents", res.BalanceCents).Msg("Counter tender voided")
	}
	return res, err
}

func (s *Service) voidTender(ctx context.Context, a Actor, orderID, tid string, in TenderVoidIn) (*TenderVoidResult, error) {
	if a.Callcenter {
		return nil, notAllowed(terminal.NotAllowedRefund) // §2.10ab: the call center never gives money back
	}
	st, cfg, err := s.staffOf(a)
	if err != nil {
		return nil, err
	}
	if err := terminal.TenderVoidAllowed(st); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(orderID); err != nil {
		return nil, order.ErrNotFound
	}
	if err := requireKey(in.ClientKey); err != nil {
		return nil, err
	}
	reason, err := tenderVoidReason(in.Reason)
	if err != nil {
		return nil, err
	}
	hash := terminal.Fingerprint(map[string]any{"tender_void": tid, "reason": reason})
	// The same key again: the stored answer (or 409 key_reused) before anything else.
	if prev, err := s.repo.PaymentByKey(ctx, orderID, in.ClientKey); err != nil {
		return nil, err
	} else if prev != nil {
		if !terminal.SameRequest(prev.RequestHash, hash) {
			return nil, &terminal.KeyReused{ClientKey: in.ClientKey, OrderID: orderID}
		}
		return s.tenderVoidAnswer(ctx, orderID, *prev, true, false)
	}
	// Closed batches stay read-only (open batch or carried only), checked
	// before Square is asked for anything.
	if earlierDay(ctx, s.repo, orderID) {
		return nil, terminal.ErrNotPayable
	}
	view, err := s.Order(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if view.Order.Status.State == order.StateCancelled {
		return nil, terminal.ErrNotPayable
	}
	tender, tip, already, err := terminal.TenderToVoid(view.Payments, tid)
	if err != nil {
		return nil, err
	}
	if already != nil {
		return s.tenderVoidAnswer(ctx, orderID, *already, false, true)
	}
	pay := terminal.Payment{ID: uuid.Must(uuid.NewV7()).String(), OrderID: orderID, ClientKey: in.ClientKey,
		At: s.now().UTC().Truncate(time.Millisecond), Tender: tender.Tender, AmountCents: tender.AmountCents, TipCents: tip,
		Refund: true, Reason: reason, Last4: tender.Last4, Brand: tender.Brand, StaffID: st.ID, DeviceID: a.DeviceID,
		RefundKind: terminal.RefundTenderVoid, RequestHash: hash}
	pay.CardInfo = terminal.CardInfo{RefundOf: tid}
	// A reader / typed card with Square's id goes back on Square first, for
	// what Square charged (its own amount + tip).
	if (tender.Tender == terminal.TenderCard || tender.Tender == terminal.TenderManualCard) && tender.SquarePaymentID != "" {
		if s.square == nil {
			return nil, ErrSquareUnavailable
		}
		cents := tender.AmountCents + tender.TipCents
		res, err := s.square.Refund(ctx, tender.SquarePaymentID, cents, squareterm.RefundKey(orderID, in.ClientKey), reason)
		if err != nil {
			s.logger.Warn().Ctx(ctx).Err(err).Str("order_id", orderID).Str("square_payment_id", tender.SquarePaymentID).
				Int64("amount_cents", cents).Msg("Square refund for a tender void not given (nothing recorded)")
			return nil, err
		}
		pay.CardInfo = terminal.CardInfo{SquarePaymentID: tender.SquarePaymentID, SquareRefundID: res.RefundID, RefundOf: tid}
		s.logger.Info().Ctx(ctx).Str("order_id", orderID).Str("square_refund_id", res.RefundID).Str("status", res.Status).
			Int64("amount_cents", cents).Msg("Square refund given (tender void)")
	}
	so, got, dup, err := s.repo.AddCounterPayment(ctx, orderID, in.ClientKey, hash, func(cur order.StoredOrder, paid []terminal.Payment) ([]terminal.Payment, []terminal.Entry, error) {
		if cur.Status.EarlierBatch {
			return nil, nil, terminal.ErrNotPayable
		}
		_, tipNow, again, err := terminal.TenderToVoid(paid, tid)
		if err != nil {
			return nil, nil, err
		}
		if again != nil {
			return nil, nil, errTenderVoidedMeanwhile
		}
		pay.TipCents = tipNow
		if err := terminal.RefundRefused(paid, pay); err != nil {
			return nil, nil, err
		}
		d := map[string]any{"payment_id": tid, "refund_payment_id": pay.ID, "tender": pay.Tender, "amount_cents": pay.AmountCents,
			"tip_cents": pay.TipCents, "reason": pay.Reason}
		if pay.SquareRefundID != "" {
			d["square_refund_id"], d["square_payment_id"] = pay.SquareRefundID, pay.SquarePaymentID
		}
		if refundsInSquare(pay) {
			d["refund_in_square"] = true
		}
		entries := []terminal.Entry{{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionTenderVoid, OrderID: orderID, Detail: detail(d)}}
		if cents := kicks([]terminal.Payment{pay}); cents > 0 {
			entries = append(entries, kickEntry(a, st, orderID, "tender_void", cents))
		}
		pay.DrawerStart = cfg.DrawerStartCents()
		return []terminal.Payment{pay}, entries, nil
	})
	if errors.Is(err, errTenderVoidedMeanwhile) {
		if pay.SquareRefundID != "" {
			s.logger.Error().Ctx(ctx).Str("order_id", orderID).Str("square_refund_id", pay.SquareRefundID).
				Msg("Square refunded a tender another void already gave back: check Square's dashboard")
		}
		v, verr := s.Order(ctx, orderID)
		if verr != nil {
			return nil, verr
		}
		if _, _, al, aerr := terminal.TenderToVoid(v.Payments, tid); aerr == nil && al != nil {
			return s.tenderVoidAnswer(ctx, orderID, *al, false, true)
		}
		return nil, err
	}
	if err != nil {
		if pay.SquareRefundID != "" {
			s.logger.Error().Ctx(ctx).Err(err).Str("order_id", orderID).Str("square_refund_id", pay.SquareRefundID).
				Msg("Square refunded a tender void that could not be recorded: the same key again records it")
		}
		return nil, err
	}
	if !dup {
		s.orders.CounterOrderChanged(ctx, so, nil, false)
		s.publish(EventOrderPaid, map[string]any{"order": so, "payment": got})
		s.deliveryUpdated(ctx, so)
	}
	res := &TenderVoidResult{Order: so, Voided: tender, Refund: got, Duplicate: dup, BalanceCents: so.Status.CashDueCents,
		KickDrawer: !dup && kicks([]terminal.Payment{got}) > 0, RefundInSquare: refundsInSquare(got)}
	return res, nil
}

// tenderVoidAnswer is the answer for a void that exists (a key sent again,
// or the tender voided before under another key).
func (s *Service) tenderVoidAnswer(ctx context.Context, orderID string, refund terminal.Payment, dup, already bool) (*TenderVoidResult, error) {
	v, err := s.Order(ctx, orderID)
	if err != nil {
		return nil, err
	}
	var voided terminal.Payment
	for _, p := range v.Payments {
		if p.ID == refund.RefundOf {
			voided = p
		}
	}
	return &TenderVoidResult{Order: v.Order, Voided: voided, Refund: refund, Duplicate: dup, Already: already,
		BalanceCents: v.Order.Status.CashDueCents, RefundInSquare: refundsInSquare(refund)}, nil
}
