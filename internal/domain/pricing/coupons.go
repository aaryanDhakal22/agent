package pricing

import (
	"sort"
	"time"
)

// Coupons (the owner's Centurion pass, 2026-11-12; contract/main.md
// "Families + coupons"). A coupon is a deal of kind coupon: parts of
// families × qty at one price. It applies only when the units it takes fill
// every part; the highest-priced item carries the coupon price and the
// others cost $0.00; on each unit the part's N cheapest regular mods are
// free, every other mod is charged. Each cart line unit is in at most one
// coupon. Keep this file in step with engine.ts (couponLine … coupons).

// MaxCouponSlots bounds the units one coupon takes (8 parts × 10).
const MaxCouponSlots = 80

// cline is a cart item line as coupons and the automatic deals see it.
type cline struct {
	ok          bool // an item line the deals see (a bank item, no issues, no price change)
	qty         int
	used        int   // units in coupons
	unit        int64 // the regular unit price (item + mods)
	base        int64 // the item's own price at its size, before mods
	halfUp      int64 // a half and half upcharge
	item        string
	category    string
	size        string
	family      string // "" = in no coupon
	mods        []cmod
	taxCut      int64 // its coupon units' regular − coupon prices
	couponCents int64 // its coupon units' prices in their coupons
}

// cmod is one charged mod of a line's unit.
type cmod struct {
	price int64
	class string
	label string
}

func (e *engine) couponLine(p priced, qty int) cline {
	c := cline{ok: true, qty: qty, unit: p.unit, base: p.base, halfUp: p.halfUp, item: p.bankItem, category: e.category(p.bankItem),
		size: p.size, family: e.ctx.Items[p.bankItem].Family}
	for k, o := range p.opts {
		if o.PriceCents <= 0 || o.RemovedDefault || o.Mode == ModeSpecialty {
			continue
		}
		id := ""
		if k < len(p.optIDs) {
			id = p.optIDs[k]
		}
		c.mods = append(c.mods, cmod{price: o.PriceCents, class: e.ctx.Options[id].Class, label: o.Label})
	}
	return c
}

// unitIn prices one unit of the line on a part with n free regular mods:
// the n cheapest regular mods are free (ties: the first picked), the rest
// and every other mod charged, a half and half upcharge on top.
func (c cline) unitIn(n int) (int64, []string) {
	var regular []int
	for k, m := range c.mods {
		if m.class == ClassRegular {
			regular = append(regular, k)
		}
	}
	sort.SliceStable(regular, func(a, b int) bool { return c.mods[regular[a]].price < c.mods[regular[b]].price })
	free := map[int]bool{}
	for k := 0; k < n && k < len(regular); k++ {
		free[regular[k]] = true
	}
	var mods int64
	labels := []string{}
	for k, m := range c.mods {
		if free[k] {
			labels = append(labels, m.label)
		} else {
			mods += m.price
		}
	}
	if c.halfUp > 0 {
		mods += c.halfUp
	}
	return mods, labels
}

func fitsPart(c cline, pt Part) bool {
	return c.ok && c.family != "" && contains(pt.Families, c.family) && (len(pt.Sizes) == 0 || (c.size != "" && contains(pt.Sizes, c.size)))
}

// couponFit is a coupon matched to units and priced.
type couponFit struct {
	units                 []CouponUnit
	lines                 []int
	regular, total, saved int64
}

// fit matches a coupon's parts to units (cand: one line index per unit, in
// the order they are preferred) so that every part is filled (Kuhn's
// augmenting paths, slots in part order), then prices it.
func fit(d *Deal, cand []int, clines []cline) (couponFit, bool) {
	var slots []int
	for k, pt := range d.Parts {
		for j := 0; j < pt.Qty; j++ {
			slots = append(slots, k)
		}
	}
	if len(slots) == 0 || len(slots) > MaxCouponSlots || len(cand) < len(slots) {
		return couponFit{}, false
	}
	owner := make([]int, len(cand))
	for u := range owner {
		owner[u] = -1
	}
	var try func(s int, seen []bool) bool
	try = func(s int, seen []bool) bool {
		for u, li := range cand { // a free unit first: keep the natural order
			if owner[u] < 0 && fitsPart(clines[li], d.Parts[slots[s]]) {
				owner[u] = s
				return true
			}
		}
		for u, li := range cand {
			if seen[u] || !fitsPart(clines[li], d.Parts[slots[s]]) {
				continue
			}
			seen[u] = true
			if owner[u] < 0 || try(owner[u], seen) {
				owner[u] = s
				return true
			}
		}
		return false
	}
	for s := range slots {
		if !try(s, make([]bool, len(cand))) {
			return couponFit{}, false
		}
	}
	bySlot := make([]int, len(slots))
	for u, s := range owner {
		if s >= 0 {
			bySlot[s] = cand[u]
		}
	}
	// The carrier: the highest-priced item (a tie: the first line, then the first slot).
	carrier := 0
	for s := 1; s < len(slots); s++ {
		a, b := clines[bySlot[s]], clines[bySlot[carrier]]
		if a.base > b.base || (a.base == b.base && bySlot[s] < bySlot[carrier]) {
			carrier = s
		}
	}
	f := couponFit{units: []CouponUnit{}, lines: []int{}}
	for s, k := range slots {
		li := bySlot[s]
		mods, free := clines[li].unitIn(d.Parts[k].RegularModsIncluded)
		var item int64
		if s == carrier {
			item = d.PriceCents
		}
		u := CouponUnit{Line: li, Part: k, Carrier: s == carrier, ItemCents: item, ModsCents: mods, FreeMods: free,
			UnitCents: item + mods, RegularCents: clines[li].unit}
		f.units = append(f.units, u)
		f.lines = append(f.lines, li)
		f.regular += u.RegularCents
		f.total += u.UnitCents
	}
	sort.Ints(f.lines)
	f.saved = f.regular - f.total
	return f, true
}

