package terminal

import (
	"encoding/json"
	"os"
	"testing"

	"quiccpos/agent/internal/domain/pricing"
	"quiccpos/agent/internal/domain/terminal"
)

// Coupons on the Pi (the owner, 2026-11-12): a coupon is not a discount, it
// re-prices its lines. The order's lines carry the coupon prices (one line
// per price a cart line sold at, the coupon's name on its lines), the
// subtotal is their sum, no discount comes from a coupon and the money still
// adds up.

// couponMenu sends the shared coupon cases' context as the counter menu and
// returns case file's cart.
func (r *rig) couponMenu(t *testing.T, version int64, file string, fn func(ctx map[string]any)) pricing.Cart {
	t.Helper()
	raw, err := os.ReadFile("../../domain/pricing/testdata/cases/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Context map[string]any `json:"context"`
		Cart    pricing.Cart   `json:"cart"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if fn != nil {
		fn(c.Context)
	}
	r.main.set(PathCounterMenu, map[string]any{"menu_version": version, "build": map[string]any{"view": map[string]any{"kind": "counter"}},
		"pricing": c.Context, "routes": map[string]any{}})
	if changed, err := r.svc.Store().Refresh(r.ctx, terminal.KindCounterMenu); err != nil || !changed {
		t.Fatalf("menu refresh: %v %v", changed, err)
	}
	return c.Cart
}

func TestCouponOrderLinesAddUp(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	cart := r.couponMenu(t, 50, "57_two_coupons.json", nil)
	v := r.quote(t, Cart{Cart: cart})
	q := rq(v)
	if len(q.Coupons) != 2 || len(q.Discounts) != 0 {
		t.Fatalf("quote coupons %d discounts %+v warnings %v", len(q.Coupons), q.Discounts, q.Warnings)
	}
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: Cart{Cart: cart}})
	if err != nil {
		t.Fatal(err)
	}
	o := res.Order.Order
	var sum int64
	coupons := map[int]string{}
	for _, it := range o.Items {
		sum += it.LineTotal
		var parts int64
		for _, c := range it.Children {
			parts += c.LineTotal
		}
		if it.UnitPrice*int64(it.Quantity)+parts != it.LineTotal {
			t.Fatalf("line %s doesn't add up: %d × %d + %d != %d", it.Name, it.UnitPrice, it.Quantity, parts, it.LineTotal)
		}
		if it.Coupon != "" {
			coupons[it.CouponNo] = it.Coupon
			if it.RungCents <= 0 {
				t.Fatalf("coupon line %s without its own price", it.Name)
			}
		}
	}
	if sum != o.Money.Subtotal || o.Money.Subtotal != q.SubtotalCents || o.Money.Total != q.TotalCents || len(o.Money.Discounts) != 0 {
		t.Fatalf("money: lines %d, %+v, quote %d / %d", sum, o.Money, q.SubtotalCents, q.TotalCents)
	}
	if coupons[1] != q.Coupons[0].Name || coupons[2] != q.Coupons[1].Name {
		t.Fatalf("coupon names on the lines: %v", coupons)
	}
	// The two cheesesteaks: one at its own price, one carrying the combo.
	if o.Items[0].Coupon != "" || o.Items[0].LineTotal != 899 || o.Items[1].Coupon == "" || o.Items[1].LineTotal != 1099 || o.Items[1].RungCents != 899 {
		t.Fatalf("cheesesteaks %+v / %+v", o.Items[0], o.Items[1])
	}
}

// A kept line (its menu price moved after SEND) keeps its coupon on an
// edit: the kept price is the Pi's keep_price, the line's own price (never
// the coupon's), and the coupon re-prices it again.
func TestKeptLineKeepsItsCoupon(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	cart := r.couponMenu(t, 50, "54_coupon_highest_item_carries.json", nil)
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: Cart{Cart: cart}})
	if err != nil {
		t.Fatal(err)
	}
	id, before := res.Order.Order.ID, res.Order.Order.Money
	// The cheesesteak and the can go up a dollar (the items only: a coupon
	// unit's charged mods are at today's prices).
	r.couponMenu(t, 51, "54_coupon_highest_item_carries.json", func(ctx map[string]any) {
		for _, c := range ctx["menu"].(map[string]any)["categories"].([]any) {
			for _, it := range c.(map[string]any)["items"].([]any) {
				m := it.(map[string]any)
				if m["id"] != "00000000-0000-0000-0000-0000000000a1" && m["id"] != "00000000-0000-0000-0000-0000000000a3" {
					continue
				}
				m["price_cents"] = m["price_cents"].(float64) + 100
				for _, sz := range asList(m["sizes"]) {
					sz.(map[string]any)["price_cents"] = sz.(map[string]any)["price_cents"].(float64) + 100
				}
			}
		}
	})
	v, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: cart}, OrderID: id})
	if err != nil {
		t.Fatal(err)
	}
	q := rq(v)
	if len(q.Coupons) != 1 || len(q.Warnings) != 0 || len(v.KeptPrices) == 0 {
		t.Fatalf("kept lines lost their coupon: coupons %d warnings %v kept %+v", len(q.Coupons), q.Warnings, v.KeptPrices)
	}
	if q.SubtotalCents != before.Subtotal || q.TotalCents != before.Total {
		t.Fatalf("an edit with nothing new moved the money: %d / %d, was %d / %d", q.SubtotalCents, q.TotalCents, before.Subtotal, before.Total)
	}
	cart.Lines = append(cart.Lines, pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: "00000000-0000-0000-0000-0000000000a8"}})
	ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: cart}})
	if err != nil {
		t.Fatal(err)
	}
	o := ch.Order.Order
	var sum int64
	n := 0
	for _, it := range o.Items {
		sum += it.LineTotal
		if it.Coupon != "" {
			n++
		}
	}
	if n != 3 || sum != o.Money.Subtotal || o.Money.Subtotal != before.Subtotal+699 || len(o.Money.Discounts) != 0 {
		t.Fatalf("after the edit: %d coupon lines, lines %d, %+v (was %+v)", n, sum, o.Money, before)
	}
	if len(ch.Added) != 1 || len(ch.Removed) != 0 {
		t.Fatalf("kitchen change: +%v -%v", ch.Added, ch.Removed)
	}
	// Journal: applied once (at SEND), the edit that kept it says nothing; taking it off is coupon.removed.
	if j := journalOf(t, r, terminal.ActionCouponApplied); len(j) != 1 || j[0]["name"] != "8\" Sub Combo" {
		t.Fatalf("coupon.applied %v", j)
	}
	cart.Coupons = nil
	if _, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: cart}}); err != nil {
		t.Fatal(err)
	}
	if j := journalOf(t, r, terminal.ActionCouponRemoved); len(j) != 1 || j[0]["name"] != "8\" Sub Combo" {
		t.Fatalf("coupon.removed %v", j)
	}
}

// The price doesn't change under an order (the owner): on an edited sent
// order a coupon unit's charged toppings keep their rung prices too (the
// order's pick_cents → the engine's kept option prices), not today's.
func TestKeptCouponToppingsKeepTheirPrices(t *testing.T) {
	r := newRig(t)
	r.login(t, till, "1234")
	cart := r.couponMenu(t, 60, "56_coupon_premium_never_free.json", nil)
	res, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: Cart{Cart: cart}})
	if err != nil {
		t.Fatal(err)
	}
	id, before := res.Order.Order.ID, res.Order.Order.Money
	if before.Subtotal != 1849 { // the pizza carries $14.99 + Steak $3.50, the cans $0.00
		t.Fatalf("placed %+v", before)
	}
	// The pizza and every topping go up a dollar (Steak $3.50 → $4.50).
	r.couponMenu(t, 61, "56_coupon_premium_never_free.json", func(ctx map[string]any) {
		raise("00000000-0000-0000-0000-0000000000a5", 100)(ctx["menu"].(map[string]any))
	})
	v, err := r.svc.Quote(r.ctx, till, QuoteIn{Cart: Cart{Cart: cart}, OrderID: id})
	if err != nil {
		t.Fatal(err)
	}
	q := rq(v)
	if len(q.Coupons) != 1 || q.Coupons[0].Units[0].ModsCents != 350 || q.SubtotalCents != before.Subtotal || q.TotalCents != before.Total {
		t.Fatalf("the coupon pizza's Steak moved: coupons %+v, %d / %d (was %d / %d)", q.Coupons, q.SubtotalCents, q.TotalCents,
			before.Subtotal, before.Total)
	}
	// An edit: the Steak stays $3.50 on the order's lines and in the next edit too.
	for n := 0; n < 2; n++ {
		cart.Lines = append(cart.Lines, pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: "00000000-0000-0000-0000-0000000000a8"}})
		ch, err := r.svc.ChangeCart(r.ctx, till, id, ChangeIn{ClientKey: tk(), Cart: Cart{Cart: cart}})
		if err != nil {
			t.Fatal(err)
		}
		o := ch.Order.Order
		steak := int64(-1)
		for _, it := range o.Items {
			for _, c := range it.Children {
				if c.Name == "Steak" {
					steak = c.UnitPrice
				}
			}
		}
		if steak != 350 || o.Money.Subtotal != before.Subtotal+int64(n+1)*699 {
			t.Fatalf("edit %d: Steak %d, %+v", n, steak, o.Money)
		}
	}
	// A new order prices today's Steak.
	res2, err := r.svc.PlaceOrder(r.ctx, till, OrderIn{ClientKey: tk(), Type: "counter", Cart: Cart{Cart: cart}})
	if err != nil || res2.Order.Order.Money.Subtotal == before.Subtotal+2*699 {
		t.Fatalf("a new order kept the old prices: %v %+v", err, res2.Order.Order.Money)
	}
}
