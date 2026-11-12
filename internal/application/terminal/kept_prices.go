package terminal

import (
	"encoding/json"
	"time"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Editing a sent order keeps its prices (contract/agent.md §2.10s §6, the
// owner 2026-11-12, HARDENING-PLAN M-15): a line the order already had keeps
// the price it was rung at; only new lines are priced at today's menu.
//
//   - Time: a cart that no longer prices now (a deal window ended, a daypart
//     item) is priced at the order's placed time instead, when that works.
//   - Menu price: a kept plain line (not a deal, a custom line, a half and
//     half or a price already set on it) whose item's price changed since is
//     priced at its rung base price (a price override the Pi makes itself:
//     never a price change needing a manager, never journaled). Its options
//     price at today's menu.

// storedCart is the cart the order was last placed / changed with.
func storedCart(snap order.Snapshot) (Cart, bool) {
	tm, ok := snap.SourceMeta["terminal"].(map[string]any)
	if !ok || tm["cart"] == nil {
		return Cart{}, false
	}
	raw, err := json.Marshal(tm["cart"])
	if err != nil {
		return Cart{}, false
	}
	var c Cart
	if json.Unmarshal(raw, &c) != nil {
		return Cart{}, false
	}
	return c, true
}

// lineKey tells cart lines apart for "kept" (quantity and note left out).
func lineKey(l pricing.Line) string {
	l.Qty, l.Note = 0, ""
	raw, _ := json.Marshal(l)
	return string(raw)
}

// rungBases maps the stored cart's plain lines to the base price they were
// rung at (the stored order's items, built one per plain line in cart order;
// nil when the two don't line up).
func rungBases(snap order.Snapshot, c Cart, ctx pricing.Context) map[string][]int64 {
	out := map[string][]int64{}
	idx := 0
	for _, l := range c.Lines {
		switch {
		case l.Custom:
			idx++
		case l.DealID != "":
			idx += len(l.Picks)
		default:
			if idx >= len(snap.Items) {
				return nil
			}
			it := snap.Items[idx]
			idx++
			r := pricing.Resolve(ctx, l.Selection, 0)
			if r.Name != it.Name || r.SizeName != it.Size {
				return nil // the lines don't match: keep today's prices
			}
			if l.PriceCents == nil && len(l.Halves) == 0 {
				k := lineKey(l)
				out[k] = append(out[k], it.UnitPrice)
			}
		}
	}
	return out
}

// keepRungPrices prices a changed cart for a placed order: the cart with
// kept plain lines at their rung base price, at now or (when now fails) the
// order's placed time. implicit = the cart lines the Pi set a price on.
func keepRungPrices(menu *terminal.CounterMenu, tz *time.Location, typ string, cur order.Snapshot, in Cart, now time.Time,
	maxRepeat int) (p priced, implicit map[int]bool, err error) {
	first, err := priceCart(menu, tz, typ, in, now, maxRepeat)
	if err != nil {
		return first, nil, err
	}
	cart := in
	implicit = map[int]bool{}
	if stored, ok := storedCart(cur); ok {
		if bases := rungBases(cur, stored, first.ctx); bases != nil {
			cart.Lines = append([]pricing.Line(nil), in.Lines...)
			for i, l := range cart.Lines {
				if l.Custom || l.DealID != "" || l.PriceCents != nil || len(l.Halves) > 0 {
					continue
				}
				k := lineKey(l)
				if len(bases[k]) == 0 {
					continue
				}
				b := bases[k][0]
				bases[k] = bases[k][1:]
				if pricing.Resolve(first.ctx, l.Selection, 0).BaseCents == b {
					continue // the price didn't change
				}
				b2 := b
				cart.Lines[i].PriceCents = &b2
				implicit[i] = true
			}
		}
	}
	p = first
	if len(implicit) > 0 {
		if p, err = priceCart(menu, tz, typ, cart, now, maxRepeat); err != nil {
			return p, nil, err
		}
	}
	if len(p.quote.Errors) > 0 && !cur.PlacedAt.IsZero() {
		if alt, err := priceCart(menu, tz, typ, cart, cur.PlacedAt, maxRepeat); err == nil && len(alt.quote.Errors) == 0 {
			p = alt
		}
	}
	// The Pi's own overrides are not price changes.
	var changes []terminal.PriceChange
	for _, c := range p.changes {
		if !implicit[c.Line] {
			changes = append(changes, c)
		}
	}
	p.changes = changes
	return p, implicit, nil
}
