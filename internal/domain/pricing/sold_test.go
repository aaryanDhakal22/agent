package pricing

import "testing"

// Every cart line's sold groups add up to its quote total, the units left
// at the line's own price first, then each coupon's.
func TestSoldGroupsAddUp(t *testing.T) {
	ctx, cart, at := couponCase(t, "57_two_coupons.json")
	q := Price(ctx, cart, at)
	if len(q.Coupons) != 2 {
		t.Fatalf("coupons %v %v", q.Coupons, q.Warnings)
	}
	for i, l := range cart.Lines {
		var sum int64
		n := 0
		gs := SoldGroups(q, i, l.Qty)
		for _, g := range gs {
			sum += g.UnitCents * int64(g.Qty)
			n += g.Qty
		}
		if sum != q.Lines[i].TotalCents || n != l.Qty {
			t.Fatalf("line %d: groups %+v = %d × %d, quote %d", i, gs, n, sum, q.Lines[i].TotalCents)
		}
	}
	// The cheesesteaks: one at its own price, one carrying the combo.
	gs := SoldGroups(q, 0, 2)
	if len(gs) != 2 || gs[0].Coupon != -1 || gs[0].UnitCents != 899 || gs[1].Name != q.Coupons[0].Name || gs[1].UnitCents != 1099 {
		t.Fatalf("cheesesteaks %+v", gs)
	}
	// The fries: one in each coupon, both $0.00.
	gs = SoldGroups(q, 1, 2)
	if len(gs) != 2 || gs[0].Coupon != 0 || gs[1].Coupon != 1 || gs[0].UnitCents != 0 || gs[1].UnitCents != 0 {
		t.Fatalf("fries %+v", gs)
	}
}

// A coupon unit's free regular mods are Included on its options; the rest stay charged.
func TestFreeOptions(t *testing.T) {
	ctx, cart, at := couponCase(t, "55_coupon_cheapest_regular_mods_free.json")
	q := Price(ctx, cart, at)
	g := SoldGroups(q, 0, 1)
	if len(g) != 1 || g[0].Coupon != 0 {
		t.Fatalf("groups %+v", g)
	}
	r := Resolve(ctx, cart.Lines[0].Selection, 0)
	opts := FreeOptions(r.Options, g[0].FreeMods)
	free, charged := 0, int64(0)
	for _, o := range opts {
		if o.Included {
			free++
		} else if !o.RemovedDefault {
			charged += o.PriceCents
		}
	}
	if free != len(g[0].FreeMods) || free != 2 {
		t.Fatalf("free %d of %v", free, g[0].FreeMods)
	}
	for _, o := range r.Options {
		if o.Included {
			t.Fatal("FreeOptions changed its input")
		}
	}
	// The carrier's unit = the coupon price + its charged mods (+ any half and half upcharge).
	if q.Coupons[0].Units[0].ModsCents != charged {
		t.Fatalf("charged %d, quote mods %d", charged, q.Coupons[0].Units[0].ModsCents)
	}
}
