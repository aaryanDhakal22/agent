package terminal

import (
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/pricing"
)

func TestOnlineMatchAndMoney(t *testing.T) {
	items := []order.LineItem{
		{Name: "Pizza", Size: "Large", Quantity: 1, UnitPrice: 1200, LineTotal: 1300, Children: []order.LineItem{{Name: "Pepperoni", Quantity: 1, UnitPrice: 100}}},
		{Name: "Wings", Size: "Serving", Quantity: 3, UnitPrice: 300, LineTotal: 900, Notes: "hot"},
	}
	own := OnlineLines(items)
	if own[0].Name != "Large Pizza" || own[0].Note != "Pepperoni" || own[0].Unit != 1300 || own[1].Name != "Wings" || own[1].Note != "hot" || own[1].Qty != 3 {
		t.Fatalf("own lines: %+v", own)
	}
	cart := OnlineCartLines(items)
	// Keep the pizza, two of the three wings, add a soda and one more pizza (extra quantity = added).
	cart[1].Qty = 2
	cart[0].Qty = 2
	cart = append(cart, pricing.Line{Qty: 1, Selection: pricing.Selection{ItemID: "soda"}})
	m := MatchOnline(own, cart)
	if m.Kept[0] != 1 || m.Kept[1] != 2 || len(m.Added) != 2 || m.Added[0].Qty != 1 || m.AddedFrom[0] != 0 || m.AddedFrom[1] != 2 {
		t.Fatalf("match: %+v", m)
	}
	base := order.OnlineBase{Items: items, PaidCents: 2730,
		Money: order.Money{Subtotal: 2200, Tax: 220, Tip: 300, Total: 2720, Fees: []order.Fee{{Type: "delivery", Amount: 0}}}}
	out := OnlineMoney(OnlineMoneyIn{Base: base, Own: own, Kept: m.Kept, Added: order.Money{Subtotal: 1500, Tax: 150, Total: 1650}})
	if out.RemovedCents != 300 || out.RemovedTax != 30 || out.Money.Total != 2720-330+1650 || out.Money.Tip != 300 ||
		out.Money.Subtotal != 2200-300+1500 || out.Money.Tax != 220-30+150 || len(out.Items) != 2 || out.Items[1].Quantity != 2 || out.Items[1].LineTotal != 600 {
		t.Fatalf("money: %+v", out)
	}
	if owed, due := OnlineOwed(2390, 2720, 0); owed != 330 || due != 0 {
		t.Fatalf("owed %d due %d", owed, due)
	}
	if owed, due := OnlineOwed(3000, 2720, 100); owed != 0 || due != 180 {
		t.Fatalf("owed %d due %d", owed, due)
	}
}
