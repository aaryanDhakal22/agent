package terminal

import (
	"errors"
	"fmt"
	"strings"

	"quiccpos/agent/internal/domain/order"
)

// Kitchen changes after SEND (contract §2.10d): a terminal re-sends a placed
// counter order's whole cart; the Pi re-prices it, diffs the lines and
// prints an ADD-ON ticket for what was added and a VOID ticket for what was
// removed.

// EventOrderChanged is the floor event (to main) carrying the changed
// order's full snapshot.
const EventOrderChanged = "order.changed"

// ActionOrderChanged is the journal action of a kitchen change.
const ActionOrderChanged = "order.changed"

var (
	// ErrNotEditable: only a counter order of the open batch that is
	// accepted or ready (not out, delivered or voided) takes a new cart.
	ErrNotEditable = errors.New("this order can't be changed any more")
	// ErrRefundFirst: the new total is below what was paid - give the
	// difference back first.
	ErrRefundFirst = errors.New("the new total is below what was paid: refund the difference first")
)

// MaxEditKeys is how many change client_keys an order remembers.
const MaxEditKeys = 20

// LineSig is a line's kitchen identity: the item, its size, its options
// (name, portion, mode) and its note. Quantity and prices are left out: two
// lines with the same signature are the same thing to the kitchen.
func LineSig(li order.LineItem) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(li.Name))
	b.WriteString("\x1f")
	b.WriteString(strings.TrimSpace(li.Size))
	b.WriteString("\x1f")
	b.WriteString(strings.TrimSpace(li.Notes))
	for _, c := range li.Children {
		b.WriteString("\x1e")
		b.WriteString(LineSig(order.LineItem{Name: c.Name, Children: c.Children}))
		b.WriteString("\x1f")
		b.WriteString(c.Portion)
		b.WriteString("\x1f")
		b.WriteString(c.Mode)
	}
	return b.String()
}

// LineLabel is the feed / journal text of a line: "2× Cheese Pizza (16 XL):
// Pepperoni, No Onion". qty is the quantity to show.
func LineLabel(li order.LineItem, qty int) string {
	s := strings.TrimSpace(li.Name)
	if sz := strings.TrimSpace(li.Size); sz != "" {
		s += " (" + sz + ")"
	}
	var mods []string
	for _, c := range li.Children {
		if n := strings.TrimSpace(c.Name); n != "" {
			mods = append(mods, n)
		}
	}
	if len(mods) > 0 {
		s += ": " + strings.Join(mods, ", ")
	}
	if qty > 1 {
		s = fmt.Sprintf("%d× %s", qty, s)
	}
	return s
}

// LineDelta is a line added or removed by a change: the line (its quantity
// is the difference) and its label.
type LineDelta struct {
	Line  order.LineItem
	Label string
}

// withQty is a copy of li for qty pieces (children follow the parent's
// quantity, except a removed default, which stays 1).
func withQty(li order.LineItem, qty int) order.LineItem {
	out := li
	out.Quantity = qty
	out.LineTotal = li.UnitPrice * int64(qty)
	out.Children = make([]order.LineItem, len(li.Children))
	for i, c := range li.Children {
		if c.Quantity == li.Quantity {
			c.Quantity = qty
		}
		c.LineTotal = c.UnitPrice * int64(c.Quantity)
		out.LineTotal += c.LineTotal
		out.Children[i] = c
	}
	return out
}

// DiffLines compares an order's lines before and after a change by
// signature and quantity: added are the new lines (or extra pieces of a
// line), in the new order's order; removed the lines (or pieces) that went,
// in the old order's order.
func DiffLines(before, after []order.LineItem) (added, removed []LineDelta) {
	qty := func(items []order.LineItem) (map[string]int, []string, map[string]order.LineItem) {
		n := map[string]int{}
		var keys []string
		first := map[string]order.LineItem{}
		for _, it := range items {
			k := LineSig(it)
			if _, ok := n[k]; !ok {
				keys = append(keys, k)
				first[k] = it
			}
			n[k] += max(it.Quantity, 1)
		}
		return n, keys, first
	}
	oldN, oldKeys, oldFirst := qty(before)
	newN, newKeys, newFirst := qty(after)
	for _, k := range newKeys {
		if d := newN[k] - oldN[k]; d > 0 {
			li := withQty(newFirst[k], d)
			added = append(added, LineDelta{Line: li, Label: LineLabel(li, d)})
		}
	}
	for _, k := range oldKeys {
		if d := oldN[k] - newN[k]; d > 0 {
			li := withQty(oldFirst[k], d)
			removed = append(removed, LineDelta{Line: li, Label: LineLabel(li, d)})
		}
	}
	return added, removed
}

// KeepLineIDs gives the new lines that are unchanged (same signature and
// quantity as an old line) the old line's ids, so the tablets and main keep
// tracking them.
func KeepLineIDs(before []order.LineItem, after []order.LineItem) {
	pool := map[string][]order.LineItem{}
	for _, it := range before {
		k := LineSig(it)
		pool[k] = append(pool[k], it)
	}
	for i := range after {
		k := LineSig(after[i])
		for j, old := range pool[k] {
			if old.Quantity != after[i].Quantity {
				continue
			}
			after[i].ID = old.ID
			if len(old.Children) == len(after[i].Children) {
				for c := range after[i].Children {
					after[i].Children[c].ID = old.Children[c].ID
				}
			}
			pool[k] = append(pool[k][:j], pool[k][j+1:]...)
			break
		}
	}
}

// Labels are the deltas' labels.
func Labels(ds []LineDelta) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Label)
	}
	return out
}

// Lines are the deltas' lines.
func Lines(ds []LineDelta) []order.LineItem {
	out := make([]order.LineItem, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Line)
	}
	return out
}

// ChangeCheck decides a kitchen change inside the repository's transaction
// from the order as it is now and its payments: the new snapshot (nil =
// nothing to write) and the journal entries to store with it.
type ChangeCheck func(cur order.StoredOrder, paid []Payment) (*order.Snapshot, []Entry, error)

func init() { reserved[ActionOrderChanged] = true }
