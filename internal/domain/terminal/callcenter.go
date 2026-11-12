package terminal

import (
	"fmt"
	"strings"
)

// The call center (contract/agent.md §2.10ab, CALLCENTER-PLAN.md): the
// call-center web app is a remote terminal. main relays its calls to the
// Pi (RPC terminal.relay), which runs them through the terminal handlers
// as the store's one call-center device with the agent main signed in.

// RoleCallcenter is a call-center agent: their PIN works only in the
// call-center app (main checks it), never on a store terminal; no refunds,
// no discounts, never a manager.
const RoleCallcenter = "callcenter"

// DeviceKindCallcenter is Snapshot.DeviceKind of an order the call center
// placed (main's per-agent report reads it).
const DeviceKindCallcenter = "callcenter"

// TakenByCallcenter is how an order names the agent who took it: on the
// order (taken_by), the checks list, the kitchen tickets and the receipt.
func TakenByCallcenter(st Staff) string {
	n := strings.TrimSpace(st.Name())
	if n == "" {
		return "Call center"
	}
	return "Call center - " + n
}

// What a call-center call may not carry (403 not_allowed_for_callcenter
// {what}).
const (
	NotAllowedRoute       = "route"
	NotAllowedDiscount    = "discount"
	NotAllowedPriceChange = "price_change"
	NotAllowedOpenPrice   = "open_price"
	NotAllowedComp        = "comp"
	NotAllowedTender      = "tender"
	NotAllowedRefund      = "refund"
	NotAllowedManager     = "manager"
	NotAllowedType        = "type"
)

// NotAllowedForCallcenter: the call center may not do this (403
// not_allowed_for_callcenter {what}), decided before any money or print.
type NotAllowedForCallcenter struct{ What string }

func (e *NotAllowedForCallcenter) Error() string {
	switch e.What {
	case NotAllowedDiscount:
		return "the call center can't give a discount (coupons only)"
	case NotAllowedPriceChange:
		return "the call center can't change a price"
	case NotAllowedOpenPrice:
		return "the call center can't add an open-price line"
	case NotAllowedComp:
		return "the call center can't comp"
	case NotAllowedTender:
		return "the call center takes a typed card only"
	case NotAllowedRefund:
		return "the call center can't refund"
	case NotAllowedManager:
		return "the call center can't use a manager's PIN"
	case NotAllowedType:
		return "the call center takes pickup and delivery orders only"
	}
	return "the call center can't do this"
}

// PaidLocked: the call center tried to remove, lower or change lines of an
// order that has money on it (409 paid_locked {lines}); adding is allowed.
type PaidLocked struct{ Lines []string }

func (e *PaidLocked) Error() string {
	return fmt.Sprintf("this order is paid: the call center can add to it, not remove or change what is on it (%s)", strings.Join(e.Lines, "; "))
}

// ActionDraftTakeOverRefused journals a call-center take-over of a draft a
// store terminal holds (refused: the store wins). ActionReprint: the
// customer receipt printed again from a terminal.
const (
	ActionDraftTakeOverRefused = "draft.take_over_refused"
	ActionReprint              = "reprint"
)

func init() { reserved[ActionDraftTakeOverRefused], reserved[ActionReprint] = true, true }
