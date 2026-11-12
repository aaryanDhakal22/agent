package terminal

import (
	"errors"
	"strings"
)

// General refund (the owner, 2026-11-12; contract/agent.md §2.10x): money
// given back at the counter with no order attached ("doesn't ask for a
// ticket number"). Cash only: a card can't go back without its original
// Square payment (a manager does those in Square's dashboard). Stored as an
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

// ErrGeneralRefundCashOnly: a general refund is cash (400).
var ErrGeneralRefundCashOnly = errors.New("a general refund is cash only: refund a card on its order, or in Square's dashboard")

// IsGeneralRefund reports whether p is a general refund (no order).
func IsGeneralRefund(p Payment) bool {
	return p.Refund && p.RefundKind == RefundGeneral && strings.TrimSpace(p.OrderID) == ""
}

// GeneralRefundNeed is what a manager must approve for a general refund of
// cents (nil = nothing), the same rule as an order's cash refund: a cashier
// gives back cash up to the cashier limit (cashier_refund_cents, default
// $20) without a manager. There is no order to add earlier refunds up on, so
// the limit is per general refund; a manager (or anyone main made
// unlimited) needs no one. The action is "refund" (the order refund's: the
// same manager approval covers it).
func GeneralRefundNeed(c *Config, st Staff, cents int64) *ManagerRequired {
	lim := c.LimitsOf(st)
	if lim.Unlimited || cents <= lim.RefundCents {
		return nil
	}
	return &ManagerRequired{Action: ActionRefund, Limit: lim.RefundCents, Requested: cents}
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
