package terminal

import "errors"

// Void a tender (batch C, the owner 2026-11-14; contract/agent.md §2.10al):
// Cash Out's Void on one payment of an order. Any cashier, no manager,
// whatever the tender or amount: the payment is given back as one refund of
// kind RefundTenderVoid pointing at it (refund_of), cash out of the drawer,
// a card through Square. It is never counted against a cashier's refund
// limit (CashierRefunded skips it): undoing a tender taken by mistake is not
// a refund under the limit (contract/money-rules.json tender_voids TV13).

// RefundTenderVoid is a tender void's Payment.RefundKind.
const RefundTenderVoid = "tender_void"

// ActionTenderVoid is the journal action of a tender void.
const ActionTenderVoid = "tender.void"

// DefaultTenderVoidReason is the reason a void without one gets.
const DefaultTenderVoidReason = "Tender voided"

// Tender void refusals (contract §2.10al).
var (
	// ErrTenderNotFound: tid is not a payment of this order (404 not_found).
	ErrTenderNotFound = errors.New("no such payment on this order")
	// ErrNotVoidableTender: a refund, or a tip added afterwards (amount 0),
	// is not a tender (409 not_voidable_tender).
	ErrNotVoidableTender = errors.New("only a payment taken can be voided (not a refund or a tip added afterwards)")
	// ErrTenderRefunded: a refund through Square already gave money back on
	// it (409 tender_refunded): refund the rest under the refund rules.
	ErrTenderRefunded = errors.New("this payment was partly refunded already: refund the rest instead")
	// ErrDriverNotAllowed: a driver's login (403 driver_not_allowed).
	ErrDriverNotAllowed = errors.New("a driver's login has only his deliveries and his report")
)

// IsTenderVoid reports whether p is a tender void.
func IsTenderVoid(p Payment) bool { return p.Refund && p.RefundKind == RefundTenderVoid }

// TenderVoidAllowed: who may void a tender. Any staff login but a driver;
// the call center never gives money back (§2.10ab).
func TenderVoidAllowed(st Staff) error {
	switch st.Role {
	case RoleDriver:
		return ErrDriverNotAllowed
	case RoleCallcenter:
		return &NotAllowedForCallcenter{What: NotAllowedRefund}
	}
	return nil
}

// TenderToVoid finds the tender tid among the order's payments and says what
// its void gives back: the payment itself, its tip now (its own + the net of
// the tips added to it afterwards, contract 2.10p), and the void that exists
// already (nil = none). A refund or a $0 tip payment is not a tender; a
// tender a Square refund already gave money back on is refused.
func TenderToVoid(paid []Payment, tid string) (tender Payment, tip int64, already *Payment, err error) {
	found := false
	for _, p := range paid {
		if p.ID == tid {
			tender, found = p, true
		}
	}
	if !found {
		return Payment{}, 0, nil, ErrTenderNotFound
	}
	if tender.Refund || tender.AmountCents <= 0 {
		return Payment{}, 0, nil, ErrNotVoidableTender
	}
	tip = tender.TipCents
	for i := range paid {
		p := paid[i]
		switch {
		case p.ID == tid:
		case p.Refund && p.RefundOf == tid && p.RefundKind == RefundTenderVoid:
			already = &paid[i]
		case p.Refund && p.RefundOf == tid:
			return Payment{}, 0, nil, ErrTenderRefunded
		case p.Ref == tid && p.AmountCents == 0: // a tip added afterwards (or lowered)
			_, t := p.Signed()
			tip += t
		}
	}
	return tender, max(tip, 0), already, nil
}
