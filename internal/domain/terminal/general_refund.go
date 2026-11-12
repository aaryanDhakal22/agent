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
