package pricing

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// couponCase loads a shared coupon case's context and cart.
func couponCase(t *testing.T, file string) (Context, Cart, time.Time) {
	t.Helper()
	raw, err := os.ReadFile("testdata/cases/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var c pricingCase
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.Context, c.Cart, c.At
}

// Overlapping parts: the first part may take a sub or fries, the second
// only a sub. The priciest unit (the sub) goes to the first part first; the
// matching moves it so both parts fill.
func TestCouponPartsOverlap(t *testing.T) {
	ctx, cart, at := couponCase(t, "52_coupon_offered.json")
	sub, fries := ctx.Deals[0].Parts[0].Families[0], ctx.Deals[0].Parts[1].Families[0]
	ctx.Deals = []Deal{{ID: "x", Kind: DealCoupon, Name: "Overlap", Active: true, PriceCents: 1000,
		Parts: []Part{{Families: []string{sub, fries}, Qty: 1}, {Families: []string{sub}, Qty: 1}}}}
	cart.Lines = cart.Lines[:2] // the sub ($8.99) and fries ($3.99)
	q := Price(ctx, cart, at)
	if len(q.CouponOffers) != 1 || q.CouponOffers[0].SavedCents != 298 {
		t.Fatalf("offers %+v", q.CouponOffers)
	}
	cart.Coupons = []CartCoupon{{ID: "x", Lines: []int{1, 0}}}
	q = Price(ctx, cart, at)
	if len(q.Coupons) != 1 {
		t.Fatalf("not applied: %v %v", q.Errors, q.Warnings)
	}
	u := q.Coupons[0].Units
	if u[0].Line != 1 || u[1].Line != 0 || !u[1].Carrier || u[1].ItemCents != 1000 || u[0].ItemCents != 0 {
		t.Fatalf("units %+v", u)
	}
	if q.CouponOffers != nil {
		t.Fatalf("nothing left to offer: %+v", q.CouponOffers)
	}
}

// A coupon names a line that isn't there / a custom line / too many units.
func TestCouponBadLines(t *testing.T) {
	ctx, cart, at := couponCase(t, "52_coupon_offered.json")
	cart.Lines = append(cart.Lines, Line{Custom: true, Name: "Open", Qty: 1, Selection: Selection{PriceCents: ptr(int64(100))}})
	for _, lines := range [][]int{{0, 1, 9}, {0, 1, 3}, {0, 1, 2, 2}, {-1, 1, 2}} {
		c := cart
		c.Coupons = []CartCoupon{{ID: ctx.Deals[0].ID, Lines: lines}}
		q := Price(ctx, c, at)
		if len(q.Coupons) != 0 || len(q.Warnings) != 1 || len(q.Errors) != 0 {
			t.Fatalf("%v: coupons %d, warnings %v, errors %v", lines, len(q.Coupons), q.Warnings, q.Errors)
		}
	}
}

func ptr[T any](v T) *T { return &v }
