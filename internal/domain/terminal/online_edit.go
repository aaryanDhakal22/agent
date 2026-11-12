package terminal

import (
	"fmt"
	"strings"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
)

// Online orders = called-in orders (agent contract §2.10aa): an online /
// website order is edited at a terminal like ours. Its own lines reach the
// terminal as custom lines (OnlineCartLines); a PUT cart is matched back to
// them (MatchOnline), the rest is added at our price; its money is its own
// with the removed lines (and their share of its tax) taken off and the added
// ones put on (OnlineMoney). Pure: tests in online_edit_test.go.

// Custom line limits (the engine's).
const onlineNoteMax = 140

// OnlineLine is one of an online order's own top-level lines as the
// terminals see it: a custom line at its unit price with its modifiers.
type OnlineLine struct {
	Item  order.LineItem
	Name  string
	Note  string
	Unit  int64 // cents for one, modifiers included
	Qty   int
	Index int
}

// OnlineLines are the order's own lines as custom lines.
func OnlineLines(items []order.LineItem) []OnlineLine {
	out := make([]OnlineLine, 0, len(items))
	for i, it := range items {
		qty := max(it.Quantity, 1)
		unit := it.UnitPrice
		for _, c := range it.Children {
			unit += c.UnitPrice * int64(max(c.Quantity, 1))
		}
		if it.LineTotal > 0 && it.LineTotal%int64(qty) == 0 {
			unit = it.LineTotal / int64(qty)
		}
		out = append(out, OnlineLine{Item: it, Name: onlineName(it), Note: onlineNote(it), Unit: max(unit, 0), Qty: qty, Index: i})
	}
	return out
}

func onlineName(it order.LineItem) string {
	n := strings.TrimSpace(it.Name)
	if sz := strings.TrimSpace(it.Size); sz != "" && !strings.EqualFold(sz, "serving") {
		n = sz + " " + n
	}
	return cut(n, pricing.MaxCustomName)
}

func onlineNote(it order.LineItem) string {
	var parts []string
	for _, c := range it.Children {
		n := strings.TrimSpace(c.Name)
		if c.Quantity > 1 {
			n = fmt.Sprintf("%dx %s", c.Quantity, n)
		}
		if n != "" {
			parts = append(parts, n)
		}
	}
	note := strings.Join(parts, ", ")
	if n := strings.TrimSpace(it.Notes); n != "" {
		if note != "" {
			note += "; "
		}
		note += n
	}
	return cut(note, onlineNoteMax)
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return strings.TrimSpace(string(r[:n]))
	}
	return s
}

// OnlineCartLines is the cart a terminal edits an unedited online order
// with: one custom line per own line.
func OnlineCartLines(items []order.LineItem) []pricing.Line {
	var out []pricing.Line
	for _, l := range OnlineLines(items) {
		price := l.Unit
		out = append(out, pricing.Line{Custom: true, Name: l.Name, Note: l.Note, Qty: l.Qty, Selection: pricing.Selection{PriceCents: &price}})
	}
	return out
}

// OnlineMatch is a PUT cart matched against an online order's own lines:
// how many of each are kept (by OnlineLine.Index), and the cart left to add
// at our price.
type OnlineMatch struct {
	Kept  map[int]int
	Added []pricing.Line
	// AddedFrom: each added line's index in the cart.
	AddedFrom []int
}

// MatchOnline keeps each custom cart line that names one of the order's own
// lines (same name, unit price and note), up to that line's quantity; extra
// quantity and every other line are added.
func MatchOnline(own []OnlineLine, cart []pricing.Line) OnlineMatch {
	m := OnlineMatch{Kept: map[int]int{}}
	left := map[int]int{}
	for _, l := range own {
		left[l.Index] = l.Qty
	}
	for ci, c := range cart {
		qty := max(c.Qty, 1)
		if c.Custom && c.PriceCents != nil {
			for _, l := range own {
				if qty == 0 {
					break
				}
				if left[l.Index] == 0 || strings.TrimSpace(c.Name) != l.Name || strings.TrimSpace(c.Note) != l.Note || *c.PriceCents != l.Unit {
					continue
				}
				take := min(qty, left[l.Index])
				left[l.Index] -= take
				m.Kept[l.Index] += take
				qty -= take
			}
		}
		if qty > 0 {
			add := c
			add.Qty = qty
			m.Added = append(m.Added, add)
			m.AddedFrom = append(m.AddedFrom, ci)
		}
	}
	return m
}

