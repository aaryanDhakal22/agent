package terminal

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Prices don't move under an order being taken (the owner, 2026-11-12;
// contract/agent.md §2.10u, extending §2.10s §6 / M-15).
//
//   - A cart line may carry Rung: the unit price (rung_cents), name and mods
//     the terminal first got for that line from a quote. When the menu's
//     price moved since, the Pi prices the line at rung_cents (a price
//     override the Pi makes itself: never a price change, no manager; the
//     options and halves are in it too, as the quote's unit_cents was).
//   - A line whose item, size, choice, half, option or deal is no longer on
//     the menu is priced as an open-price custom line at rung_cents named
//     rung_name (the mods go in its note), journaled menu_gone_line, never
//     an error. Without rung_cents such a line is still a 422.
//   - A sent order's own lines (PUT .../cart, quote with order_id) keep the
//     price they were charged (the stored line's unit price, options and
//     halves included) and become a custom line the same way when gone.
//   - A cart that no longer prices now (a deal window ended) is priced at
//     the order's placed time instead, when that works (sent orders only).

// Rung is what a terminal got for a cart line from its first quote.
type Rung struct {
	Cents *int64   `json:"rung_cents,omitempty"`
	Name  string   `json:"rung_name,omitempty"`
	Mods  []string `json:"rung_mods,omitempty"`
}

// cartWire is the Cart without its rungs (the JSON the engine knows).
type cartWire struct {
	pricing.Cart
	Discount *Discount `json:"discount,omitempty"`
}

// UnmarshalJSON reads the cart and each line's rung_* fields.
func (c *Cart) UnmarshalJSON(b []byte) error {
	var w cartWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	var r struct {
		Lines []Rung `json:"lines"`
	}
	_ = json.Unmarshal(b, &r) // the lines decoded above: this can't fail on them
	c.Cart, c.Discount, c.Rungs = w.Cart, w.Discount, nil
	for _, x := range r.Lines {
		if x.Cents != nil {
			c.Rungs = r.Lines
			break
		}
	}
	return nil
}

