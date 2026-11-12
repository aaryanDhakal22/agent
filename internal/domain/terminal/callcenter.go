package terminal

import (
	"context"
	"encoding/json"
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

// OutOfArea: a call-center delivery to an address outside the store's area
// (past the radius + the warn-only margin) without the agent's
// acknowledgement (409 out_of_area {area, radius_mi, margin_mi}); with
// out_of_area_ack: true it is placed and marked.
type OutOfArea struct {
	Area     Area
	RadiusMi float64
	MarginMi float64
}

func (e *OutOfArea) Error() string {
	if e.Area.DistanceMi != nil {
		return fmt.Sprintf("this address is %.1f miles away, outside the store's delivery area (%.1f miles)", *e.Area.DistanceMi, e.RadiusMi)
	}
	return "this address is outside the store's delivery area"
}

// AreaCheck is the call center's area check of a delivery address, carried
// on the call's context to where the order is stored: Out = outside the
// area and acknowledged (the order is marked + journaled), else the address
// is in / near / unknown (a changed address takes an old mark off).
type AreaCheck struct {
	Area Area
	Out  bool
}

type areaCheckKey struct{}

// WithAreaCheck puts the relay's area check on ctx.
func WithAreaCheck(ctx context.Context, c AreaCheck) context.Context {
	return context.WithValue(ctx, areaCheckKey{}, c)
}

// AreaCheckOf is the area check on ctx (nil = none: not a call-center
// delivery with an address to check).
func AreaCheckOf(ctx context.Context) *AreaCheck {
	if c, ok := ctx.Value(areaCheckKey{}).(AreaCheck); ok {
		return &c
	}
	return nil
}

// OutOfAreaEntry journals an acknowledged out-of-area delivery under the
// agent who took it.
func OutOfAreaEntry(c AreaCheck, staffID, deviceID, orderID string) Entry {
	d := map[string]any{"acked": true}
	if c.Area.DistanceMi != nil {
		d["distance_mi"] = *c.Area.DistanceMi
	}
	raw, _ := json.Marshal(d)
	return Entry{StaffID: staffID, DeviceID: deviceID, Action: ActionOutOfArea, OrderID: orderID, Detail: raw}
}

// ActionDraftTakeOverRefused journals a call-center take-over of a draft a
// store terminal holds (refused: the store wins). ActionReprint: the
// customer receipt printed again from a terminal.
const (
	ActionDraftTakeOverRefused = "draft.take_over_refused"
	ActionReprint              = "reprint"
	// ActionOutOfArea: a call-center delivery outside the store's area,
	// acknowledged by the agent {distance_mi, acked: true}.
	ActionOutOfArea = "delivery.out_of_area"
)

func init() {
	reserved[ActionDraftTakeOverRefused], reserved[ActionReprint], reserved[ActionOutOfArea] = true, true, true
}
