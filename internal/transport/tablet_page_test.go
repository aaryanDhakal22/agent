package transport

import (
	"fmt"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/infra/ssebroker"
)

// A tablet's /api/orders pages count the orders it shows (2.5.4): Quibase
// pages by offset until a short page, so a page shortened by the filter
// would end its paging early and skip older orders.
func TestTabletPageCountsShownOrders(t *testing.T) {
	b := ssebroker.New()
	b.SetTabletFilter(func(so order.StoredOrder) bool { return so.ShownOnTablet(500) })
	s := &Server{broker: b}
	var all []order.StoredOrder
	for i := range 1300 { // every third a hidden soda
		so := order.StoredOrder{Order: order.Snapshot{ID: fmt.Sprint(i), Source: order.SourceCounter, Money: order.Money{Total: 1377},
			Items: []order.LineItem{{Routing: order.RoutingMatched}}}}
		if i%3 == 0 {
			so.Order.Money.Total, so.Order.Items[0].Routing = 135, order.RoutingNone
		}
		all = append(all, so)
	}
	page := func(off, lim int) ([]order.StoredOrder, error) {
		if off >= len(all) {
			return nil, nil
		}
		return all[off:min(off+lim, len(all))], nil
	}
	var got []string
	for off := 0; ; {
		p, err := s.tabletPage(page, off, 200)
		if err != nil {
			t.Fatal(err)
		}
		for _, so := range p {
			got = append(got, so.Order.ID)
		}
		if len(p) < 200 {
			break
		}
		off += len(p)
	}
	var want []string
	for i := range 1300 {
		if i%3 != 0 {
			want = append(want, fmt.Sprint(i))
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paged %d shown orders, want %d (in order)", len(got), len(want))
	}
}
