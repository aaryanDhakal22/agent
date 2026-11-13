package order

import "testing"

func TestShownOnTablet(t *testing.T) {
	soda := LineItem{Name: "Can Coke", Routing: RoutingNone}
	pizza := LineItem{Name: "Cheese Pizza", Routing: RoutingMatched, PrinterID: "p1"}
	custom := LineItem{Name: "Extra crust", SKU: SKUCustom, Routing: RoutingUnmatched}
	pre := LineItem{Name: "Vendor pizza"} // routing "" (an older order)
	so := func(src, kind string, total int64, items ...LineItem) StoredOrder {
		return StoredOrder{Order: Snapshot{Source: src, DeviceKind: kind, Money: Money{Total: total}, Items: items}}
	}
	delivery := func(total int64, items ...LineItem) StoredOrder {
		o := so(SourceCounter, "", total, items...)
		o.Order.Fulfillment.Type = "delivery"
		return o
	}
	takeout := func(total int64, items ...LineItem) StoredOrder {
		o := so(SourceCounter, "", total, items...)
		o.Order.Fulfillment.Type = "pickup"
		return o
	}
	cases := []struct {
		name string
		o    StoredOrder
		want bool
	}{
		{"terminal soda under $5", so(SourceCounter, "", 135, soda), false},
		{"terminal sodas over $5", so(SourceCounter, "", 673, soda, soda, soda, soda, soda), false},
		{"terminal pizza over $5", so(SourceCounter, "", 1377, pizza), true},
		{"terminal pizza + soda", so(SourceCounter, "", 1646, pizza, soda), true},
		{"terminal kitchen line exactly $5", so(SourceCounter, "", 500, pizza), false},
		{"terminal kitchen line $5.01", so(SourceCounter, "", 501, pizza), true},
		{"terminal kitchen line under $5", so(SourceCounter, "", 80, pizza), false},
		{"terminal custom line over $5", so(SourceCounter, "", 954, custom), true},
		{"terminal pre-routing line over $5", so(SourceCounter, "", 954, pre), true},
		{"terminal no lines", so(SourceCounter, "", 900), false},
		{"terminal takeout soda", takeout(135, soda), false},
		{"terminal takeout pizza", takeout(1377, pizza), true},
		// Deliveries always show (the owner, 2026-11-13).
		{"terminal delivery soda + fee", delivery(535, soda), true},
		{"terminal delivery ranch + fee, under $5", delivery(480, pizza), true},
		{"terminal delivery pizza", delivery(1777, pizza), true},
		{"call center soda", so(SourceCounter, deviceKindCallcenter, 135, soda), true},
		{"online soda", so(SourceOnline, "", 135, soda), true},
		{"old online (no source) soda", so("", "", 135, soda), true},
		{"website soda", so(SourceWeb, "", 135, soda), true},
	}
	for _, c := range cases {
		if got := c.o.ShownOnTablet(500); got != c.want {
			t.Errorf("%s: ShownOnTablet(500) = %v, want %v", c.name, got, c.want)
		}
	}
	// X = 0: any total, the kitchen rule still holds.
	if !so(SourceCounter, "", 80, pizza).ShownOnTablet(0) || so(SourceCounter, "", 900, soda).ShownOnTablet(0) {
		t.Error("X = 0: a kitchen line shows at any total, a drink-only order never")
	}
}
