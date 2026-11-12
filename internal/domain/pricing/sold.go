package pricing

// Order lines from a quote with coupons (main's channel orders and the Pi's
// counter orders build them the same way). A coupon re-prices its units
// (never a discount), so a cart line whose units are in coupons becomes one
// order line per price it sold at: the units left at the line's own price,
// then each coupon's units priced alike. Their totals add up to the quote
// line's total_cents, and the order's subtotal to the quote's.

// SoldGroup is a run of one cart line's units sold at one price.
type SoldGroup struct {
	Qty int
	// Coupon is the coupon's index in Quote.Coupons (-1: the line's own
	// price); Name its name ("" when none).
	Coupon int
	Name   string
	// UnitCents is one unit as sold (a coupon unit's unit_cents; the line's
	// own unit_cents otherwise); FreeMods a coupon unit's free regular mods.
	UnitCents int64
	FreeMods  []string
}

// SoldGroups splits cart line i (qty units) into the units left at the
// line's own price (first, when any) and its coupon units, grouped by
// coupon and price, in coupon order.
func SoldGroups(q Quote, i, qty int) []SoldGroup {
	var unit int64
	if i >= 0 && i < len(q.Lines) {
		unit = q.Lines[i].UnitCents
	}
	var groups []SoldGroup
	used := 0
	for ci, c := range q.Coupons {
		for _, u := range c.Units {
			if u.Line != i {
				continue
			}
			used++
			found := false
			for k := range groups {
				g := &groups[k]
				if g.Coupon == ci && g.UnitCents == u.UnitCents && sameLabels(g.FreeMods, u.FreeMods) {
					g.Qty++
					found = true
					break
				}
			}
			if !found {
				groups = append(groups, SoldGroup{Qty: 1, Coupon: ci, Name: c.Name, UnitCents: u.UnitCents, FreeMods: append([]string{}, u.FreeMods...)})
			}
		}
	}
	if rest := qty - used; rest > 0 || len(groups) == 0 {
		groups = append([]SoldGroup{{Qty: max(rest, 1), Coupon: -1, UnitCents: unit}}, groups...)
	}
	return groups
}

func sameLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

// FreeOptions marks a coupon unit's free regular mods (its free_mods, by
// label, each label once per free mod) Included on a copy of the line's
// resolved options, so its order line charges only the others.
func FreeOptions(opts []ResolvedOption, free []string) []ResolvedOption {
	out := append([]ResolvedOption{}, opts...)
	left := map[string]int{}
	for _, l := range free {
		left[l]++
	}
	for k := range out {
		o := &out[k]
		if o.PriceCents <= 0 || o.RemovedDefault || o.Mode == ModeSpecialty || o.Included || left[o.Label] == 0 {
			continue
		}
		left[o.Label]--
		o.Included = true
	}
	return out
}
