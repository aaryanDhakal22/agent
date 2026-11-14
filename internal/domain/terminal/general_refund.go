package terminal

import (
	"errors"
	"strings"
)

// General refund (the owner, 2026-11-12; contract/agent.md §2.10x): money
// given back at the counter with no order attached ("doesn't ask for a
// ticket number"). Cash, or (batch J, 2026-11-14) a card through Square: an
// unlinked refund to the card typed into Square's card field, or a refund
// linked to one of the store's recent Square payments. Stored as an
// order_payments row with order_id "" and refund_kind "general", so it lands
// in the paying terminal's drawer (expected cash goes down), on the drawer /
// till report and the Z under Refunds as "General refund", and reaches main
// as the floor event refund.general (an unlinked refund row with its batch).

// RefundGeneral is a general refund's Payment.RefundKind.
const RefundGeneral = "general"

// EventRefundGeneral is the floor event a general refund sends to main
// {event_id, type, at, payment (Payment.Wire, negative), reason, batch_id}.
const EventRefundGeneral = "refund.general"

// GeneralRefundLabel is how reports and slips name it.
const GeneralRefundLabel = "General refund"

// ErrGeneralRefundTender: a general refund is cash, or a card through
// Square (batch J, 2026-11-14); a Square-app card, a comp or anything else is
// refused (400).
var ErrGeneralRefundTender = errors.New("a general refund is cash, or a card through Square's card field")

// GeneralRefundTender checks a general refund's tender ("" = cash): cash,
// or (batch J, the owner 2026-11-14) a card given back through Square
// (manual_card: typed into Square's card field; card: the fallback's
// payment read by the reader).
func GeneralRefundTender(t string) (string, error) {
	switch t = strings.TrimSpace(t); t {
	case "", TenderCash:
		return TenderCash, nil
	case TenderCard, TenderManualCard:
		return t, nil
	}
	return "", ErrGeneralRefundTender
}

// GeneralRefundCheck is the money rule for a general refund of cents on
// tender (contract/money-rules.json general_refunds): the tender first, then
// a manager every time, whatever the tender (GeneralRefundNeed).
func GeneralRefundCheck(c *Config, st Staff, tender string, cents int64) (*ManagerRequired, error) {
	if _, err := GeneralRefundTender(tender); err != nil {
		return nil, err
	}
	return GeneralRefundNeed(c, st, cents), nil
}

// ActionSquareGeneralRefund: the Pi's call to main for a general card
// refund (batch J) {amount_cents, idempotency_key, unlinked, ok,
// square_refund_id?, code?, square_code?}. Never the card or its token.
const ActionSquareGeneralRefund = "square.general_refund"

// IsGeneralRefund reports whether p is a general refund (no order).
func IsGeneralRefund(p Payment) bool {
	return p.Refund && p.RefundKind == RefundGeneral && strings.TrimSpace(p.OrderID) == ""
}

// GeneralRefundNeed is what a manager must approve for a general refund of
// cents (nil = nothing). The owner, 2026-11-12: **a manager every time** -
// no cashier allowance (unlike an order's cash refund). A logged-in manager
// needs no one; anyone else (a cashier, a driver, a cashier main made
// unlimited) needs a manager's PIN or approval. The action is "refund" (the
// order refund's: the same manager approval covers it); limit 0.
func GeneralRefundNeed(c *Config, st Staff, cents int64) *ManagerRequired {
	if st.Role == RoleManager {
		return nil
	}
	return &ManagerRequired{Action: ActionRefund, Limit: 0, Requested: cents}
}

// IsGeneralCardRefund: a general refund given back on a card (never in a
// drawer: its money is on the card's tender / brand).
func IsGeneralCardRefund(p Payment) bool {
	return IsGeneralRefund(p) && p.Tender != TenderCash
}

// GeneralRefunds sums the general refunds among pays (count, cents).
func GeneralRefunds(pays []Payment) ZAmount {
	var z ZAmount
	for _, p := range pays {
		if IsGeneralRefund(p) {
			z.Count++
			z.Cents += p.AmountCents + p.TipCents
		}
	}
	return z
}