// MarshalJSON writes the cart back with each line's rung_* fields. A cart
// without any is encoded exactly as before (stored fingerprints stay).
func (c Cart) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(cartWire{Cart: c.Cart, Discount: c.Discount})
	if err != nil || len(c.Rungs) == 0 {
		return raw, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	var lines []map[string]json.RawMessage
	if err := json.Unmarshal(m["lines"], &lines); err != nil {
		return nil, err
	}
	for i := range lines {
		if i >= len(c.Rungs) || c.Rungs[i].Cents == nil {
			continue
		}
		extra, _ := json.Marshal(c.Rungs[i])
		var em map[string]json.RawMessage
		_ = json.Unmarshal(extra, &em)
		for k, v := range em {
			lines[i][k] = v
		}
	}
	if m["lines"], err = json.Marshal(lines); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

// checkRungs: a rung price is 0 to the custom-line limit.
func (c Cart) checkRungs() error {
	for _, r := range c.Rungs {
		if r.Cents != nil && (*r.Cents < 0 || *r.Cents > pricing.MaxCustomCents) {
			return badRequest("rung_cents is 0-999999")
		}
	}
	return nil
}

// rungAt is a line's price to keep: its unit price, and what to call it
// when its item is gone.
type rungAt struct {
	unit int64
	name string
	mods []string
	note *string // a stored custom line's own note (mods already in it)
	from string  // terminal | order
}

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

// goneCodes are the engine's issues for something no longer on the menu.
var goneCodes = map[string]bool{pricing.IssueUnknownItem: true, pricing.IssueUnknownSize: true, pricing.IssueUnknownOpt: true}

// goneReason: why a line can't be sold from today's menu any more ("" =
// it can).
func goneReason(ctx pricing.Context, l pricing.Line) string {
	switch {
	case l.Custom:
		return ""
	case l.DealID != "":
		var deal *pricing.Deal
		for k := range ctx.Deals {
			if ctx.Deals[k].ID == l.DealID && ctx.Deals[k].Kind == pricing.DealBundle {
				deal = &ctx.Deals[k]
			}
		}
		if deal == nil || !deal.Active {
			return pricing.IssueDealEnded
		}
		for _, pk := range l.Picks {
			for _, is := range pricing.Resolve(ctx, pk, 0).Issues {
				if goneCodes[is.Code] {
					return is.Code
				}
			}
		}
		return ""
	}
	for _, is := range pricing.Resolve(ctx, l.Selection, 0).Issues {
		if goneCodes[is.Code] {
			return is.Code
		}
	}
	return ""
}

// storedRungs maps the stored cart's lines to what they were charged (the
// stored order's items, one per plain line in cart order, a bundle's picks
// one each; nil when the two don't line up). A line sold as a custom line
// because its item was gone keeps that custom line.
func storedRungs(snap order.Snapshot, c Cart, ctx pricing.Context) map[string][]rungAt {
	out := map[string][]rungAt{}
	idx := 0
	for _, l := range c.Lines {
		if idx >= len(snap.Items) {
			return nil
		}
		it := snap.Items[idx]
		switch {
		case l.Custom:
			idx++
		case it.SKU == order.SKUCustom: // a gone line, sold as a custom line
			idx++
			note := it.Notes
			k := lineKey(l)
			out[k] = append(out[k], rungAt{unit: it.UnitPrice, name: it.Name, note: &note, from: terminal.RungFromOrder})
		case l.DealID != "":
			idx += len(l.Picks)
		default:
			idx++
			r := pricing.Resolve(ctx, l.Selection, 0)
			if goneReason(ctx, l) == "" && (r.Name != it.Name || r.SizeName != it.Size) {
				return nil // the lines don't match: keep today's prices
			}
			mods := []string{}
			if it.Size != "" {
				mods = append(mods, it.Size)
			}
			for _, ch := range it.Children {
				if ch.Name != "" {
					mods = append(mods, ch.Name)
				}
			}
			k := lineKey(l)
			out[k] = append(out[k], rungAt{unit: it.LineTotal / int64(max(it.Quantity, 1)), name: it.Name, mods: mods,
				from: terminal.RungFromOrder})
		}
	}
	return out
}

// rungsOf is each cart line's price to keep: a sent order's own line first
// (what it was charged), else the line's rung_cents.
func rungsOf(in Cart, cur *order.Snapshot, ctx pricing.Context) map[int]rungAt {
	out := map[int]rungAt{}
	for i, l := range in.Lines {
		if l.Custom || i >= len(in.Rungs) || in.Rungs[i].Cents == nil {
			continue
		}
		r := in.Rungs[i]
		out[i] = rungAt{unit: *r.Cents, name: r.Name, mods: r.Mods, from: terminal.RungFromTerminal}
	}
	if cur == nil {
		return out
	}
	stored, ok := storedCart(*cur)
	if !ok {
		return out
	}
	kept := storedRungs(*cur, stored, ctx)
	if kept == nil {
		return out
	}
	for i, l := range in.Lines {
		if l.Custom || l.DealID != "" {
			continue
		}
		k := lineKey(l)
		if q := kept[k]; len(q) > 0 {
			out[i], kept[k] = q[0], q[1:]
		}
	}
	return out
}

// goneLine is the custom line a gone line is sold as: the rung's name (cut
// to the custom-line limit) at its unit price, its mods and note as the note.
func goneLine(l pricing.Line, r rungAt) pricing.Line {
	name := strings.TrimSpace(r.name)
	if utf8.RuneCountInString(name) > pricing.MaxCustomName {
		name = strings.TrimSpace(string([]rune(name)[:pricing.MaxCustomName]))
	}
	price := min(max(r.unit, 0), pricing.MaxCustomCents)
	var note string
	if r.note != nil {
		note = *r.note
	} else {
		own := strings.TrimSpace(l.Note)
		var parts []string
		for _, m := range r.mods {
			m = strings.TrimSpace(m)
			if m == "" || m == "“"+own+"”" { // the quote shows the note as a mod
				continue
			}
			parts = append(parts, m)
		}
		note = strings.Join(parts, ", ")
		if own != "" {
			if note != "" {
				note += "; "
			}
			note += own
		}
	}
	if utf8.RuneCountInString(note) > 200 {
		note = string([]rune(note)[:200])
	}
	return pricing.Line{Custom: true, Name: name, Qty: l.Qty, Note: note, Selection: pricing.Selection{PriceCents: &price}}
}

// counterCtx is the pricing context priceCart prices with.
func counterCtx(menu *terminal.CounterMenu, tz *time.Location, maxRepeat int) pricing.Context {
	ctx := menu.Pricing
	ctx.Types = pricing.Types{Pickup: true, Delivery: true}
	ctx.PriceOverrides = true
	ctx.Counter = true
	ctx.MaxRepeat = maxRepeat
	if ctx.Timezone == "" && tz != nil {
		ctx.Timezone = tz.String()
	}
	return ctx
}

// priceRung prices a cart keeping its lines' rung prices (cur = the sent
// order being changed, nil for a new one). implicit = the cart lines the Pi
// set a price on; p.gone / p.kept say what it did.
func priceRung(menu *terminal.CounterMenu, tz *time.Location, typ string, cur *order.Snapshot, in Cart, now time.Time,
	maxRepeat int) (p priced, implicit map[int]bool, err error) {
	if err := in.checkRungs(); err != nil {
		return priced{}, nil, err
	}
	ctx := counterCtx(menu, tz, maxRepeat)
	rungs := rungsOf(in, cur, ctx)
	idx := make([]int, 0, len(rungs))
	for i := range rungs {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	cart := in
	cart.Lines = append([]pricing.Line(nil), in.Lines...)
	// 1. Lines no longer on the menu: custom lines at their rung price.
	var gone []terminal.GoneLine
	for _, i := range idx {
		l, r := cart.Lines[i], rungs[i]
		reason := goneReason(ctx, l)
		if reason == "" {
			continue
		}
		cart.Lines[i] = goneLine(l, r)
		gone = append(gone, terminal.GoneLine{Line: i, ItemID: l.ItemID, DealID: l.DealID, Name: cart.Lines[i].Name,
			PriceCents: *cart.Lines[i].PriceCents, Qty: min(max(l.Qty, 1), 50), Reason: reason, From: r.from})
	}
	first, err := priceCart(menu, tz, typ, cart, now, maxRepeat)
	if err != nil {
		return first, nil, err
	}
	// 2. Lines whose menu price moved: priced at the rung unit price (the
	// override replaces the item's price with its halves; its options still
	// add, so the override is the rung less today's options).
	implicit = map[int]bool{}
	var kept []terminal.KeptPrice
	for _, i := range idx {
		l, r := cart.Lines[i], rungs[i]
		if l.Custom || l.DealID != "" || l.PriceCents != nil || i >= len(first.quote.Lines) {
			continue
		}
		ql := first.quote.Lines[i]
		if ql.UnitCents == r.unit {
			continue
		}
		res := pricing.Resolve(first.ctx, l.Selection, 0)
		if len(res.Issues) > 0 {
			continue // sold out, or a rule broken: the engine says so
		}
		base := min(max(r.unit-(res.UnitCents-res.BaseCents), 0), pricing.MaxOpenPriceCents)
		cart.Lines[i].PriceCents = &base
		implicit[i] = true
		kept = append(kept, terminal.KeptPrice{Line: i, ItemID: l.ItemID, Name: ql.Name, MenuCents: ql.UnitCents, RungCents: r.unit, From: r.from})
	}
	p = first
	if len(implicit) > 0 {
		if p, err = priceCart(menu, tz, typ, cart, now, maxRepeat); err != nil {
			return p, nil, err
		}
	}
	if len(p.quote.Errors) > 0 && cur != nil && !cur.PlacedAt.IsZero() {
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
	p.changes, p.gone, p.kept = changes, gone, kept
	return p, implicit, nil
}

// isGone: the cart line was sold as a custom line because its item is gone.
func (p priced) isGone(i int) bool {
	for _, g := range p.gone {
		if g.Line == i {
			return true
		}
	}
	return false
}

// rungEntries journals the gone lines and the kept prices of a placed /
// changed order. kept = the order's earlier custom lines (customLineKey →
// count): a gone line it had already isn't journaled again; a price kept
// from the order itself never is (M-15).
func rungEntries(a Actor, st terminal.Staff, p priced, orderID string, keptCustom map[string]int) []terminal.Entry {
	var out []terminal.Entry
	for _, g := range p.gone {
		if k := customLineKey(g.Name, g.PriceCents); keptCustom[k] > 0 {
			keptCustom[k]--
			continue
		}
		d := map[string]any{"line": g.Line, "name": g.Name, "price_cents": g.PriceCents, "qty": g.Qty, "reason": g.Reason, "from": g.From}
		if g.ItemID != "" {
			d["item_id"] = g.ItemID
		}
		if g.DealID != "" {
			d["deal_id"] = g.DealID
		}
		out = append(out, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionMenuGoneLine, OrderID: orderID, Detail: detail(d)})
	}
	for _, k := range p.kept {
		if k.From != terminal.RungFromTerminal {
			continue
		}
		qty := 1
		if k.Line < len(p.cart.Lines) {
			qty = min(max(p.cart.Lines[k.Line].Qty, 1), 50)
		}
		out = append(out, terminal.Entry{StaffID: st.ID, DeviceID: a.DeviceID, Action: terminal.ActionPriceKept, OrderID: orderID,
			Detail: detail(map[string]any{"line": k.Line, "item_id": k.ItemID, "name": k.Name, "menu_cents": k.MenuCents,
				"rung_cents": k.RungCents, "qty": qty})})
	}
	return out
}