func cardOf(d *Deal) []string {
	out := []string{}
	return append(out, d.Card...)
}

// coupons applies the cart's coupons (in order; one that is ended, not on
// now or doesn't fit as asked is left out: a warning on the counter, an
// error elsewhere), sets each touched line's coupon_cents, lists the offers
// the units left fully fit, and returns what the applied ones save.
func (e *engine) coupons(q *Quote, cart Cart, clines []cline, at time.Time, issue func(code, msg string, line *int)) int64 {
	ctx := e.ctx
	problem := func(code, msg string) {
		if ctx.Counter {
			if !contains(q.Warnings, msg) {
				q.Warnings = append(q.Warnings, msg)
			}
			return
		}
		issue(code, msg, nil)
	}
	var off int64
	for ci, cc := range cart.Coupons {
		var d *Deal
		for k := range ctx.Deals {
			if ctx.Deals[k].ID == cc.ID && ctx.Deals[k].Kind == DealCoupon {
				d = &ctx.Deals[k]
			}
		}
		if d == nil || !d.Active {
			problem(IssueCouponEnded, "A coupon in the order has ended.")
			continue
		}
		if !Live(*d, ctx.Timezone, at, cart.Type) {
			problem(IssueCouponNotNow, d.Name+" isn't available right now.")
			continue
		}
		taken := map[int]int{}
		bad, busy := false, false
		for _, li := range cc.Lines {
			if li < 0 || li >= len(clines) || !clines[li].ok || clines[li].family == "" {
				bad = true
				break
			}
			taken[li]++
			if clines[li].used+taken[li] > clines[li].qty {
				busy = true
			}
		}
		if bad {
			problem(IssueCouponFit, d.Name+": a line in it can't be in a coupon.")
			continue
		}
		if busy {
			problem(IssueCouponFit, d.Name+": a line in it is already in another coupon.")
			continue
		}
		cand := append([]int{}, cc.Lines...)
		sort.SliceStable(cand, func(a, b int) bool {
			if clines[cand[a]].unit != clines[cand[b]].unit {
				return clines[cand[a]].unit > clines[cand[b]].unit
			}
			return cand[a] < cand[b]
		})
		f, ok := fit(d, cand, clines)
		if !ok || len(cand) != len(f.units) {
			problem(IssueCouponFit, d.Name+" doesn't fit the order.")
			continue
		}
		if f.saved <= 0 {
			problem(IssueCouponFit, d.Name+" doesn't save anything on this order.")
			continue
		}
		for _, u := range f.units {
			c := &clines[u.Line]
			c.used++
			c.taxCut += u.RegularCents - u.UnitCents
			c.couponCents += u.UnitCents
		}
		q.Coupons = append(q.Coupons, AppliedCoupon{Index: ci, ID: d.ID, Name: d.Name, Card: cardOf(d), PriceCents: d.PriceCents, Lines: f.lines,
			RegularCents: f.regular, TotalCents: f.total, SavedCents: f.saved, Units: f.units})
		q.Discounts = append(q.Discounts, Discount{Type: DealCoupon, DealID: d.ID, Label: d.Name, AmountCents: f.saved})
		off += f.saved
	}
	for i, c := range clines {
		if c.used > 0 && i < len(q.Lines) {
			v := int64(c.qty-c.used)*c.unit + c.couponCents
			q.Lines[i].CouponCents = &v
		}
	}
	// Offers: the units not in a coupon, priciest first (a tie: line order).
	var free []int
	for li, c := range clines {
		if !c.ok || c.family == "" {
			continue
		}
		for k := c.used; k < c.qty && k < c.used+MaxCouponSlots; k++ {
			free = append(free, li)
		}
	}
	sort.SliceStable(free, func(a, b int) bool { return clines[free[a]].unit > clines[free[b]].unit })
	for k := range ctx.Deals {
		d := &ctx.Deals[k]
		if d.Kind != DealCoupon || !Live(*d, ctx.Timezone, at, cart.Type) {
			continue
		}
		f, ok := fit(d, free, clines)
		if !ok || f.saved <= 0 {
			continue
		}
		q.CouponOffers = append(q.CouponOffers, CouponOffer{ID: d.ID, Name: d.Name, Card: cardOf(d), PriceCents: d.PriceCents, Lines: f.lines,
			RegularCents: f.regular, TotalCents: f.total, SavedCents: f.saved})
	}
	sort.SliceStable(q.CouponOffers, func(a, b int) bool { return q.CouponOffers[a].SavedCents > q.CouponOffers[b].SavedCents })
	return off
}
