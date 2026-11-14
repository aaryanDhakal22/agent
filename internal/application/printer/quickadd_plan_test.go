package printer

import (
	"reflect"
	"testing"

	"quiccpos/agent/internal/domain/order"
	"quiccpos/agent/internal/domain/printer"
)

// The owner, 2026-11-14 (#17): a Quicc Add the cashier sent nowhere (stored
// routing none) prints on no printer at all for a counter order (2.5.7: no
// receipt either) and on no other station's ITEMS TO OTHER PRINTER; one sent
// to a printer prints there alone, never on every station.
func TestPlan_QuiccAddPrintsOnlyWherePicked(t *testing.T) {
	roster := []RosterEntry{front, pizza, wings}
	add := func(routing, printerID string) order.LineItem {
		it := item("Pizza dough", routing, printerID)
		it.SKU = order.SKUCustom
		return it
	}
	counter := func(items ...order.LineItem) order.Snapshot {
		return order.Snapshot{Source: order.SourceCounter, Fulfillment: order.Fulfillment{Type: "counter"}, Items: items}
	}
	for _, mode := range []printer.Mode{printer.ModeAccept, printer.ModePlaced, printer.ModeKitchen} {
		if tk, un := PlanFor(counter(add(order.RoutingNone, "")), roster, mode); len(tk) != 0 || un != nil {
			t.Errorf("%s: a Quicc Add sent nowhere printed %+v (unrouted %v)", mode, tk, un)
		}
	}
	tk, _ := PlanFor(counter(item("Pepperoni", order.RoutingMatched, pizza.ID), add(order.RoutingNone, "")), roster, printer.ModePlaced)
	if len(tk) != 1 || len(tk[0].Others) != 0 {
		t.Fatalf("not under ITEMS TO OTHER PRINTER: %+v", tk)
	}
	tk, un := PlanFor(counter(add(order.RoutingMatched, wings.ID)), roster, printer.ModePlaced)
	if un != nil || len(tk) != 1 || tk[0].TargetID != wings.ID || !reflect.DeepEqual(names(tk[0].Mine), []string{"Pizza dough"}) {
		t.Fatalf("on the picked printer alone: %+v %v", tk, un)
	}
}
