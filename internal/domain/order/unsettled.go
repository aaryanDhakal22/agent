package order

import (
	"fmt"
	"time"
)

// Closing the day with money still due (contract/agent.md §2.10s §4, the
// owner 2026-11-12): an unsettled check is an order of the open batch (or a
// carried one) that isn't cancelled and still has money due, except a
// delivery already delivered (its driver collected it).

// UnsettledCheck is one line of the Unsettled Check List.
type UnsettledCheck struct {
	OrderID     string              `json:"order_id"`
	Number      int64               `json:"number"`
	Type        string              `json:"type"` // counter | pickup | delivery
	Source      string              `json:"source"`
	State       State               `json:"state"`
	PlacedAt    time.Time           `json:"placed_at"`
	BatchID     int64               `json:"batch_id"`
	Carried     bool                `json:"carried"`
	Customer    UnsettledCustomer   `json:"customer"`
	StaffID     string              `json:"staff_id"`
	StaffName   string              `json:"staff_name"`
	TotalCents  int64               `json:"total_cents"`
	PaidCents   int64               `json:"paid_cents"`
	DueCents    int64               `json:"due_cents"`
	PickedUp    bool                `json:"picked_up"`
	BlocksClose bool                `json:"blocks_close"`
	Items       []UnsettledLineItem `json:"items"`
}

// UnsettledCustomer is the check's customer (empty strings when none).
type UnsettledCustomer struct {
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

// UnsettledLineItem is one line of an unsettled check.
type UnsettledLineItem struct {
	Qty        int      `json:"qty"`
	Name       string   `json:"name"`
	Mods       []string `json:"mods"`
	PriceCents int64    `json:"price_cents"`
}

// UnsettledList is GET /api/terminal/unsettled and a close's `unsettled`.
type UnsettledList struct {
	BatchID     int64            `json:"batch_id"`
	GeneratedAt time.Time        `json:"generated_at"`
	Count       int              `json:"count"`
	DueCents    int64            `json:"due_cents"`
	Blocking    int              `json:"blocking"`
	Checks      []UnsettledCheck `json:"checks"`
}

// UnsettledError: a manual close refused while an unpaid counter check is
// open (409 unsettled_checks {checks}).
type UnsettledError struct{ Checks []UnsettledCheck }

func (e *UnsettledError) Error() string {
	return fmt.Sprintf("%d unpaid counter check(s) are open: take the payment or void them, then close", len(e.Checks))
}

// TerminalType is a terminal order's type (counter | pickup | delivery); an
// online order's is its fulfillment's (pickup | delivery).
func TerminalType(so StoredOrder) string {
	switch {
	case so.IsDelivery():
		return "delivery"
	case so.Order.Source == SourceCounter && so.Order.Fulfillment.Type == "dine_in":
		return "counter"
	}
	return "pickup"
}

// IsUnsettled: not cancelled, money due, and not a delivered delivery.
func IsUnsettled(so StoredOrder) bool {
	if so.Status.State == StateCancelled || so.Status.CashDueCents <= 0 {
		return false
	}
	if so.Order.Source != SourceCounter && so.Status.Paid {
		return false
	}
	return !(so.IsDelivery() && so.Status.State == StateDelivered)
}

// BlocksClose: an unsettled counter (dine-in) check blocks a manual close.
func BlocksClose(so StoredOrder) bool {
	return IsUnsettled(so) && TerminalType(so) == "counter"
}

// Carries: an unsettled check a close carries to the next batch (still
// payable): a pickup at a manual close, every non-delivery one at an
// auto-close (contract 2.10s §4). A delivery is finished as before.
func Carries(so StoredOrder, auto bool) bool {
	if !IsUnsettled(so) || so.IsDelivery() {
		return false
	}
	return auto || TerminalType(so) == "pickup"
}

// Settled: a carried check is done with - cancelled, or nothing due and
// (a pickup) picked up.
func Settled(so StoredOrder) bool {
	if so.Status.State == StateCancelled {
		return true
	}
	if IsUnsettled(so) {
		return false
	}
	if TerminalType(so) == "pickup" {
		return so.Status.FinishedAt != nil
	}
	return true
}

// UnsettledCheckOf builds an order's line on the list (staffName resolves a
// staff id; nil = none).
func UnsettledCheckOf(so StoredOrder, staffName func(id string) string) UnsettledCheck {
	snap := so.Order
	c := UnsettledCheck{OrderID: snap.ID, Type: TerminalType(so), Source: snap.Source, State: so.Status.State,
		PlacedAt: snap.PlacedAt, BatchID: so.Status.BatchID, Carried: so.Status.Carried, TotalCents: snap.Money.Total,
		DueCents: max(so.Status.CashDueCents, 0), PickedUp: so.Status.FinishedAt != nil, Items: []UnsettledLineItem{}}
	if c.PlacedAt.IsZero() {
		c.PlacedAt = so.Status.ArrivalDate
	}
	if so.Status.KitchenNumber != nil {
		c.Number = *so.Status.KitchenNumber
	}
	base := snap.Money.Total
	if snap.Source != SourceCounter {
		base = snap.CashDue()
	}
	c.PaidCents = max(base-c.DueCents, 0)
	c.Customer = UnsettledCustomer{Name: snap.Customer.DisplayName, Phone: snap.Customer.Phone}
	if t, ok := snap.SourceMeta["terminal"].(map[string]any); ok {
		if id, ok := t["staff_id"].(string); ok {
			c.StaffID = id
		}
		if n, ok := t["staff_name"].(string); ok {
			c.StaffName = n
		}
	}
	if c.StaffName == "" && c.StaffID != "" && staffName != nil {
		c.StaffName = staffName(c.StaffID)
	}
	for _, it := range snap.Items {
		li := UnsettledLineItem{Qty: max(it.Quantity, 1), Name: it.Name, Mods: []string{}, PriceCents: it.LineTotal}
		for _, ch := range it.Children {
			li.Mods = append(li.Mods, ch.Name)
		}
		c.Items = append(c.Items, li)
	}
	c.BlocksClose = BlocksClose(so)
	return c
}
