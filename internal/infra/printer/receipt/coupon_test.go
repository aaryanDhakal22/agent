package receipt

import (
	"strings"
	"testing"

	"quiccpos/agent/internal/domain/order"
)

// Coupons (the owner, 2026-11-12): the coupon's name prints over its lines
// (which sit together, whatever the cart order), the lines at their coupon
// prices, no discount line for it; both formats.
func TestBuild_CouponOverItsLines(t *testing.T) {
	snap := previewPickup()
	snap.Items = []order.LineItem{
		{Name: "Cheesesteak", Size: "8\"", Quantity: 1, UnitPrice: 1099, LineTotal: 1099, Coupon: "8\" Sub Combo", CouponNo: 1, RungCents: 899},
		{Name: "Garden Salad", Quantity: 1, UnitPrice: 699, LineTotal: 699},
		{Name: "French Fries", Quantity: 1, UnitPrice: 0, LineTotal: 0, Coupon: "8\" Sub Combo", CouponNo: 1, RungCents: 399},
		{Name: "Coke Can", Quantity: 1, UnitPrice: 0, LineTotal: 0, Coupon: "8\" Sub Combo", CouponNo: 1, RungCents: 150},
		{Name: "Cheesesteak", Size: "8\"", Quantity: 1, UnitPrice: 1099, LineTotal: 1099, Coupon: "8\" Sub Combo", CouponNo: 2, RungCents: 899},
	}
	snap.Money = order.Money{Subtotal: 2897, Tax: 174, Total: 3071}
	for _, format := range []string{order.FormatExpanded, order.FormatCondensed} {
		got := string(Build(snap, previewStore(), receiptTicket(), prefsWith(func(p *order.ReceiptPrefs) { p.Format = format })))
		if n := strings.Count(got, "** 8\" Sub Combo **"); n != 2 {
			t.Fatalf("%s: %d coupon bands:\n%s", format, n, got)
		}
		band := strings.Index(got, "** 8\" Sub Combo **")
		fries, salad := strings.Index(got, "French Fries"), strings.Index(got, "Garden Salad")
		if !(band < fries && fries < salad) {
			t.Fatalf("%s: the coupon's lines aren't together under its band:\n%s", format, got)
		}
		if strings.Contains(strings.ToLower(got), "coupon:") || strings.Contains(strings.ToLower(got), "saved") {
			t.Fatalf("%s: a coupon is not a discount:\n%s", format, got)
		}
	}
}