// OnlineMoneyIn is what an online order's new money is made of.
type OnlineMoneyIn struct {
	Base  order.OnlineBase
	Own   []OnlineLine
	Kept  map[int]int
	Added order.Money // the added lines priced as a pickup (no delivery fee); zero = none
}

// OnlineMoneyOut is the new money and the order's own lines that stay.
type OnlineMoneyOut struct {
	Money        order.Money
	Items        []order.LineItem // kept own lines (a partly kept one with its new quantity)
	RemovedCents int64            // the removed own lines' value (no tax)
	RemovedTax   int64
}

// OnlineMoney is the online order's money after an edit: its own money less
// the removed lines and their share of its tax, plus the added lines' total
// (less any tip the pricing put on it); its fees, tip and discounts stay.
func OnlineMoney(in OnlineMoneyIn) OnlineMoneyOut {
	var out OnlineMoneyOut
	for _, l := range in.Own {
		kept := min(in.Kept[l.Index], l.Qty)
		if gone := l.Qty - kept; gone > 0 {
			out.RemovedCents += l.Unit * int64(gone)
		}
		if kept == 0 {
			continue
		}
		it := l.Item
		if kept != l.Qty {
			it.Quantity = kept
			it.LineTotal = l.Unit * int64(kept)
		}
		out.Items = append(out.Items, it)
	}
	b := in.Base.Money
	if b.Subtotal > 0 && out.RemovedCents > 0 {
		out.RemovedTax = (b.Tax*out.RemovedCents + b.Subtotal/2) / b.Subtotal
		out.RemovedTax = min(out.RemovedTax, b.Tax)
	}
	a := in.Added
	addedTotal := a.Total - a.Tip
	m := order.Money{Subtotal: b.Subtotal - out.RemovedCents + a.Subtotal, Tax: b.Tax - out.RemovedTax + a.Tax, Tip: b.Tip,
		Fees:      append(append([]order.Fee(nil), b.Fees...), a.Fees...),
		Discounts: append(append([]order.Discount(nil), b.Discounts...), a.Discounts...),
		Total:     b.Total - out.RemovedCents - out.RemovedTax + addedTotal}
	if len(m.Fees) == 0 {
		m.Fees = nil
	}
	if len(m.Discounts) == 0 {
		m.Discounts = nil
	}
	out.Money = m
	return out
}

// OnlineOwed is what an edited online order owes its customer and what is
// still due: paidOnline (what it arrived paid with) + terminalNet (what the
// terminals took) against its new total.
func OnlineOwed(total, paidOnline, terminalNet int64) (owed, due int64) {
	return max(paidOnline+terminalNet-total, 0), max(total-paidOnline-terminalNet, 0)
}

// RefundDue is one change's refund owed to an online order's customer (the
// REFUND DUE slip, journal refund.owed, main's order.changed refund_due).
type RefundDue struct {
	Cents      int64           `json:"cents"`
	TotalOwed  int64           `json:"total_owed_cents"`
	Lines      []RefundDueLine `json:"lines"`
	Reference  string          `json:"reference"`
	Source     string          `json:"source"`
	Number     int64           `json:"number"`
	StaffName  string          `json:"staff_name"`
	DeviceName string          `json:"device_name"`
	At         string          `json:"at"`
}

// RefundDueLine is a removed line on the slip.
type RefundDueLine struct {
	Name  string `json:"name"`
	Qty   int    `json:"qty"`
	Cents int64  `json:"cents"`
}

// ActionRefundOwed journals a refund owed to an online order's customer.
const ActionRefundOwed = "refund.owed"

func init() { reserved[ActionRefundOwed] = true }

// OnlineBaseDue is what an order from main owed at the counter / door before
// what the terminals took (terminalNet), for the Z and the till: the one cash
// rule; for an online order edited at a terminal, its total less what it
// arrived paid with as far as that still covers it (a refund owed lowers the
// online money counted, so the identities hold with no money moved).
func OnlineBaseDue(snap order.Snapshot, terminalNet int64) int64 {
	ob := snap.OnlineBase()
	if ob == nil {
		return snap.CashDue()
	}
	return snap.Money.Total - min(ob.PaidCents, max(snap.Money.Total-terminalNet, 0))
}
